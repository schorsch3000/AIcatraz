// SPDX-License-Identifier: GPL-3.0-only

package devices

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// NodeCommand is the hidden subcommand that creates and removes device nodes.
// The launcher runs it inside the jail with `docker exec --privileged`: it is
// our own static binary, so it works on any image, and the jail itself never
// has the capability to make device nodes.
const NodeCommand = "__dev"

// reserved paths belong to the container runtime. A node there is never made
// or removed, whatever the config says.
func reserved(p string) bool {
	switch p {
	case "/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/ptmx",
		"/dev/console", "/dev/fd", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/pts", "/dev/shm",
		"/dev/mqueue", "/dev/core", "/dev/mem", "/dev/kmem", "/dev/port":
		return true
	}
	return strings.HasPrefix(p, "/dev/pts/") || strings.HasPrefix(p, "/dev/shm/") || strings.HasPrefix(p, "/dev/mqueue/")
}

// Reserved reports whether a path may not be given to the jail.
func Reserved(p string) bool { return reserved(path.Clean(p)) }

const linkDir = "/dev/serial/by-id/"

func validNode(p string) error {
	if p != path.Clean(p) || !strings.HasPrefix(p, "/dev/") || len(p) > 200 || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%q is not a path under /dev", p)
	}
	if reserved(p) {
		return fmt.Errorf("%s belongs to the container", p)
	}
	return nil
}

func validLink(p string) error {
	if p != path.Clean(p) || !strings.HasPrefix(p, linkDir) || len(p) > 200 || strings.ContainsRune(p, 0) || strings.Contains(p[len(linkDir):], "/") {
		return fmt.Errorf("%q is not a name under %s", p, linkDir)
	}
	return nil
}

// RunNodeCommand is the body of `__dev`:
//
//	add <path> <major> <minor> <uid> <gid> [<link>...]
//	remove <path> [<link>...]
func RunNodeCommand(args []string, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "aicatraz "+NodeCommand+":", err)
		return 1
	}
	if len(args) < 2 {
		return fail(errors.New("usage: add path major minor uid gid [link...] | remove path [link...]"))
	}
	switch args[0] {
	case "add":
		if len(args) < 6 {
			return fail(errors.New("usage: add path major minor uid gid [link...]"))
		}
		var nums [4]int
		for i := range nums {
			n, err := strconv.Atoi(args[2+i])
			if err != nil || n < 0 {
				return fail(fmt.Errorf("%q is not a number", args[2+i]))
			}
			nums[i] = n
		}
		if err := AddNode(args[1], uint32(nums[0]), uint32(nums[1]), nums[2], nums[3], args[6:]); err != nil {
			return fail(err)
		}
	case "remove":
		if err := RemoveNode(args[1], args[2:]); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unknown operation %q", args[0]))
	}
	return 0
}

// replaceable reports whether whatever is at p may be replaced: nothing, a
// device node, or a symlink. Never a directory or a regular file.
func replaceable(p string) (bool, error) {
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return st.Mode()&os.ModeDevice != 0 || st.Mode()&os.ModeSymlink != 0, nil
}

// AddNode makes the character device node p for major:minor, owned by
// uid:gid, with a symlink for each link.
func AddNode(p string, major, minor uint32, uid, gid int, links []string) error {
	if err := validNode(p); err != nil {
		return err
	}
	for _, l := range links {
		if err := validLink(l); err != nil {
			return err
		}
	}
	if ok, err := replaceable(p); err != nil || !ok {
		return fmt.Errorf("%s exists and is not a device node", p)
	}
	if err := os.MkdirAll(path.Dir(p), 0o755); err != nil {
		return err
	}
	os.Remove(p)
	if err := unix.Mknod(p, syscall.S_IFCHR|0o660, int(unix.Mkdev(major, minor))); err != nil {
		return fmt.Errorf("mknod %s: %w", p, err)
	}
	if err := os.Chown(p, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(p, 0o660); err != nil {
		return err
	}
	for _, l := range links {
		if ok, err := replaceable(l); err != nil || !ok {
			return fmt.Errorf("%s exists and is not a link", l)
		}
		if err := os.MkdirAll(path.Dir(l), 0o755); err != nil {
			return err
		}
		os.Remove(l)
		if err := os.Symlink(p, l); err != nil {
			return err
		}
	}
	return nil
}

// RemoveNode removes the node and its links, and the directories that are
// left empty below /dev/bus and /dev/serial.
func RemoveNode(p string, links []string) error {
	if err := validNode(p); err != nil {
		return err
	}
	for _, l := range links {
		if err := validLink(l); err != nil {
			return err
		}
	}
	var first error
	remove := func(x string, want os.FileMode) {
		st, err := os.Lstat(x)
		if err != nil {
			return
		}
		if st.Mode()&want == 0 {
			return // not what we made: leave it
		}
		if err := os.Remove(x); err != nil && first == nil {
			first = err
		}
	}
	for _, l := range links {
		remove(l, os.ModeSymlink)
	}
	remove(p, os.ModeDevice)
	for _, dir := range []string{path.Dir(p), "/dev/serial/by-id", "/dev/serial"} {
		if strings.HasPrefix(dir, "/dev/bus/") || strings.HasPrefix(dir, "/dev/serial") {
			os.Remove(dir) // only succeeds if empty
		}
	}
	return first
}
