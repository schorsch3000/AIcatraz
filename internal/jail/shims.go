package jail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
)

// ShimCommands lists the command names that get a shim symlink: every rule
// command that is a plain name (a glob such as `*` can't be a file name).
func ShimCommands(rules []config.Rule) []string {
	seen := map[string]bool{}
	var names []string
	for _, r := range rules {
		if r.Command == "" || strings.ContainsAny(r.Command, "*?[") || seen[r.Command] {
			continue
		}
		seen[r.Command] = true
		names = append(names, r.Command)
	}
	return names
}

// WriteShimDir recreates dir with one symlink per command, each pointing at
// the aicatraz binary as mounted inside the container. The links dangle on the
// host; they only resolve inside the jail.
func WriteShimDir(dir string, commands []string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, c := range commands {
		if err := os.Symlink(ContainerBinary, filepath.Join(dir, c)); err != nil {
			return fmt.Errorf("shim for %s: %w", c, err)
		}
	}
	return nil
}
