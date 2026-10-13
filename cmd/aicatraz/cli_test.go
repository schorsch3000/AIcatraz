package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schorsch3000/aicatraz/internal/shim"
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

	// With a broker socket present (inside a jail) "az" is a proxied command.
	sock := filepath.Join(t.TempDir(), "broker.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	shim.SocketPath = sock
	if code := run("az", []string{"version"}); code == 0 {
		t.Errorf("az inside a jail should run the shim, got the CLI")
	}
}
