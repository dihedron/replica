// veho-plugin-file serves veho's file storage driver as a process plugin
// (.specs/veho-plugins.md §6); veho starts it, never a person, except for
// "veho-plugin-file version".
package main

import (
	"os"

	"github.com/dihedron/veho/pkg/metadata"
	"github.com/dihedron/veho/pkg/plugin/file"
	"github.com/dihedron/veho/pkg/plugin/process"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		if len(os.Args) > 2 && (os.Args[2] == "--verbose" || os.Args[2] == "-v") {
			metadata.PrintFull(os.Stdout)
			return
		}
		metadata.Print(os.Stdout)
		return
	}
	process.Serve(file.New())
}
