package jail

import (
	"bytes"
	"io"
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

func TestEnsureHiddenIgnored(t *testing.T) {
	ctx := t.TempDir()
	b := Build{Dockerfile: filepath.Join(ctx, "Dockerfile.az"), Context: ctx}
	hidden := []string{filepath.Join(ctx, ".env"), filepath.Join(ctx, "aicatraz.yaml")}
	ignore := filepath.Join(ctx, ".dockerignore")

	// Without a way to ask, nothing is touched and the build is refused.
	if err := EnsureHiddenIgnored(b, hidden, nil); err == nil {
		t.Fatal("must refuse without asking")
	}
	if _, err := os.Stat(ignore); err == nil {
		t.Fatal(".dockerignore created without asking")
	}

	// Every path is asked about; a no leaves it out, and the refusal stays.
	var asked []IgnoreGap
	answers := map[string]bool{".env": true, "aicatraz.yaml": false}
	ask := func(g IgnoreGap) bool { asked = append(asked, g); return answers[g.Rel] }
	err := EnsureHiddenIgnored(b, hidden, ask)
	if err == nil || !strings.Contains(err.Error(), "aicatraz.yaml") {
		t.Fatalf("a declined path must stay an error, got %v", err)
	}
	if len(asked) != 2 || asked[0].Exists || !asked[1].Exists {
		t.Errorf("asked = %+v (the file is created by the first yes, so the second sees it)", asked)
	}
	if got, _ := os.ReadFile(ignore); string(got) != ".env\n" {
		t.Errorf(".dockerignore = %q", got)
	}

	// Next time it asks again, and a yes completes the job without touching existing lines.
	os.WriteFile(ignore, []byte("node_modules"), 0o644) // no trailing newline
	asked = nil
	answers[".env"], answers["aicatraz.yaml"] = true, true
	if err := EnsureHiddenIgnored(b, hidden, ask); err != nil {
		t.Fatalf("all yes should pass: %v", err)
	}
	if got, _ := os.ReadFile(ignore); string(got) != "node_modules\n.env\naicatraz.yaml\n" {
		t.Errorf(".dockerignore = %q", got)
	}
	asked = nil
	if err := EnsureHiddenIgnored(b, hidden, ask); err != nil || len(asked) != 0 {
		t.Errorf("nothing left to ask: err=%v asked=%v", err, asked)
	}
}

func TestAskIgnoreOnlyAYesCounts(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "\n": false, "n\n": false, "yep\n": false, "": false} {
		var out bytes.Buffer
		got := askIgnore(strings.NewReader(in), &out)(IgnoreGap{File: "/p/.dockerignore", Rel: ".env"})
		if got != want {
			t.Errorf("input %q: got %v", in, got)
		}
		if !strings.Contains(out.String(), "Create") {
			t.Errorf("a missing file should say it is created: %s", out.String())
		}
	}
	// Input after the answer's newline stays unread.
	r := strings.NewReader("y\nrest")
	askIgnore(r, io.Discard)(IgnoreGap{File: "/p/.dockerignore", Rel: ".env", Exists: true})
	if r.Len() != len("rest") {
		t.Errorf("read past the answer: %d bytes left", r.Len())
	}
}
