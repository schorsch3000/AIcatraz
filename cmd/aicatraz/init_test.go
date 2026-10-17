// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bytes"
	"github.com/schorsch3000/aicatraz/internal/osrelease"
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
	if err == nil && !strings.Contains(string(b), "#   id: proj\n") {
		t.Errorf("the example project id is the directory's name:\n%s", b)
	}
	if err != nil || !strings.Contains(string(b), "- .:rw") {
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

func TestBaseImage(t *testing.T) {
	cases := []struct{ rel, want string }{
		{"ID=debian\nVERSION_ID=\"13\"", "debian:13"},
		{"ID=debian", "debian:testing"},
		{"ID=ubuntu\nVERSION_ID=\"24.04\"", "ubuntu:24.04"},
		{"ID=fedora\nVERSION_ID=41", "fedora:41"},
		{"ID=alpine\nVERSION_ID=3.20.3", "alpine:3.20"},
		{"ID=\"rocky\"\nVERSION_ID=\"9.4\"", "rockylinux:9"},
		{"ID=\"almalinux\"\nVERSION_ID=\"9.4\"", "almalinux:9"},
		{"ID=\"centos\"\nVERSION_ID=\"9\"", "quay.io/centos/centos:stream9"},
		{"ID=\"rhel\"\nVERSION_ID=\"9.4\"", "registry.access.redhat.com/ubi9/ubi"},
		{"ID=arch", "archlinux:latest"},
		{"ID=opensuse-leap\nVERSION_ID=\"15.6\"", "opensuse/leap:15.6"},
		{"ID=opensuse-tumbleweed\nVERSION_ID=\"20250101\"", "opensuse/tumbleweed:latest"},
		{"ID=amzn\nVERSION_ID=\"2023\"", "amazonlinux:2023"},
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\nUBUNTU_CODENAME=noble", "ubuntu:noble"},
		{"ID=raspbian\nID_LIKE=debian", "debian:stable"},
	}
	for _, c := range cases {
		got, _, ok := baseImage(osrelease.Parse(strings.NewReader(c.rel)))
		if !ok || got != c.want {
			t.Errorf("%q: got %q (ok=%v), want %q", c.rel, got, ok, c.want)
		}
	}
	if _, _, ok := baseImage(osrelease.Parse(strings.NewReader("ID=plan9"))); ok {
		t.Error("unknown distribution should not match")
	}
}

func TestInitDockerfile(t *testing.T) {
	dir, _ := inDir(t)
	rel := filepath.Join(t.TempDir(), "os-release")
	os.WriteFile(rel, []byte("NAME=\"Debian GNU/Linux\"\nID=debian\nVERSION_ID=\"13\"\n"), 0o644)
	old := osReleasePath
	osReleasePath = rel
	defer func() { osReleasePath = old }()

	var out, errb bytes.Buffer
	if code := cli([]string{"init", "dockerfile"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Dockerfile.az"))
	if string(b) != "FROM debian:13\n" {
		t.Errorf("Dockerfile.az = %q", b)
	}

	// No silent overwrite.
	os.WriteFile("Dockerfile.az", []byte("mine\n"), 0o644)
	errb.Reset()
	if code := cli([]string{"init", "dockerfile"}, &out, &errb); code != 1 {
		t.Errorf("existing file: exit %d", code)
	}
	if b, _ := os.ReadFile("Dockerfile.az"); string(b) != "mine\n" {
		t.Errorf("file was overwritten: %q", b)
	}
	if code := cli([]string{"init", "dockerfile", "--force"}, &out, &errb); code != 0 {
		t.Errorf("--force: exit %d", code)
	}
}
