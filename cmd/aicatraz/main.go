// SPDX-License-Identifier: GPL-3.0-only

// Command aicatraz is a single binary that acts as launcher, broker and shim
// depending on the name it is invoked under.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/shim"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

var version = "dev"

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

// run dispatches on the invocation name: "aicatraz", a release file name
// ("aicatraz-v0.1.1-linux-amd64", which is what a re-executed binary is called
// once its symlink is resolved) and the short alias "az" are the CLI, any other
// name is a proxied command and runs as the shim.
// Inside a jail (the broker socket is mounted) "az" stays a proxied command,
// so a rule for the Azure CLI keeps working. "aicatraz" and release names
// refuse to run inside a jail.
func run(name string, args []string) int {
	if isReleaseName(name) {
		name = "aicatraz"
	}
	if name == "aicatraz" && insideJail() {
		fmt.Fprintln(os.Stderr, "aicatraz: refusing to run inside a jail")
		return 1
	}
	if name == "aicatraz" || (name == "az" && !insideJail()) {
		ui.Prog = name
		return runCLI(args)
	}
	return runShim(name, args)
}

// isReleaseName reports whether name is the file name of a release binary.
func isReleaseName(name string) bool {
	return strings.HasPrefix(name, "aicatraz-")
}

func insideJail() bool {
	_, err := os.Stat(shim.SocketPath)
	return err == nil
}

func runShim(name string, args []string) int {
	return shim.Run(name, args, shim.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}
