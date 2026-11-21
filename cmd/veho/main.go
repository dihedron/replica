package main

import (
	"fmt"
	"os"

	"github.com/dihedron/veho/pkg/metadata"
	"github.com/jessevdk/go-flags"
)

func main() {

	defer cleanup()

	if len(os.Args) >= 2 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "v" || os.Args[1] == "--ver") {
		if len(os.Args) >= 3 && (os.Args[2] == "--verbose" || os.Args[2] == "-v") {
			metadata.PrintFull(os.Stdout)
			os.Exit(0)
		}
		metadata.Print(os.Stdout)
		os.Exit(0)
	}

	command := Command{}
	parser := flags.NewParser(&command, flags.HelpFlag|flags.PassDoubleDash)
	// without a subcommand, the root's Execute explains what to run
	parser.SubcommandsOptional = true
	if _, err := parser.Parse(); err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			_, _ = fmt.Fprintln(os.Stdout, err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if parser.Active == nil {
		if err := command.Execute(nil); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
}
