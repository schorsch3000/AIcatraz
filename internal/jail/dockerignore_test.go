package jail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoreMatching(t *testing.T) {
	tests := []struct {
		rules string
		path  string
		want  bool
	}{
		{".env", ".env", true},
		{".env", "sub/.env", false}, // no ** : only the root file
		{"**/.env", "sub/.env", true},
		{"**/.env", ".env", true},
		{".env*", ".env.local", true},
		{"secrets", "secrets/a/b.txt", true}, // parent directory excluded
		{"secrets/", "secrets/a.txt", true},
		{"/secrets", "secrets", true},
		{"*.pem", "a.pem", true},
		{"*.pem", "dir/a.pem", false},
		{"**/*.pem", "dir/deep/a.pem", true},
		{"secrets\n!secrets/keep", "secrets/keep", false},
		{"secrets\n!secrets/keep\nsecrets/keep", "secrets/keep", true}, // last match wins
		{"# comment\n\n.env", ".env", true},
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file12.txt", false},
		{"[ab].txt", "a.txt", true},
		{"[!ab].txt", "a.txt", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a.b", "aXb", false}, // dot is literal
	}
	for _, tc := range tests {
		ig, err := ParseIgnore(strings.NewReader(tc.rules))
		if err != nil {
			t.Fatalf("%q: %v", tc.rules, err)
		}
		if got := ig.Excluded(tc.path); got != tc.want {
			t.Errorf("rules %q path %q: got %v, want %v", tc.rules, tc.path, got, tc.want)
		}
	}
}

func TestCheckHidden(t *testing.T) {
	ctx := t.TempDir()
	df := filepath.Join(ctx, "Dockerfile.az")
	b := Build{Dockerfile: df, Context: ctx}
	hidden := []string{filepath.Join(ctx, ".env"), filepath.Join(ctx, "secrets")}
	outside := filepath.Join(t.TempDir(), "elsewhere")

	if err := CheckHidden(b, []string{outside}); err != nil {
		t.Errorf("hidden path outside the context needs nothing: %v", err)
	}
	if err := CheckHidden(b, hidden); err == nil || !strings.Contains(err.Error(), "no .dockerignore") {
		t.Errorf("missing .dockerignore must fail, got %v", err)
	}

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(ctx, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".dockerignore", ".env\n")
	if err := CheckHidden(b, hidden); err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Errorf("partial exclusion must fail naming the leak, got %v", err)
	}
	write(".dockerignore", ".env\nsecrets\n")
	if err := CheckHidden(b, hidden); err != nil {
		t.Errorf("full exclusion should pass: %v", err)
	}

	// A Dockerfile-specific ignore file may take precedence in BuildKit, so it must pass too.
	write("Dockerfile.az.dockerignore", "node_modules\n")
	if err := CheckHidden(b, hidden); err == nil || !strings.Contains(err.Error(), "Dockerfile.az.dockerignore") {
		t.Errorf("Dockerfile-specific ignore that leaks must fail, got %v", err)
	}
	write("Dockerfile.az.dockerignore", ".env\nsecrets\n")
	if err := CheckHidden(b, hidden); err != nil {
		t.Errorf("both files excluding should pass: %v", err)
	}
}
