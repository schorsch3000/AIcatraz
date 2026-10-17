// SPDX-License-Identifier: GPL-3.0-only

package config

import (
	"os"
	"strings"
	"testing"
)

var opts = Options{Home: "/home/test"}

func TestRelativeVolumesResolveAgainstTheConfigDirectory(t *testing.T) {
	o := Options{Home: "/home/test", Dir: "/srv/checkout/app"}
	cfg, ps := Parse([]byte("volumes:\n  - .:rw\n  - ./.env:hi\n  - ../shared:ro\n  - ~/x:ro\n  - /opt/y:ro\n"), o)
	if len(ps) != 0 {
		t.Fatalf("problems: %+v", ps)
	}
	want := []string{"/srv/checkout/app", "/srv/checkout/app/.env", "/srv/checkout/shared", "/home/test/x", "/opt/y"}
	for i, w := range want {
		if cfg.Resolved[i].Path != w {
			t.Errorf("volume %d: got %q, want %q", i, cfg.Resolved[i].Path, w)
		}
	}

	// Bare names stay an error; relative paths can't sneak in the home directory.
	_, ps = Parse([]byte("volumes:\n  - foo:rw\n"), o)
	if len(ps) != 1 || !strings.Contains(ps[0].Msg, "./") {
		t.Errorf("bare name: %+v", ps)
	}
	_, ps = Parse([]byte("volumes:\n  - .:rw\n"), Options{Home: "/srv/checkout/app", Dir: "/srv/checkout/app"})
	if len(ps) != 1 || !strings.Contains(ps[0].Msg, "home directory") {
		t.Errorf("relative path to home: %+v", ps)
	}
}

func TestValidExample(t *testing.T) {
	data, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, ps := Parse(data, opts)
	if len(ps) != 0 {
		t.Fatalf("unexpected problems: %+v", ps)
	}
	want := []Volume{
		{Path: "/home/test/git/myproject", Mode: ModeRW},
		{Path: "/home/test/usr/local/bin/mytool", Mode: ModeRO},
		{Path: "/home/test/git/myproject/.env", Mode: ModeHide},
	}
	if len(cfg.Resolved) != len(want) {
		t.Fatalf("got %d volumes", len(cfg.Resolved))
	}
	for i, w := range want {
		g := cfg.Resolved[i]
		if g.Path != w.Path || g.Mode != w.Mode {
			t.Errorf("volume %d: got %s:%s, want %s:%s", i, g.Path, g.Mode, w.Path, w.Mode)
		}
	}
	if !cfg.Rules[1].DependsOn.Auto {
		t.Error("depends_on: auto not parsed")
	}
	if got := cfg.Rules[3].DependsOn.Patterns; len(got) != 2 || got[0] != "{args[0]}" {
		t.Errorf("depends_on patterns: %v", got)
	}
	if !cfg.Agent.SkipPermissionsOrDefault() {
		t.Error("skip_permissions should be on")
	}
}

func TestImageForms(t *testing.T) {
	cfg, ps := Parse([]byte("image: debian:13\n"), opts)
	if len(ps) != 0 || cfg.Image.Ref != "debian:13" {
		t.Fatalf("ref form: %+v %+v", cfg.Image, ps)
	}
	_, ps = Parse([]byte("image:\n  dockerfile: Dockerfile.az\n  bogus: x\n"), opts)
	if len(ps) != 1 || ps[0].Line != 3 || !strings.Contains(ps[0].Msg, "bogus") {
		t.Fatalf("unknown image key: %+v", ps)
	}
	_, ps = Parse([]byte("image:\n  dockerfile: Dockerfile.az\n"), opts)
	if len(ps) != 0 {
		t.Fatalf("%+v", ps)
	}
}

func TestSkipPermissionsDefault(t *testing.T) {
	cfg, _ := Parse([]byte("agent: {tool: claude}\n"), opts)
	if !cfg.Agent.SkipPermissionsOrDefault() {
		t.Error("default should be on")
	}
	cfg, _ = Parse([]byte("agent: {tool: claude, skip_permissions: false}\n"), opts)
	if cfg.Agent.SkipPermissionsOrDefault() {
		t.Error("explicit false must turn it off")
	}
}

func TestProblems(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		line int
		sev  Severity
		msg  string
	}{
		{"unknown top-level key", "volumez: []\n", 1, Error, "volumez"},
		{"unknown rule key", "rules:\n  - command: make\n    decision: allow\n    argz: [x]\n", 4, Error, "argz"},
		{"missing mode", "volumes:\n  - ~/a\n", 2, Error, "needs a mode"},
		{"bad mode", "volumes:\n  - ~/a:rwx\n", 2, Error, "unknown mode"},
		{"relative path", "volumes:\n  - foo/bar:rw\n", 2, Error, "absolute"},
		{"~user", "volumes:\n  - ~bob/x:rw\n", 2, Error, "~user"},
		{"home itself", "volumes:\n  - ~:rw\n", 2, Error, "home directory"},
		{"root", "volumes:\n  - /:ro\n", 2, Error, "home directory"},
		{"parent of home", "volumes:\n  - /home:ro\n", 2, Error, "home directory"},
		{"duplicate", "volumes:\n  - ~/a:rw\n  - ~/a/:ro\n", 3, Error, "already used"},
		{"hide without parent", "volumes:\n  - ~/a/.env:hi\n", 2, Error, "inside an rw or ro"},
		{"hide inside hide only", "volumes:\n  - ~/a:hi\n  - ~/a/b:hi\n", 2, Error, "inside an rw or ro"},
		{"comma in path", "volumes:\n  - ~/a,b:rw\n", 2, Error, "commas"},
		{"bad memory", "limits:\n  memory: lots\n", 2, Error, "invalid memory"},
		{"negative pids", "limits:\n  pids: -1\n", 2, Error, "negative"},
		{"bad approval timeout", "approval:\n  timeout: soon\n", 2, Error, "invalid duration"},
		{"approval timeout too short", "approval:\n  timeout: 1s\n", 2, Error, "between"},
		{"bad canary action", "canary:\n  action: shrug\n", 2, Error, "unknown action"},
		{"bad canary interval", "canary:\n  interval: often\n", 2, Error, "invalid duration"},
		{"canary interval too short", "canary:\n  interval: 1ms\n", 2, Error, "between"},
		{"bad project id", "project:\n  id: \"a b\"\n", 2, Error, "invalid project id"},
		{"static and from_host", "env:\n  static: {A: b}\n  from_host: [A]\n", 3, Error, "both"},
		{"secret from_host", "env:\n  from_host: [GITHUB_TOKEN]\n", 2, Warning, "looks like a secret"},
		{"bad env name", "env:\n  from_host: ['1X']\n", 2, Error, "invalid variable name"},
		{"missing decision", "rules:\n  - command: make\n", 2, Error, "needs a decision"},
		{"bad decision", "rules:\n  - command: make\n    decision: maybe\n", 3, Error, "unknown decision"},
		{"command is a path", "rules:\n  - command: /usr/bin/make\n    decision: allow\n", 2, Error, "not a path"},
		{"missing command", "rules:\n  - decision: allow\n", 2, Error, "needs a command"},
		{"deny_flags without dash", "rules:\n  - command: make\n    decision: ask\n    deny_flags: [f]\n", 4, Error, "must start with"},
		{"never-pass env", "rules:\n  - command: make\n    decision: allow\n    env_pass: [LD_PRELOAD]\n", 4, Error, "never be passed"},
		{"never-pass PATH", "rules:\n  - command: make\n    decision: allow\n    env_pass: [PATH]\n", 4, Error, "never be passed"},
		{"depends_on bad scalar", "rules:\n  - command: make\n    decision: allow\n    depends_on: manual\n", 4, Error, "auto"},
		{"depends_on bad placeholder", "rules:\n  - command: make\n    decision: allow\n    depends_on: [\"{args[x]}\"]\n", 4, Error, "placeholder"},
		{"depends_on on local", "rules:\n  - command: make\n    decision: local\n    depends_on: auto\n", 4, Warning, "ignored"},
		{"env_pass on deny", "rules:\n  - command: make\n    decision: deny\n    env_pass: [FOO]\n", 4, Warning, "ignored"},
		{"auto without preset", "rules:\n  - command: curl\n    decision: ask\n    depends_on: auto\n", 4, Warning, "no built-in file set"},
		{"unknown agent", "agent:\n  tool: gemini\n", 2, Error, "unknown agent"},
		{"bad glob", "rules:\n  - command: make\n    decision: allow\n    args: ['[']\n", 4, Error, "bad pattern"},
		{"bad yaml", "volumes: [\n", 0, Error, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ps := Parse([]byte(tc.yaml), opts)
			if len(ps) == 0 {
				t.Fatal("expected a problem, got none")
			}
			if tc.name == "bad yaml" {
				if !HasErrors(ps) {
					t.Fatalf("expected an error: %+v", ps)
				}
				return
			}
			// Lint warnings may come first; look for the one this case is about.
			p := ps[0]
			for _, cand := range ps {
				if strings.Contains(cand.Msg, tc.msg) {
					p = cand
					break
				}
			}
			if p.Severity != tc.sev || p.Line != tc.line || !strings.Contains(p.Msg, tc.msg) {
				t.Errorf("got line %d sev %d %q; want line %d sev %d containing %q", p.Line, p.Severity, p.Msg, tc.line, tc.sev, tc.msg)
			}
		})
	}
}

func TestHasErrors(t *testing.T) {
	if HasErrors([]Problem{{Severity: Warning}}) {
		t.Error("warnings are not errors")
	}
	if !HasErrors([]Problem{{Severity: Warning}, {Severity: Error}}) {
		t.Error("error missed")
	}
}

func TestIsNeverPass(t *testing.T) {
	for _, n := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "PATH", "GIT_CONFIG_COUNT", "BASH_ENV"} {
		if !IsNeverPass(n) {
			t.Errorf("%s should be never-pass", n)
		}
	}
	for _, n := range []string{"SSH_AUTH_SOCK", "HOME", "PATHS"} {
		if IsNeverPass(n) {
			t.Errorf("%s should be allowed", n)
		}
	}
}

func TestEmptyFile(t *testing.T) {
	cfg, ps := Parse([]byte(""), opts)
	if cfg == nil || len(ps) != 0 {
		t.Fatalf("empty config should be valid: %+v", ps)
	}
}

func TestApprovalTimeout(t *testing.T) {
	cfg, ps := Parse([]byte("approval:\n  timeout: 90s\n"), opts)
	if len(ps) != 0 || cfg.Approval.TimeoutOrDefault().Seconds() != 90 {
		t.Fatalf("%+v %v", ps, cfg.Approval.TimeoutOrDefault())
	}
	cfg, _ = Parse([]byte(""), opts)
	if cfg.Approval.TimeoutOrDefault() != DefaultApprovalTimeout {
		t.Error("default")
	}
}

func lintMessages(t *testing.T, yaml string) []Problem {
	t.Helper()
	_, ps := Parse([]byte(yaml), opts)
	for _, p := range ps {
		if p.Severity == Error {
			t.Fatalf("unexpected error: %+v", p)
		}
	}
	return ps
}

func TestLintBadConfigs(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		line int
		msg  string
	}{
		{"bare star allow", "rules:\n  - command: \"*\"\n    decision: allow\n", 2, "any command on the host"},
		{"shell with open args", "rules:\n  - command: bash\n    decision: allow\n", 2, "arbitrary code execution"},
		{"python with a final star", "rules:\n  - command: python3\n    args: [\"*\"]\n    decision: allow\n", 2, "arbitrary code execution"},
		{"make without depends_on", "rules:\n  - command: make\n    args: [deploy]\n    decision: allow\n", 2, "depends_on: auto"},
		{"git allow without depends_on", "rules:\n  - command: git\n    args: [status]\n    decision: allow\n", 2, "depends_on: auto"},
		{"secret passed without asking", "rules:\n  - command: deploy\n    decision: allow\n    env_pass: [DEPLOY_TOKEN]\n", 4, "DEPLOY_TOKEN"},
		{"shadowed by catch-all", "rules:\n  - command: \"*\"\n    decision: deny\n  - command: ls\n    decision: allow\n", 4, "never reached"},
		{"duplicate", "rules:\n  - command: ls\n    decision: allow\n  - command: ls\n    decision: ask\n", 4, "never reached"},
		{"shadowed by a wider args rule", "rules:\n  - command: git\n    args: [push, \"*\"]\n    decision: ask\n    depends_on: auto\n  - command: git\n    args: [push, origin]\n    decision: allow\n    depends_on: auto\n", 6, "rules[0]"},
		{"shadowed by a glob command", "rules:\n  - command: \"py*\"\n    decision: deny\n  - command: python3\n    args: [x.py]\n    decision: allow\n    depends_on: auto\n", 4, "never reached"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ps := lintMessages(t, tc.yaml)
			for _, p := range ps {
				if p.Severity == Warning && p.Line == tc.line && strings.Contains(p.Msg, tc.msg) {
					return
				}
			}
			t.Errorf("no warning at line %d containing %q; got %+v", tc.line, tc.msg, ps)
		})
	}
}

func TestLintLeavesGoodConfigsAlone(t *testing.T) {
	good := []string{
		// Exact arguments and pinned files.
		"rules:\n  - command: make\n    args: [deploy-test]\n    decision: allow\n    depends_on: auto\n",
		// A shell is fine behind a human.
		"rules:\n  - command: bash\n    decision: ask\n",
		// Different arguments are not shadowed.
		"rules:\n  - command: git\n    args: [status]\n    decision: allow\n    depends_on: auto\n  - command: git\n    args: [log]\n    decision: allow\n    depends_on: auto\n",
		// A narrower rule in front of a broader one is how carve-outs work.
		"rules:\n  - command: make\n    args: [deploy]\n    decision: deny\n  - command: make\n    decision: ask\n",
		// A rule with denied flags does not cover a later rule that allows them.
		"rules:\n  - command: make\n    args: [\"*\"]\n    deny_flags: [-f]\n    decision: ask\n    depends_on: auto\n  - command: make\n    args: [-f, x]\n    decision: ask\n    depends_on: auto\n",
		// Exact args after an open rule that comes later.
		"rules:\n  - command: ls\n    args: [-l]\n    decision: allow\n  - command: ls\n    decision: ask\n",
		// An explicit catch-all deny at the end.
		"rules:\n  - command: ls\n    decision: allow\n  - command: \"*\"\n    decision: deny\n",
		// local commands and denies are not host code execution.
		"rules:\n  - command: bash\n    decision: local\n  - command: make\n    decision: local\n",
	}
	for i, y := range good {
		if ps := lintMessages(t, y); len(ps) != 0 {
			t.Errorf("config %d: unexpected findings %+v\n%s", i, ps, y)
		}
	}
}

func TestArgsCover(t *testing.T) {
	star := []string{"*"}
	for _, tc := range []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{nil, []string{"x"}, true},
		{[]string{"x"}, nil, false},
		{star, nil, true},
		{star, []string{"x", "y"}, true},
		{[]string{"x"}, []string{"x"}, true},
		{[]string{"x"}, []string{"y"}, false},
		{[]string{"x"}, []string{"x", "y"}, false},
		{[]string{"x", "*"}, []string{"x"}, true},
		{[]string{"x", "*"}, []string{"x", "y", "z"}, true},
		{[]string{"x", "*"}, []string{"y", "z"}, false},
		{[]string{"x", "y", "*"}, []string{"x", "*"}, false}, // b also matches plain `x`
		{[]string{"x"}, []string{"x", "*"}, false},           // b also matches `x y`
		{[]string{"*", "b"}, []string{"a", "b"}, true},
		{[]string{}, []string{}, true},
		{[]string{}, []string{"x"}, false},
	} {
		if got := argsCover(tc.a, tc.b); got != tc.want {
			t.Errorf("argsCover(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCanaryDefaults(t *testing.T) {
	cfg, ps := Parse([]byte(""), opts)
	if len(ps) != 0 || cfg.Canary.ActionOrDefault() != "stop" || cfg.Canary.IntervalOrDefault() != DefaultCanaryInterval {
		t.Fatalf("%+v %+v", ps, cfg.Canary)
	}
	cfg, ps = Parse([]byte("canary:\n  action: warn\n  interval: 500ms\n"), opts)
	if len(ps) != 0 || cfg.Canary.ActionOrDefault() != "warn" || cfg.Canary.IntervalOrDefault().Milliseconds() != 500 {
		t.Fatalf("%+v %+v", ps, cfg.Canary)
	}
}

func TestSudoOption(t *testing.T) {
	cfg, ps := Parse([]byte("sudo: true\n"), opts)
	if len(ps) != 0 || !cfg.Sudo {
		t.Errorf("sudo: true: %+v %v", ps, cfg.Sudo)
	}
	cfg, ps = Parse([]byte("volumes: []\n"), opts)
	if len(ps) != 0 || cfg.Sudo {
		t.Errorf("sudo must default to off: %+v %v", ps, cfg.Sudo)
	}
	if _, ps = Parse([]byte("sudo: maybe\n"), opts); len(ps) == 0 {
		t.Error("sudo: maybe must be rejected")
	}
}

func TestNetworkSection(t *testing.T) {
	cfg, ps := Parse([]byte(`
network:
  default: deny
  dns: restricted
  resolvers: [1.1.1.1, 9.9.9.9]
  rules:
    - to: api.anthropic.com
      decision: allow
    - to: "*.github.com"
      ports: [22, 443, 8000-8080]
      decision: allow
    - to: 10.20.0.0/16
      ports: "5432, 5433"
      proto: [tcp, udp]
      decision: allow
    - to: 1.1.1.1
      ports: 53
      proto: udp
      decision: allow
    - to: 8.8.8.8
      proto: icmp
      decision: allow
    - to: "*"
      ports: "*"
      decision: ask
`), opts)
	if len(ps) != 0 {
		t.Fatalf("problems: %+v", ps)
	}
	n := cfg.Network
	if n == nil || n.DefaultOrAsk() != "deny" || n.DNS != "restricted" || len(n.Resolvers) != 2 || len(cfg.NetRules) != 6 {
		t.Fatalf("network: %+v rules %d", n, len(cfg.NetRules))
	}
	r := cfg.NetRules
	if r[1].Ports.String() != "22, 443, 8000-8080" || r[2].Ports.String() != "5432-5433" || r[3].Ports.String() != "53" || !r[5].Ports.IsAny() {
		t.Errorf("ports: %v | %v | %v", r[1].Ports, r[2].Ports, r[3].Ports)
	}
	if !r[3].AppliesTo("udp") || r[3].AppliesTo("tcp") || !r[4].AppliesTo("icmp") || r[0].AppliesTo("udp") {
		t.Error("protocols")
	}

	cfg, ps = Parse([]byte("network: {}\n"), opts)
	if len(ps) != 0 || cfg.Network == nil || cfg.Network.DefaultOrAsk() != "ask" {
		t.Errorf("an empty network section means ask: %+v %v", cfg.Network, ps)
	}
	cfg, _ = Parse([]byte("volumes: []\n"), opts)
	if cfg.Network != nil {
		t.Error("no section: the network is open")
	}
}

func TestNetworkSectionErrors(t *testing.T) {
	for yaml, want := range map[string]string{
		"network: {default: maybe}":                                              "unknown default",
		"network: {dns: closed}":                                                 "unknown dns mode",
		`network: {resolvers: ["::1"]}`:                                          "not an IPv4",
		"network: {rules: [{decision: allow}]}":                                  "needs `to`",
		"network: {rules: [{to: example.com}]}":                                  "needs a decision",
		"network: {rules: [{to: example.com, decision: maybe}]}":                 "unknown decision",
		"network: {rules: [{to: '::1', decision: allow}]}":                       "IPv6",
		"network: {rules: [{to: 'a b', decision: allow}]}":                       "not a host name",
		"network: {rules: [{to: example.com, ports: '99999', decision: allow}]}": "bad port",
		"network: {rules: [{to: example.com, ports: 'x', decision: allow}]}":     "bad port",
		"network: {rules: [{to: example.com, proto: sctp, decision: allow}]}":    "unknown protocol",
		"network: {rules: [{to: example.com, proto: udp, decision: allow}]}":     "need an IP address",
		"network: {rules: [{to: '*', proto: icmp, decision: allow}]}":            "need an IP address",
	} {
		_, ps := Parse([]byte(yaml+"\n"), opts)
		found := false
		for _, p := range ps {
			found = found || p.Severity == Error && strings.Contains(p.Msg, want)
		}
		if !found {
			t.Errorf("%s: want an error containing %q, got %+v", yaml, want, ps)
		}
	}
	for yaml, want := range map[string]string{
		"network: {rules: [{to: 1.2.3.4, proto: udp, decision: ask}]}":               "ask works for TCP only",
		"network: {rules: [{to: 0.0.0.0/0, decision: allow}]}":                       "connect anywhere",
		"network: {rules: [{to: '*', decision: allow}]}":                             "connect anywhere",
		"network: {rules: [{to: 1.2.3.4, proto: icmp, ports: 80, decision: allow}]}": "mean nothing for icmp",
	} {
		_, ps := Parse([]byte(yaml+"\n"), opts)
		found := false
		for _, p := range ps {
			found = found || p.Severity == Warning && strings.Contains(p.Msg, want)
		}
		if !found || HasErrors(ps) {
			t.Errorf("%s: want a warning containing %q, got %+v", yaml, want, ps)
		}
	}
	// `*` on a port list is not "anywhere".
	if _, ps := Parse([]byte("network: {rules: [{to: '*', ports: 443, decision: allow}]}\n"), opts); len(ps) != 0 {
		t.Errorf("%+v", ps)
	}
}
