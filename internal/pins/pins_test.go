// SPDX-License-Identifier: GPL-3.0-only

package pins

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pins"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustCheck(t *testing.T, s *Store, files []string, ms []Matcher) Report {
	t.Helper()
	r, err := Check(s, files, ms)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStoreRoundTrip(t *testing.T) {
	s := newStore(t)
	f := filepath.Join(t.TempDir(), "Makefile")
	write(t, f, "all:\n\techo hi\n")
	sn, err := Take(f)
	if err != nil || !sn.Exists || sn.Content == nil {
		t.Fatalf("%+v %v", sn, err)
	}
	if got, _ := s.Get(f); got != nil {
		t.Fatal("nothing pinned yet")
	}
	if err := s.Put(sn); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(f)
	if err != nil || got == nil || got.Sum != sn.Sum || string(got.Content) != "all:\n\techo hi\n" {
		t.Fatalf("%+v %v", got, err)
	}
	if paths, _ := s.Paths(); !slices.Equal(paths, []string{f}) {
		t.Errorf("paths %v", paths)
	}
	st, _ := os.Stat(s.dir)
	if st.Mode().Perm() != 0o700 {
		t.Errorf("store dir mode %v", st.Mode().Perm())
	}
	if err := s.Forget(f); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f); got != nil {
		t.Error("forgotten pin still there")
	}
}

func TestLargeFilesAreHashedButNotStored(t *testing.T) {
	f := filepath.Join(t.TempDir(), "big")
	write(t, f, strings.Repeat("x", MaxStoredSize+10))
	a, _ := Take(f)
	if a.Content != nil || a.Sum == "" || a.Size != MaxStoredSize+10 {
		t.Fatalf("size %d content-nil=%v", a.Size, a.Content == nil)
	}
	write(t, f, strings.Repeat("x", MaxStoredSize+9)+"y")
	b, _ := Take(f)
	if b.Sum == a.Sum {
		t.Error("hash must reflect the content")
	}
	s := newStore(t)
	s.Put(a)
	rep := mustCheck(t, s, []string{f}, nil)
	it := rep.Items[0]
	if it.Status != Changed || it.Diff != "" || !strings.Contains(it.Note, "too large") {
		t.Errorf("%+v", it)
	}
}

func TestCheckStates(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t)
	mk := filepath.Join(dir, "Makefile")
	write(t, mk, "all:\n\techo one\n")

	rep := mustCheck(t, s, []string{mk}, nil)
	if !rep.Dirty() || rep.Items[0].Status != New || !strings.Contains(rep.Items[0].Diff, "+\techo one") {
		t.Fatalf("new file: %+v", rep.Items[0])
	}

	sn, _ := Take(mk)
	s.Put(sn)
	if rep := mustCheck(t, s, []string{mk}, nil); rep.Dirty() {
		t.Fatalf("pinned file must be clean: %+v", rep)
	}

	write(t, mk, "all:\n\techo two\n")
	rep = mustCheck(t, s, []string{mk}, nil)
	it := rep.Items[0]
	if it.Status != Changed || !strings.Contains(it.Diff, "-\techo one") || !strings.Contains(it.Diff, "+\techo two") ||
		!strings.Contains(it.Diff, "approved: Makefile") {
		t.Fatalf("changed: %+v", it)
	}
	if !strings.Contains(rep.Summary(), "Makefile: changed") {
		t.Errorf("summary %q", rep.Summary())
	}

	// Pinning the new version makes it clean again; the old one is dirty.
	now, _ := Take(mk)
	s.Put(now)
	if mustCheck(t, s, []string{mk}, nil).Dirty() {
		t.Error("re-pinned file must be clean")
	}
	write(t, mk, "all:\n\techo one\n")
	if !mustCheck(t, s, []string{mk}, nil).Dirty() {
		t.Error("reverting is a change too")
	}
}

func TestDeletedFileIsNoticed(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t)
	a, b := filepath.Join(dir, "a.mk"), filepath.Join(dir, "b.mk")
	write(t, a, "A=1\n")
	write(t, b, "B=1\n")
	for _, f := range []string{a, b} {
		sn, _ := Take(f)
		s.Put(sn)
	}
	res, err := Resolve(Spec{Patterns: []string{"*.mk"}}, "make", nil, dir)
	if err != nil || len(res.Files) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	os.Remove(b)
	res, _ = Resolve(Spec{Patterns: []string{"*.mk"}}, "make", nil, dir)
	rep := mustCheck(t, s, res.Files, res.Matchers)
	var gone *Item
	for i := range rep.Items {
		if rep.Items[i].Status == Gone {
			gone = &rep.Items[i]
		}
	}
	if gone == nil || gone.Path != b || !strings.Contains(gone.Diff, "-B=1") {
		t.Fatalf("deleted file not reported: %+v", rep.Items)
	}
}

func TestNewFileMatchingAGlobIsNoticed(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t)
	write(t, filepath.Join(dir, "mk", "a.mk"), "A=1\n")
	res, _ := Resolve(Spec{Patterns: []string{"mk/*.mk"}}, "make", nil, dir)
	for _, f := range res.Files {
		sn, _ := Take(f)
		s.Put(sn)
	}
	write(t, filepath.Join(dir, "mk", "evil.mk"), "all:\n\trm -rf ~\n")
	res, _ = Resolve(Spec{Patterns: []string{"mk/*.mk"}}, "make", nil, dir)
	rep := mustCheck(t, s, res.Files, res.Matchers)
	att := rep.Attention()
	if len(att) != 1 || att[0].Status != New || filepath.Base(att[0].Path) != "evil.mk" {
		t.Fatalf("%+v", rep.Items)
	}
}

func TestBinaryAndSymlinkChanges(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t)
	bin := filepath.Join(dir, "tool")
	os.WriteFile(bin, []byte{0, 1, 2, 3}, 0o755)
	sn, _ := Take(bin)
	s.Put(sn)
	os.WriteFile(bin, []byte{0, 1, 2, 4}, 0o755)
	it := mustCheck(t, s, []string{bin}, nil).Items[0]
	if it.Status != Changed || it.Diff != "" || it.Note != "binary file" {
		t.Errorf("%+v", it)
	}

	// A symlink pointing somewhere else with identical content is still a change.
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	write(t, a, "same")
	write(t, b, "same")
	link := filepath.Join(dir, "link")
	os.Symlink(a, link)
	l1, _ := Take(link)
	s.Put(l1)
	os.Remove(link)
	os.Symlink(b, link)
	it = mustCheck(t, s, []string{link}, nil).Items[0]
	if it.Status != Changed || !strings.Contains(it.Note, "now a link to") {
		t.Errorf("%+v", it)
	}
}

func TestFingerprintTracksContent(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t)
	f := filepath.Join(dir, "x")
	write(t, f, "1")
	a := mustCheck(t, s, []string{f}, nil).Fingerprint()
	if a != mustCheck(t, s, []string{f}, nil).Fingerprint() {
		t.Error("fingerprint must be stable")
	}
	write(t, f, "2")
	if a == mustCheck(t, s, []string{f}, nil).Fingerprint() {
		t.Error("fingerprint must change with content")
	}
}

func TestResolvePatterns(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"Makefile", "mk/a.mk", "mk/deep/b.mk", "scripts/run.py", "other.txt", ".git/config", "node_modules/x/y.mk"} {
		write(t, filepath.Join(dir, f), f)
	}
	res := func(pats ...string) []string {
		t.Helper()
		r, err := Resolve(Spec{Patterns: pats}, "x", []string{"scripts/run.py", "weird*name"}, dir)
		if err != nil {
			t.Fatal(err)
		}
		var rel []string
		for _, f := range r.Files {
			x, _ := filepath.Rel(dir, f)
			rel = append(rel, x)
		}
		return rel
	}
	if got := res("Makefile"); !slices.Equal(got, []string{"Makefile"}) {
		t.Errorf("literal: %v", got)
	}
	if got := res("mk/*.mk"); !slices.Equal(got, []string{"mk/a.mk"}) {
		t.Errorf("single star stays in one segment: %v", got)
	}
	if got := res("mk/**/*.mk"); !slices.Equal(got, []string{"mk/a.mk", "mk/deep/b.mk"}) {
		t.Errorf("double star: %v", got)
	}
	if got := res("**/*.mk"); slices.Contains(got, "node_modules/x/y.mk") {
		t.Errorf("node_modules must be skipped unless named: %v", got)
	}
	if got := res("{args[0]}"); !slices.Equal(got, []string{"scripts/run.py"}) {
		t.Errorf("placeholder: %v", got)
	}
	if got := res("{args[5]}"); len(got) != 0 {
		t.Errorf("missing argument drops the pattern: %v", got)
	}
	if got := res("nope", "missing/*.x"); len(got) != 0 {
		t.Errorf("non-matching: %v", got)
	}
	if got := res(".git/*"); !slices.Equal(got, []string{".git/config"}) {
		t.Errorf("a named .git is searched: %v", got)
	}
	// A hostile argument must not become a wildcard.
	if got := res("{args[1]}"); len(got) != 0 {
		t.Errorf("an argument with * must match literally: %v", got)
	}
}

func TestResolveTooMany(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < MaxFiles+5; i++ {
		write(t, filepath.Join(dir, "f", strings.Repeat("a", 1)+string(rune('a'+i%26))+string(rune('a'+i/26))), "x")
	}
	if _, err := Resolve(Spec{Patterns: []string{"f/*"}}, "x", nil, dir); err != ErrTooMany {
		t.Errorf("got %v", err)
	}
}

func TestMakePresetFollowsIncludes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Makefile"), "include config.mk\n-include local/*.mk\ninclude $(VAR)/x.mk\nall:\n\ttrue\n")
	write(t, filepath.Join(dir, "config.mk"), "include deep.mk\n")
	write(t, filepath.Join(dir, "deep.mk"), "X=1\n")
	write(t, filepath.Join(dir, "local", "l.mk"), "Y=1\n")
	write(t, filepath.Join(dir, "unrelated.txt"), "z")
	res, err := Resolve(Spec{Auto: true}, "make", nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, f := range res.Files {
		x, _ := filepath.Rel(dir, f)
		rel = append(rel, x)
	}
	want := []string{"Makefile", "config.mk", "deep.mk", "local/l.mk"}
	if !slices.Equal(rel, want) {
		t.Errorf("got %v, want %v", rel, want)
	}
}

func TestPresets(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".git", "config"), "[core]\n")
	write(t, filepath.Join(dir, ".git", "hooks", "pre-push"), "#!/bin/sh\n")
	write(t, filepath.Join(dir, "sub", "package.json"), "{}")
	write(t, filepath.Join(dir, "sub", "deploy.py"), "print(1)")
	write(t, filepath.Join(dir, "Dockerfile"), "FROM x")
	write(t, filepath.Join(dir, "compose.yaml"), "x: 1")

	rels := func(name string, args []string, cwd string) []string {
		t.Helper()
		r, err := Resolve(Spec{Auto: true}, name, args, cwd)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range r.Files {
			x, _ := filepath.Rel(dir, f)
			out = append(out, x)
		}
		return out
	}
	if got := rels("git", nil, filepath.Join(dir, "sub")); !slices.Equal(got, []string{".git/config", ".git/hooks/pre-push"}) {
		t.Errorf("git from a subdirectory finds the repository root: %v", got)
	}
	if got := rels("npm", nil, filepath.Join(dir, "sub")); !slices.Equal(got, []string{"sub/package.json"}) {
		t.Errorf("npm: %v", got)
	}
	if got := rels("python3", []string{"-u", "deploy.py", "x"}, filepath.Join(dir, "sub")); !slices.Equal(got, []string{"sub/deploy.py"}) {
		t.Errorf("script: %v", got)
	}
	if got := rels("python3", []string{"-c", "print(1)"}, dir); len(got) != 0 {
		t.Errorf("inline code has no file: %v", got)
	}
	if got := rels("docker", nil, dir); !slices.Equal(got, []string{"Dockerfile", "compose.yaml"}) {
		t.Errorf("docker: %v", got)
	}
	for _, n := range []string{"make", "git", "npm", "pnpm", "python3.11", "bash", "node", "docker"} {
		if !HasPreset(n) {
			t.Errorf("%s should have a preset", n)
		}
	}
	for _, n := range []string{"ls", "curl", "pythonista"} {
		if HasPreset(n) {
			t.Errorf("%s should not have a preset", n)
		}
	}
}
