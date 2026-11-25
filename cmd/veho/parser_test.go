package main

import (
	"errors"
	"testing"

	"github.com/jessevdk/go-flags"
)

// Every command's options parse: go-flags refuses two options of the same
// name only when it parses, which the tests that run commands as structs
// never do.
func TestParser(t *testing.T) {
	command := Command{}
	p := flags.NewParser(&command, flags.HelpFlag|flags.PassDoubleDash)
	var walk func(c *flags.Command, path []string)
	walk = func(c *flags.Command, path []string) {
		for _, sub := range c.Commands() {
			args := append(append([]string{}, path...), sub.Name)
			var fe *flags.Error
			if _, err := p.ParseArgs(append(args, "--help")); !errors.As(err, &fe) || fe.Type != flags.ErrHelp {
				t.Errorf("veho %v --help: %v", args, err)
			}
			walk(sub, args)
		}
	}
	walk(p.Command, nil)
}
