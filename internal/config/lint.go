package config

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/pins"
)

// Commands that run whatever they are given, so an `allow` with unrestricted
// arguments is arbitrary code execution on the host.
var codeRunner = regexp.MustCompile(`^(sh|bash|dash|zsh|ksh|fish|csh|tcsh|env|xargs|sudo|doas|su|ssh|find|awk|gawk|perl|ruby|node|php|lua|tclsh|busybox|nohup|nice|timeout|time|watch|strace|gdb|python[0-9.]*)$`)

// lint adds warnings for rules that are valid but probably not what was meant.
func (v *validator) lint(c *Config) {
	for i, r := range c.Rules {
		at := func(field string) []any { return []any{"rules", i, field} }
		allow := r.Decision == "allow"

		if allow && r.Command == "*" && openArgs(r.Args) {
			v.warnf(at("command"), `command "*" with allow lets the agent run any command on the host`)
		}

		if allow && codeRunner.MatchString(r.Command) && openArgs(r.Args) {
			v.warnf(at("command"), "allow on %q with unrestricted arguments is arbitrary code execution on the host; list the exact arguments, or use ask", r.Command)
		}

		// A rule that runs files from the workspace, without pinning them.
		if allow && !r.DependsOn.IsSet() && r.Command != "" && !strings.ContainsAny(r.Command, "*?[") && pins.HasPreset(r.Command) {
			v.warnf(at("command"), "%q runs files from the workspace that the agent can edit; add `depends_on: auto` so a change needs your approval", r.Command)
		}

		if allow {
			for j, name := range r.EnvPass {
				if secretName.MatchString(name) {
					v.warnf([]any{"rules", i, "env_pass", j}, "%s is passed to a command that runs without asking; anything that command starts can read it", name)
				}
			}
		}

		for j := 0; j < i; j++ {
			if covers(c.Rules[j], r) {
				v.warnf([]any{"rules", i}, "this rule is never reached: rules[%d] (%s) already matches everything it would", j, describe(c.Rules[j]))
				break
			}
		}
	}
}

func describe(r Rule) string {
	s := r.Command
	if r.Args != nil {
		s += " " + strings.Join(r.Args, " ")
	}
	return fmt.Sprintf("%s -> %s", strings.TrimSpace(s), r.Decision)
}

// openArgs reports whether a rule accepts arbitrary arguments: no `args` key
// at all, or a final `*`.
func openArgs(args []string) bool {
	return args == nil || (len(args) > 0 && args[len(args)-1] == "*")
}

// covers reports whether every call that b would match is already matched by
// a. It only claims coverage when sure: patterns are compared literally (or as
// the wildcard `*`), so some shadowing goes unnoticed, but nothing is reported
// wrongly.
func covers(a, b Rule) bool {
	if a.Command != "*" && a.Command != b.Command {
		if strings.ContainsAny(b.Command, "*?[") {
			return false
		}
		if ok, _ := path.Match(a.Command, b.Command); !ok {
			return false
		}
	}
	// a refuses calls that use its denied flags; b must refuse at least those too.
	for _, f := range a.DenyFlags {
		found := false
		for _, g := range b.DenyFlags {
			found = found || f == g
		}
		if !found {
			return false
		}
	}
	return argsCover(a.Args, b.Args)
}

func argsCover(a, b []string) bool {
	if a == nil {
		return true
	}
	if b == nil { // b accepts anything
		return len(a) == 1 && a[0] == "*"
	}
	aRest := len(a) > 0 && a[len(a)-1] == "*"
	bRest := len(b) > 0 && b[len(b)-1] == "*"
	aFixed, bFixed := a, b
	if aRest {
		aFixed = a[:len(a)-1]
	}
	if bRest {
		bFixed = b[:len(b)-1]
	}
	if aRest {
		if len(bFixed) < len(aFixed) {
			return false
		}
	} else if bRest || len(bFixed) != len(aFixed) {
		return false
	}
	for i, t := range aFixed {
		if t != "*" && t != bFixed[i] {
			return false
		}
	}
	return true
}
