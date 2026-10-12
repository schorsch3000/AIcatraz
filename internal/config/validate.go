package config

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	projectID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	secretName  = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL)`)
	placeholder = regexp.MustCompile(`\{args\[\d+\]\}`)

	knownTools = []string{"claude", "opencode"}
	decisions  = []string{"allow", "ask", "deny", "local"}
)

// NeverPassEnv lists environment variables (glob patterns) that are never
// handed to a host command, whatever the config says.
var NeverPassEnv = []string{
	"LD_*", "PATH", "BASH_ENV", "ENV", "PYTHONPATH", "NODE_OPTIONS",
	"GIT_SSH_COMMAND", "GIT_CONFIG_*", "PERL5LIB", "RUBYOPT",
}

// IsNeverPass reports whether name is in the hardcoded never-pass set.
func IsNeverPass(name string) bool {
	for _, p := range NeverPassEnv {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

type validator struct {
	root     *yaml.Node
	home     string
	problems []Problem
}

func (v *validator) add(sev Severity, msg string, p ...any) {
	v.problems = append(v.problems, Problem{
		Line:     lineOf(v.root, p...),
		Path:     pathString(p),
		Severity: sev,
		Msg:      msg,
	})
}

func (v *validator) errorf(p []any, format string, a ...any) {
	v.add(Error, fmt.Sprintf(format, a...), p...)
}

func (v *validator) warnf(p []any, format string, a ...any) {
	v.add(Warning, fmt.Sprintf(format, a...), p...)
}

func (v *validator) validate(c *Config) {
	v.project(c)
	v.image(c)
	v.volumes(c)
	v.env(c)
	v.global(c)
	v.rules(c)
	v.agent(c)
}

func (v *validator) project(c *Config) {
	if c.Project.ID != "" && !projectID.MatchString(c.Project.ID) {
		v.errorf([]any{"project", "id"}, "invalid project id %q (use letters, digits, '.', '_', '-')", c.Project.ID)
	}
}

func (v *validator) image(c *Config) {
	p := []any{"image"}
	if strings.ContainsAny(c.Image.Ref, " \t") {
		v.errorf(p, "image reference %q contains whitespace", c.Image.Ref)
	}
}

// expandHome expands a leading ~ using the host home directory.
func (v *validator) expandHome(s string) (string, error) {
	if s == "~" {
		return v.home, nil
	}
	if strings.HasPrefix(s, "~/") {
		return filepath.Join(v.home, s[2:]), nil
	}
	if strings.HasPrefix(s, "~") {
		return "", fmt.Errorf("%q: ~user is not supported, use ~/ or an absolute path", s)
	}
	return s, nil
}

func inside(parent, child string) bool {
	return child != parent && strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

func (v *validator) volumes(c *Config) {
	seen := map[string]int{}
	var vols []Volume
	var idx []int
	for i, spec := range c.Volumes {
		p := []any{"volumes", i}
		cut := strings.LastIndex(spec, ":")
		if cut < 0 {
			v.errorf(p, "volume %q needs a mode: path:rw, path:ro or path:hi", spec)
			continue
		}
		rawPath, mode := spec[:cut], Mode(spec[cut+1:])
		if mode != ModeRW && mode != ModeRO && mode != ModeHide {
			v.errorf(p, "volume %q: unknown mode %q (use rw, ro or hi)", spec, mode)
			continue
		}
		full, err := v.expandHome(rawPath)
		if err != nil {
			v.errorf(p, "volume %s", err)
			continue
		}
		if !filepath.IsAbs(full) {
			v.errorf(p, "volume %q: path must be absolute or start with ~/", spec)
			continue
		}
		full = filepath.Clean(full)

		if mode != ModeHide && (full == "/" || full == v.home || inside(full, v.home)) {
			v.errorf(p, "volume %q would expose your home directory; mount specific subdirectories instead", spec)
			continue
		}
		if prev, dup := seen[full]; dup {
			v.errorf(p, "volume %q: path already used by volumes[%d]", spec, prev)
			continue
		}
		seen[full] = i
		vols = append(vols, Volume{Path: full, Mode: mode, Spec: spec})
		idx = append(idx, i)
	}

	for k, vol := range vols {
		if vol.Mode != ModeHide {
			continue
		}
		parent := -1
		for j, other := range vols {
			if other.Mode != ModeHide && inside(other.Path, vol.Path) {
				if parent < 0 || len(other.Path) > len(vols[parent].Path) {
					parent = j
				}
			}
		}
		if parent < 0 {
			v.errorf([]any{"volumes", idx[k]}, "volume %q: a hidden path must be inside an rw or ro volume", vol.Spec)
		}
	}
	c.Resolved = vols
}

func (v *validator) env(c *Config) {
	for name := range c.Env.Static {
		if !envName.MatchString(name) {
			v.errorf([]any{"env", "static", name}, "invalid variable name %q", name)
		}
	}
	for i, name := range c.Env.FromHost {
		p := []any{"env", "from_host", i}
		if !envName.MatchString(name) {
			v.errorf(p, "invalid variable name %q", name)
			continue
		}
		if _, dup := c.Env.Static[name]; dup {
			v.errorf(p, "%s is set in both env.static and env.from_host", name)
		}
		if secretName.MatchString(name) {
			v.warnf(p, "%s looks like a secret; the agent can read it and has network access", name)
		}
	}
}

func (v *validator) global(c *Config) {
	for i, pat := range c.Global.EnvStrip {
		if _, err := path.Match(pat, ""); err != nil {
			v.errorf([]any{"global", "env_strip", i}, "bad pattern %q: %v", pat, err)
		}
	}
}

func (v *validator) rules(c *Config) {
	for i, r := range c.Rules {
		base := []any{"rules", i}
		at := func(field string) []any { return []any{"rules", i, field} }

		switch {
		case r.Command == "":
			v.errorf(base, "rule needs a command")
		case strings.Contains(r.Command, "/"):
			v.errorf(at("command"), "command %q must be a name, not a path", r.Command)
		default:
			if _, err := path.Match(r.Command, ""); err != nil {
				v.errorf(at("command"), "bad pattern %q: %v", r.Command, err)
			}
		}

		if !contains(decisions, r.Decision) {
			if r.Decision == "" {
				v.errorf(base, "rule needs a decision (%s)", strings.Join(decisions, ", "))
			} else {
				v.errorf(at("decision"), "unknown decision %q (use %s)", r.Decision, strings.Join(decisions, ", "))
			}
		}

		for j, a := range r.Args {
			if _, err := path.Match(a, ""); err != nil {
				v.errorf([]any{"rules", i, "args", j}, "bad pattern %q: %v", a, err)
			}
		}
		for j, f := range r.DenyFlags {
			if !strings.HasPrefix(f, "-") {
				v.errorf([]any{"rules", i, "deny_flags", j}, "deny_flags entry %q must start with '-'", f)
			}
		}
		for j, name := range r.EnvPass {
			p := []any{"rules", i, "env_pass", j}
			switch {
			case !envName.MatchString(name):
				v.errorf(p, "invalid variable name %q", name)
			case IsNeverPass(name):
				v.errorf(p, "%s can never be passed to a host command", name)
			}
		}
		for j, pat := range r.DependsOn.Patterns {
			p := []any{"rules", i, "depends_on", j}
			if pat == "" {
				v.errorf(p, "empty depends_on entry")
				continue
			}
			probe := placeholder.ReplaceAllString(pat, "x")
			if strings.ContainsAny(probe, "{}") {
				v.errorf(p, "bad placeholder in %q (only {args[N]} is supported)", pat)
				continue
			}
			if _, err := path.Match(probe, ""); err != nil {
				v.errorf(p, "bad pattern %q: %v", pat, err)
			}
		}

		if r.Decision == "local" || r.Decision == "deny" {
			if r.DependsOn.IsSet() {
				v.warnf(at("depends_on"), "depends_on is ignored for %q rules", r.Decision)
			}
			if len(r.EnvPass) > 0 {
				v.warnf(at("env_pass"), "env_pass is ignored for %q rules", r.Decision)
			}
		}
	}
}

func (v *validator) agent(c *Config) {
	a := c.Agent
	if a.Tool != "" && !contains(knownTools, a.Tool) {
		v.errorf([]any{"agent", "tool"}, "unknown agent %q (known: %s)", a.Tool, strings.Join(knownTools, ", "))
	}
	if strings.ContainsAny(a.Version, " \t") {
		v.errorf([]any{"agent", "version"}, "invalid version %q", a.Version)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
