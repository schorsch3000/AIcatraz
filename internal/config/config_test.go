package config

import (
	"os"
	"strings"
	"testing"
)

var opts = Options{Home: "/home/test"}

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
			p := ps[0]
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
