// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// BarInfo is what the status bar shows.
type BarInfo struct {
	Project string    // left
	Label   string    // the program, centered together with the uptime
	Key     string    // the detach key, right
	Started time.Time // when the session began
	Colors  Colors    // zero means DefaultColors
}

// style is the SGR sequence that starts the bar.
func (b BarInfo) style() string {
	c := b.Colors
	if c.FG == "" || c.BG == "" {
		c = DefaultColors
	}
	return "\x1b[0;" + c.FG + ";" + c.BG + "m"
}

// clean makes text safe for the bar: no control characters.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
}

// fit shortens s to at most n columns, with an ellipsis.
func fit(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func uptime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "up <1m"
	case d < time.Hour:
		return fmt.Sprintf("up %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("up %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("up %dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

// Render draws the bar for a terminal width: exactly width columns, styled.
// The project is on the left and the detach key on the right; the program and
// uptime sit in the middle if there is room for them.
func (b BarInfo) Render(width int, now time.Time) string {
	if width < 1 {
		return ""
	}
	right := " " + clean(b.Key) + " detach "
	left := " AIcatraz: "
	if width < utf8.RuneCountInString(left)+2 { // hardly any room: the name, cut off
		s := fit(left, width)
		return b.style() + s + strings.Repeat(" ", width-utf8.RuneCountInString(s)) + "\x1b[0m"
	}
	project := clean(b.Project)
	if utf8.RuneCountInString(left)+utf8.RuneCountInString(project)+1+utf8.RuneCountInString(right) > width {
		room := width - utf8.RuneCountInString(left) - 1 - utf8.RuneCountInString(right)
		if room < 3 { // too narrow for the key hint
			right = ""
			room = width - utf8.RuneCountInString(left) - 1
		}
		project = fit(project, room)
	}
	l := utf8.RuneCountInString(left) + utf8.RuneCountInString(project) + 1 // a space after the project
	r := utf8.RuneCountInString(right)

	center := ""
	if b.Label != "" {
		center = clean(b.Label) + " · " + uptime(now.Sub(b.Started))
	} else {
		center = uptime(now.Sub(b.Started))
	}
	c := utf8.RuneCountInString(center)
	start := (width - c) / 2
	if start < l+2 || start+c > width-r-2 {
		center, c, start = "", 0, 0
	}

	var sb strings.Builder
	sb.WriteString(b.style())
	sb.WriteString(" \x1b[1mAIcatraz:\x1b[22m ")
	sb.WriteString(project)
	sb.WriteString(" ")
	pos := l
	if center != "" {
		sb.WriteString(strings.Repeat(" ", start-pos))
		sb.WriteString(center)
		pos = start + c
	}
	gap := width - r - pos
	if gap < 0 {
		gap = 0
	}
	sb.WriteString(strings.Repeat(" ", gap))
	sb.WriteString(right)
	sb.WriteString("\x1b[0m")
	return sb.String()
}
