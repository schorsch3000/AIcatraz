// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"io"
	"os"
	"path/filepath"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/examples"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// ensureUserConfig writes the user-wide config, every option commented out with
// its default, if there is none yet. It is best effort: a home that can't be
// written to is not worth stopping for.
func ensureUserConfig(stderr io.Writer) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := config.UserConfigPath(home)
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
	ui.Note(stderr, "wrote %s with every user setting and its default, commented out", path)
}
