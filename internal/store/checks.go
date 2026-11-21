package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/dihedron/veho/internal/store/migrate"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The failures of the start-up checks (.specs/veho-store.md §7).
var (
	// ErrNotPrimary: STO-C1, the session is on a standby.
	ErrNotPrimary = errors.New("STO-C1: connected to a standby, not to the primary")
	// ErrPooler: STO-C1, the sessions are not stable backends (a
	// transaction-mode pooler, EXT-4b).
	ErrPooler = errors.New("STO-C1: the backend changes between statements: connect directly, not through a transaction-mode pooler")
	// ErrSyncCommit: STO-C2.
	ErrSyncCommit = errors.New("STO-C2: synchronous_commit is off or local")
	// ErrNoQuorum: STO-C3.
	ErrNoQuorum = errors.New("STO-C3: synchronous_standby_names is not a quorum (ANY n (...), n >= 1)")
)

// quorumRE matches synchronous_standby_names of the quorum form.
var quorumRE = regexp.MustCompile(`(?i)^\s*ANY\s+([1-9][0-9]*)\s*\(.+\)\s*$`)

// quorum is true when names is ANY n (...) with n >= 1.
func quorum(names string) bool { return quorumRE.MatchString(names) }

// Checks runs the start-up checks of §7 on pool and returns their failures,
// each wrapping one of the errors above or of migrate (STO-C4); the caller
// applies the configured severity.
func Checks(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) []error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return []error{fmt.Errorf("store: checks: %w", err)}
	}
	defer conn.Release()
	var (
		errs                 []error
		recovery             bool
		pid1, pid2           int
		syncCommit, stdNames string
	)
	if err := conn.QueryRow(ctx, "select pg_is_in_recovery(), pg_backend_pid(), current_setting('synchronous_commit'), current_setting('synchronous_standby_names')").
		Scan(&recovery, &pid1, &syncCommit, &stdNames); err != nil {
		return []error{fmt.Errorf("store: checks: %w", err)}
	}
	if err := conn.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid2); err != nil {
		return []error{fmt.Errorf("store: checks: %w", err)}
	}
	if recovery {
		errs = append(errs, ErrNotPrimary)
	}
	if pid1 == 0 || pid1 != pid2 {
		errs = append(errs, ErrPooler)
	}
	if syncCommit == "off" || syncCommit == "local" {
		errs = append(errs, fmt.Errorf("%w (%s)", ErrSyncCommit, syncCommit))
	}
	if !quorum(stdNames) {
		errs = append(errs, fmt.Errorf("%w (%q)", ErrNoQuorum, stdNames))
	}
	if err := migrate.Check(ctx, conn, fsys); err != nil {
		errs = append(errs, fmt.Errorf("STO-C4: %w", err))
	}
	return errs
}
