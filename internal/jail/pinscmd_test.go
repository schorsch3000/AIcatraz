package jail

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schorsch3000/aicatraz/internal/pins"
)

// pinProject sets up a project with a config and returns its directory and
// a function that runs `aicatraz pins` for it.
func pinProject(t *testing.T) (dir string, run func(args ...string) (string, error)) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	cfg := filepath.Join(dir, "aicatraz.yaml")
	if err := os.WriteFile(cfg, []byte("project: {id: pt}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run = func(args ...string) (string, error) {
		var out, errb bytes.Buffer
		err := Pins(append([]string{"-c", cfg}, args...), &out, &errb)
		return out.String(), err
	}
	return dir, run
}

func pinFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	state, _ := StateDir("pt")
	store, err := pins.Open(filepath.Join(state, "pins"))
	if err != nil {
		t.Fatal(err)
	}
	sn, err := pins.Take(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(sn); err != nil {
		t.Fatal(err)
	}
}

func TestPinsListShowsStateOfEveryPin(t *testing.T) {
	dir, run := pinProject(t)
	if out, err := run("list"); err != nil || !strings.Contains(out, "no pins for pt") {
		t.Fatalf("no pins yet: %q %v", out, err)
	}
	mk, ok, gone := filepath.Join(dir, "Makefile"), filepath.Join(dir, "ok.sh"), filepath.Join(dir, "gone.sh")
	pinFile(t, mk, "all:\n")
	pinFile(t, ok, "echo hi\n")
	pinFile(t, gone, "x\n")

	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{mk, ok, gone} {
		if !strings.Contains(out, p) {
			t.Errorf("list lacks %s:\n%s", p, out)
		}
	}
	if strings.Contains(out, "changed") || strings.Contains(out, "missing") || strings.Count(out, "ok ") < 3 {
		t.Errorf("everything is as approved:\n%s", out)
	}

	os.WriteFile(mk, []byte("all:\n\trm -rf /\n"), 0o644)
	os.Remove(gone)
	out, _ = run("list")
	line := func(path string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasSuffix(l, path) {
				return l
			}
		}
		return ""
	}
	if !strings.HasPrefix(line(mk), "changed") || !strings.HasPrefix(line(gone), "missing") || !strings.HasPrefix(line(ok), "ok") {
		t.Errorf("states wrong:\n%s", out)
	}
	if !strings.Contains(out, "ask for approval") {
		t.Errorf("a hint is missing:\n%s", out)
	}
}

func TestPinsForget(t *testing.T) {
	dir, run := pinProject(t)
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	pinFile(t, a, "1")
	pinFile(t, b, "2")
	pinFile(t, c, "3")
	state, _ := StateDir("pt")
	store, _ := pins.Open(filepath.Join(state, "pins"))
	count := func() int { p, _ := store.Paths(); return len(p) }

	// A relative path is resolved against the working directory.
	t.Chdir(dir)
	if out, err := run("forget", "a"); err != nil || !strings.Contains(out, "forgot") || count() != 2 {
		t.Fatalf("forget a: %q %v (%d left)", out, err, count())
	}
	if _, err := run("forget", "never-pinned"); err == nil || !strings.Contains(err.Error(), "not pinned") || count() != 2 {
		t.Errorf("an unknown path is an error and changes nothing: %v", err)
	}
	if _, err := run("forget", b, "nope"); err == nil || count() != 2 {
		t.Errorf("one unknown path stops the whole command before anything is forgotten: %v (%d)", err, count())
	}

	os.Remove(c)
	if out, err := run("forget", "--missing"); err != nil || !strings.Contains(out, c) || count() != 1 {
		t.Fatalf("--missing: %q %v (%d left)", out, err, count())
	}
	if out, _ := run("forget", "--missing"); !strings.Contains(out, "nothing to forget") {
		t.Errorf("%q", out)
	}
	if _, err := run("forget", "--all"); err != nil || count() != 0 {
		t.Errorf("--all left %d", count())
	}
	if p, err := pins.Take(b); err != nil || !p.Exists {
		t.Error("forgetting a pin must never touch the file")
	}
}

func TestPinsUsageErrors(t *testing.T) {
	_, run := pinProject(t)
	pinFile(t, filepath.Join(t.TempDir(), "x"), "1")
	for _, args := range [][]string{{"forget"}, {"forget", "--all", "x"}, {"forget", "--all", "--missing"}, {"forget", "--bogus"}, {"list", "extra"}, {"nonsense"}} {
		if _, err := run(args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
