// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Key is the key that detaches from a session: a Ctrl combination, which a
// terminal sends as one control byte (or, in the kitty keyboard mode, as an
// escape sequence).
type Key struct {
	Name string // as shown to the user, for example "Ctrl-Y"
	b    byte
	code int // the key's own code in the kitty and xterm keyboard protocols
}

// DefaultKey is Ctrl-Y. It is a plain Ctrl+letter on every keyboard layout,
// unlike Ctrl-\ or Ctrl-], which need AltGr on a German one, and unlike most
// other letters nothing in a shell or in the coding agents binds it to
// something you would miss: it is readline's "yank".
var DefaultKey = mustKey("ctrl-y")

func mustKey(spec string) Key {
	k, err := ParseKey(spec)
	if err != nil {
		panic(err)
	}
	return k
}

// refused would break the terminal, or the program, if they detached.
var refused = map[byte]string{
	'c': "interrupts the program", 'd': "ends its input", 'z': "suspends it",
	'm': "is Enter", 'j': "is Enter", 'i': "is Tab", 'h': "is Backspace", '[': "is Escape",
}

// ParseKey reads a key such as "ctrl-y", "ctrl+q", "^Y" or "ctrl-space":
// Ctrl with a letter, space, \, ], ^ or _.
func ParseKey(spec string) (Key, error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	switch {
	case strings.HasPrefix(s, "ctrl-"), strings.HasPrefix(s, "ctrl+"):
		s = s[5:]
	case strings.HasPrefix(s, "^"):
		s = s[1:]
	default:
		return Key{}, fmt.Errorf("detach key %q: write it as ctrl-<key>, for example ctrl-y", spec)
	}
	var b byte
	var base int // the key's code in the kitty protocol
	switch {
	case s == "space":
		b, base = 0, ' '
	case len(s) == 1 && s[0] >= 'a' && s[0] <= 'z':
		b, base = s[0]&0x1f, int(s[0])
	case s == `\`, s == "]", s == "^", s == "_":
		b, base = s[0]&0x1f, int(s[0])
	default:
		return Key{}, fmt.Errorf("detach key %q: use Ctrl with a letter, space, \\, ], ^ or _", spec)
	}
	if len(s) == 1 {
		if why, bad := refused[s[0]]; bad {
			return Key{}, fmt.Errorf("detach key %q is not usable: it %s", spec, why)
		}
	}
	name := "Ctrl-" + strings.ToUpper(s)
	if s == "space" {
		name = "Ctrl-Space"
	}
	return Key{Name: name, b: b, code: base}, nil
}

// A terminal with an enhanced keyboard mode (the kitty protocol, or xterm's
// modifyOtherKeys) sends a Ctrl chord as an escape sequence instead of the
// control byte: CSI code ; modifiers [: event] u, or CSI 27 ; modifiers ; code ~.
var (
	kittyKey = regexp.MustCompile(`\x1b\[(\d+)(?::\d*)*;(\d+)(?::(\d+))?(?:;[\d:]*)?u`)
	xtermKey = regexp.MustCompile(`\x1b\[27;(\d+);(\d+)~`)
)

// ctrlOnly reports whether a protocol's modifier field means Ctrl and nothing
// else but the lock keys (caps lock 64, num lock 128, which kitty adds).
func ctrlOnly(field string) bool {
	m, err := strconv.Atoi(field)
	return err == nil && m >= 1 && (m-1)&^(64|128) == 4
}

// find returns where the key is in typed bytes, or -1. Only a press counts:
// the release of a key, which the kitty protocol can report, does not.
func (k Key) find(typed []byte) int {
	best := -1
	if i := bytes.IndexByte(typed, k.b); i >= 0 {
		best = i
	}
	for _, m := range kittyKey.FindAllSubmatchIndex(typed, -1) {
		code, _ := strconv.Atoi(string(typed[m[2]:m[3]]))
		if code != k.code || !ctrlOnly(string(typed[m[4]:m[5]])) {
			continue
		}
		if m[6] >= 0 && string(typed[m[6]:m[7]]) == "3" { // a release
			continue
		}
		if best < 0 || m[0] < best {
			best = m[0]
		}
		break
	}
	for _, m := range xtermKey.FindAllSubmatchIndex(typed, -1) {
		code, _ := strconv.Atoi(string(typed[m[4]:m[5]]))
		if code == k.code && ctrlOnly(string(typed[m[2]:m[3]])) && (best < 0 || m[0] < best) {
			best = m[0]
			break
		}
	}
	return best
}

// Byte is the control byte the key sends.
func (k Key) Byte() byte { return k.b }
