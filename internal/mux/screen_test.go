// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func out(s *Screen, parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.Write(s.Out([]byte(p)))
	}
	return b.String()
}

func TestScreenKeepsTheProgramAboveTheBar(t *testing.T) {
	s := NewScreen(BarBottom, 30, 100)
	if s.AgentRows() != 29 {
		t.Fatalf("agent rows %d", s.AgentRows())
	}
	for in, want := range map[string]string{
		"\x1b[1;1Hhi":          "\x1b[1;1Hhi", // row addresses need no translation: the program's rows come first
		"\x1b[H\x1b[5d\x1b[3A": "\x1b[H\x1b[5d\x1b[3A",
		"\x1b[r":               "\x1b[1;29r", // the program's own scroll region stays above the bar
		"\x1b[3;10r":           "\x1b[3;10r",
		"\x1b[5;99r":           "\x1b[5;29r",
		"\x1b[;20r":            "\x1b[1;20r",
		"plain \x1b[31mtext":   "plain \x1b[31mtext",
		"\x1b7\x1b8\x1bM":      "\x1b7\x1b8\x1bM",
		"\x1b(B\x1b)0":         "\x1b(B\x1b)0",
		"\xc3\xbc\xe2\x82\xac": "\xc3\xbc\xe2\x82\xac",
	} {
		if got := out(s, in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	if s.barRowForTest() != 30 {
		t.Error("the bar is on the last row")
	}
}

func TestScreenOutSplitAcrossReads(t *testing.T) {
	s := NewScreen(BarBottom, 30, 100)
	if got := out(s, "\x1b", "[3", ";3", "0rx"); got != "\x1b[3;29rx" {
		t.Errorf("got %q", got)
	}
	// An unfinished sequence is held, and given back unchanged when the program goes quiet.
	if got := out(s, "a\x1b[3"); got != "a" {
		t.Errorf("got %q", got)
	}
	if got := string(s.FlushOut()); got != "\x1b[3" {
		t.Errorf("flush %q", got)
	}
	if got := out(s, "b"); got != "b" {
		t.Errorf("after the flush: %q", got)
	}
}

func TestScreenLeavesStringsAlone(t *testing.T) {
	s := NewScreen(BarBottom, 30, 100)
	for _, in := range []string{
		"\x1b]0;title \x1b[3;10r\x07",
		"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\",
		"\x1bP1;1|\x1b[2J\x1b\\",
		"\x1b_Gf=100;\x1b[5;5r\x1b\\",
	} {
		if got := out(s, in); got != in {
			t.Errorf("%q changed to %q", in, got)
		}
	}
	if s.TakeDirty() {
		t.Error("a screen erase inside a string is no erase")
	}
}

func TestScreenRepairsTheBarWhenItMayBeDamaged(t *testing.T) {
	s := NewScreen(BarBottom, 30, 100)
	s.TakeDirty()
	out(s, "text \x1b[1J \x1b[K \x1b[2K \x1b[1K \x1b[?1J")
	if s.TakeDirty() {
		t.Error("erasing above the cursor or within a line can't reach the bar row")
	}
	// Erasing from the cursor to the end of the screen does, wherever the
	// cursor is: the bar is below every row of the program.
	for _, in := range []string{"\x1b[J", "\x1b[0J", "\x1b[2J", "\x1b[3J", "\x1b[?J", "\x1b[?0J", "\x1b[?2J",
		"\x1b[?1049h", "\x1b[?1049l", "\x1b[?47h", "\x1b[?1047l", "\x1bc", "\x1b[!p"} {
		got := out(s, in)
		if !s.TakeDirty() {
			t.Errorf("%q must mark the bar damaged", in)
		}
		if strings.Contains(in, "h") || strings.Contains(in, "l") || in == "\x1bc" || in == "\x1b[!p" {
			if !strings.Contains(got, "\x1b[1;29r") {
				t.Errorf("%q: the scroll region must be set again: %q", in, got)
			}
		}
	}
}

func TestScreenOffAndTiny(t *testing.T) {
	off := NewScreen(BarOff, 30, 100)
	if off.Active() || off.AgentRows() != 30 || off.Setup() != "" || off.Teardown() != "" || out(off, "\x1b[r") != "\x1b[r" {
		t.Error("with the bar off the layer must do nothing")
	}
	if tiny := NewScreen(BarBottom, 4, 40); tiny.Active() {
		t.Error("a terminal of four rows gets no bar")
	}
	s := NewScreen(BarBottom, 30, 100)
	if got := s.Setup(); got != "\x1b[?6l\x1b[1;29r\x1b[1;1H" {
		t.Errorf("setup %q", got)
	}
	if got := s.Teardown(); !strings.HasPrefix(got, "\x1b7\x1b[r") || !strings.HasSuffix(got, "\x1b[30;1H\x1b[2K\x1b8") {
		t.Errorf("teardown %q", got)
	}
}

func visible(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestBarRender(t *testing.T) {
	now := time.Now()
	info := BarInfo{Project: "myproj-1a2b", Label: "claude", Key: "Ctrl-Y", Started: now.Add(-72 * time.Minute)}

	wide := visible(info.Render(100, now))
	if utf8.RuneCountInString(wide) != 100 {
		t.Fatalf("width %d: %q", utf8.RuneCountInString(wide), wide)
	}
	if !strings.HasPrefix(wide, " AIcatraz: myproj-1a2b ") || !strings.HasSuffix(wide, " Ctrl-Y detach ") {
		t.Errorf("left and right: %q", wide)
	}
	if !strings.Contains(wide, "claude · up 1h12m") {
		t.Errorf("the middle: %q", wide)
	}
	mid := strings.Index(wide, "claude")
	if mid < 30 || mid > 45 {
		t.Errorf("the middle is not in the middle (%d): %q", mid, wide)
	}
	if raw := info.Render(100, now); !strings.HasPrefix(raw, "\x1b[0;38;5;231;48;5;22m") || !strings.HasSuffix(raw, "\x1b[0m") {
		t.Errorf("white on dark green: %q", raw)
	}

	for _, w := range []int{80, 50, 40, 30, 20, 12, 8, 1} {
		v := visible(info.Render(w, now))
		if utf8.RuneCountInString(v) != w {
			t.Errorf("width %d: got %d columns: %q", w, utf8.RuneCountInString(v), v)
		}
	}
	if v := visible(info.Render(40, now)); strings.Contains(v, "claude") {
		t.Errorf("no room for the middle at 40 columns: %q", v)
	}
	if v := visible(info.Render(40, now)); !strings.Contains(v, "AIcatraz: myproj") || !strings.HasSuffix(v, "Ctrl-Y detach ") {
		t.Errorf("40 columns: %q", v)
	}

	// Hostile text can't draw on the bar.
	evil := BarInfo{Project: "a\x1b[2Jb\nc", Label: "x\x07y", Key: "Ctrl-Y", Started: now}
	if raw := evil.Render(100, now); strings.Count(raw, "\x1b") != 4 || strings.ContainsAny(raw, "\n\x07") {
		t.Errorf("control characters got through: %q", raw)
	}
	for in, want := range map[time.Duration]string{0: "up <1m", 5 * time.Minute: "up 5m", 61 * time.Minute: "up 1h01m", 50 * time.Hour: "up 2d2h"} {
		if got := uptime(in); got != want {
			t.Errorf("uptime(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestParseBar(t *testing.T) {
	for in, want := range map[string]Bar{"": BarBottom, "bottom": BarBottom, "On": BarBottom, "off": BarOff, "none": BarOff} {
		if got, err := ParseBar(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"left", "top"} {
		if _, err := ParseBar(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestSessionWithBarTellsTheProgramOneRowLess(t *testing.T) {
	s := startSession(t, `sleep 0.5; stty size; sleep 30`)
	tm := attachBar(t, s, BarBottom)
	tm.expect("29 100")     // the terminal has 30 rows: the program is told 29
	tm.expect("\x1b[1;29r") // the scroll region keeps the program off the bar
	tm.expect("\x1b[30;1H") // the bar is on the last row
	tm.expect("AIcatraz:")
	tm.expect("proj")
	tm.expect("Ctrl-Y detach")
	tm.expect("testprog · up") // the label and the uptime
	tm.type_(string([]byte{DefaultKey.Byte()}))
	tm.result()
	tm.expect("\x1b[r") // detach gives the scroll region back
	s.stop()
}

func TestParseColor(t *testing.T) {
	for _, c := range []struct {
		spec string
		bg   bool
		want string
	}{
		{"red", false, "31"}, {"red", true, "41"}, {"Bright-Blue", false, "94"}, {"brightcyan", true, "106"},
		{"light-green", false, "92"}, {"gray", false, "90"}, {"grey", true, "100"}, {"default", false, "39"}, {"default", true, "49"},
		{"18", true, "48;5;18"}, {"255", false, "38;5;255"}, {"0", false, "38;5;0"},
		{"#005f87", true, "48;2;0;95;135"}, {"#FFF", false, "38;2;255;255;255"}, {"#1f2a44", true, "48;2;31;42;68"},
	} {
		if got, err := ParseColor(c.spec, c.bg); err != nil || got != c.want {
			t.Errorf("%q (bg %v): got %q %v, want %q", c.spec, c.bg, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "256", "-1", "#12", "#gggggg", "#1234567", "purple", "red;1", "38;5;1"} {
		if _, err := ParseColor(bad, false); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestThemesAreValidAndReadable(t *testing.T) {
	seen := map[string]bool{}
	for _, th := range Themes {
		if seen[th.Name] {
			t.Errorf("theme %s twice", th.Name)
		}
		seen[th.Name] = true
		c, err := ResolveColors(th.Name, "", "")
		if err != nil {
			t.Errorf("%s: %v", th.Name, err)
		}
		if c.FG == "" || c.BG == "" {
			t.Errorf("%s: %+v", th.Name, c)
		}
		if ratio := Contrast(th.FG, th.BG); ratio < 7 {
			t.Errorf("theme %s is hard to read: contrast %.1f:1 (WCAG AAA wants 7:1)", th.Name, ratio)
		}
	}
	if DefaultTheme != "forest" || DefaultColors.FG != "38;5;231" || DefaultColors.BG != "48;5;22" {
		t.Errorf("the default is white on dark green: %+v", DefaultColors)
	}
}

func TestResolveColors(t *testing.T) {
	c, err := ResolveColors("", "", "")
	if err != nil || c != DefaultColors {
		t.Errorf("%+v %v", c, err)
	}
	c, _ = ResolveColors("amber", "", "")
	if c.FG != "38;5;16" || c.BG != "48;5;214" {
		t.Errorf("amber: %+v", c)
	}
	c, _ = ResolveColors("amber", "white", "#102030")
	if c.FG != "37" || c.BG != "48;2;16;32;48" {
		t.Errorf("overrides: %+v", c)
	}
	c, _ = ResolveColors("", "red", "")
	if c.FG != "31" || c.BG != "48;5;22" {
		t.Errorf("only the text color: %+v", c)
	}
	if _, err := ResolveColors("nope", "", ""); err == nil || !strings.Contains(err.Error(), "navy") {
		t.Errorf("unknown theme: %v", err)
	}
	if _, err := ResolveColors("", "purple", ""); err == nil {
		t.Error("bad color")
	}
	if got := (BarInfo{Colors: Colors{FG: "31", BG: "47"}}).Render(60, time.Now()); !strings.HasPrefix(got, "\x1b[0;31;47m") {
		t.Errorf("the bar must use its colors: %q", got)
	}
	if got := (BarInfo{}).Render(60, time.Now()); !strings.HasPrefix(got, "\x1b[0;38;5;231;48;5;22m") {
		t.Errorf("no colors means the default: %q", got)
	}
}

func TestPreviewShowsEveryTheme(t *testing.T) {
	var b strings.Builder
	Preview(&b, 80, false)
	for _, th := range Themes {
		if !strings.Contains(b.String(), th.Name) || !strings.Contains(b.String(), th.Note) {
			t.Errorf("theme %s missing from the preview", th.Name)
		}
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Error("an unstyled preview has no escape sequences")
	}
	if !strings.Contains(b.String(), "AIcatraz: myproject") || !strings.Contains(b.String(), "(default)") {
		t.Errorf("preview: %s", b.String())
	}
}

func TestScreenHidesTheProgramsWindowTitle(t *testing.T) {
	for _, bar := range []Bar{BarBottom, BarOff} {
		s := NewScreen(bar, 30, 100)
		s.HideTitles = true
		for in, want := range map[string]string{
			"a\x1b]0;my title\x07b":                   "ab",
			"a\x1b]2;my title\x1b\\b":                 "ab",
			"a\x1b]1;icon\x07b":                       "ab",
			"\x1b]0;\x1b]0;x\x07":                     "",                                     // an aborted title goes with the one that follows
			"\x1b]0;cut\x1b]8;;u\x07t\x1b]8;;\x07":    "\x1b]8;;u\x07t\x1b]8;;\x07",           // an aborted title, then a hyperlink: the link stays
			"\x1b]8;;http://x\x07link\x1b]8;;\x07":    "\x1b]8;;http://x\x07link\x1b]8;;\x07", // hyperlinks stay
			"\x1b]52;c;aGk=\x07":                      "\x1b]52;c;aGk=\x07",
			"\x1b]66;w=1;x\x1b\\":                     "\x1b]66;w=1;x\x1b\\",
			"\x1b]4;1;?\x07\x1b]10;?\x07\x1b]112\x07": "\x1b]4;1;?\x07\x1b]10;?\x07\x1b]112\x07",
			"\x1b]20;title-like\x07":                  "\x1b]20;title-like\x07", // only 0, 1 and 2
			"x\x1b[31my":                              "x\x1b[31my",
		} {
			if got := out(s, in); got != want {
				t.Errorf("bar %v: %q: got %q, want %q", bar, in, got, want)
			}
		}
		// Split across reads.
		if got := out(s, "a\x1b", "]", "0;ti", "tle", "\x07", "b"); got != "ab" {
			t.Errorf("split: %q", got)
		}
		// A very long command that is not a title streams through unchanged.
		long := "\x1b]52;c;" + strings.Repeat("A", 10000) + "\x07"
		if got := out(s, long); got != long {
			t.Errorf("a long command must pass intact (%d bytes in, %d out)", len(long), len(got))
		}
	}
	// Left alone unless asked.
	s := NewScreen(BarBottom, 30, 100)
	if got := out(s, "\x1b]0;t\x07"); got != "\x1b]0;t\x07" {
		t.Errorf("titles are only hidden on request: %q", got)
	}
	if got := out(NewScreen(BarOff, 30, 100), "\x1b]0;t\x07\x1b[r"); got != "\x1b]0;t\x07\x1b[r" {
		t.Errorf("with no bar and no request nothing changes: %q", got)
	}
	// Without a bar, scroll regions are not rewritten even while titles are hidden.
	off := NewScreen(BarOff, 30, 100)
	off.HideTitles = true
	if got := out(off, "\x1b[3;40r\x1b[2J"); got != "\x1b[3;40r\x1b[2J" {
		t.Errorf("no bar, no rewriting: %q", got)
	}
}
