// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schorsch3000/aicatraz/internal/config"
)

func TestOfferMigrateMovesOldLayout(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("aicatraz.yaml", "volumes:\n  - .:rw\n  - ./aicatraz.yaml:hi\nimage:\n  dockerfile: Dockerfile.az # mine\n")
	write("Dockerfile.az", "FROM debian\n")
	write("Dockerfile.az.dockerignore", ".env\n")

	var out bytes.Buffer
	if moved, err := OfferMigrate(dir, strings.NewReader("n\n"), &out); moved || err != nil {
		t.Fatalf("no: %v %v", moved, err)
	}
	if !OldLayout(dir) {
		t.Fatal("a no must change nothing")
	}
	if moved, err := OfferMigrate(dir, strings.NewReader("y\n"), &out); !moved || err != nil {
		t.Fatalf("yes: %v %v", moved, err)
	}
	for _, f := range []string{"aicatraz.yaml", "Dockerfile.az", "Dockerfile.az.dockerignore"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s left behind", f)
		}
		if _, err := os.Stat(filepath.Join(dir, ".az", f)); err != nil {
			t.Errorf("%s not moved: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".az", "aicatraz.yaml"))
	want := "volumes:\n  - .:rw\n  - ./.az/aicatraz.yaml:hi\nimage:\n  dockerfile: .az/Dockerfile.az # mine\n"
	if string(b) != want {
		t.Errorf("config = %q, want %q", b, want)
	}
	if OldLayout(dir) {
		t.Error("still the old layout")
	}
}

func TestProjectIDSurvivesMigration(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	old := filepath.Join(dir, "aicatraz.yaml")
	os.WriteFile(old, []byte("{}\n"), 0o644)
	before, err := ProjectID(old, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLayout(dir); err != nil {
		t.Fatal(err)
	}
	after, err := ProjectID(filepath.Join(dir, ".az", "aicatraz.yaml"), &config.Config{})
	if err != nil || after != before {
		t.Errorf("id %q -> %q (%v)", before, after, err)
	}
}
