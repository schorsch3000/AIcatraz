// SPDX-License-Identifier: GPL-3.0-only

package policy

import (
	"testing"

	"github.com/schorsch3000/aicatraz/internal/config"
)

func mustPolicy(t *testing.T, yaml string) *Policy {
	t.Helper()
	cfg, ps := config.Parse([]byte(yaml), config.Options{Home: "/home/u"})
	if config.HasErrors(ps) {
		t.Fatalf("config: %+v", ps)
	}
	p, err := New(cfg.Rules)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const rules = `
rules:
  - command: make
    args: [build, test]
    decision: local
  - command: make
    args: [deploy-test]
    decision: allow
  - command: make
    args: ["*"]
    deny_flags: [-f, --file, -C, --directory]
    decision: ask
  - command: git
    args: [push, "*"]
    decision: ask
  - command: git
    args: [status]
    decision: allow
  - command: py*
    args: ["scripts/*.py"]
    decision: allow
  - command: nopolicy
    decision: allow
`

func TestDecide(t *testing.T) {
	p := mustPolicy(t, rules)
	tests := []struct {
		name string
		args []string
		want Action
		rule int
	}{
		{"make", []string{"build", "test"}, Local, 0},
		{"make", []string{"build"}, Ask, 2}, // not [build, test]; falls to the catch-all make rule
		{"make", []string{"deploy-test"}, Allow, 1},
		{"make", []string{"deploy-test", "extra"}, Ask, 2}, // exact token list: an extra token no longer matches rule 1
		{"make", []string{"-f", "evil.mk", "deploy-test"}, Deny, NoRule},
		{"make", []string{"deploy-test", "-f", "evil.mk"}, Deny, NoRule},
		{"make", []string{"--file=evil.mk"}, Deny, NoRule},
		{"make", []string{"--fi", "evil.mk"}, Deny, NoRule}, // GNU abbreviation of --file
		{"make", []string{"-nf", "evil.mk"}, Deny, NoRule},  // bundled short flags
		{"make", []string{"-fevil.mk"}, Deny, NoRule},       // value glued to the flag
		{"make", []string{"-C", "/tmp"}, Deny, NoRule},
		{"make", []string{"--directory=/"}, Deny, NoRule},
		{"make", []string{"-j4", "all"}, Ask, 2},
		{"make", nil, Ask, 2}, // a final * also matches no arguments
		{"git", []string{"push"}, Ask, 3},
		{"git", []string{"push", "origin", "main"}, Ask, 3},
		{"git", []string{"status"}, Allow, 4},
		{"git", []string{"status", "--short"}, Deny, NoRule},
		{"git", []string{"commit"}, Deny, NoRule},
		{"python3", []string{"scripts/run.py"}, Allow, 5},
		{"python3", []string{"scripts/a/b/c.py"}, Allow, 5}, // * crosses slashes inside a token
		{"python3", []string{"other.py"}, Deny, NoRule},
		{"nopolicy", []string{"anything", "--at-all"}, Allow, 6}, // no args key: any arguments
		{"nopolicy", nil, Allow, 6},
		{"rm", []string{"-rf", "/"}, Deny, NoRule},
		{"", nil, Deny, NoRule},
	}
	for _, tc := range tests {
		d := p.Decide(tc.name, tc.args)
		if d.Action != tc.want || d.Rule != tc.rule {
			t.Errorf("%s %q: got %s rule %d, want %s rule %d", tc.name, tc.args, d.Action, d.Rule, tc.want, tc.rule)
		}
	}
}

func TestFirstMatchWins(t *testing.T) {
	p := mustPolicy(t, `
rules:
  - command: make
    args: [deploy]
    decision: deny
  - command: make
    decision: allow
`)
	if d := p.Decide("make", []string{"deploy"}); d.Action != Deny {
		t.Errorf("carve-out must win: %+v", d)
	}
	if d := p.Decide("make", []string{"other"}); d.Action != Allow {
		t.Errorf("broader rule: %+v", d)
	}
}

func TestEmptyArgsListMeansNoArguments(t *testing.T) {
	p := mustPolicy(t, "rules:\n  - command: ls\n    args: []\n    decision: allow\n")
	if d := p.Decide("ls", nil); d.Action != Allow {
		t.Errorf("no arguments: %+v", d)
	}
	if d := p.Decide("ls", []string{"-la"}); d.Action != Deny {
		t.Errorf("explicit empty args must not match arguments: %+v", d)
	}
}

func TestCatchAllDeny(t *testing.T) {
	p := mustPolicy(t, "rules:\n  - command: ls\n    decision: allow\n  - command: \"*\"\n    decision: deny\n")
	d := p.Decide("rm", nil)
	if d.Action != Deny || d.Rule != 1 {
		t.Errorf("explicit catch-all: %+v", d)
	}
}

func TestEmptyPolicyDeniesEverything(t *testing.T) {
	p, _ := New(nil)
	if d := p.Decide("ls", nil); d.Action != Deny || d.Rule != NoRule || d.Config != nil {
		t.Errorf("%+v", d)
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pat, s string
		want   bool
	}{
		{"a*c", "abc", true},
		{"a*c", "a/b/c", true},
		{"a*c", "ab", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"[abc]x", "bx", true},
		{"[!abc]x", "bx", false},
		{"a.b", "aXb", false},
		{`a\*b`, "a*b", true},
		{`a\*b`, "aXb", false},
		{"*", "", true},
		{"(x)", "(x)", true},
	} {
		re, err := glob(tc.pat)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(tc.s); got != tc.want {
			t.Errorf("%q ~ %q: got %v", tc.pat, tc.s, got)
		}
	}
	if _, err := glob("[x"); err == nil {
		t.Error("unterminated class must fail")
	}
}
