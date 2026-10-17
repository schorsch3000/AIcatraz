// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeCfg(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "aicatraz.yaml")
	os.WriteFile(p, []byte("project: {id: demo}\n"), 0o644)
	return p
}

func TestUpArgsNewSession(t *testing.T) {
	cfg := writeCfg(t)
	args := UpArgs("/usr/bin/tmux", "/opt/aicatraz", cfg, "/work", "/run/env-1", false,
		Options{ApprovalTTY: "/dev/pts/9", Command: []string{"make", "build"}})
	joined := strings.Join(args, " ")
	for _, want := range []string{"new-session -s aicatraz-demo -c /work", "/opt/aicatraz __with-env /run/env-1 run -c " + cfg, "--approval-tty /dev/pts/9", "-- make build"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	// The launcher's environment must never be on the tmux command line.
	t.Setenv("MY_API_TOKEN", "hunter2")
	if strings.Contains(strings.Join(UpArgs("tmux", "self", cfg, "/", "/e", false, Options{}), " "), "hunter2") {
		t.Error("secret in tmux arguments")
	}
}

func TestUpArgsAttach(t *testing.T) {
	args := UpArgs("tmux", "self", writeCfg(t), "/", "", true, Options{})
	if !slices.Equal(args, []string{"tmux", "attach-session", "-t", "=aicatraz-demo"}) {
		t.Errorf("%v", args)
	}
}

func TestEnvRoundTrip(t *testing.T) {
	t.Setenv("AZ_TEST_SECRET", "s3cr=et")
	t.Setenv("TMUX", "old")
	file, err := saveEnv()
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(file)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("env file mode %v", st.Mode().Perm())
	}
	t.Setenv("AZ_TEST_SECRET", "changed")
	t.Setenv("TMUX", "new-pane-value")
	t.Setenv("TMUX_PANE", "%3")
	if err := RestoreEnv(file); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AZ_TEST_SECRET") != "s3cr=et" {
		t.Errorf("value with '=' not restored: %q", os.Getenv("AZ_TEST_SECRET"))
	}
	if os.Getenv("TMUX") != "new-pane-value" || os.Getenv("TMUX_PANE") != "%3" {
		t.Error("the current pane's TMUX variables must win")
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("env file must be deleted after use")
	}
}
