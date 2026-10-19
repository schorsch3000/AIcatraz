// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"fmt"
	"strconv"
	"strings"
)

// Bar says whether the status bar is shown. It sits on the bottom row.
type Bar int

const (
	BarOff Bar = iota
	BarBottom
)

// ParseBar reads "bottom" (or "on") and "off".
func ParseBar(s string) (Bar, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bottom", "on", "":
		return BarBottom, nil
	case "off", "none", "no", "false":
		return BarOff, nil
	case "top":
		return BarOff, fmt.Errorf("status bar %q: a bar on the top row is gone (it took the terminal's scrollback with it); use bottom or off", s)
	}
	return BarOff, fmt.Errorf("status bar %q: use bottom or off", s)
}

// minRows is the smallest terminal that gets a bar.
const minRows = 6

// Screen keeps the bottom row of the real terminal for the status bar. The
// program on the pty is told the terminal has one row less, and a scroll
// region over the rows above the bar keeps its output off the bar. Because
// the program's rows are the first rows of the screen, no row address needs
// translating (which is why the bar is at the bottom). Screen only rewrites
// the program's own scroll region requests, so they stay above the bar, and
// notices when the bar may have been damaged: a screen erase, a switch of
// screen buffer, a reset. It is not a terminal emulator and keeps no screen.
type Screen struct {
	bar  Bar
	rows int // rows of the real terminal
	cols int

	dirty bool // the bar row may have been damaged: draw it again

	// HideTitles drops the program's window title commands (OSC 0, 1 and 2):
	// the title belongs to the session.
	HideTitles bool

	out outParser
}

// NewScreen creates a layer for a real terminal of rows x cols. With the bar
// off, or a terminal too small for it, the layer does nothing.
func NewScreen(bar Bar, rows, cols int) *Screen {
	s := &Screen{bar: bar, rows: rows, cols: cols}
	if rows < minRows {
		s.bar = BarOff
	}
	return s
}

// Active reports whether a bar is shown.
func (s *Screen) Active() bool { return s.bar != BarOff }

// Resize tells the layer the real terminal's new size.
func (s *Screen) Resize(rows, cols int) {
	s.rows, s.cols = rows, cols
	s.dirty = true
}

// AgentRows is the number of rows the program is told it has.
func (s *Screen) AgentRows() int {
	if s.Active() {
		return s.rows - 1
	}
	return s.rows
}

// Setup is written first on a cleared terminal: the scroll region, and the
// cursor at home.
func (s *Screen) Setup() string {
	if !s.Active() {
		return ""
	}
	s.dirty = true
	return fmt.Sprintf("\x1b[?6l\x1b[1;%dr\x1b[1;1H", s.AgentRows())
}

// Begin sets the scroll region without moving the cursor, for a session that
// starts where the cursor is.
func (s *Screen) Begin() string {
	s.dirty = true
	return s.Margins()
}

// Margins sets the scroll region again without moving the cursor, for when
// the terminal may have forgotten it.
func (s *Screen) Margins() string {
	if !s.Active() {
		return ""
	}
	return fmt.Sprintf("\x1b7\x1b[?6l\x1b[1;%dr\x1b8", s.AgentRows())
}

// Teardown gives the terminal back: the full scroll region, the bar row empty.
func (s *Screen) Teardown() string {
	if !s.Active() {
		return ""
	}
	// The cursor stays where the program left it: resetting the scroll region
	// homes it, so it is saved first.
	return fmt.Sprintf("\x1b7\x1b[r\x1b[?6l\x1b[%d;1H\x1b[2K\x1b8", s.rows)
}

// TakeDirty reports whether the bar has to be drawn again, and clears the flag.
func (s *Screen) TakeDirty() bool {
	d := s.dirty
	s.dirty = false
	return d
}

// Draw returns the sequence that paints text on the bar row without moving the
// cursor or changing its attributes (the terminal saves and restores both).
func (s *Screen) Draw(text string) string {
	return fmt.Sprintf("\x1b7\x1b[?6l\x1b[%d;1H%s\x1b8", s.rows, text)
}

// ---- program -> terminal ----

type outParser struct {
	state int
	buf   []byte
}

const (
	pGround = iota
	pEsc
	pEscInter // ESC followed by intermediate bytes, such as ESC ( B
	pCSI
	pString // DCS, APC, PM, SOS: up to BEL or ST
	pStringEsc
	pOSC // an operating system command, held back whole to see whether it sets the title
	pOSCEsc
)

// Out rewrites the program's output for the real terminal.
func (s *Screen) Out(p []byte) []byte {
	if !s.Active() && !s.HideTitles {
		return p
	}
	o := &s.out
	res := make([]byte, 0, len(p)+16)
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch o.state {
		case pGround:
			if c == 0x1b {
				o.state, o.buf = pEsc, append(o.buf[:0], c)
			} else {
				res = append(res, c)
			}

		case pEsc:
			o.buf = append(o.buf, c)
			switch {
			case c == '[':
				o.state = pCSI
			case c == ']':
				o.state = pOSC
			case c == 'P' || c == '_' || c == '^' || c == 'X':
				o.state = pString
			case c >= 0x20 && c <= 0x2f:
				o.state = pEscInter
			case c == 0x1b:
				res = append(res, o.buf[:len(o.buf)-1]...) // a lone ESC, and a new one starts
				o.buf = append(o.buf[:0], c)
			default: // a two-byte sequence: ESC c, ESC 7, ESC M, ...
				res = append(res, o.buf...)
				if c == 'c' { // full reset: the terminal forgets the scroll region
					res = append(res, s.Setup()...)
				}
				o.state = pGround
			}

		case pEscInter:
			o.buf = append(o.buf, c)
			if (c >= 0x30 && c <= 0x7e) || c < 0x20 || len(o.buf) > 8 {
				res = append(res, o.buf...)
				o.state = pGround
			}

		case pCSI:
			o.buf = append(o.buf, c)
			switch {
			case c >= 0x20 && c <= 0x3f:
				if len(o.buf) > 80 { // not a sequence we know: let it through
					res = append(res, o.buf...)
					o.state = pGround
				}
			case c >= 0x40 && c <= 0x7e:
				res = append(res, s.csiOut(o.buf[2:len(o.buf)-1], c, o.buf)...)
				o.state = pGround
			default: // a control character inside the sequence: give up on it
				res = append(res, o.buf...)
				o.state = pGround
			}

		case pOSC:
			o.buf = append(o.buf, c)
			switch {
			case c == 0x07:
				res = append(res, s.finishOSC(o.buf)...)
				o.state = pGround
			case c == 0x1b:
				o.state = pOSCEsc
			case len(o.buf) > 4096: // not a title: let the rest of it stream through
				res = append(res, o.buf...)
				o.buf = o.buf[:0]
				o.state = pString
			}

		case pOSCEsc:
			o.buf = append(o.buf, c)
			switch {
			case c == '\\':
				res = append(res, s.finishOSC(o.buf)...)
				o.state = pGround
			case c == 0x1b: // still waiting for the backslash
			case c == 0x07:
				res = append(res, s.finishOSC(o.buf)...)
				o.state = pGround
			default:
				// An escape that is no string terminator ends the command and starts
				// a new sequence. What was cut off is dropped if it was a title, or
				// the terminal would wait for an end that never comes.
				cut := o.buf[:len(o.buf)-2]
				if !s.isTitle(cut) {
					res = append(res, cut...)
				}
				o.buf = append(o.buf[:0], 0x1b)
				o.state = pEsc
				i-- // the byte after the escape is read again, as the start of that sequence
			}

		case pString:
			res = append(res, o.buf...) // the introducer, once
			o.buf = o.buf[:0]
			res = append(res, c)
			switch c {
			case 0x07:
				o.state = pGround
			case 0x1b:
				o.state = pStringEsc
			}

		case pStringEsc:
			res = append(res, c)
			if c == '\\' {
				o.state = pGround
			} else if c != 0x1b {
				o.state = pString
			}
		}
	}
	return res
}

// finishOSC decides what becomes of a complete operating system command: a
// window title is dropped when titles are hidden, anything else goes through.
func (s *Screen) finishOSC(seq []byte) []byte {
	if s.isTitle(seq) {
		return nil
	}
	return seq
}

// isTitle reports whether seq (ESC ] ...) sets the window or icon title, and
// titles are hidden.
func (s *Screen) isTitle(seq []byte) bool {
	return s.HideTitles && len(seq) >= 4 && seq[0] == 0x1b && seq[1] == ']' &&
		(seq[2] == '0' || seq[2] == '1' || seq[2] == '2') && seq[3] == ';'
}

// FlushOut returns what the parser is holding back, unchanged, for when the
// program has gone quiet in the middle of a sequence.
func (s *Screen) FlushOut() []byte {
	o := &s.out
	if len(o.buf) == 0 || o.state == pGround || o.state == pString || o.state == pStringEsc || o.state == pOSC || o.state == pOSCEsc {
		return nil
	}
	b := append([]byte(nil), o.buf...)
	o.buf, o.state = o.buf[:0], pGround
	return b
}

// params splits "r;c" into numbers; empty or missing fields are 0.
func params(b []byte) []int {
	if len(b) == 0 {
		return nil
	}
	var out []int
	for _, f := range strings.Split(string(b), ";") {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}

func (s *Screen) csiOut(param []byte, final byte, whole []byte) []byte {
	if !s.Active() {
		return whole
	}
	private := len(param) > 0 && (param[0] == '?' || param[0] == '>' || param[0] == '<' || param[0] == '=')
	inter := len(param) > 0 && param[len(param)-1] >= 0x20 && param[len(param)-1] <= 0x2f

	if private {
		if param[0] == '?' && final == 'J' { // selective erase
			if p := string(param[1:]); p == "" || p == "0" || p == "2" || p == "3" {
				s.dirty = true
			}
		}
		if param[0] == '?' && (final == 'h' || final == 'l') {
			for _, n := range params(param[1:]) {
				if n == 47 || n == 1047 || n == 1049 {
					// Another buffer: it has no bar yet, and the margins may be gone.
					s.dirty = true
					return append(append([]byte(nil), whole...), s.Margins()...)
				}
			}
		}
		return whole
	}
	if inter {
		if final == 'p' && string(param) == "!" { // soft reset forgets the scroll region
			s.dirty = true
			return append(append([]byte(nil), whole...), s.Margins()...)
		}
		return whole
	}

	switch final {
	case 'r': // the program sets its scroll region: keep it above the bar
		p := params(param)
		for len(p) < 2 {
			p = append(p, 0)
		}
		h := s.AgentRows()
		t, b := p[0], p[1]
		if t == 0 {
			t = 1
		}
		if b == 0 || b > h {
			b = h
		}
		return []byte(fmt.Sprintf("\x1b[%d;%dr", t, b))
	case 'H', 'f', 'd': // absolute row (CUP, HVP, VPA): the bar row is not the program's
		p := params(param)
		h := s.AgentRows()
		row := 0
		if len(p) > 0 {
			row = p[0]
		}
		if row <= h {
			return whole
		}
		if final == 'd' {
			return []byte(fmt.Sprintf("\x1b[%dd", h))
		}
		col := ""
		if len(p) > 1 && p[1] > 0 {
			col = strconv.Itoa(p[1])
		}
		return []byte(fmt.Sprintf("\x1b[%d;%s%c", h, col, final))
	case 'J': // erase in display: from the cursor to the end of the screen (the default), or all of it, reaches the bar row
		if p := string(param); p == "" || p == "0" || p == "2" || p == "3" {
			s.dirty = true
		}
	}
	return whole
}

// barRowForTest is the screen row of the bar.
func (s *Screen) barRowForTest() int { return s.rows }
