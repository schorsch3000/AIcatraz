// Package approve asks the human whether a host command may run. The prompt
// is shown on a terminal the agent can't type into or write to.
package approve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/schorsch3000/aicatraz/internal/ui"
)

// Answer is the human's decision.
type Answer int

const (
	Deny         Answer = iota
	AllowOnce           // run this call
	AllowSession        // run it, and allow the same call again until the session ends
	AllowPin            // run it, and remember the shown file versions as approved
)

func (a Answer) String() string {
	switch a {
	case AllowOnce:
		return "once"
	case AllowSession:
		return "session"
	case AllowPin:
		return "pin"
	}
	return "deny"
}

// ParseAnswer is the inverse of String; anything unknown is a deny.
func ParseAnswer(s string) Answer {
	switch strings.TrimSpace(s) {
	case "once":
		return AllowOnce
	case "session":
		return AllowSession
	case "pin":
		return AllowPin
	}
	return Deny
}

// Errors an Approver can return. All of them mean "not approved".
var (
	ErrUnavailable = errors.New("no approval terminal is available")
	ErrTimeout     = errors.New("approval timed out")
)

// Request describes a command waiting for approval. Every string in it is
// controlled by the agent and must be treated as hostile when displayed.
type Request struct {
	Name      string   `json:"name"`
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	Binary    string   `json:"binary"`
	EnvPassed []string `json:"env_passed,omitempty"`
	Rule      int      `json:"rule"`
	RuleText  string   `json:"rule_text,omitempty"`
	// Warnings are shown prominently.
	Warnings []string `json:"warnings,omitempty"`
	// Files are dependent files that are new or changed since the last approval.
	Files []FileChange `json:"files,omitempty"`
	// CanPin offers "run and remember these files as approved".
	CanPin bool `json:"can_pin,omitempty"`
	// Kind is "" for a host command, or KindBuild for an image build.
	Kind string `json:"kind,omitempty"`
}

// KindBuild marks a request to build the project's image.
const KindBuild = "build"

// FileChange describes one dependent file that needs attention.
type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status"` // e.g. "changed since last approved"
	Diff   string `json:"diff,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Approver shows a request to the human and returns the answer. It must
// return when ctx is done (the agent gave up, or the broker is closing) and
// after timeout. Implementations show one prompt at a time.
type Approver interface {
	Ask(ctx context.Context, req Request, timeout time.Duration) (Answer, error)
}

const (
	maxArgLen    = 300
	maxArgsShown = 40
)

// Quote renders s so nothing in it can act on the terminal: control
// characters, escape sequences and non-ASCII are shown as escapes. Long values
// are cut and say so.
func Quote(s string) string {
	cut := ""
	if len(s) > maxArgLen {
		cut = fmt.Sprintf("…(+%d bytes)", len(s)-maxArgLen)
		s = s[:maxArgLen]
	}
	q := strconv.QuoteToASCII(s)
	if cut != "" {
		q = q[:len(q)-1] + cut + `"`
	}
	return q
}

// QuoteArgs renders an argument vector, name first.
func QuoteArgs(name string, args []string) string {
	parts := []string{Quote(name)}
	for i, a := range args {
		if i == maxArgsShown {
			parts = append(parts, fmt.Sprintf("…(+%d more arguments)", len(args)-maxArgsShown))
			break
		}
		parts = append(parts, Quote(a))
	}
	return strings.Join(parts, " ")
}

const maxDiffLines = 40

// Sanitize makes agent-controlled text safe to print: unprintable characters
// (control, escape, bidi overrides, ...) become visible escapes; tabs become
// spaces. Unlike Quote it leaves quotes and backslashes alone, so file
// contents stay readable.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\n':
			b.WriteRune(r)
		case strconv.IsPrint(r):
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			fmt.Fprintf(&b, "\\U%08x", r)
		}
	}
	return b.String()
}

// indentLines sanitises text, colors it as a diff, and prefixes each line,
// cutting it at max lines.
func indentLines(text, prefix string, max int, st ui.Style) string {
	lines := strings.Split(strings.TrimRight(Sanitize(text), "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		if i == max {
			fmt.Fprintf(&b, "%s%s\n", st.Dim(prefix), st.Yellow(fmt.Sprintf("... (%d more lines, not shown: review the file itself before approving)", len(lines)-max)))
			break
		}
		if len(l) > 200 {
			l = l[:200] + "..."
		}
		b.WriteString(st.Dim(prefix) + diffLine(st, l) + "\n")
	}
	return b.String()
}

// Render produces the plain text of the prompt.
func Render(r Request) string { return RenderStyled(r, ui.Style{}) }

// RenderStyled produces the prompt with colors. Only aicatraz's own text is
// colored; everything that comes from the agent is sanitized first and then
// wrapped, so color can't be used to smuggle anything onto the terminal.
func RenderStyled(r Request, st ui.Style) string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "  %s %s\n", st.Dim(fmt.Sprintf("%-8s", label)), value)
	}

	if r.Kind == KindBuild {
		b.WriteString("\n  " + st.Bold(ui.Prog+": the project's build files need approval before the image is built") + "\n\n")
		line("build", st.Bold(QuoteArgs(r.Name, r.Args)))
		line("context", Quote(r.Cwd))
	} else {
		b.WriteString("\n  " + st.Bold(ui.Prog+": the agent wants to run a command ") + st.BoldRed("ON THE HOST") + "\n\n")
		line("command", st.Bold(QuoteArgs(r.Name, r.Args)))
		line("cwd", Quote(r.Cwd))
		line("binary", Quote(r.Binary))
		if len(r.EnvPassed) > 0 {
			quoted := make([]string, len(r.EnvPassed))
			for i, n := range r.EnvPassed {
				quoted[i] = Quote(n)
			}
			line("env", st.Yellow("passes "+strings.Join(quoted, ", ")))
		} else {
			line("env", st.Dim("no variables beyond the basics"))
		}
		if r.RuleText != "" {
			line("rule", st.Dim(Quote(r.RuleText)))
		}
	}
	for _, w := range r.Warnings {
		b.WriteString("\n  " + st.BoldYellow("!! "+Quote(w)) + "\n")
	}
	for _, f := range r.Files {
		fmt.Fprintf(&b, "\n  %s %s: %s\n", st.BoldYellow("!!"), Quote(f.Path), statusStyle(st, f.Status))
		if f.Note != "" {
			b.WriteString("     " + st.Dim(Sanitize(f.Note)) + "\n")
		}
		if f.Diff != "" {
			b.WriteString(indentLines(f.Diff, "     | ", maxDiffLines, st))
		}
	}
	b.WriteString("\n" + options(r, st))
	return b.String()
}

// statusStyle colors a file status: new files yellow, changed or deleted red.
func statusStyle(st ui.Style, status string) string {
	switch {
	case strings.HasPrefix(status, "never"):
		return st.BoldYellow(status)
	case strings.HasPrefix(status, "changed"), strings.HasPrefix(status, "deleted"):
		return st.BoldRed(status)
	}
	return st.Bold(status)
}

// diffLine colors one line of a unified diff.
func diffLine(st ui.Style, l string) string {
	switch {
	case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		return st.Bold(l)
	case strings.HasPrefix(l, "@@"):
		return st.Cyan(l)
	case strings.HasPrefix(l, "+"):
		return st.Green(l)
	case strings.HasPrefix(l, "-"):
		return st.Red(l)
	}
	return st.Dim(l)
}

// options lists the answers, one per line. The key is highlighted, and the
// default (what Enter does) is marked.
func options(r Request, st ui.Style) string {
	var b strings.Builder
	opt := func(key, text string) {
		b.WriteString("  " + st.BoldGreen("["+key+"]") + " " + text + "\n")
	}
	if r.Kind == KindBuild {
		opt("y", "build once")
		if r.CanPin {
			opt("p", "build, and remember these files as approved")
		}
		b.WriteString("  " + st.BoldRed("[n] cancel") + st.Dim("   (default: Enter)") + "\n")
		return b.String()
	}
	opt("y", "run once")
	opt("s", "run, and allow this exact call for the session")
	if r.CanPin {
		opt("p", "run, and remember these files as approved")
	}
	b.WriteString("  " + st.BoldRed("[n] deny") + st.Dim("   (default: Enter)") + "\n")
	return b.String()
}
