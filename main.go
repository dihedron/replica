package main

import (
	"fmt"
	"os"

	"github.com/dihedron/replica/metadata"
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
	if args, err := flags.Parse(&command); err != nil {
		switch flagsErr := err.(type) {
		case flags.ErrorType:
			if flagsErr == flags.ErrHelp {
				os.Exit(0)
			}
			os.Exit(1)
		case *flags.Error:
			fmt.Fprintf(os.Stderr, "error: %s (%T)\n", err, err)
			os.Exit(1)
		default:
			os.Exit(1)
		}
	} else {
		err = command.Execute(args)
		if err != nil {
			os.Exit(1)
		}
	}
}
