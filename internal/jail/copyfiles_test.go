// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyIfMissing(t *testing.T) {
	tmp := t.TempDir()
	root, _ := filepath.EvalSymlinks(filepath.Join(tmp, "proj"))
	if err := os.MkdirAll(filepath.Join(tmp, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(filepath.Join(tmp, "proj"))
	src := filepath.Join(tmp, "src.txt")
	os.WriteFile(src, []byte("new"), 0o600)

	made, err := copyIfMissing(src, root, "a/b.txt")
	if err != nil || !made {
		t.Fatalf("first copy: %v %v", made, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a/b.txt")); string(b) != "new" {
		t.Fatalf("content %q", b)
	}

	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("mine"), 0o644)
	if made, err := copyIfMissing(src, root, "keep.txt"); made || err != nil {
		t.Fatalf("existing: %v %v", made, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "keep.txt")); string(b) != "mine" {
		t.Fatalf("overwritten: %q", b)
	}

	outside := filepath.Join(tmp, "outside")
	os.Mkdir(outside, 0o755)
	os.Symlink(outside, filepath.Join(root, "esc"))
	if _, err := copyIfMissing(src, root, "esc/x.txt"); err == nil {
		t.Fatal("copied through a symlink out of the project")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("file escaped")
	}
}
