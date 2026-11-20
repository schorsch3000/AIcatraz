// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/schorsch3000/aicatraz/internal/config"
)

func TestWritableIn(t *testing.T) {
	vols := []config.Volume{
		{Path: "/p", Mode: config.ModeRW},
		{Path: "/p/.az/aicatraz.yaml", Mode: config.ModeHide},
		{Path: "/p/ro", Mode: config.ModeRO},
	}
	for p, want := range map[string]bool{
		"/p/.az/claude":        true,
		"/p":                   true,
		"/p/ro/x":              false, // the deepest entry wins
		"/p/.az/aicatraz.yaml": false,
		"/q/.az/claude":        false, // not mounted at all
		"/pp/x":                false, // a prefix is not a parent
	} {
		if got := writableIn(vols, p); got != want {
			t.Errorf("writableIn(%s) = %v, want %v", p, got, want)
		}
	}
}

func TestMakeAgentDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".az", "claude")
	if err := makeAgentDir(root, dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("not a private directory: %v %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err != nil || string(b) != "*\n" {
		t.Errorf(".gitignore %q %v", b, err)
	}

	// Loose permissions are tightened, and an edited .gitignore is kept.
	os.Chmod(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("mine\n"), 0o600)
	if err := makeAgentDir(root, dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(b) != "mine\n" {
		t.Errorf(".gitignore overwritten: %q", b)
	}

	// A symlink is refused, and what it points to is left alone.
	target := t.TempDir()
	os.Chmod(target, 0o755)
	link := filepath.Join(root, "link")
	os.Symlink(target, link)
	if err := makeAgentDir(root, link); err == nil {
		t.Error("a symlink was accepted")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Errorf("the link's target was changed: %v", fi.Mode())
	}

	// A symlink further up (.az itself) is refused too, before anything is created.
	proj := t.TempDir()
	os.Symlink(target, filepath.Join(proj, ".az"))
	if err := makeAgentDir(proj, filepath.Join(proj, ".az", "claude")); err == nil {
		t.Error("a symlinked .az was accepted")
	}
	if _, err := os.Lstat(filepath.Join(target, "claude")); err == nil {
		t.Error("created a directory through the link")
	}
}

func TestNoSymlinks(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".az", "claude", "plugins"), 0o700)
	if err := noSymlinks(proj, filepath.Join(proj, ".az", "claude", "plugins")); err != nil {
		t.Error(err)
	}
	if err := noSymlinks(proj, filepath.Join(proj, ".az", "opencode", "plugins")); err != nil {
		t.Errorf("missing parts are fine: %v", err)
	}
	os.RemoveAll(filepath.Join(proj, ".az", "claude", "plugins"))
	os.Symlink("/etc", filepath.Join(proj, ".az", "claude", "plugins"))
	if err := noSymlinks(proj, filepath.Join(proj, ".az", "claude", "plugins")); err == nil {
		t.Error("a symlink was accepted")
	}
	if err := noSymlinks(proj, "/etc"); err == nil {
		t.Error("a path outside the project was accepted")
	}
}
