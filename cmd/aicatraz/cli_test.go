// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/aicatraz/internal/shim"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aicatraz.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigCheck(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		code   int
		stdout string
		stderr string
	}{
		{"ok", "agent: {tool: claude}\n", 0, ": ok", ""},
		{"error", "volumes:\n  - nope\n", 1, "", ":2: error: "},
		{"warning only", "env:\n  from_host: [MY_TOKEN]\n", 0, ": ok", ":2: warning: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := cli([]string{"config", "check", write(t, tc.yaml)}, &out, &errb)
			if code != tc.code {
				t.Errorf("exit %d, want %d (stderr %q)", code, tc.code, errb.String())
			}
			if !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errb.String(), tc.stderr) {
				t.Errorf("stdout %q stderr %q", out.String(), errb.String())
			}
		})
	}
}

func TestCLIUsageAndMissingFile(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cli(nil, &out, &errb); code != 2 {
		t.Errorf("no args: exit %d", code)
	}
	errb.Reset()
	if code := cli([]string{"config", "check", "/nonexistent/x.yaml"}, &out, &errb); code != 2 {
		t.Errorf("missing file: exit %d", code)
	}
	if code := cli([]string{"version"}, &out, &errb); code != 0 {
		t.Errorf("version: exit %d", code)
	}
}

func TestDispatch(t *testing.T) {
	// Any name other than "aicatraz" is a proxied command: the shim, which
	// can't find a broker here.
	shim.SocketPath = filepath.Join(t.TempDir(), "none.sock")
	shim.DialTimeout = 100 * time.Millisecond
	if code := run("make", nil); code != shim.ExitUnavailable {
		t.Errorf("shim name should run the shim, got %d", code)
	}
	if code := run("az", []string{"version"}); code != 0 {
		t.Fatalf("az alias: exit %d", code)
	}
	if code := run("aicatraz", []string{"version"}); code != 0 {
		t.Errorf("aicatraz name should run the CLI, got %d", code)
	}

	if code := run("aicatraz-v0.1.1-linux-amd64", []string{"version"}); code != 0 {
		t.Errorf("release file name should run the CLI, got %d", code)
	}

	// With a broker socket present (inside a jail) "az" is a proxied command.
	sock := filepath.Join(t.TempDir(), "broker.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	shim.SocketPath = sock
	if code := run("az", []string{"version"}); code == 0 {
		t.Errorf("az inside a jail should run the shim, got the CLI")
	}
	for _, n := range []string{"aicatraz", "aicatraz-v0.1.1-linux-amd64"} {
		if code := run(n, []string{"version"}); code == 0 {
			t.Errorf("%s inside a jail should refuse, got exit 0", n)
		}
	}
}

func TestUsageUsesInvokedName(t *testing.T) {
	old := ui.Prog
	defer func() { ui.Prog = old }()
	ui.Prog = "az"
	out := styledUsage(ui.Style{})
	if strings.Contains(out, "aicatraz <command>") || !strings.Contains(out, "usage: az <command> [flags]") {
		t.Errorf("usage should name the alias:\n%s", out)
	}
	if got := ui.Cmd("try `aicatraz run`"); got != "try `az run`" {
		t.Errorf("Cmd = %q", got)
	}
}

// Every command in the usage text is one the program knows, and the other way round.
func TestUsageListsEveryCommand(t *testing.T) {
	out := styledUsage(ui.Style{})
	hidden := map[string]bool{"help": true}
	for _, v := range completionVerbs {
		if hidden[v.name] {
			continue
		}
		if !strings.Contains(out, "\n  "+v.name) && !strings.Contains(out, "| "+v.name) {
			t.Errorf("usage does not list %q", v.name)
		}
	}
}

func TestBadArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // in stderr
		hint string
	}{
		{"unknown flag", []string{"up", "--bogus"}, "unknown flag --bogus for `aicatraz up`", "aicatraz help up"},
		{"flag of another command", []string{"down", "--tmux"}, "unknown flag --tmux for `aicatraz down`", "aicatraz help down"},
		{"missing value", []string{"up", "--detach-key"}, "--detach-key needs a value", "aicatraz help up"},
		{"value to a switch", []string{"up", "--tmux=yes"}, "--tmux does not take a value", "aicatraz help up"},
		{"unknown command", []string{"bogus"}, `unknown command "bogus"`, "aicatraz help`"},
		{"typo", []string{"sesions"}, "did you mean sessions?", "aicatraz help`"},
		{"stray argument", []string{"down", "-c", write(t, "agent: {tool: claude}\n"), "extra"}, `unexpected argument "extra" for `, "aicatraz help down"},
		{"config needs check", []string{"config"}, "config needs a sub-command", "aicatraz help config"},
		{"help of nothing", []string{"help", "bogus"}, `unknown command "bogus"`, "aicatraz help`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := cli(tc.args, &out, &errb); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if !strings.Contains(errb.String(), tc.want) || !strings.Contains(errb.String(), tc.hint) {
				t.Errorf("stderr %q, want %q and %q", errb.String(), tc.want, tc.hint)
			}
			if strings.Contains(errb.String(), "Start a jail") {
				t.Errorf("a mistake should not print the whole usage:\n%s", errb.String())
			}
		})
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		var out, errb bytes.Buffer
		if code := cli(args, &out, &errb); code != 0 || !strings.Contains(out.String(), "Start a jail") {
			t.Errorf("%v: exit %d, out %q", args, code, out.String())
		}
	}
	// Per-command help works anywhere and never reads a config.
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"help", "up"}, {"up", "--help"}, {"network", "-h"}, {"run", "-c", "x.yaml", "--help"}} {
		var out, errb bytes.Buffer
		if code := cli(args, &out, &errb); code != 0 || !strings.Contains(out.String(), "usage: aicatraz ") || !strings.Contains(out.String(), "flags:") {
			t.Errorf("%v: exit %d, out %q, err %q", args, code, out.String(), errb.String())
		}
	}
	var out, errb bytes.Buffer
	cli([]string{"up", "--help"}, &out, &errb)
	for _, f := range []string{"--config <file>", "--detach-key <value>", "--status-bar <bar>", "--approval-tty", "--agent <tool>", "--resume"} {
		if !strings.Contains(out.String(), f) {
			t.Errorf("help of up lacks %q:\n%s", f, out.String())
		}
	}
}

func TestNormalizeArgs(t *testing.T) {
	up := findVerb("up")
	got, err := normalizeArgs(up, []string{"--tmux", "--detach-key=ctrl-b", "-c", "x.yaml", "--", "make", "--print", "-c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--config", "x.yaml", "--tmux", "--detach-key", "ctrl-b", "--", "make", "--print", "-c"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestConfigFlagWorksEverywhere(t *testing.T) {
	good := write(t, "agent: {tool: claude}\n")
	for _, args := range [][]string{
		{"config", "check", good},
		{"config", "check", "-c", good},
		{"config", "-c", good, "check"},
		{"config", "check", "--config=" + good},
	} {
		var out, errb bytes.Buffer
		if code := cli(args, &out, &errb); code != 0 {
			t.Errorf("%v: exit %d, %s", args, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := cli([]string{"config", "check", "-c", good, good}, &out, &errb); code != 2 {
		t.Errorf("two files: exit %d", code)
	}
}

func TestMissingConfigSaysWhatToDo(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, verb := range []string{"run", "up", "down", "shell", "sessions", "network", "pins", "claude", "resume"} {
		var out, errb bytes.Buffer
		if code := cli([]string{verb}, &out, &errb); code != 2 {
			t.Errorf("%s: exit %d", verb, code)
		}
		if !strings.Contains(errb.String(), "no aicatraz.yaml in this directory; run `aicatraz init`") {
			t.Errorf("%s: %q", verb, errb.String())
		}
	}
	var out, errb bytes.Buffer
	cli([]string{"run", "-c", "other.yaml"}, &out, &errb)
	if !strings.Contains(errb.String(), "config file other.yaml not found") {
		t.Errorf("named file: %q", errb.String())
	}
}
