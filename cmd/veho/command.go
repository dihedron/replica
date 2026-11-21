package main

import "errors"

// Command is the root of the command line. The subcommands (serve, migrate,
// job, policy, ...) are added by the chunks that implement them
// (.specs/veho-implementation-plan.md).
type Command struct{}

// Execute runs when no subcommand is given.
func (cmd *Command) Execute(_ []string) error {
	return errors.New("no command given: run \"veho version\" or \"veho --help\"")
}
