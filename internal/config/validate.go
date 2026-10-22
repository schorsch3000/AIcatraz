// SPDX-License-Identifier: GPL-3.0-only

package config

import (
	"fmt"
	"net/netip"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/schorsch3000/aicatraz/internal/devices"
	"github.com/schorsch3000/aicatraz/internal/egress"
	"github.com/schorsch3000/aicatraz/internal/mux"
	"github.com/schorsch3000/aicatraz/internal/pins"
)

var (
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	memorySize  = regexp.MustCompile(`^[1-9][0-9]*[bkmgBKMG]?$`)
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
	dir      string // base for relative volume paths
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
	v.limits(c)
	v.approval(c)
	v.canary(c)
	v.global(c)
	v.rules(c)
	v.lint(c)
	v.agent(c)
	v.network(c)
	v.ports(c)
	v.devices(c)
	v.bar(c)
	v.copyFiles(c)
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

// isRelative reports whether a volume path is written relative to the config
// file: `.`, `..`, `./x` or `../x`. A bare `x` is not, to keep typos visible.
func isRelative(s string) bool {
	return s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// expandHome expands a leading ~ using the host home directory, and a
// relative path using the config file's directory.
func (v *validator) expandHome(s string) (string, error) {
	if isRelative(s) {
		if v.dir == "" {
			return "", fmt.Errorf("%q: relative paths need to know the config file's directory", s)
		}
		return filepath.Join(v.dir, s), nil
	}
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
			v.errorf(p, "volume %q: path must be absolute, or start with ~/, ./ or ../", spec)
			continue
		}
		full = filepath.Clean(full)
		if strings.ContainsAny(full, ",\n") {
			v.errorf(p, "volume %q: commas and newlines in paths are not supported", spec)
			continue
		}

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

func (v *validator) limits(c *Config) {
	if c.Limits.Pids < 0 {
		v.errorf([]any{"limits", "pids"}, "pids must not be negative")
	}
	if m := c.Limits.Memory; m != "" && !memorySize.MatchString(m) {
		v.errorf([]any{"limits", "memory"}, "invalid memory size %q (examples: 512m, 2g)", m)
	}
}

func (v *validator) approval(c *Config) {
	check := func(key, val string, lo, hi time.Duration, loText, hiText, examples string) {
		if val == "" {
			return
		}
		d, err := time.ParseDuration(val)
		switch {
		case err != nil:
			v.errorf([]any{"approval", key}, "invalid duration %q (examples: %s)", val, examples)
		case d < lo || d > hi:
			v.errorf([]any{"approval", key}, "%s must be between %s and %s", key, loText, hiText)
		}
	}
	check("timeout", c.Approval.Timeout, 5*time.Second, time.Hour, "5s", "1h", "90s, 2m")
	check("idle", c.Approval.Idle, 0, 10*time.Minute, "0", "10m", "5s, 0")
	check("guard", c.Approval.Guard, 100*time.Millisecond, 5*time.Second, "100ms", "5s", "500ms, 1s")
}

func (v *validator) canary(c *Config) {
	switch c.Canary.Action {
	case "", "stop", "warn", "off":
	default:
		v.errorf([]any{"canary", "action"}, "unknown action %q (use stop, warn or off)", c.Canary.Action)
	}
	if t := c.Canary.Interval; t != "" {
		d, err := time.ParseDuration(t)
		switch {
		case err != nil:
			v.errorf([]any{"canary", "interval"}, "invalid duration %q (examples: 3s, 10s)", t)
		case d < 200*time.Millisecond || d > time.Hour:
			v.errorf([]any{"canary", "interval"}, "interval must be between 200ms and 1h")
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

		if r.DependsOn.Auto && r.Command != "" && !strings.ContainsAny(r.Command, "*?[") && !pins.HasPreset(r.Command) {
			v.warnf(at("depends_on"), "no built-in file set for %q: list the files it depends on explicitly", r.Command)
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
	if a.Tool != "" && c.NetworkMode() == NetOff {
		v.warnf([]any{"agent", "tool"}, "the network is off, so %s can't reach its service: add `network: open`, or rules, to the config", a.Tool)
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

func (v *validator) network(c *Config) {
	n := c.Network
	if n == nil || n.Mode != NetFiltered {
		return
	}
	switch n.Default {
	case "", "ask", "deny":
	default:
		v.errorf([]any{"network", "default"}, "unknown default %q (use ask or deny)", n.Default)
	}
	switch n.DNS {
	case "", "open", "restricted":
	default:
		v.errorf([]any{"network", "dns"}, "unknown dns mode %q (use open or restricted)", n.DNS)
	}
	for i, r := range n.Resolvers {
		if a, err := netip.ParseAddr(r); err != nil || !a.Is4() {
			v.errorf([]any{"network", "resolvers", i}, "resolver %q is not an IPv4 address", r)
		}
	}

	for i, r := range n.Rules {
		at := func(field string) []any { return []any{"network", "rules", i, field} }
		rule := egress.Rule{Decision: egress.Decision(r.Decision)}
		bad := false

		tg, err := egress.ParseTarget(r.To)
		if err != nil {
			if r.To == "" {
				v.errorf([]any{"network", "rules", i}, "rule needs `to`: a host name, *.domain, an IP address, a CIDR range or *")
			} else {
				v.errorf(at("to"), "%v", err)
			}
			bad = true
		}
		rule.Target = tg

		switch r.Decision {
		case "allow", "ask", "deny":
		case "":
			v.errorf([]any{"network", "rules", i}, "rule needs a decision (allow, ask, deny)")
			bad = true
		default:
			v.errorf(at("decision"), "unknown decision %q (use allow, ask or deny)", r.Decision)
			bad = true
		}

		if r.Ports != "" {
			if rule.Ports, err = egress.ParsePorts(string(r.Ports)); err != nil {
				v.errorf(at("ports"), "%v", err)
				bad = true
			}
		}
		if r.Proto != "" {
			if rule.Protos, err = egress.ParseProtos([]string{string(r.Proto)}); err != nil {
				v.errorf(at("proto"), "%v", err)
				bad = true
			}
		}
		if bad {
			continue
		}

		for _, p := range rule.Protos {
			if p != egress.TCP && !rule.Target.IsAddress() {
				v.errorf(at("proto"), "%s rules need an IP address or CIDR range as `to`: only TCP knows the names (and * means any TCP destination)", p)
			}
			if p == egress.ICMP && !rule.Ports.IsEmpty() {
				v.warnf(at("ports"), "ports mean nothing for icmp")
			}
		}
		if rule.Decision == egress.Ask && (rule.AppliesTo(egress.UDP) || rule.AppliesTo(egress.ICMP)) && rule.Target.IsAddress() {
			v.warnf(at("decision"), "ask works for TCP only: for udp and icmp this rule counts as deny")
		}
		if rule.Decision == egress.Allow && rule.Target.IsAddress() && rule.Target.Prefix().Bits() == 0 {
			v.warnf(at("to"), "allowing 0.0.0.0/0 lets the jail connect anywhere; the network section then limits nothing")
		}
		if rule.Decision == egress.Allow && rule.Target.IsAny() && rule.Ports.IsEmpty() {
			v.warnf(at("to"), "allowing * on every port lets the jail connect anywhere public; the network section then limits nothing")
		}
		if rule.Target.IsAddress() && !egress.IsSpecial(rule.Target.Prefix().Addr()) && rule.Decision == egress.Allow &&
			rule.Target.Prefix().Bits() < 8 && rule.Target.Prefix().Bits() > 0 {
			v.warnf(at("to"), "%s is a very large range", rule.Target)
		}
		c.NetRules = append(c.NetRules, rule)
	}
}

func (v *validator) ports(c *Config) {
	if len(c.Ports) == 0 {
		return
	}
	if c.NetworkMode() == NetOff {
		v.errorf([]any{"ports"}, "ports need a network: the jail has none (add `network: open`, or rules, to the config)")
		return
	}
	seen := map[string]int{}
	for i, spec := range c.Ports {
		p := []any{"ports", i}
		pm, err := ParsePortMap(spec)
		if err != nil {
			v.errorf(p, "%v", err)
			continue
		}
		for port := pm.HostLo; port <= pm.HostHi; port++ {
			key := fmt.Sprintf("%s/%s:%d", pm.Proto, pm.HostIP, port)
			if prev, dup := seen[key]; dup {
				v.errorf(p, "host port %d (%s) is already published by ports[%d]", port, pm.Proto, prev)
				break
			}
			seen[key] = i
		}
		if !pm.Loopback() {
			v.warnf(p, "%s is published on %s: other machines can connect to it, not only this one", spec, pm.HostIP)
		}
		c.PortMaps = append(c.PortMaps, pm)
	}
}

var usbID = regexp.MustCompile(`^[0-9a-fA-F?*]{1,4}:[0-9a-fA-F?*]{1,4}$`)

func (v *validator) devices(c *Config) {
	for i, d := range c.Devices {
		at := func(field string) []any { return []any{"devices", i, field} }
		base := []any{"devices", i}

		set := 0
		for _, s := range []string{d.Serial, d.USB, d.Path} {
			if s != "" {
				set++
			}
		}
		if set != 1 {
			v.errorf(base, "a device needs exactly one of serial, usb and path")
			continue
		}
		if d.USBSerial != "" && d.USB == "" {
			v.errorf(at("usb_serial"), "usb_serial goes with usb")
			continue
		}
		spec := devices.Spec{Serial: d.Serial, USB: strings.ToLower(d.USB), USBSerial: d.USBSerial, Path: d.Path}

		switch {
		case d.Serial != "":
			if _, err := path.Match(d.Serial, ""); err != nil || strings.Contains(d.Serial, "/") {
				v.errorf(at("serial"), "serial %q must be a name like usb-Arduino_*, as listed in /dev/serial/by-id", d.Serial)
				continue
			}
		case d.USB != "":
			if !usbID.MatchString(d.USB) {
				v.errorf(at("usb"), "usb %q must be vendor:product in hex, such as 2341:0043 or 2e8a:* (see `aicatraz devices`)", d.USB)
				continue
			}
			if _, err := path.Match(d.USBSerial, ""); err != nil {
				v.errorf(at("usb_serial"), "bad pattern %q", d.USBSerial)
				continue
			}
		default:
			clean := path.Clean(d.Path)
			switch {
			case clean != d.Path || !strings.HasPrefix(d.Path, "/dev/"):
				v.errorf(at("path"), "path %q must be a clean path under /dev", d.Path)
				continue
			case devices.Reserved(clean):
				v.errorf(at("path"), "%s can't be given to the jail: it belongs to the container or is too dangerous", d.Path)
				continue
			case clean == "/dev/*" || clean == "/dev/**":
				v.errorf(at("path"), "path %q matches every device; name the ones you mean", d.Path)
				continue
			}
			if _, err := path.Match(d.Path, ""); err != nil {
				v.errorf(at("path"), "bad pattern %q", d.Path)
				continue
			}
			if strings.ContainsAny(d.Path, "*?[") {
				v.warnf(at("path"), "%s can match more than one device; each one that is plugged in is given to the jail", d.Path)
			}
		}
		c.DeviceSpecs = append(c.DeviceSpecs, spec)
	}
}

func (v *validator) bar(c *Config) {
	b := c.Bar
	if b.Symbol != nil {
		sym := *b.Symbol
		switch {
		case utf8.RuneCountInString(sym) > 8:
			v.errorf([]any{"bar", "symbol"}, "symbol is too long (at most 8 characters)")
		case strings.ContainsFunc(sym, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }):
			v.errorf([]any{"bar", "symbol"}, "symbol must not contain control characters")
		}
	}
	if w := b.SymbolWidth; w != nil && (*w < 0 || *w > 4) {
		v.errorf([]any{"bar", "symbol_width"}, "symbol_width must be between 0 and 4")
	}
	// The same checks that pick the colors at launch, so a mistake shows up here.
	if b.Theme != "" {
		if _, err := mux.ResolveColors(b.Theme, "", ""); err != nil {
			v.errorf([]any{"bar", "theme"}, "%v", err)
		}
	}
	if b.FG != "" {
		if _, err := mux.ParseColor(b.FG, false); err != nil {
			v.errorf([]any{"bar", "fg"}, "%v", err)
		}
	}
	if b.BG != "" {
		if _, err := mux.ParseColor(b.BG, true); err != nil {
			v.errorf([]any{"bar", "bg"}, "%v", err)
		}
	}
}

func (v *validator) copyFiles(c *Config) {
	seen := map[string]bool{}
	for i, f := range c.Copy {
		p := []any{"copy", i}
		if f.From == "" || f.To == "" {
			v.errorf(p, "copy needs both `from` and `to`")
			continue
		}
		from, err := v.expandHome(f.From)
		if err != nil {
			v.errorf(append(p, "from"), "%v", err)
			continue
		}
		to := path.Clean(filepath.ToSlash(f.To))
		if strings.HasSuffix(f.To, "/") { // a directory: keep the file's name
			base := filepath.Base(from)
			if base == "/" || base == "." {
				v.errorf(append(p, "from"), "%q has no file name to keep", f.From)
				continue
			}
			to = path.Join(to, base)
		}
		if filepath.IsAbs(f.To) || to == "." || to == ".." || strings.HasPrefix(to, "../") {
			v.errorf(append(p, "to"), "%q must be a path inside the project directory", f.To)
			continue
		}
		if seen[to] {
			v.errorf(append(p, "to"), "%q is already the target of another entry", f.To)
			continue
		}
		seen[to] = true
		c.CopyFiles = append(c.CopyFiles, CopyFile{From: from, To: to})
	}
}
