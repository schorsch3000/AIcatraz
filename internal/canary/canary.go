// Package canary checks, from inside the container, that hidden paths are
// still hidden.
//
// A hidden file is /dev/null bind-mounted over it; a hidden directory is an
// empty tmpfs. The mount sits on a specific file or directory. If something on
// the host replaces that file by renaming a new one over it (most editors save
// that way), the name now points at a new inode with nothing mounted over it,
// and the agent could read the real contents. The check looks at what the
// container sees: a hidden file must be the /dev/null character device, a
// hidden directory must be a tmpfs.
package canary

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Marker starts every line that reports an exposed path.
const Marker = "AICATRAZ-EXPOSED "

const tmpfsMagic = 0x01021994

// Entry is a hidden path and whether it is a directory.
type Entry struct {
	Path string
	Dir  bool
}

// Arg encodes an entry as a command-line argument.
func (e Entry) Arg() string {
	if e.Dir {
		return "dir:" + e.Path
	}
	return "file:" + e.Path
}

// ParseArg is the inverse of Arg.
func ParseArg(a string) (Entry, error) {
	switch {
	case strings.HasPrefix(a, "dir:"):
		return Entry{Path: a[4:], Dir: true}, nil
	case strings.HasPrefix(a, "file:"):
		return Entry{Path: a[5:]}, nil
	}
	return Entry{}, fmt.Errorf("bad canary argument %q", a)
}

// Exposed returns the paths that are no longer hidden. A path that can't be
// inspected counts as exposed: the safe answer when unsure.
func Exposed(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		if !hidden(e) {
			out = append(out, e.Path)
		}
	}
	return out
}

func hidden(e Entry) bool {
	if e.Dir {
		var fs unix.Statfs_t
		if err := unix.Statfs(e.Path, &fs); err != nil {
			return false
		}
		entries, err := os.ReadDir(e.Path)
		return err == nil && len(entries) == 0 && hidingTmpfs(fs)
	}
	var st unix.Stat_t
	if err := unix.Stat(e.Path, &st); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFCHR && unix.Major(uint64(st.Rdev)) == 1 && unix.Minor(uint64(st.Rdev)) == 3
}

// hidingTmpfs recognises the filesystem laid over a hidden directory: a
// read-only tmpfs of a single block. Just "is a tmpfs" is not enough, since a
// project that lives under /tmp or /dev/shm sits on a tmpfs itself.
func hidingTmpfs(fs unix.Statfs_t) bool {
	return int64(fs.Type) == tmpfsMagic && fs.Flags&unix.ST_RDONLY != 0 && fs.Blocks <= 16
}

// Run is the `__canary` subcommand: it prints one marker line per exposed path
// and returns 1 if there is any.
func Run(args []string, out interface{ Write([]byte) (int, error) }) int {
	var entries []Entry
	for _, a := range args {
		e, err := ParseArg(a)
		if err != nil {
			fmt.Fprintln(out, err)
			return 2
		}
		entries = append(entries, e)
	}
	exposed := Exposed(entries)
	for _, p := range exposed {
		fmt.Fprintf(out, "%s%s\n", Marker, p)
	}
	if len(exposed) > 0 {
		return 1
	}
	return 0
}
