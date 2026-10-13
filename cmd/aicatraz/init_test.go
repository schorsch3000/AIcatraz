package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inDir(t *testing.T) (dir, home string) {
	t.Helper()
	home = t.TempDir()
	dir = filepath.Join(home, "git", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(dir)
	return dir, home
}

func TestInitWritesAValidConfig(t *testing.T) {
	dir, _ := inDir(t)
	var out, errb bytes.Buffer
	if code := cli([]string{"init"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "aicatraz.yaml"))
	if err != nil || !strings.Contains(string(b), "~/git/proj:rw") {
		t.Fatalf("%v\n%s", err, b)
	}
	if errb.Len() != 0 {
		t.Errorf("a fresh starter config must have no findings: %s", errb.String())
	}
	if !strings.Contains(out.String(), "aicatraz config check") {
		t.Errorf("next steps missing: %s", out.String())
	}
	// And the file really passes the checker.
	out.Reset()
	if code := cli([]string{"config", "check"}, &out, &errb); code != 0 || errb.Len() != 0 {
		t.Errorf("config check: exit %d\n%s", code, errb.String())
	}
}

func TestInitNeverOverwritesUnlessAsked(t *testing.T) {
	dir, _ := inDir(t)
	cfg := filepath.Join(dir, "aicatraz.yaml")
	os.WriteFile(cfg, []byte("# mine\n"), 0o644)
	var out, errb bytes.Buffer
	if code := cli([]string{"init", "--example", "claude"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "already exist") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(cfg); string(b) != "# mine\n" {
		t.Error("the existing file was changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile.az")); err == nil {
		t.Error("nothing at all may be written when something would be overwritten")
	}
	errb.Reset()
	if code := cli([]string{"init", "--example", "claude", "--force"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), "tool: claude") {
		t.Error("--force must overwrite")
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile.az")); err != nil {
		t.Error("the Dockerfile of the example must be written too")
	}
}

func TestInitListAndErrors(t *testing.T) {
	inDir(t)
	var out, errb bytes.Buffer
	if code := cli([]string{"init", "--list"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "deploy-with-make") {
		t.Errorf("list: %d %s", code, out.String())
	}
	if code := cli([]string{"init", "--example", "nope"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "available") {
		t.Errorf("unknown example: %d %s", code, errb.String())
	}
	if code := cli([]string{"init", "--bogus"}, &out, &errb); code != 2 {
		t.Errorf("bad flag: %d", code)
	}
}

func TestInitRefusesTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	var out, errb bytes.Buffer
	if code := cli([]string{"init"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "home directory") {
		t.Errorf("%d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, "aicatraz.yaml")); err == nil {
		t.Error("nothing may be written")
	}
}
