package main

import "errors"

// Command is the root of the command line. The subcommands are added by the
// chunks that implement them (.specs/veho-implementation-plan.md).
type Command struct {
	Migrate MigrateCommand `command:"migrate" description:"Apply the database migrations embedded in this binary, as veho_migrator"`
	Serve   ServeCommand   `command:"serve" description:"Run a node: the workflow executor (the API comes with C7)"`
	KEK     KEKCommand     `command:"kek" description:"Manage the key-encryption keys of the secrets, as veho_migrator"`
}

// Execute runs when no subcommand is given.
func (cmd *Command) Execute(_ []string) error {
	return errors.New("no command given: run \"veho version\" or \"veho --help\"")
}
