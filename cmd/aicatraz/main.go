// Command aicatraz is a single binary that acts as launcher, broker and shim
// depending on the name it is invoked under.
package main

import (
	"os"
	"path/filepath"

	"github.com/schorsch3000/aicatraz/internal/shim"
)

var version = "dev"

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

// run dispatches on the invocation name: "aicatraz" is the CLI, any other
// name is a proxied command and runs as the shim.
func run(name string, args []string) int {
	if name == "aicatraz" {
		return runCLI(args)
	}
	return runShim(name, args)
}

func runShim(name string, args []string) int {
	return shim.Run(name, args, shim.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}
