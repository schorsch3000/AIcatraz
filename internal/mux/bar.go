// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// BarInfo is what the status bar shows.
type BarInfo struct {
	Project string    // left
	Label   string    // the program, centered together with the uptime
	Key     string    // the detach key, right
	InfoKey string    // the key that shows the session's settings; empty: not offered
	Started time.Time // when the session began
	Colors  Colors    // zero means DefaultColors
	Alert   string    // a prompt is waiting: shown in the middle, blinking, instead of the program and uptime

	AlertSymbol      string // drawn before the alert text; empty: none
	AlertSymbolWidth int    // the columns the terminal draws it in
}

// DefaultAlertSymbol is drawn before the text of an alert.
const DefaultAlertSymbol = "⚠️"

// SymbolWidth guesses how many columns a terminal draws a symbol in: two for an
// emoji (a character from the emoji planes, or any character followed by the
// emoji selector U+FE0F), one for any other character. Terminals disagree
// about some of them, which is why the width can be set.
func SymbolWidth(sym string) int {
	w := 0
	rs := []rune(sym)
	for i, r := range rs {
		switch {
		case r == 0xFE0F || r == 0x200D || (r >= 0x300 && r < 0x370):
			// no column of its own
		case r >= 0x1F000 || (i+1 < len(rs) && rs[i+1] == 0xFE0F):
			w += 2
		default:
			w++
		}
	}
	return w
}

// The two looks of an alert, swapped every blinkPeriod: black on yellow and
// yellow on black, from the fixed part of the 256-color palette so that a
// terminal theme can't change them.
const (
	alertOn     = "\x1b[1;38;5;16;48;5;220m"
	alertOff    = "\x1b[1;38;5;220;48;5;16m"
	blinkPeriod = 500 * time.Millisecond
)

// AlertPhase says which of the two looks an alert has at a moment.
func AlertPhase(now time.Time) bool { return now.UnixMilli()/blinkPeriod.Milliseconds()%2 == 0 }

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
	if b.InfoKey != "" {
		right = " " + clean(b.InfoKey) + " info ·" + right
	}
	left := " AIcatraz: "
	if width < utf8.RuneCountInString(left)+2 { // hardly any room: the name, cut off
		s := fit(left, width)
		return b.style() + s + strings.Repeat(" ", width-utf8.RuneCountInString(s)) + "\x1b[0m"
	}
	project := clean(b.Project)
	if b.InfoKey != "" && utf8.RuneCountInString(left)+utf8.RuneCountInString(project)+1+utf8.RuneCountInString(right) > width {
		right = " " + clean(b.Key) + " detach " // the detach hint matters more than the info hint
	}
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
	alert := b.Alert != ""
	if alert { // what is waiting matters more than the uptime and the key hints
		room := width - l - r - 2
		if room < 12 {
			right, r = "", 0
			room = width - l - 1
		}
		prefix, pw := "", 0
		if sym := clean(b.AlertSymbol); sym != "" {
			prefix, pw = sym+" ", b.AlertSymbolWidth+1
		}
		text := fit(clean(b.Alert), max(room-pw, 0))
		center = prefix + text
		c = pw + utf8.RuneCountInString(text)
		start = min(max((width-c)/2, l+1), width-r-1-c)
		start = max(start, l)
	}

	var sb strings.Builder
	sb.WriteString(b.style())
	sb.WriteString(" \x1b[1mAIcatraz:\x1b[22m ")
	sb.WriteString(project)
	sb.WriteString(" ")
	pos := l
	if center != "" {
		sb.WriteString(strings.Repeat(" ", start-pos))
		if alert {
			look := alertOff
			if AlertPhase(now) {
				look = alertOn
			}
			sb.WriteString(look + center + b.style())
		} else {
			sb.WriteString(center)
		}
		pos = start + c
	}
	gap := width - r - pos
	if gap < 0 {
		gap = 0
	}
	sb.WriteString(strings.Repeat(" ", gap))
	sb.WriteString(right)
	sb.WriteString("\x1b[0m")
	if b.Colors.Rainbow {
		return rainbow(sb.String(), width)
	}
	return sb.String()
}

// rainbowStops are the colors of the rainbow theme, light enough for black text.
var rainbowStops = [][3]int{{255, 107, 107}, {255, 169, 77}, {255, 224, 102}, {105, 219, 124}, {102, 217, 232}, {116, 167, 255}, {177, 151, 252}}

// rainbowAt is the background of a column: the stops spread over the width.
func rainbowAt(col, width int) string {
	pos := 0.0
	if width > 1 {
		pos = float64(col) / float64(width-1) * float64(len(rainbowStops)-1)
	}
	i := min(int(pos), len(rainbowStops)-2)
	f := pos - float64(i)
	a, z := rainbowStops[i], rainbowStops[i+1]
	mix := func(k int) int { return a[k] + int(math.Round(float64(z[k]-a[k])*f)) }
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", mix(0), mix(1), mix(2))
}

// rainbow gives every column of a rendered bar its own background, leaving
// an alert, which has colors of its own, alone.
func rainbow(bar string, width int) string {
	var sb strings.Builder
	col, last, alert := 0, "", false
	for i := 0; i < len(bar); {
		if bar[i] == 0x1b && i+1 < len(bar) && bar[i+1] == '[' {
			j := i + 2
			for j < len(bar) && (bar[j] < 0x40 || bar[j] > 0x7e) {
				j++
			}
			j = min(j+1, len(bar))
			seq := bar[i:j]
			switch seq {
			case alertOn, alertOff:
				alert = true
			default:
				if strings.HasPrefix(seq, "\x1b[0;") {
					alert, last = false, "" // the bar's own style came back and reset the background
				}
			}
			sb.WriteString(seq)
			i = j
			continue
		}
		_, n := utf8.DecodeRuneInString(bar[i:])
		if !alert {
			if bg := rainbowAt(col, width); bg != last {
				sb.WriteString(bg)
				last = bg
			}
		}
		sb.WriteString(bar[i : i+n])
		col++
		i += n
	}
	return sb.String()
}
