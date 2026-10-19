// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestBarAlertReplacesTheMiddleAndKeepsTheWidth(t *testing.T) {
	b := BarInfo{Project: "proj", Label: "claude", Key: "Ctrl-Y", InfoKey: "Ctrl-Q", Started: time.Now(), Alert: "Network github.com:443  Ctrl-Q", AlertSymbol: "⚠️", AlertSymbolWidth: 2}
	on := time.UnixMilli(1_000_000) // an even half second
	off := on.Add(blinkPeriod)      // the next one
	for _, width := range []int{120, 80, 60, 40, 24, 12} {
		for _, now := range []time.Time{on, off} {
			text := sgr.ReplaceAllString(b.Render(width, now), "")
			if n := utf8.RuneCountInString(text); n != width {
				t.Errorf("width %d: drew %d columns: %q", width, n, text)
			}
		}
	}
	text := sgr.ReplaceAllString(b.Render(100, on), "")
	if !strings.Contains(text, "⚠️ Network github.com:443  Ctrl-Q") || strings.Contains(text, "claude") {
		t.Errorf("the alert should replace the program and uptime: %q", text)
	}
	if a, c := b.Render(100, on), b.Render(100, off); !strings.Contains(a, alertOn) || !strings.Contains(c, alertOff) || strings.Contains(a, alertOff) {
		t.Errorf("the alert should swap its colors every %v", blinkPeriod)
	}
	// Whatever the symbol, the bar keeps its width (here the symbols are drawn
	// one column per character, so the characters can be counted); no symbol,
	// no prefix.
	for _, sym := range []string{"⚠", "!!", "[!]", ""} {
		b.AlertSymbol, b.AlertSymbolWidth = sym, utf8.RuneCountInString(sym)
		for _, width := range []int{100, 40, 20} {
			text := sgr.ReplaceAllString(b.Render(width, on), "")
			if n := utf8.RuneCountInString(text); n != width {
				t.Errorf("symbol %q, width %d: drew %d columns: %q", sym, width, n, text)
			}
		}
		if text := sgr.ReplaceAllString(b.Render(100, on), ""); !strings.Contains(text, sym+" Network") && sym != "" || !strings.Contains(text, "Network") {
			t.Errorf("symbol %q: %q", sym, text)
		}
	}
	b.AlertSymbol, b.AlertSymbolWidth = "⚠️", 2
	b.Alert = ""
	if text := sgr.ReplaceAllString(b.Render(100, on), ""); !strings.Contains(text, "claude") || strings.Contains(text, "Network") {
		t.Errorf("without an alert the bar is as before: %q", text)
	}
}

func TestSymbolWidthGuess(t *testing.T) {
	for sym, want := range map[string]int{"⚠️": 2, "⚠": 1, "🔔": 2, "!": 1, "": 0, "[!]": 3, "❗": 1, "❗️": 2} {
		if got := SymbolWidth(sym); got != want {
			t.Errorf("SymbolWidth(%q) = %d, want %d", sym, got, want)
		}
	}
}
