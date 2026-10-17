// SPDX-License-Identifier: GPL-3.0-only

// Package policy decides what happens to a command the agent asks to run.
// First matching rule wins; no match means deny.
package policy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
)

// Action is the outcome of a decision.
type Action string

const (
	Allow Action = "allow"
	Ask   Action = "ask"
	Deny  Action = "deny"
	Local Action = "local"
)

// NoRule is the Rule index reported when no rule matched.
const NoRule = -1

// Decision is the result of matching a request.
type Decision struct {
	Action Action
	Rule   int          // index into the config's rules, or NoRule
	Config *config.Rule // the matching rule, nil if none
}

// Policy is a compiled rule list.
type Policy struct {
	rules []compiled
}

type compiled struct {
	rule    config.Rule
	command *regexp.Regexp
	args    []*regexp.Regexp // nil: any arguments
	rest    bool             // the last pattern is a bare `*`: it absorbs the remaining arguments
}

// New compiles rules.
func New(rules []config.Rule) (*Policy, error) {
	p := &Policy{}
	for i, r := range rules {
		c := compiled{rule: r}
		var err error
		if c.command, err = glob(r.Command); err != nil {
			return nil, fmt.Errorf("rules[%d]: command: %w", i, err)
		}
		if r.Args != nil {
			pats := r.Args
			if n := len(pats); n > 0 && pats[n-1] == "*" {
				c.rest = true
				pats = pats[:n-1]
			}
			c.args = make([]*regexp.Regexp, 0, len(pats))
			for _, a := range pats {
				re, err := glob(a)
				if err != nil {
					return nil, fmt.Errorf("rules[%d]: args: %w", i, err)
				}
				c.args = append(c.args, re)
			}
		}
		p.rules = append(p.rules, c)
	}
	return p, nil
}

// Decide matches a command name and its arguments against the rules.
func (p *Policy) Decide(name string, args []string) Decision {
	for i := range p.rules {
		c := &p.rules[i]
		if c.matches(name, args) {
			return Decision{Action: Action(c.rule.Decision), Rule: i, Config: &c.rule}
		}
	}
	return Decision{Action: Deny, Rule: NoRule}
}

func (c *compiled) matches(name string, args []string) bool {
	if !c.command.MatchString(name) {
		return false
	}
	if c.args != nil {
		if c.rest {
			if len(args) < len(c.args) {
				return false
			}
		} else if len(args) != len(c.args) {
			return false
		}
		for i, re := range c.args {
			if !re.MatchString(args[i]) {
				return false
			}
		}
	}
	// A rule never grants an argument vector that uses one of its denied flags.
	for _, a := range args {
		for _, f := range c.rule.DenyFlags {
			if usesFlag(a, f) {
				return false
			}
		}
	}
	return true
}

// usesFlag reports whether argument a could be the flag f. It leans towards
// "yes" when unsure, because a rule that doesn't match falls through to later
// rules and finally to deny:
//   - `--file`, `--file=x`, and any prefix of a long flag (GNU tools accept
//     unambiguous abbreviations, so `--fi` can mean `--file`);
//   - `-f`, `-ffile`, and `-nf` (short flags bundled into one argument).
func usesFlag(a, f string) bool {
	if a == "--" || !strings.HasPrefix(a, "-") {
		return false
	}
	if strings.HasPrefix(f, "--") {
		if !strings.HasPrefix(a, "--") {
			return false
		}
		name, _, _ := strings.Cut(a, "=")
		return len(name) > 2 && strings.HasPrefix(f, name)
	}
	if strings.HasPrefix(a, "--") {
		return false
	}
	return strings.Contains(a[1:], f[1:])
}

// glob compiles a pattern where `*` matches any characters (including `/`),
// `?` one character, `[...]` a class (`[!...]` negated), `\` escapes.
func glob(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`^(?s:`)
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := strings.IndexByte(g[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("pattern %q: unterminated character class", g)
			}
			class := g[i+1 : i+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += j
		case '\\':
			if i+1 < len(g) {
				i++
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", g, err)
	}
	return re, nil
}
