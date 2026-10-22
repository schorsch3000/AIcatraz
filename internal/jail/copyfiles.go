// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
)

// CopyUserFiles puts the files of the user config's `copy` section into the
// project directory dir. A target that exists, whatever it is, is left alone.
// A file that cannot be copied is reported and skipped; it does not stop the launch.
func CopyUserFiles(home, dir string, stdout, stderr io.Writer) error {
	user, err := config.LoadUser(home)
	if err != nil || user == nil {
		return err
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	for _, f := range user.CopyFiles {
		made, err := copyIfMissing(f.From, root, f.To)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "warning: copy %s: %v\n", f.To, err)
		case made:
			fmt.Fprintf(stdout, "copied %s\n", f.To)
		}
	}
	return nil
}

// copyIfMissing copies the file from to root/to unless root/to exists. The
// jail can write the project directory, so a parent directory that is a
// symlink out of root is refused rather than followed.
func copyIfMissing(from, root, to string) (bool, error) {
	src, err := os.Open(from)
	if err != nil {
		return false, err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, errors.New(from + " is not a regular file")
	}
	dst := filepath.Join(root, filepath.FromSlash(to))
	if err := insideRoot(root, filepath.Dir(dst)); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dst))
	if err != nil {
		return false, err
	}
	out, err := os.OpenFile(filepath.Join(parent, filepath.Base(dst)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out.Name())
		return false, err
	}
	return true, nil
}

// insideRoot checks that the nearest existing ancestor of dir, with symlinks
// resolved, lies in root, so that creating the missing directories stays inside.
func insideRoot(root, dir string) error {
	for {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return errors.New("its directory leads out of the project")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// a dangling symlink also reports not-exist: refuse it
		if _, lerr := os.Lstat(dir); lerr == nil {
			return errors.New("its directory leads out of the project")
		}
		next := filepath.Dir(dir)
		if next == dir {
			return err
		}
		dir = next
	}
}
