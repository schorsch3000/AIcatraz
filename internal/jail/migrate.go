// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// layoutFiles are the files that moved into .az.
var layoutFiles = []string{config.FileName, defaultDockerfile, defaultDockerfile + ".dockerignore"}

// OldLayout reports whether dir holds a project set up before .az: an
// aicatraz.yaml next to the project and none in .az.
func OldLayout(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, config.DefaultFile)); err == nil {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, config.FileName))
	return err == nil && !st.IsDir()
}

var (
	selfVolume     = regexp.MustCompile(`(?m)^(\s*-\s*['"]?)\./aicatraz\.yaml(['"]?:)`)
	dockerfileLine = regexp.MustCompile(`(?m)^(\s*dockerfile:\s*['"]?)(?:\./)?Dockerfile\.az(['"]?)(\s|$)`)
)

// MigrateLayout moves the config, Dockerfile.az and its ignore file of the
// project in dir into dir/.az, and points the two references the config makes
// to them (the volume that hides it, an explicit dockerfile:) at the new place.
// It returns the files it moved.
func MigrateLayout(dir string) ([]string, error) {
	az := filepath.Join(dir, config.AzDir)
	for _, f := range layoutFiles {
		if _, err := os.Lstat(filepath.Join(az, f)); err == nil {
			if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
				return nil, fmt.Errorf("%s exists in both %s and %s; move it by hand", f, dir, az)
			}
		}
	}
	if err := os.MkdirAll(az, 0o755); err != nil {
		return nil, err
	}
	var moved []string
	for _, f := range layoutFiles {
		from := filepath.Join(dir, f)
		if _, err := os.Lstat(from); err != nil {
			continue
		}
		if err := os.Rename(from, filepath.Join(az, f)); err != nil {
			return moved, err
		}
		moved = append(moved, f)
	}
	cfg := filepath.Join(az, config.FileName)
	data, err := os.ReadFile(cfg)
	if err != nil {
		return moved, err
	}
	text := selfVolume.ReplaceAllString(string(data), "${1}./.az/aicatraz.yaml${2}")
	text = dockerfileLine.ReplaceAllString(text, "${1}.az/Dockerfile.az${2}${3}")
	if text != string(data) {
		if err := os.WriteFile(cfg, []byte(text), 0o644); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// OfferMigrate asks, if dir has the old layout, whether to move it into .az,
// and does so on a yes. It reports whether it moved anything. Only an explicit
// yes counts.
func OfferMigrate(dir string, in io.Reader, out io.Writer) (bool, error) {
	if !OldLayout(dir) {
		return false, nil
	}
	st := ui.For(out)
	fmt.Fprintf(out, "%s this project keeps its files in the project directory; they now live in %s.\n", st.BoldYellow(ui.Prog+":"), st.Bold(config.AzDir+"/"))
	fmt.Fprintf(out, "  Move %s into %s? [y/N] ", st.Bold(strings.Join(existing(dir), ", ")), st.Bold(config.AzDir+"/"))
	ans := readLine(in)
	fmt.Fprintln(out)
	if !strings.EqualFold(ans, "y") && !strings.EqualFold(ans, "yes") {
		return false, nil
	}
	moved, err := MigrateLayout(dir)
	for _, f := range moved {
		fmt.Fprintf(out, "%s %s -> %s\n", st.BoldGreen("moved"), f, filepath.Join(config.AzDir, f))
	}
	if err != nil {
		return len(moved) > 0, err
	}
	fmt.Fprintf(out, "%s a moved Dockerfile.az is asked about once more, since the pin follows its path.\n", st.BoldCyan("note:"))
	return true, nil
}

func existing(dir string) []string {
	var out []string
	for _, f := range layoutFiles {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			out = append(out, f)
		}
	}
	return out
}
