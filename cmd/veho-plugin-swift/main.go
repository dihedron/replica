// veho-plugin-swift serves veho's swift storage driver as a process plugin
// (.specs/veho-plugins.md §6); veho starts it, never a person, except for
// "veho-plugin-swift version".
package main

import (
	"os"

	"github.com/dihedron/veho/pkg/metadata"
	"github.com/dihedron/veho/pkg/plugin/process"
	"github.com/dihedron/veho/pkg/plugin/swift"
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
	process.Serve(swift.Factory)
}
