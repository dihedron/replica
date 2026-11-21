package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// The operator's actions on the KEKs (§4). They run as veho_migrator, which
// owns veho.kek and reads the secrets of every tenant.

// KEKInfo is a row of veho.kek, with the number of DEKs it wraps.
type KEKInfo struct {
	Version     int
	State       string
	UnwrapKeyID string
	CreatedAt   time.Time
	RetiredAt   *time.Time
	Secrets     int
}

// ShowKEKs lists the KEK versions.
func ShowKEKs(ctx context.Context, conn *pgx.Conn) ([]KEKInfo, error) {
	rows, err := conn.Query(ctx, `select k.version, k.state, k.unwrap_key_id, k.created_at, k.retired_at,
		(select count(*) from veho.secrets s where s.kek_version = k.version)::int
		from veho.kek k order by k.version`)
	if err != nil {
		return nil, fmt.Errorf("secrets: show the KEKs: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[KEKInfo])
	if err != nil {
		return nil, fmt.Errorf("secrets: show the KEKs: %w", err)
	}
	return out, nil
}

// ErrInitialized refuses InitKEK on a table that already has KEKs.
var ErrInitialized = errors.New("the KEKs are already initialised")

// InitKEK creates KEK version 1, active, wrapped by src's key.
func InitKEK(ctx context.Context, conn *pgx.Conn, src UnwrapKeySource) error {
	key, err := src.Load(ctx)
	if err != nil {
		return err
	}
	defer clear(key)
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "lock table veho.kek in exclusive mode"); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, "select count(*) from veho.kek").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrInitialized
		}
		return insertKEK(ctx, tx, key, 1)
	})
	if err != nil {
		return fmt.Errorf("secrets: init: %w", err)
	}
	slog.InfoContext(ctx, "KEK created", "kek_version", 1, "unwrap_key_id", keyID(key))
	return nil
}

// insertKEK adds a new active KEK version, wrapped by key.
func insertKEK(ctx context.Context, tx pgx.Tx, key []byte, version int) error {
	kek := newKey()
	defer clear(kek)
	wrapped, err := seal(key, kek, kekAAD(version))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "insert into veho.kek (version, wrapped, unwrap_key_id, state) values ($1, $2, $3, 'active')", version, wrapped, keyID(key))
	return err
}

// RotateKEK creates KEK version n+1, active, and makes the active one
// retiring (SEC-27); it returns the new version. Of two concurrent rotates,
// one fails and changes nothing.
func RotateKEK(ctx context.Context, conn *pgx.Conn, src UnwrapKeySource) (int, error) {
	key, err := src.Load(ctx)
	if err != nil {
		return 0, err
	}
	defer clear(key)
	var version int
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// waits for the Puts that read the active KEK, and holds the next
		// ones until the new KEK is visible (§3)
		if _, err := tx.Exec(ctx, "lock table veho.secrets in share mode"); err != nil {
			return err
		}
		rows, err := readKEKs(ctx, tx)
		if err != nil {
			return err
		}
		kek, err := unwrapKEKs(key, rows) // the key is the one of the rows
		if err != nil {
			return err
		}
		for _, k := range kek {
			clear(k)
		}
		if err := tx.QueryRow(ctx, "select max(version) + 1 from veho.kek").Scan(&version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "update veho.kek set state = 'retiring' where state = 'active'"); err != nil {
			return err
		}
		return insertKEK(ctx, tx, key, version)
	})
	if err != nil {
		return 0, fmt.Errorf("secrets: rotate: %w", err)
	}
	slog.InfoContext(ctx, "KEK rotated", "kek_version", version)
	return version, nil
}

// RewrapResult is what RewrapDEKs did.
type RewrapResult struct {
	// Rewrapped DEKs, and Skipped ones (their secret changed meanwhile).
	Rewrapped, Skipped int
	// Retired KEK versions.
	Retired []int
}

// RewrapDEKs re-wraps, batch by batch, the DEKs that are not under the
// active KEK, then retires the retiring versions that wrap none (SEC-27).
// It can be interrupted and run again.
func RewrapDEKs(ctx context.Context, conn *pgx.Conn, src UnwrapKeySource, batch int) (RewrapResult, error) {
	var res RewrapResult
	if batch <= 0 {
		return res, errors.New("secrets: rewrap: the batch must be positive")
	}
	ring, err := LoadKeyring(ctx, conn, src)
	if err != nil {
		return res, err
	}
	defer func() {
		for _, k := range ring.kek {
			clear(k)
		}
	}()
	type dek struct {
		Tenant              uuid.UUID
		Name                string
		Version, KEKVersion int
		Wrapped             []byte
	}
	for {
		var todo []dek
		rewrapped := 0
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			var active int
			if err := tx.QueryRow(ctx, "select version from veho.kek where state = 'active'").Scan(&active); err != nil {
				return fmt.Errorf("the active KEK: %w", err)
			}
			rows, err := tx.Query(ctx, `select tenant_id, name, version, kek_version, wrapped_dek from veho.secrets
				where kek_version <> $1 order by tenant_id, name limit $2`, active, batch)
			if err != nil {
				return err
			}
			if todo, err = pgx.CollectRows(rows, pgx.RowToStructByPos[dek]); err != nil {
				return err
			}
			for _, d := range todo {
				plain, err := ring.UnwrapDEK(ctx, d.KEKVersion, dekAAD(d.Tenant, d.Name, d.Version, d.KEKVersion), d.Wrapped)
				if err != nil {
					return fmt.Errorf("tenant %s, secret %s: %w", d.Tenant, d.Name, err)
				}
				wrapped, err := ring.WrapDEK(ctx, active, dekAAD(d.Tenant, d.Name, d.Version, active), plain)
				clear(plain)
				if err != nil {
					return err
				}
				tag, err := tx.Exec(ctx, `update veho.secrets set kek_version = $1, wrapped_dek = $2
					where tenant_id = $3 and name = $4 and version = $5 and kek_version = $6`,
					active, wrapped, d.Tenant, d.Name, d.Version, d.KEKVersion)
				if err != nil {
					return err
				}
				rewrapped += int(tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("secrets: rewrap: %w", err)
		}
		res.Rewrapped += rewrapped
		res.Skipped += len(todo) - rewrapped
		if len(todo) == 0 {
			break
		}
		if rewrapped == 0 {
			return res, errors.New("secrets: rewrap: a batch made no progress")
		}
	}
	rows, err := conn.Query(ctx, `update veho.kek k set state = 'retired', retired_at = now()
		where state = 'retiring' and not exists (select 1 from veho.secrets s where s.kek_version = k.version)
		returning version`)
	if err != nil {
		return res, fmt.Errorf("secrets: retire: %w", err)
	}
	if res.Retired, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
		return res, fmt.Errorf("secrets: retire: %w", err)
	}
	slog.InfoContext(ctx, "DEKs re-wrapped", "rewrapped", res.Rewrapped, "skipped", res.Skipped, "retired", res.Retired)
	return res, nil
}

// RekeyKEKs re-wraps the KEKs that are not retired with the key of next
// (SEC-27: a change of the unwrap key).
func RekeyKEKs(ctx context.Context, conn *pgx.Conn, current, next UnwrapKeySource) error {
	oldKey, err := current.Load(ctx)
	if err != nil {
		return err
	}
	defer clear(oldKey)
	newKey, err := next.Load(ctx)
	if err != nil {
		return err
	}
	defer clear(newKey)
	if keyID(oldKey) == keyID(newKey) {
		return errors.New("secrets: rekey: the new unwrap key is the current one")
	}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "lock table veho.kek in exclusive mode"); err != nil {
			return err
		}
		rows, err := readKEKs(ctx, tx)
		if err != nil {
			return err
		}
		kek, err := unwrapKEKs(oldKey, rows)
		if err != nil {
			return err
		}
		defer func() {
			for _, k := range kek {
				clear(k)
			}
		}()
		for v, k := range kek {
			wrapped, err := seal(newKey, k, kekAAD(v))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "update veho.kek set wrapped = $1, unwrap_key_id = $2 where version = $3", wrapped, keyID(newKey), v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("secrets: rekey: %w", err)
	}
	slog.InfoContext(ctx, "KEKs re-wrapped with a new unwrap key", "unwrap_key_id", keyID(newKey))
	return nil
}
