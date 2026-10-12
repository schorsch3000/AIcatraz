// Package jail turns a validated config into a hardened container and runs it.
package jail

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/schorsch3000/aicatraz/internal/config"
)

var unsafeID = regexp.MustCompile(`[^a-z0-9_.-]+`)

// sanitize makes s usable in docker names and tags.
func sanitize(s string) string {
	s = unsafeID.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-_.")
	if s == "" {
		s = "project"
	}
	return s
}

// ProjectID identifies a project: the configured project.id, or the config
// directory's name plus a short hash of the config file's real path.
func ProjectID(cfgPath string, cfg *config.Config) (string, error) {
	if cfg.Project.ID != "" {
		return sanitize(cfg.Project.ID), nil
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(real))
	return fmt.Sprintf("%s-%s", sanitize(filepath.Base(filepath.Dir(real))), hex.EncodeToString(sum[:])[:8]), nil
}

// StateDir is where AIcatraz keeps per-project state on the host, outside
// every mounted volume.
func StateDir(id string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "aicatraz", id), nil
}

// RunBase is the directory holding per-session socket directories. Unix
// socket paths are limited to about 100 bytes, so it lives under
// $XDG_RUNTIME_DIR (or /tmp) rather than in the possibly deep state directory.
// It is created private to the user and checked, since /tmp is shared.
func RunBase() (string, error) {
	uid := os.Getuid()
	base := filepath.Join(os.TempDir(), fmt.Sprintf("aicatraz-%d", uid))
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		if st, err := os.Stat(rt); err == nil && st.IsDir() {
			base = filepath.Join(rt, "aicatraz")
		}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	st, err := os.Lstat(base)
	if err != nil {
		return "", err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(owner.Uid) != uid || st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a private directory owned by you; remove it and retry", base)
	}
	return base, nil
}
