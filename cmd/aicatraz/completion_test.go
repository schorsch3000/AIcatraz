package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "sh", "zsh", "fish"} {
		var out, errb bytes.Buffer
		if code := cli([]string{"completion", shell}, &out, &errb); code != 0 {
			t.Fatalf("%s: exit %d: %s", shell, code, errb.String())
		}
		s := out.String()
		for _, want := range []string{"aicatraz", "az", "resume", "dockerfile", "deploy-with-make", "opencode", "approve-build"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s script lacks %q", shell, want)
			}
		}
		if shell == "bash" {
			f := filepath.Join(t.TempDir(), "c.bash")
			os.WriteFile(f, out.Bytes(), 0o644)
			if b, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
				t.Errorf("bash -n: %v\n%s", err, b)
			}
		}
	}
}

func TestCompletionEveryVerbIsKnown(t *testing.T) {
	// A verb added to the CLI must be added to the completion table too.
	for _, v := range []string{"up", "run", "shell", "claude", "opencode", "agent", "sessions", "resume", "tools", "init", "config", "version"} {
		found := false
		for _, cv := range completionVerbs {
			found = found || cv.name == v
		}
		if !found {
			t.Errorf("verb %q missing from completionVerbs", v)
		}
	}
}

func TestCompletionBadArguments(t *testing.T) {
	for _, args := range [][]string{{"completion"}, {"completion", "tcsh"}, {"completion", "bash", "x"}} {
		var out, errb bytes.Buffer
		if code := cli(args, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
}
