package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/dihedron/veho/internal/store/migrate"
	"github.com/dihedron/veho/migrations"
)

// MigrateCommand is "veho migrate" (.specs/veho-store.md §5): it applies the
// migrations that are not yet applied, as veho_migrator. The DSN comes from
// the environment, never from the command line, where it would show in the
// process list.
type MigrateCommand struct{ dbOptions }

// Execute applies what is missing and prints each migration it applied.
func (cmd *MigrateCommand) Execute([]string) error {
	ctx := context.Background()
	conn, err := cmd.connect(ctx, "migrate")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	done, err := migrate.Apply(ctx, conn, migrations.FS)
	for _, name := range done {
		fmt.Println("applied", name)
	}
	if err != nil {
		slog.Error("migration failed", "error", err)
		return fmt.Errorf("migrate: %w", err)
	}
	if len(done) == 0 {
		fmt.Println("the database is up to date")
	}
	return nil
}
