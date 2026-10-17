package mux

import (
	"bytes"
	"fmt"
	"strings"
)

// Key is the key that detaches from a session: a Ctrl combination, which a
// terminal sends as one control byte (or, in the kitty keyboard mode, as an
// escape sequence).
type Key struct {
	Name string // as shown to the user, for example "Ctrl-Y"
	b    byte
	csiu []byte
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
	return Key{Name: name, b: b, csiu: []byte(fmt.Sprintf("\x1b[%d;5u", base))}, nil
}

// find returns where the key is in typed bytes, or -1.
func (k Key) find(typed []byte) int {
	if i := bytes.IndexByte(typed, k.b); i >= 0 {
		return i
	}
	return bytes.Index(typed, k.csiu)
}

// Byte is the control byte the key sends.
func (k Key) Byte() byte { return k.b }
