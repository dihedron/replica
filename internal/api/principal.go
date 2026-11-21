// Package api serves veho's API (.specs/veho-api-server.md): gRPC and the
// REST gateway generated from api/veho/v1, on one TLS port, behind one chain
// of interceptors that authenticates, authorises and audits every call.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The roles, in increasing order of what they may do (SEC-4).
const (
	RoleViewer        = "viewer"
	RoleOperator      = "operator"
	RoleAdmin         = "admin"
	RolePlatformAdmin = "platform-admin"
)

var rank = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// Principal is the authenticated caller.
type Principal struct {
	ID   uuid.UUID
	Name string
	// Tenant is nil for a platform administrator.
	Tenant *uuid.UUID
	Role   string
}

// may is true when p holds role in its tenant.
func (p Principal) may(role string) bool {
	if role == RolePlatformAdmin {
		return p.Role == RolePlatformAdmin
	}
	return p.Tenant != nil && rank[p.Role] >= rank[role]
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principal is the caller of ctx; the interceptors guarantee one.
func principal(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// The format of an API key: veho_<id>_<secret> (§4.2).
const (
	keyPrefix   = "veho_"
	keyIDLen    = 12
	secretBytes = 32
)

// NewKey returns a new key, its ID and the hash of its secret, which is all
// the database keeps.
func NewKey() (token, id string, sum []byte) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, keyIDLen)
	_, _ = rand.Read(b) // never fails
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	secret := make([]byte, secretBytes)
	_, _ = rand.Read(secret)
	id = string(b)
	s := sha256.Sum256(secret)
	return keyPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret), id, s[:]
}

// errBadKey is every refused key, whatever the reason: the caller learns
// nothing about which part was wrong.
var errBadKey = errors.New("invalid credentials")

// parseKey splits a key into its ID and secret.
func parseKey(token string) (string, []byte, error) {
	rest, ok := strings.CutPrefix(token, keyPrefix)
	if !ok || len(rest) <= keyIDLen+1 || rest[keyIDLen] != '_' {
		return "", nil, errBadKey
	}
	secret, err := base64.RawURLEncoding.DecodeString(rest[keyIDLen+1:])
	if err != nil || len(secret) != secretBytes {
		return "", nil, errBadKey
	}
	return rest[:keyIDLen], secret, nil
}

// authenticate returns the principal of an API key.
func authenticate(ctx context.Context, pool *pgxpool.Pool, token string) (Principal, error) {
	id, secret, err := parseKey(token)
	if err != nil {
		return Principal{}, err
	}
	var (
		p     Principal
		sum   []byte
		valid bool
	)
	err = pool.QueryRow(ctx, "select principal_id, tenant_id, name, role, sha256, valid from veho.authenticate_key($1)", id).
		Scan(&p.ID, &p.Tenant, &p.Name, &p.Role, &sum, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, errBadKey
	}
	if err != nil {
		return Principal{}, fmt.Errorf("api: authenticate: %w", err)
	}
	given := sha256.Sum256(secret)
	if subtle.ConstantTimeCompare(given[:], sum) != 1 || !valid {
		return Principal{}, errBadKey
	}
	return p, nil
}
