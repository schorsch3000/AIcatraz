// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"bufio"
	"fmt"
	"github.com/schorsch3000/aicatraz/internal/glob"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Ignore is a parsed .dockerignore file.
type Ignore struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	negate bool
	re     *regexp.Regexp
}

// ParseIgnore parses .dockerignore syntax: one pattern per line, `#`
// comments, `!` negation, `*`, `?`, `[...]` and `**` wildcards. The last
// matching pattern wins, and a pattern matching a parent directory excludes
// everything below it.
func ParseIgnore(r io.Reader) (*Ignore, error) {
	ig := &Ignore{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		neg := false
		if strings.HasPrefix(line, "!") {
			neg = true
			line = strings.TrimSpace(line[1:])
		}
		line = strings.TrimPrefix(path.Clean(line), "/")
		if line == "" || line == "." {
			continue
		}
		re, err := glob.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", line, err)
		}
		ig.patterns = append(ig.patterns, ignorePattern{negate: neg, re: re})
	}
	return ig, sc.Err()
}

// Excluded reports whether rel (slash-separated, relative to the context
// root) is excluded from the build context.
func (ig *Ignore) Excluded(rel string) bool {
	rel = path.Clean(rel)
	excluded := false
	for _, p := range ig.patterns {
		if p.matches(rel) {
			excluded = !p.negate
		}
	}
	return excluded
}

func (p ignorePattern) matches(rel string) bool {
	for cur := rel; cur != "." && cur != "/" && cur != ""; cur = path.Dir(cur) {
		if p.re.MatchString(cur) {
			return true
		}
	}
	return false
}

// IgnorePath is the ignore file of a build: <dockerfile>.dockerignore, which
// BuildKit reads ahead of the context's .dockerignore.
func IgnorePath(b Build) string { return b.Dockerfile + ".dockerignore" }

// ignoreFilesFor lists the ignore files that apply to a build: the Dockerfile's
// own, if it exists.
func ignoreFilesFor(b Build) []string {
	f := IgnorePath(b)
	if st, err := os.Stat(f); err == nil && !st.IsDir() {
		return []string{f}
	}
	return nil
}

// MigrateIgnore is the fallback for projects that only have a .dockerignore:
// with no <dockerfile>.dockerignore but a .dockerignore in the build context,
// ask is called and answers "rename" (the .dockerignore becomes the
// Dockerfile's), "create" (a blank one is written, the .dockerignore stays) or
// anything else to leave things as they are.
func MigrateIgnore(b Build, ask func(old, new string) string) error {
	dst := IgnorePath(b)
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	old := filepath.Join(b.Context, ".dockerignore")
	if st, err := os.Stat(old); err != nil || st.IsDir() {
		return nil
	}
	switch ask(old, dst) {
	case "rename":
		return os.Rename(old, dst)
	case "create":
		return os.WriteFile(dst, nil, 0o644)
	}
	return nil
}

// IgnoreGap is a hidden path inside the build context that one ignore file
// does not exclude.
type IgnoreGap struct {
	File   string // the ignore file that is missing the path
	Rel    string // the hidden path, relative to the context, slash-separated
	Exists bool   // false if File would have to be created
}

// hiddenInContext returns the hidden paths below the build context, relative
// to it.
func hiddenInContext(b Build, hidden []string) []string {
	var inCtx []string
	for _, h := range hidden {
		rel, err := filepath.Rel(b.Context, h)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		inCtx = append(inCtx, filepath.ToSlash(rel))
	}
	sort.Strings(inCtx)
	return inCtx
}

// IgnoreGaps lists, for every applicable ignore file, the hidden paths it
// does not exclude. With no ignore file at all, the Dockerfile's
// .dockerignore is the one that is missing them.
func IgnoreGaps(b Build, hidden []string) ([]IgnoreGap, error) {
	inCtx := hiddenInContext(b, hidden)
	if len(inCtx) == 0 {
		return nil, nil
	}
	files := ignoreFilesFor(b)
	var gaps []IgnoreGap
	if len(files) == 0 {
		f := IgnorePath(b)
		for _, rel := range inCtx {
			gaps = append(gaps, IgnoreGap{File: f, Rel: rel})
		}
		return gaps, nil
	}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		ig, err := ParseIgnore(fh)
		fh.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, rel := range inCtx {
			if !ig.Excluded(rel) {
				gaps = append(gaps, IgnoreGap{File: f, Rel: rel, Exists: true})
			}
		}
	}
	return gaps, nil
}

// CheckHidden refuses a build when a hidden path inside the build context is
// not excluded by every applicable ignore file. Otherwise something like
// `COPY . /app` would bake the hidden file into the image.
func CheckHidden(b Build, hidden []string) error {
	gaps, err := IgnoreGaps(b, hidden)
	if err != nil {
		return err
	}
	if len(gaps) == 0 {
		return nil
	}
	g := gaps[0]
	if !g.Exists {
		return fmt.Errorf("hidden path %q is inside the build context %s but there is no %s excluding it; "+
			"add it to %s so it can't end up in the image", g.Rel, b.Context, filepath.Base(g.File), g.File)
	}
	return fmt.Errorf("hidden path %q is inside the build context but %s does not exclude it; "+
		"add a line for it so it can't end up in the image", g.Rel, g.File)
}

// AppendIgnore adds a line for rel to an ignore file, creating the file if it
// does not exist.
func AppendIgnore(file, rel string) error {
	prefix := ""
	if data, err := os.ReadFile(file); err == nil && len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, prefix+rel+"\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// EnsureHiddenIgnored runs CheckHidden and, when ask is given, offers to fix
// every gap first: ask is called once per ignore file and path, and only a
// yes changes the file. Whatever stays unfixed is reported by CheckHidden.
func EnsureHiddenIgnored(b Build, hidden []string, ask func(IgnoreGap) bool) error {
	if ask != nil {
		gaps, err := IgnoreGaps(b, hidden)
		if err != nil {
			return err
		}
		created := map[string]bool{}
		for _, g := range gaps {
			g.Exists = g.Exists || created[g.File]
			if !ask(g) {
				continue
			}
			if err := AppendIgnore(g.File, g.Rel); err != nil {
				return err
			}
			created[g.File] = true
		}
	}
	return CheckHidden(b, hidden)
}
