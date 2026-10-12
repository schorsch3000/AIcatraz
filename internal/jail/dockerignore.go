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

// ignoreFilesFor lists the ignore files that may apply to a build: the
// context's .dockerignore and the BuildKit-style <dockerfile>.dockerignore.
// Which one the builder honours depends on the builder, so every existing
// file has to pass the check.
func ignoreFilesFor(b Build) []string {
	var files []string
	for _, f := range []string{
		filepath.Join(b.Context, ".dockerignore"),
		b.Dockerfile + ".dockerignore",
	} {
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			files = append(files, f)
		}
	}
	return files
}

// CheckHidden refuses a build when a hidden path inside the build context is
// not excluded by every applicable ignore file. Otherwise something like
// `COPY . /app` would bake the hidden file into the image.
func CheckHidden(b Build, hidden []string) error {
	var inCtx []string // relative to context
	for _, h := range hidden {
		rel, err := filepath.Rel(b.Context, h)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		inCtx = append(inCtx, filepath.ToSlash(rel))
	}
	if len(inCtx) == 0 {
		return nil
	}

	files := ignoreFilesFor(b)
	if len(files) == 0 {
		return fmt.Errorf("hidden path %q is inside the build context %s but there is no .dockerignore excluding it; "+
			"add it to %s so it can't end up in the image",
			inCtx[0], b.Context, filepath.Join(b.Context, ".dockerignore"))
	}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return err
		}
		ig, err := ParseIgnore(fh)
		fh.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		for _, rel := range inCtx {
			if !ig.Excluded(rel) {
				return fmt.Errorf("hidden path %q is inside the build context but %s does not exclude it; "+
					"add a line for it so it can't end up in the image", rel, f)
			}
		}
	}
	return nil
}
