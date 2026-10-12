package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

const (
	// maxSyncBytes bounds what one entry may copy, so a huge directory can't
	// make every launch slow without anyone noticing.
	maxSyncBytes = 256 << 20
	maxSyncDepth = 24
)

// SyncRoot brings a root's entries from the host home into dest (the
// per-project directory mounted as the root in the jail).
func SyncRoot(hostHome string, r Root, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	for _, e := range r.Entries {
		hostRel := e.HostPath
		if hostRel == "" {
			hostRel = filepath.Join(r.Path, e.Name)
		}
		if err := syncEntry(filepath.Join(hostHome, hostRel), filepath.Join(dest, e.Name), e); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
	}
	return nil
}

func syncEntry(src, dst string, e Entry) error {
	st, err := os.Stat(src) // follows symlinks: a linked skills directory is copied as what it points to
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if e.Mode == Seed {
				return nil
			}
			return os.RemoveAll(dst) // gone on the host: gone in the jail
		}
		return err
	}
	if e.Mode == Seed {
		if _, err := os.Lstat(dst); err == nil {
			return nil
		}
	}
	budget := int64(maxSyncBytes)
	if st.IsDir() {
		return mirrorDir(src, dst, e.Exclude, &budget, 0, map[string]bool{})
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is neither a file nor a directory", src)
	}
	return mirrorFile(src, dst, st, &budget)
}

func mirrorDir(src, dst string, exclude []string, budget *int64, depth int, chain map[string]bool) error {
	if depth > maxSyncDepth {
		return fmt.Errorf("%s: directories nested too deeply", src)
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	if chain[real] {
		return nil // a symlink cycle
	}
	chain[real] = true
	defer delete(chain, real)

	if st, err := os.Lstat(dst); err == nil && !st.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, de := range entries {
		name := de.Name()
		if slices.Contains(exclude, name) {
			continue
		}
		s, d := filepath.Join(src, name), filepath.Join(dst, name)
		st, err := os.Stat(s)
		if err != nil {
			continue // a dangling symlink
		}
		want[name] = true
		switch {
		case st.IsDir():
			err = mirrorDir(s, d, exclude, budget, depth+1, chain)
		case st.Mode().IsRegular():
			err = mirrorFile(s, d, st, budget)
		default:
			delete(want, name) // sockets, devices: not config
		}
		if err != nil {
			return err
		}
	}
	// Remove what the host no longer has.
	existing, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, de := range existing {
		if !want[de.Name()] {
			if err := os.RemoveAll(filepath.Join(dst, de.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// mirrorFile copies src to dst unless dst already has the same size, mode and
// modification time (copies keep the source's mtime, so this is exact enough).
func mirrorFile(src, dst string, st fs.FileInfo, budget *int64) error {
	if dstSt, err := os.Lstat(dst); err == nil && dstSt.Mode().IsRegular() &&
		dstSt.Size() == st.Size() && dstSt.Mode().Perm() == st.Mode().Perm() && dstSt.ModTime().Equal(st.ModTime()) {
		return nil
	} else if err == nil && dstSt.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	if *budget -= st.Size(); *budget < 0 {
		return fmt.Errorf("more than %d MB to copy; this entry is too large to sync", maxSyncBytes>>20)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".sync-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Credentials and the like keep their restrictive mode; everything is at
	// least private to the user.
	if err := os.Chmod(tmp.Name(), st.Mode().Perm()|0o600); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), st.ModTime(), st.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
