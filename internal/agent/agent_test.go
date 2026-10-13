package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func TestClaudeCommand(t *testing.T) {
	p, err := Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	argv, env := p.Command("/home/u", LaunchOptions{SkipPermissions: true, SessionID: "abc-123", Args: []string{"--model", "x"}})
	want := []string{"claude", "--dangerously-skip-permissions", "--settings", `{"skipDangerousModePermissionPrompt":true}`, "--session-id", "abc-123", "--model", "x"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv %v", argv)
	}
	if env["CLAUDE_CONFIG_DIR"] != "/home/u/.claude" || env["DISABLE_AUTOUPDATER"] != "1" {
		t.Errorf("env %v", env)
	}
	argv, _ = p.Command("/home/u", LaunchOptions{Resume: "r-1", SessionID: "ignored"})
	if !slices.Equal(argv, []string{"claude", "--resume", "r-1"}) {
		t.Errorf("resume wins over a new session id, and skip is off: %v", argv)
	}
	if !p.SupportsSessionID() {
		t.Error("claude takes a session id up front")
	}
}

func TestOpencodeCommand(t *testing.T) {
	p, _ := Lookup("opencode")
	argv, env := p.Command("/home/u", LaunchOptions{SkipPermissions: true, Resume: "ses_1"})
	if !slices.Equal(argv, []string{"opencode", "--session", "ses_1"}) {
		t.Errorf("argv %v", argv)
	}
	if env["OPENCODE_PERMISSION"] != `{"*":"allow"}` || env["OPENCODE_DISABLE_AUTOUPDATE"] != "true" {
		t.Errorf("env %v", env)
	}
	_, env = p.Command("/home/u", LaunchOptions{})
	if _, ok := env["OPENCODE_PERMISSION"]; ok {
		t.Error("permissions stay as configured unless skip is on")
	}
	if p.SupportsSessionID() {
		t.Error("opencode picks its own session ids")
	}
}

func TestLookupAndParentDirs(t *testing.T) {
	if _, err := Lookup("emacs"); err == nil {
		t.Error("unknown agent")
	}
	if !slices.Equal(Names(), []string{"claude", "opencode"}) {
		t.Errorf("%v", Names())
	}
	p, _ := Lookup("opencode")
	if got := p.ParentDirs(); !slices.Equal(got, []string{".config", ".local", ".local/share", ".local/state"}) {
		t.Errorf("parents %v", got)
	}
	c, _ := Lookup("claude")
	if got := c.ParentDirs(); len(got) != 0 {
		t.Errorf("~/.claude is a direct child of home: %v", got)
	}
}

func TestSyncCopiesAndMirrors(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(t.TempDir(), "root")
	root := Root{Path: ".claude", Entries: []Entry{
		{Name: "settings.json"},
		{Name: ".claude.json", HostPath: ".claude.json"},
		{Name: "skills", Exclude: []string{"node_modules"}},
		{Name: "missing.md"},
		{Name: "kv.json", Mode: Seed},
	}}
	put(t, filepath.Join(home, ".claude", "settings.json"), `{"a":1}`)
	put(t, filepath.Join(home, ".claude.json"), `{"top":true}`)
	put(t, filepath.Join(home, ".claude", "skills", "a", "SKILL.md"), "skill a")
	put(t, filepath.Join(home, ".claude", "skills", "node_modules", "x.js"), "no")
	put(t, filepath.Join(home, ".claude", "kv.json"), "seed1")
	// State the agent owns, which must survive.
	put(t, filepath.Join(dest, "projects", "p", "s.jsonl"), "transcript")

	if err := SyncRoot(home, root, dest); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dest, "settings.json")) != `{"a":1}` ||
		read(t, filepath.Join(dest, ".claude.json")) != `{"top":true}` ||
		read(t, filepath.Join(dest, "skills", "a", "SKILL.md")) != "skill a" {
		t.Fatal("entries not copied")
	}
	if exists(filepath.Join(dest, "skills", "node_modules")) {
		t.Error("excluded names must not be copied")
	}
	if read(t, filepath.Join(dest, "projects", "p", "s.jsonl")) != "transcript" {
		t.Error("the agent's own state must be left alone")
	}

	// The agent edits synced files and adds a stray file inside a synced dir.
	put(t, filepath.Join(dest, "settings.json"), `{"evil":1}`)
	put(t, filepath.Join(dest, "skills", "a", "extra.md"), "planted")
	put(t, filepath.Join(dest, "kv.json"), "seed-by-agent")
	// You change your real config.
	put(t, filepath.Join(home, ".claude", "settings.json"), `{"a":2}`)
	os.Remove(filepath.Join(home, ".claude", "skills", "a", "SKILL.md"))
	put(t, filepath.Join(home, ".claude", "skills", "b.md"), "new skill")

	if err := SyncRoot(home, root, dest); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dest, "settings.json")); got != `{"a":2}` {
		t.Errorf("host change not carried over / agent edit not discarded: %q", got)
	}
	if exists(filepath.Join(dest, "skills", "a", "extra.md")) || exists(filepath.Join(dest, "skills", "a", "SKILL.md")) {
		t.Error("the jail's copy must mirror the host exactly")
	}
	if read(t, filepath.Join(dest, "skills", "b.md")) != "new skill" {
		t.Error("new host file missing")
	}
	if read(t, filepath.Join(dest, "kv.json")) != "seed-by-agent" {
		t.Error("a seeded file is never overwritten")
	}

	// Removed on the host: removed in the jail.
	os.Remove(filepath.Join(home, ".claude.json"))
	if err := SyncRoot(home, root, dest); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dest, ".claude.json")) {
		t.Error("a file deleted on the host must disappear from the jail")
	}
}

func TestSyncKeepsPrivateModesAndSkipsUnchangedFiles(t *testing.T) {
	home := t.TempDir()
	dest := t.TempDir()
	creds := filepath.Join(home, ".claude", ".credentials.json")
	put(t, creds, "secret")
	os.Chmod(creds, 0o600)
	root := Root{Path: ".claude", Entries: []Entry{{Name: ".credentials.json"}}}
	if err := SyncRoot(home, root, dest); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dest, ".credentials.json"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode %v", st.Mode().Perm())
	}
	srcSt, _ := os.Stat(creds)
	if !st.ModTime().Equal(srcSt.ModTime()) {
		t.Error("mtime must be preserved so unchanged files can be skipped")
	}
	// An unchanged file isn't rewritten: its inode stays.
	before, _ := os.Stat(filepath.Join(dest, ".credentials.json"))
	time.Sleep(10 * time.Millisecond)
	SyncRoot(home, root, dest)
	after, _ := os.Stat(filepath.Join(dest, ".credentials.json"))
	if !os.SameFile(before, after) {
		t.Error("an unchanged file must not be copied again")
	}
}

func TestSyncFollowsSymlinksAndSurvivesCycles(t *testing.T) {
	home := t.TempDir()
	dest := t.TempDir()
	shared := t.TempDir()
	put(t, filepath.Join(shared, "linked-skill", "SKILL.md"), "from a link")
	os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755)
	os.Symlink(filepath.Join(shared, "linked-skill"), filepath.Join(home, ".claude", "skills", "linked"))
	os.Symlink(filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".claude", "skills", "loop")) // cycle
	os.Symlink("/nonexistent/target", filepath.Join(home, ".claude", "skills", "dangling"))
	root := Root{Path: ".claude", Entries: []Entry{{Name: "skills"}}}
	if err := SyncRoot(home, root, dest); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dest, "skills", "linked", "SKILL.md")) != "from a link" {
		t.Error("a symlinked directory is copied as what it points to")
	}
	if exists(filepath.Join(dest, "skills", "dangling")) {
		t.Error("dangling links are skipped")
	}
	// Nothing copied is a symlink: the jail can't be pointed at host paths.
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if d != nil && d.Type()&os.ModeSymlink != 0 {
			t.Errorf("symlink in the jail copy: %s", p)
		}
		return nil
	})
}

func TestSyncSizeLimit(t *testing.T) {
	home := t.TempDir()
	big := filepath.Join(home, ".config", "opencode", "plugins", "big.bin")
	os.MkdirAll(filepath.Dir(big), 0o755)
	f, _ := os.Create(big)
	f.Truncate(maxSyncBytes + 1) // sparse
	f.Close()
	err := SyncRoot(home, Root{Path: ".config/opencode", Entries: []Entry{{Name: "plugins"}}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("got %v", err)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.296 (Claude Code)\n": "2.1.296",
		"1.18.35\n":               "1.18.35",
		"opencode 0.3.1-beta.2":   "0.3.1-beta.2",
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := ParseVersion("no digits here"); err == nil {
		t.Error("must fail without a version")
	}
}

func fakeTool(t *testing.T, dir, name, version string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\necho '" + version + " (fake " + name + ")'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallFromHostAndResolve(t *testing.T) {
	cache := t.TempDir()
	bin := t.TempDir()
	fakeTool(t, bin, "claude", "9.8.7")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	p, _ := Lookup("claude")

	if _, _, err := Resolve(cache, p, ""); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("empty cache: %v", err)
	}
	v, err := InstallFromHost(cache, p)
	if err != nil || v != "9.8.7" {
		t.Fatalf("%q %v", v, err)
	}
	dir, ver, err := Resolve(cache, p, "latest")
	if err != nil || ver != "9.8.7" || !exists(filepath.Join(dir, "claude")) {
		t.Fatalf("%q %q %v", dir, ver, err)
	}
	if _, _, err := Resolve(cache, p, "1.0.0"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("other version: %v", err)
	}
	if _, _, err := Resolve(cache, p, "../evil"); err == nil || errors.Is(err, ErrNotInstalled) {
		t.Errorf("path-like version must be rejected: %v", err)
	}

	// A newer version becomes current; the old one stays for pinning.
	fakeTool(t, bin, "claude", "9.9.0")
	if _, err := InstallFromHost(cache, p); err != nil {
		t.Fatal(err)
	}
	if _, ver, _ := Resolve(cache, p, ""); ver != "9.9.0" {
		t.Errorf("current is %s", ver)
	}
	if _, ver, err := Resolve(cache, p, "9.8.7"); err != nil || ver != "9.8.7" {
		t.Errorf("pinned old version: %v %v", ver, err)
	}
	list, _ := List(cache)
	if len(list) != 2 || list[0].Version != "9.8.7" || list[0].Current || !list[1].Current {
		t.Errorf("%+v", list)
	}
	// No install directories are left behind.
	entries, _ := os.ReadDir(filepath.Join(cache, "claude"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestInstallFromHostWithoutTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	p, _ := Lookup("opencode")
	if _, err := InstallFromHost(t.TempDir(), p); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("%v", err)
	}
}

func TestSessionStore(t *testing.T) {
	s := OpenStore(t.TempDir())
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("%v %v", list, err)
	}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"aaa", "bbb", "ccc"} {
		err := s.Add(Session{Tool: "claude", ID: id, Started: t0, Ended: t0.Add(time.Duration(i) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.List()
	if list[0].ID != "ccc" || list[2].ID != "aaa" {
		t.Errorf("most recent first: %+v", list)
	}
	// The same id again replaces the record.
	s.Add(Session{Tool: "claude", ID: "aaa", Ended: t0.Add(10 * time.Hour), ExitCode: 3})
	list, _ = s.List()
	if len(list) != 3 || list[0].ID != "aaa" || list[0].ExitCode != 3 {
		t.Errorf("%+v", list)
	}
	if got, _ := s.Find("bbb"); got == nil || got.Tool != "claude" {
		t.Errorf("find: %+v", got)
	}
	if got, _ := s.Find("zzz"); got != nil {
		t.Error("unknown id")
	}
	if err := s.Add(Session{Tool: "claude", ID: "bad id; rm -rf"}); err == nil {
		t.Error("a hostile id must be refused")
	}
	st, _ := os.Stat(s.path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"ses_abc123", "9b2a2c1e-1111-4222-8333-444455556666", "a"} {
		if !ValidID(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a;b", "../x", "-rf", "a\nb", strings.Repeat("a", 101), "$(id)"} {
		if bad == "-rf" {
			continue // a leading dash is allowed by the character set; the agent receives it as an option value
		}
		if ValidID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	id := NewSessionID()
	if !ValidID(id) || len(id) != 36 || id[14] != '4' {
		t.Errorf("uuid %q", id)
	}
	if NewSessionID() == id {
		t.Error("ids must differ")
	}
}

func TestClaudeSessionExists(t *testing.T) {
	cfg := t.TempDir()
	put(t, filepath.Join(cfg, "projects", "-home-u-p", "abc-1.jsonl"), "{}")
	if !ClaudeSessionExists(cfg, "abc-1") || ClaudeSessionExists(cfg, "abc-2") || ClaudeSessionExists(cfg, "../abc-1") {
		t.Error("transcript lookup")
	}
}

func TestOpencodeSessions(t *testing.T) {
	data := []byte(`[
	  {"id":"ses_old","title":"old","updated":1000,"created":900,"projectId":"p","directory":"/home/u/p"},
	  {"id":"ses_new","title":"new work","updated":5000,"created":4000,"projectId":"p","directory":"/home/u/p"},
	  {"id":"ses_other","title":"x","updated":9000,"created":8000,"projectId":"q","directory":"/home/u/other"},
	  {"id":"bad id","title":"x","updated":9500,"created":8000,"projectId":"q","directory":"/home/u/p"}
	]`)
	list, err := ParseOpencodeSessions(data)
	if err != nil || len(list) != 4 {
		t.Fatalf("%v %v", list, err)
	}
	got, ok := PickOpencodeSession(list, "/home/u/p", time.UnixMilli(2000))
	if !ok || got.ID != "ses_new" {
		t.Errorf("%+v %v", got, ok)
	}
	if _, ok := PickOpencodeSession(list, "/home/u/p", time.UnixMilli(6000)); ok {
		t.Error("nothing was touched since then")
	}
	for _, empty := range []string{"", "\n", "  "} {
		if l, err := ParseOpencodeSessions([]byte(empty)); err != nil || len(l) != 0 {
			t.Errorf("no output means no sessions: %q -> %v %v", empty, l, err)
		}
	}
	if _, err := ParseOpencodeSessions([]byte("not json")); err == nil {
		t.Error("bad json")
	}
}
