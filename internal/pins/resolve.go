package pins

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/glob"
)

// ErrTooMany means a rule's dependencies are too broad to pin. Callers treat
// it as "needs attention", never as "nothing changed".
var ErrTooMany = errors.New("too many dependent files")

const (
	maxWalk        = 20000 // directory entries visited while expanding one pattern
	maxIncludeDeep = 4
)

// Spec says which files a rule depends on.
type Spec struct {
	Auto     bool     // add the built-in set for the command
	Patterns []string // globs relative to the working directory; {args[N]} stands for an argument
}

// Matcher recognises paths covered by a rule's patterns.
type Matcher struct {
	re *regexp.Regexp
}

func (m Matcher) match(path string) bool { return m.re.MatchString(filepath.ToSlash(path)) }

func matchesAny(ms []Matcher, path string) bool {
	for _, m := range ms {
		if m.match(path) {
			return true
		}
	}
	return false
}

// Resolved is the set of files a rule depends on for one particular call.
type Resolved struct {
	Files    []string  // existing regular files, absolute, sorted
	Matchers []Matcher // for noticing deleted files
}

var argPlaceholder = regexp.MustCompile(`\{args\[(\d+)\]\}`)

// Resolve expands a rule's dependencies for a call of name with args in cwd.
func Resolve(spec Spec, name string, args []string, cwd string) (Resolved, error) {
	patterns := substitute(spec.Patterns, args)
	if spec.Auto {
		if p := presets[presetKey(name)]; p != nil {
			patterns = append(patterns, p(args, cwd)...)
		}
	}

	var res Resolved
	files := map[string]bool{}
	seenPat := map[string]bool{}
	for depth := 0; len(patterns) > 0; depth++ {
		var next []string
		for _, pat := range patterns {
			abs := pat
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(cwd, pat)
			}
			abs = filepath.ToSlash(filepath.Clean(abs))
			if seenPat[abs] {
				continue
			}
			seenPat[abs] = true

			re, err := glob.Compile(abs)
			if err != nil {
				return res, err
			}
			res.Matchers = append(res.Matchers, Matcher{re})
			found, err := expand(abs, re)
			if err != nil {
				return res, err
			}
			for _, f := range found {
				if !files[f] {
					files[f] = true
					if spec.Auto && presetKey(name) == "make" && depth < maxIncludeDeep {
						next = append(next, makeIncludes(f)...)
					}
				}
			}
			if len(files) > MaxFiles {
				return res, ErrTooMany
			}
		}
		patterns = next
	}
	for f := range files {
		res.Files = append(res.Files, f)
	}
	sort.Strings(res.Files)
	return res, nil
}

// substitute fills {args[N]} placeholders. The argument is escaped so a
// hostile argument can't turn into a wildcard; a pattern whose argument isn't
// there is dropped.
func substitute(patterns, args []string) []string {
	var out []string
	for _, p := range patterns {
		ok := true
		q := argPlaceholder.ReplaceAllStringFunc(p, func(m string) string {
			n, _ := strconv.Atoi(argPlaceholder.FindStringSubmatch(m)[1])
			if n >= len(args) {
				ok = false
				return ""
			}
			return glob.Escape(args[n])
		})
		if ok {
			out = append(out, q)
		}
	}
	return out
}

// expand lists the regular files an absolute pattern matches.
func expand(abs string, re *regexp.Regexp) ([]string, error) {
	if !glob.HasMeta(abs) {
		path := filepath.FromSlash(abs)
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			return []string{path}, nil
		}
		return nil, nil
	}
	base := glob.StaticPrefix(abs)
	if base == "" {
		base = "."
	}
	var out []string
	visited := 0
	err := filepath.WalkDir(filepath.FromSlash(base), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if visited++; visited > maxWalk {
			return ErrTooMany
		}
		if d.IsDir() {
			// Don't wade through VCS and dependency trees unless the pattern names them.
			if (d.Name() == ".git" || d.Name() == "node_modules") && path != base && !strings.Contains(abs, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if re.MatchString(filepath.ToSlash(path)) {
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
				out = append(out, path)
			}
		}
		return nil
	})
	return out, err
}

// ---- built-in sets ----

var presets = map[string]func(args []string, cwd string) []string{
	"make":   func([]string, string) []string { return []string{"Makefile", "makefile", "GNUmakefile", "*.mk"} },
	"npm":    nodeProject,
	"git":    gitFiles,
	"script": scriptArg,
	"docker": func([]string, string) []string {
		return []string{"Dockerfile", "Dockerfile.*", "compose*.y*ml", "docker-compose*.y*ml", ".dockerignore"}
	},
}

func nodeProject([]string, string) []string {
	return []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml",
		"pnpm-workspace.yaml", "yarn.lock", ".npmrc", ".yarnrc", ".yarnrc.yml"}
}

var interpreter = regexp.MustCompile(`^(python[0-9.]*|bash|sh|dash|zsh|node|ruby|perl|php)$`)

// presetKey maps a command name onto a preset.
func presetKey(name string) string {
	switch name {
	case "make", "gmake":
		return "make"
	case "npm", "npx", "pnpm", "yarn":
		return "npm"
	case "git":
		return "git"
	case "docker", "docker-compose":
		return "docker"
	}
	if interpreter.MatchString(name) {
		return "script"
	}
	return ""
}

// HasPreset reports whether `depends_on: auto` knows what to pin for name.
func HasPreset(name string) bool { return presetKey(name) != "" }

// scriptArg pins the script an interpreter is asked to run: the first
// argument that isn't a flag. Inline code (-c, -e) and modules (-m) have no
// file; the full argument list is visible to the human anyway.
func scriptArg(args []string, _ string) []string {
	for _, a := range args {
		switch {
		case a == "-c" || a == "-e" || a == "-m" || a == "-E":
			return nil
		case strings.HasPrefix(a, "-"):
			continue
		}
		return []string{glob.Escape(a)}
	}
	return nil
}

func gitFiles(_ []string, cwd string) []string {
	root := cwd
	for {
		if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return nil
		}
		root = parent
	}
	return []string{
		filepath.Join(root, ".git"), // a file in worktrees and submodules; a directory is skipped
		filepath.Join(root, ".git", "config"),
		filepath.Join(root, ".git", "hooks", "*"),
		filepath.Join(root, ".gitattributes"),
		filepath.Join(root, ".gitmodules"),
	}
}

var makeInclude = regexp.MustCompile(`(?m)^[ \t]*(?:-include|sinclude|include)[ \t]+([^\n#]+)`)

// makeIncludes returns the files a makefile pulls in with `include`. Entries
// that depend on make variables or pattern rules can't be known statically and
// are skipped.
func makeIncludes(file string) []string {
	base := filepath.Base(file)
	if base != "Makefile" && base != "makefile" && base != "GNUmakefile" && !strings.HasSuffix(base, ".mk") {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil || len(data) > 1<<20 {
		return nil
	}
	var out []string
	for _, m := range makeInclude.FindAllSubmatch(data, -1) {
		for _, tok := range strings.Fields(string(m[1])) {
			if strings.ContainsAny(tok, "$%") {
				continue
			}
			out = append(out, tok)
		}
	}
	return out
}
