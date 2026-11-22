package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxValueSize bounds a secret's value (§3).
const MaxValueSize = 64 << 10

// RefPrefix starts an endpoint's credential_ref to a secret of the store.
const RefPrefix = "db://"

var (
	// ErrUnavailable wraps every failure of Fetch: the attempt is Transient
	// (SEC-10).
	ErrUnavailable = errors.New("secret unavailable")
	// ErrNotFound is a secret that does not exist.
	ErrNotFound = errors.New("secret not found")
	// ErrInUse refuses to delete a secret an endpoint references.
	ErrInUse = errors.New("secret used by an endpoint")
	// ErrInvalid is a name or a value the store refuses.
	ErrInvalid = errors.New("invalid secret")
)

// the CHECK of veho.secrets.name
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Meta is what the store shows of a secret: never its value (SEC-9).
type Meta struct {
	Name       string
	Version    int
	KEKVersion int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store is the secret service. Put, List, Show and Delete run on the API's
// pool, Fetch on the worker's; a nil pool fails its methods. Put and Delete
// are audited after their commit (veho-audit.md §4).
type Store struct {
	kek         KEK
	api, worker *pgxpool.Pool
	audit       audit.Auditor
}

// NewStore returns the store of kek on the pools of veho_api and veho_worker.
func NewStore(kek KEK, api, worker *pgxpool.Pool, aud audit.Auditor) *Store {
	return &Store{kek: kek, api: api, worker: worker, audit: aud}
}

// emit audits an action on the secret name.
func (s *Store) emit(ctx context.Context, action string, tenant uuid.UUID, name string, err error, details any) {
	outcome := audit.Success
	if err != nil {
		outcome, details = audit.Failed, map[string]string{"error": err.Error()}
	}
	s.audit.Emit(ctx, audit.Event{Action: action, Outcome: outcome, TenantID: tenant.String(), ResourceType: "secret", ResourceName: name, Details: details})
}

var errNoPool = errors.New("secrets: no pool for this role")

// sealValue encrypts value under a new DEK, and wraps the DEK (§1).
func sealValue(ctx context.Context, kek KEK, tenant uuid.UUID, name string, version, kekVersion int, value []byte) (wrappedDEK, ciphertext []byte, err error) {
	dek := newKey()
	defer clear(dek)
	if ciphertext, err = seal(dek, value, valueAAD(tenant, name, version)); err != nil {
		return nil, nil, err
	}
	if wrappedDEK, err = kek.WrapDEK(ctx, kekVersion, dekAAD(tenant, name, version, kekVersion), dek); err != nil {
		return nil, nil, err
	}
	return wrappedDEK, ciphertext, nil
}

// openValue is the reverse of sealValue.
func openValue(ctx context.Context, kek KEK, tenant uuid.UUID, name string, version, kekVersion int, wrappedDEK, ciphertext []byte) ([]byte, error) {
	dek, err := kek.UnwrapDEK(ctx, kekVersion, dekAAD(tenant, name, version, kekVersion), wrappedDEK)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	value, err := open(dek, ciphertext, valueAAD(tenant, name, version))
	if err != nil {
		return nil, fmt.Errorf("secrets: value: %w", err)
	}
	return value, nil
}

// Put creates or replaces a secret of tenant, under a new DEK wrapped by the
// active KEK.
func (s *Store) Put(ctx context.Context, tenant uuid.UUID, name string, value []byte) (Meta, error) {
	switch {
	case s.api == nil:
		return Meta{}, errNoPool
	case !validName.MatchString(name):
		return Meta{}, fmt.Errorf("%w: name %q: want %s", ErrInvalid, name, validName)
	case len(value) == 0 || len(value) > MaxValueSize:
		return Meta{}, fmt.Errorf("%w: a value of %d bytes: want 1 to %d", ErrInvalid, len(value), MaxValueSize)
	}
	var m Meta
	err := store.InTenant(ctx, s.api, tenant, func(tx pgx.Tx) error {
		// before reading the active KEK: a rotate (SHARE) waits for this
		// Put, or this Put for the rotate (§3)
		if _, err := tx.Exec(ctx, "lock table veho.secrets in row exclusive mode"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('veho.secret/' || $1 || '/' || $2))", tenant.String(), name); err != nil {
			return err
		}
		version := 0
		if err := tx.QueryRow(ctx, "select version from veho.secrets where name = $1", name).Scan(&version); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		version++
		var kekVersion int
		if err := tx.QueryRow(ctx, "select version from veho.kek where state = 'active'").Scan(&kekVersion); err != nil {
			return fmt.Errorf("the active KEK: %w", err)
		}
		wrappedDEK, ciphertext, err := sealValue(ctx, s.kek, tenant, name, version, kekVersion, value)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `insert into veho.secrets (tenant_id, name, version, kek_version, wrapped_dek, ciphertext)
			values (veho.current_tenant(), $1, $2, $3, $4, $5)
			on conflict (tenant_id, name) do update set version = excluded.version, kek_version = excluded.kek_version,
				wrapped_dek = excluded.wrapped_dek, ciphertext = excluded.ciphertext, updated_at = now()
			returning name, version, kek_version, created_at, updated_at`,
			name, version, kekVersion, wrappedDEK, ciphertext).Scan(&m.Name, &m.Version, &m.KEKVersion, &m.CreatedAt, &m.UpdatedAt)
	})
	if err != nil {
		slog.ErrorContext(ctx, "cannot write the secret", "tenant_id", tenant.String(), "secret", name, "error", err)
		err = fmt.Errorf("secrets: put %s: %w", name, err)
		s.emit(ctx, "SECRET_PUT", tenant, name, err, nil)
		return Meta{}, err
	}
	s.emit(ctx, "SECRET_PUT", tenant, name, nil, map[string]int{"version": m.Version, "kek_version": m.KEKVersion})
	return m, nil
}

const metaColumns = "name, version, kek_version, created_at, updated_at"

// List shows the tenant's secrets, by name.
func (s *Store) List(ctx context.Context, tenant uuid.UUID) ([]Meta, error) {
	if s.api == nil {
		return nil, errNoPool
	}
	var out []Meta
	err := store.InTenant(ctx, s.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+metaColumns+" from veho.secrets order by name")
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Meta])
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "cannot list the secrets", "tenant_id", tenant.String(), "error", err)
		return nil, fmt.Errorf("secrets: list: %w", err)
	}
	return out, nil
}

// Show shows one secret.
func (s *Store) Show(ctx context.Context, tenant uuid.UUID, name string) (Meta, error) {
	if s.api == nil {
		return Meta{}, errNoPool
	}
	var m Meta
	err := store.InTenant(ctx, s.api, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+metaColumns+" from veho.secrets where name = $1", name)
		if err != nil {
			return err
		}
		m, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Meta])
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Meta{}, fmt.Errorf("secrets: %s: %w", name, ErrNotFound)
	}
	if err != nil {
		slog.ErrorContext(ctx, "cannot show the secret", "tenant_id", tenant.String(), "secret", name, "error", err)
		return Meta{}, fmt.Errorf("secrets: show %s: %w", name, err)
	}
	return m, nil
}

// Delete removes a secret no endpoint references.
func (s *Store) Delete(ctx context.Context, tenant uuid.UUID, name string) error {
	if s.api == nil {
		return errNoPool
	}
	var deleted int64
	err := store.InTenant(ctx, s.api, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "delete from veho.secrets where name = $1", name)
		deleted = tag.RowsAffected()
		return err
	})
	switch pgErr, ok := errors.AsType[*pgconn.PgError](err); {
	case ok && pgErr.Code == "23503":
		err = fmt.Errorf("secrets: %s: %w", name, ErrInUse)
	case err != nil:
		slog.ErrorContext(ctx, "cannot delete the secret", "tenant_id", tenant.String(), "secret", name, "error", err)
		err = fmt.Errorf("secrets: delete %s: %w", name, err)
	case deleted == 0:
		err = fmt.Errorf("secrets: %s: %w", name, ErrNotFound)
	}
	s.emit(ctx, "SECRET_DELETE", tenant, name, err, nil)
	return err
}

// Fetch decrypts the secret of ref ("db://<name>") for one attempt; the
// caller defers its Zero. Every failure wraps ErrUnavailable (SEC-10).
func (s *Store) Fetch(ctx context.Context, tenant uuid.UUID, ref string) (*Secret, error) {
	name, ok := strings.CutPrefix(ref, RefPrefix)
	fail := func(err error) (*Secret, error) {
		slog.WarnContext(ctx, "cannot fetch the secret", "tenant_id", tenant.String(), "secret", name, "error", err)
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, name, err)
	}
	if !ok {
		return fail(fmt.Errorf("%w: reference %q: want %s<name>", ErrInvalid, ref, RefPrefix))
	}
	if s.worker == nil {
		return fail(errNoPool)
	}
	var (
		version, kekVersion    int
		wrappedDEK, ciphertext []byte
	)
	err := store.InTenant(ctx, s.worker, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select version, kek_version, wrapped_dek, ciphertext from veho.secrets where name = $1", name).
			Scan(&version, &kekVersion, &wrappedDEK, &ciphertext)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(err)
	}
	value, err := openValue(ctx, s.kek, tenant, name, version, kekVersion, wrappedDEK, ciphertext)
	if err != nil {
		return fail(fmt.Errorf("version %d: %w", version, err))
	}
	return &Secret{b: value}, nil
}

// Rewrap re-wraps up to batch DEKs that are not under the active KEK, across
// tenants, as veho_worker through SECURITY DEFINER functions, and then, when
// none is left, retires the KEK versions no secret uses (veho-secrets.md §6,
// backlog 24): the background twin of veho kek rewrap, for housekeeping. A
// row replaced or re-wrapped meanwhile is left as it is.
func (s *Store) Rewrap(ctx context.Context, batch int) (rewrapped int, retired []int, err error) {
	type dek struct {
		Tenant              uuid.UUID
		Name                string
		Version, KEKVersion int
		Wrapped             []byte
		Active              int
	}
	rows, err := s.worker.Query(ctx, "select tenant_id, name, version, kek_version, wrapped_dek, active from veho.deks_to_rewrap($1)", batch)
	if err != nil {
		return 0, nil, fmt.Errorf("secrets: rewrap: %w", err)
	}
	todo, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dek])
	if err != nil {
		return 0, nil, fmt.Errorf("secrets: rewrap: %w", err)
	}
	for _, d := range todo {
		plain, err := s.kek.UnwrapDEK(ctx, d.KEKVersion, dekAAD(d.Tenant, d.Name, d.Version, d.KEKVersion), d.Wrapped)
		if err != nil {
			return rewrapped, nil, fmt.Errorf("secrets: rewrap: tenant %s, secret %s: %w", d.Tenant, d.Name, err)
		}
		wrapped, err := s.kek.WrapDEK(ctx, d.Active, dekAAD(d.Tenant, d.Name, d.Version, d.Active), plain)
		clear(plain)
		if err != nil {
			return rewrapped, nil, fmt.Errorf("secrets: rewrap: %w", err)
		}
		var ok bool
		if err := s.worker.QueryRow(ctx, "select veho.rewrap_dek($1, $2, $3, $4, $5, $6)",
			d.Tenant, d.Name, d.Version, d.KEKVersion, d.Active, wrapped).Scan(&ok); err != nil {
			return rewrapped, nil, fmt.Errorf("secrets: rewrap: %w", err)
		}
		if ok {
			rewrapped++
		}
	}
	if len(todo) > 0 {
		return rewrapped, nil, nil
	}
	rows, err = s.worker.Query(ctx, "select veho.retire_keks()")
	if err != nil {
		return 0, nil, fmt.Errorf("secrets: retire: %w", err)
	}
	if retired, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
		return 0, nil, fmt.Errorf("secrets: retire: %w", err)
	}
	return 0, retired, nil
}
