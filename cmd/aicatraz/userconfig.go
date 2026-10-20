// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/examples"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// declinedMarker is left next to the user config when you said no, so that you
// are not asked again.
const declinedMarker = ".no-user-config"

// ensureUserConfig offers to write the user-wide config, every option commented
// out with its default, if there is none yet. It asks first, and only when
// someone can answer: scripts and pipes never get a file they didn't ask for.
// Saying no is remembered. It is best effort: a home that can't be written to
// is not worth stopping for.
func ensureUserConfig(in io.Reader, interactive bool, stderr io.Writer) {
	if !interactive {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := config.UserConfigPath(home)
	marker := filepath.Join(filepath.Dir(path), declinedMarker)
	for _, p := range []string{path, marker} {
		if _, err := os.Lstat(p); err == nil {
			return
		}
	}
	st := ui.For(stderr)
	fmt.Fprintf(stderr, "%s no user config yet. Write %s?\n", st.Dim(ui.Prog+":"), path)
	fmt.Fprintf(stderr, "  It lists every setting commented out at its default, so it changes nothing until you edit it.\n")
	fmt.Fprintf(stderr, "  Create it? [Y/n] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			os.WriteFile(marker, nil, 0o644)
		}
		ui.Note(stderr, "not written; you won't be asked again (remove %s to be asked)", marker)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return // it exists (or cannot be made)
	}
	_, werr := io.WriteString(f, examples.UserConfig())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return
	}
	ui.Note(stderr, "wrote %s", path)
}

// askUserConfig is ensureUserConfig for the real terminal.
func askUserConfig(stderr io.Writer) {
	ensureUserConfig(os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), stderr)
}
