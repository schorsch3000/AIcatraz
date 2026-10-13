// Package ui adds terminal colors to aicatraz's own output. Color is only
// used when the output is a terminal; NO_COLOR turns it off, and
// AICATRAZ_COLOR=always|never overrides the detection.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Style wraps text in color codes, or returns it unchanged if color is off.
// The zero value is "off".
type Style struct{ on bool }

// On returns a style with color enabled (for tests and forced output).
func On() Style { return Style{on: true} }

// For decides whether to color output written to w.
func For(w io.Writer) Style {
	switch strings.ToLower(os.Getenv("AICATRAZ_COLOR")) {
	case "always":
		return Style{on: true}
	case "never":
		return Style{}
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return Style{}
	}
	if os.Getenv("TERM") == "dumb" {
		return Style{}
	}
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return Style{}
	}
	return Style{on: true}
}

// Enabled reports whether this style emits color codes.
func (s Style) Enabled() bool { return s.on }

func (s Style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s Style) Bold(t string) string       { return s.wrap("1", t) }
func (s Style) Dim(t string) string        { return s.wrap("2", t) }
func (s Style) Red(t string) string        { return s.wrap("31", t) }
func (s Style) Green(t string) string      { return s.wrap("32", t) }
func (s Style) Yellow(t string) string     { return s.wrap("33", t) }
func (s Style) Cyan(t string) string       { return s.wrap("36", t) }
func (s Style) BoldRed(t string) string    { return s.wrap("1;31", t) }
func (s Style) BoldGreen(t string) string  { return s.wrap("1;32", t) }
func (s Style) BoldYellow(t string) string { return s.wrap("1;33", t) }
func (s Style) BoldCyan(t string) string   { return s.wrap("1;36", t) }

// Strip removes color codes (this package's own, and other SGR sequences).
func Strip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Prog is the name the program was started under ("aicatraz", or its alias
// "az"). Messages that mention a command or carry a prefix use it, so the
// output matches what the user typed.
var Prog = "aicatraz"

// Cmd rewrites mentions of `aicatraz <verb>` in s to use Prog.
func Cmd(s string) string {
	if Prog == "aicatraz" {
		return s
	}
	return strings.ReplaceAll(s, "`aicatraz ", "`"+Prog+" ")
}

// Error prints "aicatraz: message" with the prefix in bold red.
func Error(w io.Writer, format string, a ...any) {
	st := For(w)
	fmt.Fprintf(w, "%s %s\n", st.BoldRed(Prog+":"), Cmd(fmt.Sprintf(format, a...)))
}

// Note prints "aicatraz: message" with a dimmed prefix.
func Note(w io.Writer, format string, a ...any) {
	st := For(w)
	fmt.Fprintf(w, "%s %s\n", st.Dim(Prog+":"), Cmd(fmt.Sprintf(format, a...)))
}

// Good prints a success message: dimmed prefix, green text.
func Good(w io.Writer, format string, a ...any) {
	st := For(w)
	fmt.Fprintf(w, "%s %s\n", st.Dim(Prog+":"), st.Green(Cmd(fmt.Sprintf(format, a...))))
}
