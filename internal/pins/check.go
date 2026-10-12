package pins

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Status of one dependent file compared with its pin.
type Status int

const (
	OK      Status = iota // same as the pinned version
	New                   // never approved before
	Changed               // differs from the pinned version
	Gone                  // pinned, but missing now
)

func (s Status) String() string {
	switch s {
	case New:
		return "never approved before"
	case Changed:
		return "changed since last approved"
	case Gone:
		return "deleted since last approved"
	}
	return "unchanged"
}

// Item is one file's result.
type Item struct {
	Path   string
	Status Status
	Now    Snapshot  // the file as it is now (zero if Gone)
	Pinned *Snapshot // the approved version, nil if New
	Diff   string    // unified diff, empty if there is none to show
	Note   string    // why there is no diff, or extra context
}

// Report is the result of checking a set of files.
type Report struct {
	Items []Item
}

// Dirty reports whether anything needs the human's attention.
func (r Report) Dirty() bool {
	for _, it := range r.Items {
		if it.Status != OK {
			return true
		}
	}
	return false
}

// Attention returns the items that are not OK.
func (r Report) Attention() []Item {
	var out []Item
	for _, it := range r.Items {
		if it.Status != OK {
			out = append(out, it)
		}
	}
	return out
}

// Fingerprint identifies the exact contents of all files in the report. A
// session grant is only valid for the fingerprint it was given for.
func (r Report) Fingerprint() string {
	var parts []string
	for _, it := range r.Items {
		parts = append(parts, it.Path+"="+it.Now.Sum)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// Summary is a one-line description for logs and messages.
func (r Report) Summary() string {
	var parts []string
	for _, it := range r.Attention() {
		parts = append(parts, fmt.Sprintf("%s: %s", filepath.Base(it.Path), it.Status))
	}
	return strings.Join(parts, "; ")
}

// Snapshots returns the current state of every file in the report.
func (r Report) Snapshots() []Snapshot {
	out := make([]Snapshot, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it.Now)
	}
	return out
}

// Check takes snapshots of files and compares them with the store. patterns
// (absolute globs) are used to notice pinned files that have disappeared.
func Check(store *Store, files []string, patterns []Matcher) (Report, error) {
	var rep Report
	seen := map[string]bool{}
	for _, path := range files {
		seen[path] = true
		now, err := Take(path)
		if err != nil {
			return rep, err
		}
		pinned, err := store.Get(path)
		if err != nil {
			return rep, err
		}
		rep.Items = append(rep.Items, compare(path, now, pinned))
	}

	pinnedPaths, err := store.Paths()
	if err != nil {
		return rep, err
	}
	for _, p := range pinnedPaths {
		if seen[p] || !matchesAny(patterns, p) {
			continue
		}
		now, err := Take(p)
		if err != nil {
			return rep, err
		}
		if now.Exists {
			continue // still there but not expanded (e.g. no longer a regular file): treat via files list
		}
		pinned, err := store.Get(p)
		if err != nil || pinned == nil {
			continue
		}
		rep.Items = append(rep.Items, compare(p, now, pinned))
	}
	sort.Slice(rep.Items, func(i, j int) bool { return rep.Items[i].Path < rep.Items[j].Path })
	return rep, nil
}

func compare(path string, now Snapshot, pinned *Snapshot) Item {
	it := Item{Path: path, Now: now, Pinned: pinned}
	switch {
	case pinned == nil:
		it.Status = New
		it.Diff, it.Note = diffOf(path, nil, now.Content, now.Size, 0)
	case !now.Exists:
		it.Status = Gone
		it.Diff, it.Note = diffOf(path, pinned.Content, nil, 0, pinned.Size)
	case now.Sum != pinned.Sum:
		it.Status = Changed
		it.Diff, it.Note = diffOf(path, pinned.Content, now.Content, now.Size, pinned.Size)
	case now.Real != pinned.Real:
		it.Status = Changed
		it.Note = fmt.Sprintf("now a link to %s (was %s), same content", now.Real, pinned.Real)
	default:
		it.Status = OK
	}
	return it
}

// diffOf makes a unified diff of two versions, or says why it can't.
func diffOf(path string, old, now []byte, nowSize, oldSize int64) (diff, note string) {
	if (old == nil && oldSize > 0) || (now == nil && nowSize > 0) {
		return "", fmt.Sprintf("too large to show a diff (%d bytes before, %d now)", oldSize, nowSize)
	}
	if bytes.IndexByte(old, 0) >= 0 || bytes.IndexByte(now, 0) >= 0 {
		return "", "binary file"
	}
	ud := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(old)),
		B:        difflib.SplitLines(string(now)),
		FromFile: "approved: " + filepath.Base(path),
		ToFile:   "now: " + filepath.Base(path),
		Context:  2,
	}
	text, err := difflib.GetUnifiedDiffString(ud)
	if err != nil {
		return "", "diff failed: " + err.Error()
	}
	return text, ""
}
