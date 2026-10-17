// SPDX-License-Identifier: GPL-3.0-only

// Package pins remembers which version of a file a human last approved, so a
// rule that really means "run whatever this Makefile says" notices when the
// Makefile changes.
package pins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MaxStoredSize is the largest file whose content is kept (for diffs).
	// Bigger files are still pinned by hash.
	MaxStoredSize = 256 << 10
	// MaxFiles bounds how many files one rule may depend on.
	MaxFiles = 200
)

// Snapshot is a file as read at one moment. Everything is taken from a single
// read, so what is hashed is what is shown and what gets pinned.
type Snapshot struct {
	Path    string // the declared path (the pin's key)
	Real    string // the path after resolving symlinks
	Exists  bool
	Sum     string // hex sha256, empty if the file doesn't exist
	Size    int64
	Content []byte // nil if the file is missing or larger than MaxStoredSize
}

// Take reads path.
func Take(path string) (Snapshot, error) {
	sn := Snapshot{Path: path}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sn, nil
		}
		return sn, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return sn, err
	}
	if !st.Mode().IsRegular() {
		return sn, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(real)
	if err != nil {
		return sn, err
	}
	defer f.Close()

	h := sha256.New()
	var buf []byte
	if st.Size() <= MaxStoredSize {
		if buf, err = io.ReadAll(io.TeeReader(io.LimitReader(f, MaxStoredSize+1), h)); err != nil {
			return sn, err
		}
		if len(buf) > MaxStoredSize { // grew while reading
			buf = nil
		}
	}
	if buf == nil {
		h.Reset()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return sn, err
		}
		if _, err := io.Copy(h, f); err != nil {
			return sn, err
		}
	}
	sn.Real, sn.Exists, sn.Size, sn.Sum, sn.Content = real, true, st.Size(), hex.EncodeToString(h.Sum(nil)), buf
	if buf != nil {
		sn.Size = int64(len(buf))
	}
	return sn, nil
}

type record struct {
	Path string `json:"path"`
	Real string `json:"real"`
	Sum  string `json:"sha256"`
	Size int64  `json:"size"`
	Blob bool   `json:"blob"`
}

// Store keeps pins in a private directory on the host, outside every mount.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open creates the store directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) key(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

// Get returns the pinned snapshot for path, or nil if there is none. Its
// Content is nil if none was stored.
func (s *Store) Get(path string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(path)
}

func (s *Store) get(path string) (*Snapshot, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, s.key(path)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("corrupt pin for %s: %w", path, err)
	}
	sn := &Snapshot{Path: r.Path, Real: r.Real, Exists: true, Sum: r.Sum, Size: r.Size}
	if r.Blob {
		if sn.Content, err = os.ReadFile(filepath.Join(s.dir, s.key(path)+".blob")); err != nil {
			return nil, err
		}
	}
	return sn, nil
}

// Put pins a snapshot. Only call it with a human's approval.
func (s *Store) Put(sn Snapshot) error {
	if !sn.Exists {
		return s.Forget(sn.Path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := record{Path: sn.Path, Real: sn.Real, Sum: sn.Sum, Size: sn.Size, Blob: sn.Content != nil}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	base := filepath.Join(s.dir, s.key(sn.Path))
	if sn.Content != nil {
		if err := writeAtomic(base+".blob", sn.Content); err != nil {
			return err
		}
	} else {
		os.Remove(base + ".blob")
	}
	return writeAtomic(base+".json", data)
}

// Forget removes a pin.
func (s *Store) Forget(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := filepath.Join(s.dir, s.key(path))
	os.Remove(base + ".blob")
	if err := os.Remove(base + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Approved returns when path was pinned (the pin file's modification time),
// or the zero time if there is no pin.
func (s *Store) Approved(path string) time.Time {
	st, err := os.Stat(filepath.Join(s.dir, s.key(path)+".json"))
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// Paths lists every pinned path, sorted.
func (s *Store) Paths() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var r record
		if json.Unmarshal(data, &r) == nil && r.Path != "" {
			paths = append(paths, r.Path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
