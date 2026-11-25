// veho-plugin-s3 serves veho's s3 storage driver as a process plugin
// (.specs/veho-plugins.md §6); veho starts it, never a person, except for
// "veho-plugin-s3 version".
package main

import (
	"os"

	"github.com/dihedron/veho/pkg/metadata"
	"github.com/dihedron/veho/pkg/plugin/process"
	"github.com/dihedron/veho/pkg/plugin/s3"
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
	process.Serve(s3.Factory)
}
