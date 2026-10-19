// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/examples"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// runInit writes a starter configuration into the current directory.
func runInit(args []string, stdout, stderr io.Writer) int {
	ensureUserConfig(stderr)
	if len(args) > 0 && args[0] == "dockerfile" {
		return runInitDockerfile(args[1:], stdout, stderr)
	}
	name, force, list := "starter", false, false
	for len(args) > 0 {
		switch a := args[0]; {
		case a == "--force":
			force = true
			args = args[1:]
		case a == "--list":
			list = true
			args = args[1:]
		case (a == "--example" || a == "-e") && len(args) >= 2:
			name = args[1]
			args = args[2:]
		default:
			fmt.Fprint(stderr, styledUsage(ui.For(stderr)))
			return 2
		}
	}
	st := ui.For(stdout)

	if list {
		for _, e := range examples.List() {
			fmt.Fprintf(stdout, "%s  %s\n", st.BoldCyan(fmt.Sprintf("%-17s", e.Name)), e.Description)
		}
		return 0
	}

	home, err := os.UserHomeDir()
	if err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	if err := examples.CheckProjectDir(cwd, home); err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	files, err := examples.Render(name, cwd)
	if err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}

	// Never overwrite anything unasked.
	var exists []string
	for f := range files {
		if _, err := os.Lstat(f); err == nil {
			exists = append(exists, f)
		}
	}
	if len(exists) > 0 && !force {
		ui.Error(stderr, "%s already exist(s); nothing written (use --force to overwrite)", strings.Join(exists, ", "))
		return 1
	}

	for _, ex := range examples.List() {
		if ex.Name != name {
			continue
		}
		for _, f := range ex.Files { // config first, then the rest, in a stable order
			if err := os.WriteFile(f, []byte(files[f]), 0o644); err != nil {
				ui.Error(stderr, "%v", err)
				return 2
			}
			fmt.Fprintf(stdout, "%s %s\n", st.BoldGreen("wrote"), f)
		}
	}

	// The example must be a valid config; say so if it somehow isn't.
	_, problems, err := config.LoadFile(examples.ConfigFile, config.Options{})
	if err == nil {
		for _, p := range problems {
			fmt.Fprintln(stderr, p.FormatStyled(examples.ConfigFile, ui.For(stderr)))
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, st.Bold("Next:"))
	fmt.Fprintf(stdout, "  1. edit %s to fit the project\n", examples.ConfigFile)
	fmt.Fprintf(stdout, "  2. %s\n", st.Cyan(ui.Prog+" config check"))
	switch name {
	case "claude", "opencode":
		fmt.Fprintf(stdout, "  3. %s   (first run: installs the agent and builds the image)\n", st.Cyan(ui.Prog+" "+name))
	default:
		fmt.Fprintf(stdout, "  3. %s\n", st.Cyan(ui.Prog+" run"))
	}
	return 0
}
