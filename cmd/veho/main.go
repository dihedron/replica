package main

import (
	_ "embed"
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

	if len(os.Args) >= 2 && os.Args[1] == "completion" {
		if len(os.Args) != 3 || (os.Args[2] != "bash" && os.Args[2] != "zsh") {
			fmt.Fprintln(os.Stderr, "usage: veho completion bash|zsh")
			os.Exit(1)
		}
		fmt.Print(completionScript)
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
		os.Exit(exitCode(err))
	}
	if parser.Active == nil {
		if err := command.Execute(nil); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
}

// completionScript completes veho's commands and options in bash, and in zsh
// through bashcompinit: go-flags answers when GO_FLAGS_COMPLETION is set, so
// the completions always match the binary.
//
//go:embed completion.bash
var completionScript string
