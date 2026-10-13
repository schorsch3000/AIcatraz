package broker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// confineArgs implements `confine_paths`: every argument that is, or points
// to, a path must resolve (symlinks included) into a visible volume and not
// into a hidden one. It looks at the argument itself and at the value of
// `--flag=value`.
func (b *Broker) confineArgs(args []string, cwd string) error {
	home := ""
	for _, kv := range b.opts.HostEnv {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	for _, a := range args {
		cands := []string{a}
		if strings.HasPrefix(a, "-") {
			if _, v, ok := strings.Cut(a, "="); ok {
				cands = []string{v}
			} else {
				continue // a plain flag
			}
		}
		for _, c := range cands {
			if !looksLikePath(c, cwd) {
				continue
			}
			real, err := resolveLoose(c, cwd, home)
			if err != nil {
				return fmt.Errorf("cannot resolve argument %q: %v", c, err)
			}
			if _, bad := underAny(b.hidden, real); bad {
				return fmt.Errorf("argument %q is inside a hidden path", c)
			}
			if _, ok := underAny(b.visible, real); !ok {
				return fmt.Errorf("argument %q is outside the mounted volumes", c)
			}
		}
	}
	return nil
}

// looksLikePath decides whether an argument should be treated as a path: it
// has a slash (and isn't a URL), starts with ~ or ., or names something that
// exists in the working directory (it might be a symlink to somewhere else).
func looksLikePath(arg, cwd string) bool {
	switch {
	case arg == "":
		return false
	case strings.Contains(arg, "://"):
		return false
	case strings.ContainsRune(arg, '/'), strings.HasPrefix(arg, "~"), strings.HasPrefix(arg, "."):
		return true
	}
	_, err := os.Lstat(filepath.Join(cwd, arg))
	return err == nil
}

// resolveLoose turns an argument into an absolute path with symlinks
// resolved. The path need not exist: the nearest existing ancestor is
// resolved and the rest appended.
func resolveLoose(arg, cwd, home string) (string, error) {
	p := arg
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home == "" {
			return "", errors.New("no home directory")
		}
		p = filepath.Join(home, p[1:])
	case strings.HasPrefix(p, "~"):
		return "", errors.New("~user is not supported")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)

	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if cur == "/" || cur == filepath.Dir(cur) {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}
