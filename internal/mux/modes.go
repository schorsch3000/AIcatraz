// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Tracker follows the terminal modes a program turns on and off in its
// output: the alternate screen, mouse reporting, bracketed paste, a hidden
// cursor, and the extended keyboard modes. It is not a terminal emulator and
// keeps no screen contents. It only knows enough to put a real terminal into
// the same mode again when someone attaches or an overlay closes, and back
// into its normal mode when they leave, so a detach never leaves the user's
// shell with a dead mouse or an alternate screen.
type Tracker struct {
	state int
	buf   []byte

	modes map[int]bool // DEC private modes that were set or reset explicitly
	kitty []int        // stack of pushed kitty keyboard protocol flags
	base  int          // kitty flags set (not pushed) while nothing was pushed
	mok   int          // modifyOtherKeys level
}

const (
	stGround = iota
	stEsc
	stCSI
)

// trackedModes are the DEC private modes worth restoring, with the state a
// terminal starts in. Mode 25 (cursor visible) is on by default, the rest off.
var trackedModes = map[int]bool{
	1: false, 25: true, 47: false, 1000: false, 1001: false, 1002: false, 1003: false,
	1004: false, 1005: false, 1006: false, 1015: false, 1047: false, 1049: false, 2004: false,
}

// Feed processes output that is on its way to a terminal.
func (t *Tracker) Feed(p []byte) {
	for _, c := range p {
		switch t.state {
		case stGround:
			if c == 0x1b {
				t.state = stEsc
			}
		case stEsc:
			if c == '[' {
				t.state = stCSI
				t.buf = t.buf[:0]
			} else if c == 0x1b {
				t.state = stEsc
			} else {
				t.state = stGround
			}
		case stCSI:
			switch {
			case c >= 0x20 && c <= 0x3f: // parameter and intermediate bytes
				if len(t.buf) < 64 {
					t.buf = append(t.buf, c)
				} else {
					t.state = stGround
				}
			case c >= 0x40 && c <= 0x7e: // final byte
				t.csi(t.buf, c)
				t.state = stGround
			case c == 0x1b:
				t.state = stEsc
			default:
				t.state = stGround
			}
		}
	}
}

func (t *Tracker) csi(params []byte, final byte) {
	if len(params) == 0 {
		return
	}
	body := string(params[1:])
	switch params[0] {
	case '?':
		if final != 'h' && final != 'l' {
			return
		}
		if t.modes == nil {
			t.modes = map[int]bool{}
		}
		for _, f := range strings.Split(body, ";") {
			n, err := strconv.Atoi(f)
			if _, ok := trackedModes[n]; err == nil && ok {
				t.modes[n] = final == 'h'
			}
		}
	case '>':
		switch final {
		case 'u': // push kitty keyboard flags
			n, _ := strconv.Atoi(body)
			if len(t.kitty) < 32 {
				t.kitty = append(t.kitty, n)
			}
		case 'm': // modifyOtherKeys
			f := strings.Split(body, ";")
			if f[0] == "4" {
				t.mok = 0
				if len(f) > 1 {
					t.mok, _ = strconv.Atoi(f[1])
				}
			}
		}
	case '=':
		if final != 'u' { // set kitty keyboard flags: CSI = flags ; mode u
			return
		}
		f := strings.Split(body, ";")
		flags, _ := strconv.Atoi(f[0])
		mode := 1
		if len(f) > 1 {
			if m, err := strconv.Atoi(f[1]); err == nil {
				mode = m
			}
		}
		cur := &t.base
		if len(t.kitty) > 0 {
			cur = &t.kitty[len(t.kitty)-1]
		}
		switch mode {
		case 1:
			*cur = flags
		case 2:
			*cur |= flags
		case 3:
			*cur &^= flags
		}
	case '<':
		if final == 'u' { // pop kitty keyboard flags
			n := 1
			if v, err := strconv.Atoi(body); err == nil {
				n = v
			}
			t.kitty = t.kitty[:max(0, len(t.kitty)-n)]
		}
	}
}

func (t *Tracker) on(n int) bool {
	if v, ok := t.modes[n]; ok {
		return v
	}
	return trackedModes[n]
}

// InAlt reports whether the program is on the alternate screen.
func (t *Tracker) InAlt() bool { return t.on(47) || t.on(1047) || t.on(1049) }

// altModes come first when restoring and last when resetting.
var altModes = []int{1049, 1047, 47}

func (t *Tracker) changed() (alt, other []int) {
	for n, def := range trackedModes {
		if t.on(n) == def {
			continue
		}
		if n == 47 || n == 1047 || n == 1049 {
			continue
		}
		other = append(other, n)
	}
	sort.Ints(other)
	for _, n := range altModes {
		if t.on(n) != trackedModes[n] {
			alt = append(alt, n)
		}
	}
	return alt, other
}

// FinalReset is written when a client leaves, after the tracked modes have been
// reset: the modes and settings TUIs are known to change and that the
// tracker does not follow, or might have missed (an answer to a query can
// make a program enable more than the stream shows). Extra resets are
// harmless: they only put the terminal in the state a shell expects.
const FinalReset = "" +
	"\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l" + // mouse reports
	"\x1b[?1004l\x1b[?2004l" + // focus reports, bracketed paste
	"\x1b[?2026l" + // an unfinished synchronized update would freeze the display
	"\x1b[?2027l\x1b[?2031l\x1b[?2048l\x1b[?1007l" + // grapheme clustering, theme and resize reports, alternate scroll
	"\x1b[=0;1u\x1b[>4m" + // keyboard enhancement: kitty flags, modifyOtherKeys
	"\x1b[?1l\x1b>" + // cursor keys and keypad back to normal
	"\x1b[?7h\x1b[?25h" + // autowrap on, cursor visible
	"\x1b[0 q\x1b]112\x07" + // cursor shape and color back to the terminal's own
	"\x1b(B\x1b[0m" // character set and attributes

// Preamble returns what a freshly attached terminal needs to be in the state
// the program left it in.
func (t *Tracker) Preamble() []byte {
	var b strings.Builder
	alt, other := t.changed()
	for _, n := range alt {
		fmt.Fprintf(&b, "\x1b[?%dh", n)
	}
	for _, n := range other {
		if t.on(n) {
			fmt.Fprintf(&b, "\x1b[?%dh", n)
		} else {
			fmt.Fprintf(&b, "\x1b[?%dl", n)
		}
	}
	if t.base != 0 {
		fmt.Fprintf(&b, "\x1b[=%d;1u", t.base)
	}
	for _, f := range t.kitty {
		fmt.Fprintf(&b, "\x1b[>%du", f)
	}
	if t.mok > 0 {
		fmt.Fprintf(&b, "\x1b[>4;%dm", t.mok)
	}
	return []byte(b.String())
}

// Reset returns what puts a terminal back into its normal state: every mode
// the program changed is restored to its default, the alternate screen left.
func (t *Tracker) Reset() []byte {
	var b strings.Builder
	alt, other := t.changed()
	if len(t.kitty) > 0 {
		fmt.Fprintf(&b, "\x1b[<%du", len(t.kitty))
	}
	if t.base != 0 {
		b.WriteString("\x1b[=0;1u")
	}
	if t.mok > 0 {
		b.WriteString("\x1b[>4m")
	}
	for _, n := range other {
		if trackedModes[n] {
			fmt.Fprintf(&b, "\x1b[?%dh", n)
		} else {
			fmt.Fprintf(&b, "\x1b[?%dl", n)
		}
	}
	for i := len(alt) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "\x1b[?%dl", alt[i])
	}
	return []byte(b.String())
}
