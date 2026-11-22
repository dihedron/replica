package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"
)

// KEK wraps and unwraps data keys (SEC-29). The Keyring is the loaded
// provider; a remote one (Vault Transit, an HSM) comes later.
type KEK interface {
	WrapDEK(ctx context.Context, version int, aad, dek []byte) ([]byte, error)
	UnwrapDEK(ctx context.Context, version int, aad, wrapped []byte) ([]byte, error)
}

// Querier is a pool or a connection.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// kekRow is a row of veho.kek.
type kekRow struct {
	Version     int
	Wrapped     []byte
	UnwrapKeyID string
	State       string
}

// Keyring is the loaded KEK provider: it holds the active and retiring KEKs
// in memory, and reloads them when it meets a version it does not hold.
type Keyring struct {
	q   Querier
	src UnwrapKeySource
	mu  sync.RWMutex
	kek map[int][]byte
}

// LoadKeyring unwraps the KEKs of veho.kek with the source's key, and fails
// unless every one unwraps and one is active (SEC-26: the node fails fast).
func LoadKeyring(ctx context.Context, q Querier, src UnwrapKeySource) (*Keyring, error) {
	k := &Keyring{q: q, src: src}
	if err := k.load(ctx); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *Keyring) load(ctx context.Context) error {
	rows, err := readKEKs(ctx, k.q)
	if err != nil {
		return err
	}
	key, err := k.src.Load(ctx)
	if err != nil {
		return err
	}
	defer clear(key)
	kek, err := unwrapKEKs(key, rows)
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.kek = kek
	k.mu.Unlock()
	return nil
}

// readKEKs reads the rows that are not retired.
func readKEKs(ctx context.Context, q Querier) ([]kekRow, error) {
	rows, err := q.Query(ctx, "select version, wrapped, unwrap_key_id, state from veho.kek where state <> 'retired' order by version")
	if err != nil {
		return nil, fmt.Errorf("secrets: read the KEKs: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[kekRow])
	if err != nil {
		return nil, fmt.Errorf("secrets: read the KEKs: %w", err)
	}
	return out, nil
}

// unwrapKEKs unwraps rows with key; it fails unless one is active and every
// one unwraps.
func unwrapKEKs(key []byte, rows []kekRow) (map[int][]byte, error) {
	id := keyID(key)
	kek := map[int][]byte{}
	active := 0
	for _, r := range rows {
		if r.UnwrapKeyID != id {
			return nil, fmt.Errorf("secrets: KEK version %d needs the unwrap key %s, the key loaded is %s", r.Version, r.UnwrapKeyID, id)
		}
		k, err := open(key, r.Wrapped, kekAAD(r.Version))
		if err != nil {
			return nil, fmt.Errorf("secrets: KEK version %d: %w", r.Version, err)
		}
		kek[r.Version] = k
		if r.State == "active" {
			active++
		}
	}
	if active != 1 {
		return nil, fmt.Errorf("secrets: %d active KEKs, want 1: run veho kek init", active)
	}
	return kek, nil
}

// key returns KEK version v, reloading once if it is not held (§3: a KEK
// created after the node started).
func (k *Keyring) key(ctx context.Context, v int) ([]byte, error) {
	k.mu.RLock()
	key, ok := k.kek[v]
	k.mu.RUnlock()
	if ok {
		return key, nil
	}
	slog.InfoContext(ctx, "KEK version not held: reloading the KEKs", "kek_version", v)
	if err := k.load(ctx); err != nil {
		return nil, err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if key, ok = k.kek[v]; !ok {
		return nil, fmt.Errorf("secrets: no KEK version %d", v)
	}
	return key, nil
}

// WrapDEK implements KEK.
func (k *Keyring) WrapDEK(ctx context.Context, version int, ad, dek []byte) ([]byte, error) {
	key, err := k.key(ctx, version)
	if err != nil {
		return nil, err
	}
	return seal(key, dek, ad)
}

// UnwrapDEK implements KEK.
func (k *Keyring) UnwrapDEK(ctx context.Context, version int, ad, wrapped []byte) ([]byte, error) {
	key, err := k.key(ctx, version)
	if err != nil {
		return nil, err
	}
	dek, err := open(key, wrapped, ad)
	if err != nil {
		return nil, fmt.Errorf("secrets: data key under KEK version %d: %w", version, err)
	}
	return dek, nil
}
