// SPDX-License-Identifier: GPL-3.0-only

package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestStyleOffReturnsTextUnchanged(t *testing.T) {
	var s Style
	if s.Bold("x") != "x" || s.BoldRed("x") != "x" || s.Enabled() {
		t.Error("zero Style must not color")
	}
}

func TestStyleOn(t *testing.T) {
	s := On()
	if got := s.BoldRed("x"); got != "\x1b[1;31mx\x1b[0m" {
		t.Errorf("%q", got)
	}
	if s.Green("") != "" {
		t.Error("empty text stays empty")
	}
	if Strip(s.Cyan("a")+s.Dim("b")+" c") != "ab c" {
		t.Error("Strip")
	}
}

func TestForHonoursEnvironment(t *testing.T) {
	var buf bytes.Buffer
	if For(&buf).Enabled() {
		t.Error("a buffer is not a terminal")
	}
	t.Setenv("AICATRAZ_COLOR", "always")
	if !For(&buf).Enabled() {
		t.Error("always forces color")
	}
	t.Setenv("AICATRAZ_COLOR", "never")
	if For(os.Stdout).Enabled() {
		t.Error("never turns it off")
	}
	t.Setenv("AICATRAZ_COLOR", "")
	t.Setenv("NO_COLOR", "1")
	if For(os.Stdout).Enabled() {
		t.Error("NO_COLOR")
	}
}

func TestStripKeepsOtherEscapes(t *testing.T) {
	// Only color (SGR) sequences are removed; anything else stays visible to the caller.
	in := "a\x1b[2Jb\x1b[31mc\x1b[0m"
	if got := Strip(in); got != "a\x1b[2Jbc" {
		t.Errorf("%q", got)
	}
	if strings.Contains(Strip("\x1b[1;31m"), "\x1b") {
		t.Error("lone SGR must go")
	}
}
