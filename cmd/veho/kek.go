package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/dihedron/veho/internal/secrets"
	"github.com/dihedron/veho/internal/store"
	"github.com/jackc/pgx/v5"
)

// KEKCommand is "veho kek" (.specs/veho-secrets.md §4): the operator's
// actions on the key-encryption keys, as veho_migrator.
type KEKCommand struct {
	Show   KEKShowCommand   `command:"show" description:"List the KEK versions, their state and the secrets each wraps"`
	Init   KEKInitCommand   `command:"init" description:"Create KEK version 1, wrapped by the unwrap key"`
	Rotate KEKRotateCommand `command:"rotate" description:"Create a new active KEK version; the old one is retiring until veho kek rewrap"`
	Rewrap KEKRewrapCommand `command:"rewrap" description:"Re-wrap the data keys under the active KEK and retire the versions left unused"`
	Rekey  KEKRekeyCommand  `command:"rekey" description:"Re-wrap the KEKs with a new unwrap key"`
}

// dbOptions name the migrator's DSN, which comes from the environment, never
// from the command line.
type dbOptions struct {
	DSNEnv string `long:"dsn-env" default:"VEHO_MIGRATOR_DATABASE_URL" description:"Environment variable holding veho_migrator's DSN (sslmode=verify-full)"`

	insecureDB bool // tests only; never set by a flag
}

func (o dbOptions) connect(ctx context.Context, cmd string) (*pgx.Conn, error) {
	dsn := os.Getenv(o.DSNEnv)
	if dsn == "" {
		return nil, fmt.Errorf("%s: environment variable %q is not set", cmd, o.DSNEnv)
	}
	conn, err := store.Connect(ctx, dsn, o.insecureDB)
	if err != nil {
		slog.Error("cannot connect to the database", "error", err)
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	return conn, nil
}

// keyOptions name the unwrap key.
type keyOptions struct {
	UnwrapKey string `long:"unwrap-key" required:"true" description:"The unwrap key: file:<path> or systemd-credential:<name>"`
}

// run connects and runs fn; every error is logged and wrapped with cmd.
func run(o dbOptions, cmd string, fn func(context.Context, *pgx.Conn) error) error {
	ctx := context.Background()
	conn, err := o.connect(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := fn(ctx, conn); err != nil {
		slog.Error(cmd+" failed", "error", err)
		return fmt.Errorf("%s: %w", cmd, err)
	}
	return nil
}

// KEKShowCommand is "veho kek show".
type KEKShowCommand struct{ dbOptions }

// Execute prints the KEK versions.
func (cmd *KEKShowCommand) Execute([]string) error {
	return run(cmd.dbOptions, "kek show", func(ctx context.Context, conn *pgx.Conn) error {
		info, err := secrets.ShowKEKs(ctx, conn)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "VERSION\tSTATE\tUNWRAP KEY\tSECRETS\tCREATED\tRETIRED")
		for _, k := range info {
			retired := "-"
			if k.RetiredAt != nil {
				retired = k.RetiredAt.UTC().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\n", k.Version, k.State, k.UnwrapKeyID, k.Secrets, k.CreatedAt.UTC().Format(time.RFC3339), retired)
		}
		return w.Flush()
	})
}

// KEKInitCommand is "veho kek init".
type KEKInitCommand struct {
	dbOptions
	keyOptions
}

// Execute creates KEK version 1.
func (cmd *KEKInitCommand) Execute([]string) error {
	src, err := secrets.ParseSource(cmd.UnwrapKey)
	if err != nil {
		return err
	}
	return run(cmd.dbOptions, "kek init", func(ctx context.Context, conn *pgx.Conn) error {
		if err := secrets.InitKEK(ctx, conn, src); err != nil {
			return err
		}
		fmt.Println("created KEK version 1")
		return nil
	})
}

// KEKRotateCommand is "veho kek rotate".
type KEKRotateCommand struct {
	dbOptions
	keyOptions
}

// Execute creates the next KEK version.
func (cmd *KEKRotateCommand) Execute([]string) error {
	src, err := secrets.ParseSource(cmd.UnwrapKey)
	if err != nil {
		return err
	}
	return run(cmd.dbOptions, "kek rotate", func(ctx context.Context, conn *pgx.Conn) error {
		v, err := secrets.RotateKEK(ctx, conn, src)
		if err != nil {
			return err
		}
		fmt.Printf("KEK version %d is active; run veho kek rewrap to retire the previous one\n", v)
		return nil
	})
}

// KEKRewrapCommand is "veho kek rewrap".
type KEKRewrapCommand struct {
	dbOptions
	keyOptions
	Batch int `long:"batch" default:"500" description:"Data keys re-wrapped per transaction"`
}

// Execute re-wraps the data keys.
func (cmd *KEKRewrapCommand) Execute([]string) error {
	src, err := secrets.ParseSource(cmd.UnwrapKey)
	if err != nil {
		return err
	}
	return run(cmd.dbOptions, "kek rewrap", func(ctx context.Context, conn *pgx.Conn) error {
		res, err := secrets.RewrapDEKs(ctx, conn, src, cmd.Batch)
		if err != nil {
			return err
		}
		fmt.Printf("re-wrapped %d data keys, skipped %d replaced meanwhile, retired KEK versions %v\n", res.Rewrapped, res.Skipped, res.Retired)
		return nil
	})
}

// KEKRekeyCommand is "veho kek rekey".
type KEKRekeyCommand struct {
	dbOptions
	keyOptions
	NewUnwrapKey string `long:"new-unwrap-key" required:"true" description:"The new unwrap key: file:<path> or systemd-credential:<name>"`
}

// Execute re-wraps the KEKs with the new unwrap key.
func (cmd *KEKRekeyCommand) Execute([]string) error {
	current, err := secrets.ParseSource(cmd.UnwrapKey)
	if err != nil {
		return err
	}
	next, err := secrets.ParseSource(cmd.NewUnwrapKey)
	if err != nil {
		return err
	}
	return run(cmd.dbOptions, "kek rekey", func(ctx context.Context, conn *pgx.Conn) error {
		if err := secrets.RekeyKEKs(ctx, conn, current, next); err != nil {
			return err
		}
		fmt.Println("the KEKs are wrapped by the new unwrap key; give it to the nodes before their next start")
		return nil
	})
}
