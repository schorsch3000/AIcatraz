// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"strings"
	"testing"
)

func feed(t *Tracker, parts ...string) {
	for _, p := range parts {
		t.Feed([]byte(p))
	}
}

func TestTrackerAltScreenAndModes(t *testing.T) {
	var tr Tracker
	if tr.InAlt() || len(tr.Preamble()) != 0 || len(tr.Reset()) != 0 {
		t.Fatal("a fresh tracker has nothing to restore")
	}
	feed(&tr, "hello \x1b[?1049h\x1b[?25l\x1b[?1000;1006h\x1b[?2004h text")
	if !tr.InAlt() {
		t.Error("alt screen not noticed")
	}
	pre := string(tr.Preamble())
	if !strings.HasPrefix(pre, "\x1b[?1049h") {
		t.Errorf("the alt screen must be entered first: %q", pre)
	}
	for _, want := range []string{"\x1b[?25l", "\x1b[?1000h", "\x1b[?1006h", "\x1b[?2004h"} {
		if !strings.Contains(pre, want) {
			t.Errorf("preamble lacks %q: %q", want, pre)
		}
	}
	rst := string(tr.Reset())
	for _, want := range []string{"\x1b[?25h", "\x1b[?1000l", "\x1b[?1006l", "\x1b[?2004l", "\x1b[?1049l"} {
		if !strings.Contains(rst, want) {
			t.Errorf("reset lacks %q: %q", want, rst)
		}
	}
	if !strings.HasSuffix(rst, "\x1b[?1049l") {
		t.Errorf("the alt screen must be left last: %q", rst)
	}

	feed(&tr, "\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1006l\x1b[?2004l")
	if tr.InAlt() || len(tr.Preamble()) != 0 || len(tr.Reset()) != 0 {
		t.Errorf("everything is off again: %q %q", tr.Preamble(), tr.Reset())
	}
}

func TestTrackerSequencesSplitAcrossReads(t *testing.T) {
	var tr Tracker
	feed(&tr, "\x1b", "[", "?10", "49", "h", "\x1b[?10", "00h")
	if !tr.InAlt() || !strings.Contains(string(tr.Preamble()), "?1000h") {
		t.Errorf("split sequences lost: %q", tr.Preamble())
	}
}

func TestTrackerIgnoresWhatItDoesNotTrack(t *testing.T) {
	var tr Tracker
	feed(&tr, "\x1b[31mred\x1b[0m \x1b[2J\x1b[H \x1b[?9999h \x1b[?1049x \x1b]0;title\x07 \x1b[?7h")
	if tr.InAlt() || len(tr.Preamble()) != 0 {
		t.Errorf("unrelated sequences changed the state: %q", tr.Preamble())
	}
	// Garbage can't make it run away.
	feed(&tr, "\x1b["+strings.Repeat("9", 500)+"h", "\x1b[?25l")
	if !strings.Contains(string(tr.Preamble()), "?25l") {
		t.Error("a long bogus sequence broke the parser")
	}
}

func TestTrackerKeyboardModes(t *testing.T) {
	var tr Tracker
	feed(&tr, "\x1b[>1u", "\x1b[>5u", "\x1b[>4;2m")
	pre := string(tr.Preamble())
	if !strings.Contains(pre, "\x1b[>1u\x1b[>5u") || !strings.Contains(pre, "\x1b[>4;2m") {
		t.Errorf("preamble %q", pre)
	}
	if rst := string(tr.Reset()); !strings.Contains(rst, "\x1b[<2u") || !strings.Contains(rst, "\x1b[>4m") {
		t.Errorf("reset %q", rst)
	}
	feed(&tr, "\x1b[<u", "\x1b[>4m")
	if pre := string(tr.Preamble()); pre != "\x1b[>1u" {
		t.Errorf("after a pop and a reset: %q", pre)
	}
	feed(&tr, "\x1b[<5u")
	if len(tr.Preamble()) != 0 {
		t.Error("popping more than was pushed empties the stack")
	}
}
