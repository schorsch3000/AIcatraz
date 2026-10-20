// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schorsch3000/aicatraz/internal/ui"
)

func TestFormatAudit(t *testing.T) {
	st := ui.Style{}
	got := FormatAudit(`{"time":"2026-10-20T10:00:00Z","event":"ask","name":"make","args":["deploy"],"decision":"allow","decided_by":"human","pins":"attention"}`, st)
	for _, want := range []string{"ask", "make deploy", "allow (human)", "pinned files changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	got = FormatAudit(`{"time":"2026-10-20T10:00:05Z","event":"exit","name":"make","args":["deploy"],"exit_code":2,"duration_ms":4200}`, st)
	if !strings.Contains(got, "exit 2 in 4.2s") {
		t.Errorf("exit line: %q", got)
	}
	if got := FormatAudit("not json", st); got != "not json" {
		t.Errorf("garbage should pass through: %q", got)
	}
}

func TestLogShowsTheLastEntries(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := filepath.Join(dir, "aicatraz.yaml")
	os.WriteFile(cfg, []byte("project: {id: logtest}\n"), 0o644)

	var out bytes.Buffer
	if err := Log(LogOptions{ConfigPath: cfg, Lines: 20}, &out); err != nil || !strings.Contains(out.String(), "nothing logged") {
		t.Fatalf("empty: %v %q", err, out.String())
	}

	state, _ := StateDir("logtest")
	os.MkdirAll(state, 0o700)
	os.WriteFile(filepath.Join(state, "audit.log"), nil, 0o600) // created, but empty
	out.Reset()
	if err := Log(LogOptions{ConfigPath: cfg, Lines: 20}, &out); err != nil || !strings.Contains(out.String(), "nothing logged") {
		t.Fatalf("empty file: %v %q", err, out.String())
	}
	var lines []string
	for _, n := range []string{"one", "two", "three"} {
		lines = append(lines, `{"time":"2026-10-20T10:00:00Z","event":"request","name":"`+n+`"}`)
	}
	os.WriteFile(filepath.Join(state, "audit.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	out.Reset()
	if err := Log(LogOptions{ConfigPath: cfg, Lines: 2}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "one") || !strings.Contains(out.String(), "two") || !strings.Contains(out.String(), "three") {
		t.Errorf("-n 2: %q", out.String())
	}
	out.Reset()
	Log(LogOptions{ConfigPath: cfg}, &out)
	if !strings.Contains(out.String(), "one") {
		t.Errorf("-n 0 shows all: %q", out.String())
	}
}

func TestCleanRemovesOnlyGoneProjects(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	live := filepath.Join(t.TempDir(), "aicatraz.yaml")
	os.WriteFile(live, nil, 0o644)
	for id, cfg := range map[string]string{"live-1": live, "gone-1": "/nonexistent/aicatraz.yaml", "old-1": ""} {
		state, _ := StateDir(id)
		os.MkdirAll(state, 0o700)
		if cfg != "" {
			recordProject(state, cfg)
		}
	}
	exists := func(id string) bool {
		s, _ := StateDir(id)
		_, err := os.Stat(s)
		return err == nil
	}

	var out, errb bytes.Buffer
	// Without a terminal and without --yes nothing is removed.
	if err := Clean(false, strings.NewReader("y\n"), false, &out, &errb); err == nil || !exists("gone-1") {
		t.Fatalf("removed without asking: %v", err)
	}
	// Answering no removes nothing.
	if err := Clean(false, strings.NewReader("n\n"), true, &out, &errb); err != nil || !exists("gone-1") {
		t.Fatalf("removed after no: %v", err)
	}
	if err := Clean(false, strings.NewReader("y\n"), true, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if exists("gone-1") || !exists("live-1") || !exists("old-1") {
		t.Errorf("gone-1=%v live-1=%v old-1=%v", exists("gone-1"), exists("live-1"), exists("old-1"))
	}
	out.Reset()
	Clean(true, nil, false, &out, &errb)
	if !strings.Contains(out.String(), "nothing to clean") {
		t.Errorf("second run: %q", out.String())
	}
}
