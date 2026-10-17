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

func TestNetworkModes(t *testing.T) {
	for yaml, want := range map[string]NetMode{
		"volumes: []\n":              NetOff, // no section: no network
		"network: off\n":             NetOff,
		"network: none\n":            NetOff,
		"network: false\n":           NetOff,
		"network: open\n":            NetOpen,
		"network: true\n":            NetOpen,
		"network: {}\n":              NetFiltered,
		"network: {default: deny}\n": NetFiltered,
		"network:\n  rules:\n    - {to: a.example.com, decision: allow}\n": NetFiltered,
	} {
		cfg, ps := Parse([]byte(yaml), opts)
		if len(ps) != 0 || cfg.NetworkMode() != want {
			t.Errorf("%q: mode %q, problems %+v, want %q", yaml, cfg.NetworkMode(), ps, want)
		}
	}
	if cfg, _ := Parse([]byte("network: open\n"), opts); len(cfg.NetRules) != 0 {
		t.Error("open has no rules")
	}
}

func TestNetworkSectionIsStrict(t *testing.T) {
	for yaml, want := range map[string]string{
		"network: maybe\n":           "off, open, or a mapping",
		"network: [a]\n":             "off, open, or a mapping",
		"network: {defualt: deny}\n": "field defualt not found",
		"network: {rules: [{to: a.com, decision: allow, port: 80}]}\n": "field port not found",
	} {
		_, ps := Parse([]byte(yaml), opts)
		found := false
		for _, p := range ps {
			found = found || strings.Contains(p.Msg, want)
		}
		if !found {
			t.Errorf("%q: want %q, got %+v", yaml, want, ps)
		}
	}
}

func TestAgentWithoutNetworkIsWarnedAbout(t *testing.T) {
	_, ps := Parse([]byte("agent: {tool: claude}\n"), opts)
	if len(ps) != 1 || ps[0].Severity != Warning || !strings.Contains(ps[0].Msg, "network is off") {
		t.Errorf("an agent without a network: %+v", ps)
	}
	for _, yaml := range []string{"agent: {tool: claude}\nnetwork: open\n", "agent: {tool: claude}\nnetwork: {default: ask}\n", "network: off\n"} {
		if _, ps := Parse([]byte(yaml), opts); len(ps) != 0 {
			t.Errorf("%q: %+v", yaml, ps)
		}
	}
}

func TestPortMaps(t *testing.T) {
	for spec, want := range map[string]string{
		"3000":                      "127.0.0.1:3000:3000/tcp",
		"8080:80":                   "127.0.0.1:8080:80/tcp",
		"127.0.0.1:8080:80":         "127.0.0.1:8080:80/tcp",
		"0.0.0.0:8080:80":           "0.0.0.0:8080:80/tcp",
		"192.168.1.5:9000:9000/udp": "192.168.1.5:9000:9000/udp",
		"8080:80/TCP":               "127.0.0.1:8080:80/tcp",
		"8000-8010:8000-8010":       "127.0.0.1:8000-8010:8000-8010/tcp",
		"9000-9002:80-82":           "127.0.0.1:9000-9002:80-82/tcp",
		"5000-5000:5000":            "127.0.0.1:5000:5000/tcp",
	} {
		pm, err := ParsePortMap(spec)
		if err != nil || pm.DockerFlag() != want {
			t.Errorf("%q: got %q %v, want %q", spec, pm.DockerFlag(), err, want)
		}
	}
	for _, bad := range []string{"", "0", "65536", "abc", "80:", ":80", "1:2:3:4", "::1:80:80", "[::1]:80:80", "localhost:80:80", "80/sctp", "8000-8010:80", "9000-8000:9000-8000", "300.1.1.1:80:80"} {
		if _, err := ParsePortMap(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestPortsSection(t *testing.T) {
	cfg, ps := Parse([]byte("network: open\nports: [3000, \"8080:80\", \"127.0.0.1:9090:90/udp\"]\n"), opts)
	if len(ps) != 0 || len(cfg.PortMaps) != 3 || cfg.PortMaps[0].HostLo != 3000 || cfg.PortMaps[1].ContainLo != 80 || cfg.PortMaps[2].Proto != "udp" {
		t.Fatalf("%+v %+v", ps, cfg.PortMaps)
	}
	// Rules also give the jail a network that can be published.
	if _, ps := Parse([]byte("network: {}\nports: [3000]\n"), opts); len(ps) != 0 {
		t.Errorf("%+v", ps)
	}
	check := func(yaml, want string, sev Severity) {
		t.Helper()
		_, ps := Parse([]byte(yaml), opts)
		for _, p := range ps {
			if p.Severity == sev && strings.Contains(p.Msg, want) {
				return
			}
		}
		t.Errorf("%q: want %v containing %q, got %+v", yaml, sev, want, ps)
	}
	check("ports: [3000]\n", "the jail has none", Error)               // no network section
	check("network: off\nports: [3000]\n", "the jail has none", Error) // off
	check("network: open\nports: [oops]\n", "not a port", Error)
	check("network: open\nports: [\"8080:80\", \"8080:81\"]\n", "already published", Error)
	check("network: open\nports: [\"0.0.0.0:8080:80\"]\n", "other machines can connect", Warning)
	if _, ps := Parse([]byte("network: open\nports: [\"8080:80\", \"8080:80/udp\", \"127.0.0.1:8081:80\"]\n"), opts); len(ps) != 0 {
		t.Errorf("same number on another protocol or address is no clash: %+v", ps)
	}
}

func TestDevicesSection(t *testing.T) {
	cfg, ps := Parse([]byte(`
devices:
  - serial: "usb-Arduino_*"
  - usb: "2E8A:*"
  - usb: "1a86:7523"
    usb_serial: "ABC*"
  - path: /dev/ttyUSB0
`), opts)
	if len(ps) != 0 || len(cfg.DeviceSpecs) != 4 {
		t.Fatalf("%+v %+v", ps, cfg.DeviceSpecs)
	}
	if cfg.DeviceSpecs[0].Serial != "usb-Arduino_*" || cfg.DeviceSpecs[1].USB != "2e8a:*" || cfg.DeviceSpecs[2].USBSerial != "ABC*" || cfg.DeviceSpecs[3].Path != "/dev/ttyUSB0" {
		t.Errorf("%+v", cfg.DeviceSpecs)
	}
	check := func(yaml, want string, sev Severity) {
		t.Helper()
		_, ps := Parse([]byte(yaml+"\n"), opts)
		for _, p := range ps {
			if p.Severity == sev && strings.Contains(p.Msg, want) {
				return
			}
		}
		t.Errorf("%s: want %v containing %q, got %+v", yaml, sev, want, ps)
	}
	check("devices: [{}]", "exactly one of", Error)
	check("devices: [{serial: a, usb: '1:2'}]", "exactly one of", Error)
	check("devices: [{usb_serial: x}]", "exactly one of", Error)
	check("devices: [{path: /dev/x, usb_serial: x}]", "usb_serial goes with usb", Error)
	check("devices: [{serial: a/b}]", "must be a name", Error)
	check("devices: [{serial: '[a'}]", "must be a name", Error)
	check("devices: [{usb: '2341'}]", "vendor:product", Error)
	check("devices: [{usb: 'zzzz:0001'}]", "vendor:product", Error)
	check("devices: [{usb: '12345:0001'}]", "vendor:product", Error)
	check("devices: [{path: /etc/passwd}]", "under /dev", Error)
	check("devices: [{path: /dev/../etc/x}]", "under /dev", Error)
	check("devices: [{path: /dev/mem}]", "too dangerous", Error)
	check("devices: [{path: /dev/null}]", "belongs to the container", Error)
	check("devices: [{path: /dev/pts/1}]", "belongs to the container", Error)
	check("devices: [{path: /dev/*}]", "every device", Error)
	check("devices: [{path: '/dev/ttyUSB*'}]", "more than one device", Warning)
}

func TestBarSection(t *testing.T) {
	cfg, ps := Parse([]byte("bar: {theme: brick, fg: white, bg: '#102030'}\n"), opts)
	if len(ps) != 0 || cfg.Bar.Theme != "brick" || cfg.Bar.FG != "white" || cfg.Bar.BG != "#102030" {
		t.Fatalf("%+v %+v", ps, cfg.Bar)
	}
	if _, ps := Parse([]byte("volumes: []\n"), opts); len(ps) != 0 {
		t.Errorf("no bar section is fine: %+v", ps)
	}
	check := func(yaml, want string) {
		t.Helper()
		_, ps := Parse([]byte(yaml+"\n"), opts)
		for _, p := range ps {
			if p.Severity == Error && strings.Contains(p.Msg, want) {
				return
			}
		}
		t.Errorf("%s: want an error containing %q, got %+v", yaml, want, ps)
	}
	check("bar: {theme: nope}", "bar theme")
	check("bar: {fg: purple}", "color")
	check("bar: {bg: 300}", "0 to 255")
	check("bar: {bg: '#12'}", "color")
	check("bar: {colour: red}", "not found")
}
