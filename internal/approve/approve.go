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

// indentLines sanitises text and prefixes each line, cutting it at max lines.
func indentLines(text, prefix string, max int) string {
	lines := strings.Split(strings.TrimRight(Sanitize(text), "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		if i == max {
			fmt.Fprintf(&b, "%s... (%d more lines, not shown: review the file itself before approving)\n", prefix, len(lines)-max)
			break
		}
		if len(l) > 200 {
			l = l[:200] + "..."
		}
		b.WriteString(prefix + l + "\n")
	}
	return b.String()
}

// Render produces the text of the prompt. Plain text, safe to print.
func Render(r Request) string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "  %-8s %s\n", label, value) }

	if r.Kind == KindBuild {
		b.WriteString("\n  aicatraz: the project's build files need approval before the image is built\n\n")
		line("build", QuoteArgs(r.Name, r.Args))
		line("context", Quote(r.Cwd))
	} else {
		b.WriteString("\n  aicatraz: the agent wants to run a command ON THE HOST\n\n")
		line("command", QuoteArgs(r.Name, r.Args))
		line("cwd", Quote(r.Cwd))
		line("binary", Quote(r.Binary))
		if len(r.EnvPassed) > 0 {
			quoted := make([]string, len(r.EnvPassed))
			for i, n := range r.EnvPassed {
				quoted[i] = Quote(n)
			}
			line("env", "passes "+strings.Join(quoted, ", "))
		} else {
			line("env", "no variables beyond the basics")
		}
		if r.RuleText != "" {
			line("rule", Quote(r.RuleText))
		}
	}
	for _, w := range r.Warnings {
		b.WriteString("\n  !! " + Quote(w) + "\n")
	}
	for _, f := range r.Files {
		fmt.Fprintf(&b, "\n  !! %s: %s\n", Quote(f.Path), f.Status)
		if f.Note != "" {
			b.WriteString("     " + Sanitize(f.Note) + "\n")
		}
		if f.Diff != "" {
			b.WriteString(indentLines(f.Diff, "     | ", maxDiffLines))
		}
	}
	b.WriteString("\n  " + options(r) + "\n")
	return b.String()
}

func options(r Request) string {
	if r.Kind == KindBuild {
		if r.CanPin {
			return "[y] build once    [p] build, and remember these files as approved    [n] cancel"
		}
		return "[y] build once    [n] cancel"
	}
	opts := "[y] run once    [s] run, and allow this exact call for the session"
	if r.CanPin {
		opts += "    [p] run, and remember these files as approved"
	}
	return opts + "    [n] deny"
}
