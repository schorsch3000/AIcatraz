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
)

func (a Answer) String() string {
	switch a {
	case AllowOnce:
		return "once"
	case AllowSession:
		return "session"
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
	// Warnings are shown prominently, e.g. "Makefile changed since last approved".
	Warnings []string `json:"warnings,omitempty"`
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

// Render produces the text of the prompt. Plain text, safe to print.
func Render(r Request) string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "  %-8s %s\n", label, value) }

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
	for _, w := range r.Warnings {
		b.WriteString("\n  !! " + Quote(w) + "\n")
	}
	b.WriteString("\n  [y] run once    [s] run, and allow this exact call for the session    [n] deny\n")
	return b.String()
}
