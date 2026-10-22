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
	"github.com/schorsch3000/aicatraz/internal/jail"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// azPath is where a file of the project's AIcatraz setup goes.
func azPath(name string) string { return filepath.Join(config.AzDir, name) }

// runInit writes a starter configuration into the current directory.
func runInit(args []string, stdout, stderr io.Writer) int {
	askUserConfig(stderr)
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
			return badArgs(stderr, unexpected("init", a))
		}
	}
	st := ui.For(stdout)
	var generated string // base image of the Dockerfile.az init generates

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

	// An example without a Dockerfile.az gets one for this machine's distribution.
	var dockerNote string
	if _, ok := files[dockerfileName]; !ok {
		content, image, note, err := generatedDockerfile()
		if err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		files[dockerfileName], dockerNote = content, note
		generated = image
	}

	// Never overwrite anything unasked.
	var exists []string
	for f := range files {
		if _, err := os.Lstat(azPath(f)); err == nil {
			exists = append(exists, azPath(f))
		}
	}
	if _, err := os.Lstat(config.FileName); err == nil {
		exists = append(exists, config.FileName) // a project from before .az
	}
	if len(exists) > 0 && !force {
		ui.Error(stderr, "%s already exist(s); nothing written (use --force to overwrite)", strings.Join(exists, ", "))
		return 1
	}

	for _, ex := range examples.List() {
		if ex.Name != name {
			continue
		}
		order := ex.Files // config first, then the rest, in a stable order
		if generated != "" {
			order = append(append([]string(nil), order...), dockerfileName)
		}
		if err := os.MkdirAll(config.AzDir, 0o755); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		for _, f := range order {
			p := azPath(f)
			if err := os.WriteFile(p, []byte(files[f]), 0o644); err != nil {
				ui.Error(stderr, "%v", err)
				return 2
			}
			if f == dockerfileName && generated != "" {
				fmt.Fprintf(stdout, "%s %s  (FROM %s)\n", st.BoldGreen("wrote"), p, generated)
				continue
			}
			fmt.Fprintf(stdout, "%s %s\n", st.BoldGreen("wrote"), p)
		}
	}
	if dockerNote != "" {
		ui.Note(stderr, "%s", dockerNote)
	}

	// An empty ignore file for the image build; an existing one is kept.
	if _, err := os.Lstat(azPath(ignoreFileName)); err != nil {
		if err := os.WriteFile(azPath(ignoreFileName), nil, 0o644); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		fmt.Fprintf(stdout, "%s %s\n", st.BoldGreen("wrote"), azPath(ignoreFileName))
	}

	// Files from the user config's `copy` section; existing ones are kept.
	if err := jail.CopyUserFiles(home, cwd, stdout, stderr); err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}

	// The example must be a valid config; say so if it somehow isn't.
	_, problems, err := config.LoadFile(config.DefaultFile, config.Options{})
	if err == nil {
		for _, p := range problems {
			fmt.Fprintln(stderr, p.FormatStyled(config.DefaultFile, ui.For(stderr)))
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, st.Bold("Next:"))
	fmt.Fprintf(stdout, "  1. edit %s to fit the project\n", config.DefaultFile)
	fmt.Fprintf(stdout, "  2. %s\n", st.Cyan(ui.Prog+" config check"))
	switch name {
	case "claude", "opencode":
		fmt.Fprintf(stdout, "  3. %s   (first run: installs the agent and builds the image)\n", st.Cyan(ui.Prog+" "+name))
	default:
		fmt.Fprintf(stdout, "  3. %s\n", st.Cyan(ui.Prog+" run"))
	}
	return 0
}
