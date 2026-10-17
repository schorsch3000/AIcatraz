// SPDX-License-Identifier: GPL-3.0-only

package canary

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileHiddenByDevNull(t *testing.T) {
	// /dev/null is exactly what a hidden file looks like from inside the jail.
	if got := Exposed([]Entry{{Path: "/dev/null"}}); len(got) != 0 {
		t.Errorf("/dev/null is hidden: %v", got)
	}
	// A regular file is what an exposed one looks like.
	f := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(f, []byte("SECRET=1"), 0o644)
	if got := Exposed([]Entry{{Path: f}}); !slices.Equal(got, []string{f}) {
		t.Errorf("a regular file is exposed: %v", got)
	}
	// Other character devices are not /dev/null.
	if got := Exposed([]Entry{{Path: "/dev/zero"}}); len(got) != 1 {
		t.Errorf("/dev/zero is not the hiding device: %v", got)
	}
}

func TestMissingOrUnreadablePathCountsAsExposed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if got := Exposed([]Entry{{Path: missing}, {Path: missing, Dir: true}}); len(got) != 2 {
		t.Errorf("the safe answer when unsure: %v", got)
	}
}

func TestHidingTmpfsIsRecognisedByItsShape(t *testing.T) {
	mk := func(typ int64, flags int64, blocks uint64) unix.Statfs_t {
		var fs unix.Statfs_t
		fs.Type, fs.Flags, fs.Blocks = typ, flags, blocks
		return fs
	}
	if !hidingTmpfs(mk(tmpfsMagic, unix.ST_RDONLY, 1)) {
		t.Error("a read-only one-block tmpfs is the hiding mount")
	}
	// A project under /tmp sits on a tmpfs too: that must not count as hidden.
	if hidingTmpfs(mk(tmpfsMagic, 0, 1024*1024)) {
		t.Error("an ordinary writable tmpfs is not the hiding mount")
	}
	if hidingTmpfs(mk(0xEF53, unix.ST_RDONLY, 1)) {
		t.Error("ext4 is not tmpfs")
	}
	if hidingTmpfs(mk(tmpfsMagic, unix.ST_RDONLY, 4096)) {
		t.Error("a big read-only tmpfs is something else")
	}
}

func TestOrdinaryDirectoriesAreExposed(t *testing.T) {
	for _, d := range []string{t.TempDir(), "/proc"} {
		if got := Exposed([]Entry{{Path: d, Dir: true}}); len(got) != 1 {
			t.Errorf("%s must count as exposed (even on a tmpfs): %v", d, got)
		}
	}
	nonEmpty := t.TempDir()
	os.WriteFile(filepath.Join(nonEmpty, "f"), nil, 0o644)
	if got := Exposed([]Entry{{Path: nonEmpty, Dir: true}}); len(got) != 1 {
		t.Errorf("a directory with files is not the empty hiding mount: %v", got)
	}
}

func TestArgsAndRun(t *testing.T) {
	for _, e := range []Entry{{Path: "/a b/c"}, {Path: "/x", Dir: true}} {
		back, err := ParseArg(e.Arg())
		if err != nil || back != e {
			t.Errorf("%+v -> %q -> %+v %v", e, e.Arg(), back, err)
		}
	}
	if _, err := ParseArg("nonsense"); err == nil {
		t.Error("bad argument")
	}

	f := filepath.Join(t.TempDir(), "x")
	os.WriteFile(f, nil, 0o644)
	var out bytes.Buffer
	if code := Run([]string{"file:/dev/null", "file:" + f}, &out); code != 1 || out.String() != Marker+f+"\n" {
		t.Errorf("code %d out %q", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"file:/dev/null"}, &out); code != 0 || out.Len() != 0 {
		t.Errorf("code %d out %q", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"bogus"}, &out); code != 2 || !strings.Contains(out.String(), "bad canary") {
		t.Errorf("code %d out %q", code, out.String())
	}
}
