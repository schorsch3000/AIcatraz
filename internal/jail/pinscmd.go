// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/pins"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

const pinsUsage = "usage: aicatraz pins [-c file] list\n" +
	"       aicatraz pins [-c file] diff [<path>...]\n" +
	"       aicatraz pins [-c file] forget <path>... | --missing | --all"

// Pins implements `aicatraz pins`: look at the files whose approved version
// the project remembers, and make it forget some. A forgotten pin only means
// the next run asks again, so nothing here needs a confirmation.
func Pins(args []string, stdout, stderr io.Writer) error {
	cfgPath := "aicatraz.yaml"
	var rest []string
	for len(args) > 0 {
		if (args[0] == "-c" || args[0] == "--config") && len(args) >= 2 {
			cfgPath = args[1]
			args = args[2:]
			continue
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
	if len(rest) == 0 {
		rest = []string{"list"}
	}

	_, id, err := loadForID(cfgPath)
	if err != nil {
		return err
	}
	state, err := StateDir(id)
	if err != nil {
		return err
	}
	dir := filepath.Join(state, "pins")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			if rest[0] == "list" || rest[0] == "forget" || rest[0] == "diff" {
				fmt.Fprintf(stdout, "no pins for %s\n", id)
				return nil
			}
		}
	}
	store, err := pins.Open(dir)
	if err != nil {
		return err
	}

	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			return errors.New(pinsUsage)
		}
		return listPins(store, id, stdout)
	case "diff":
		return diffPins(store, rest[1:], stdout)
	case "forget":
		return forgetPins(store, rest[1:], stdout)
	}
	return errors.New(pinsUsage)
}

// pinState compares a pin with the file as it is now.
func pinState(store *pins.Store, path string) (state string, pinned *pins.Snapshot, err error) {
	pinned, err = store.Get(path)
	if err != nil || pinned == nil {
		return "", nil, err
	}
	now, err := pins.Take(path)
	switch {
	case err != nil:
		return "unreadable", pinned, nil
	case !now.Exists:
		return "missing", pinned, nil
	case now.Sum != pinned.Sum:
		return "changed", pinned, nil
	}
	return "ok", pinned, nil
}

func listPins(store *pins.Store, id string, stdout io.Writer) error {
	paths, err := store.Paths()
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		fmt.Fprintf(stdout, "no pins for %s\n", id)
		return nil
	}
	st := ui.For(stdout)
	fmt.Fprintln(stdout, st.Dim(fmt.Sprintf("%-10s %-9s %7s  %-16s %s", "STATE", "SHA256", "SIZE", "APPROVED", "PATH")))
	changed := 0
	for _, p := range paths {
		state, pinned, err := pinState(store, p)
		if err != nil || pinned == nil {
			continue
		}
		label := fmt.Sprintf("%-10s", state)
		switch state {
		case "ok":
			label = st.Green(label)
		case "changed":
			label = st.BoldYellow(label)
			changed++
		default:
			label = st.Red(label)
			changed++
		}
		when := ""
		if t := store.Approved(p); !t.IsZero() {
			when = t.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(stdout, "%s %-9s %7s  %-16s %s\n", label, shortSum(pinned.Sum), humanSize(pinned.Size), when, p)
	}
	if changed > 0 {
		fmt.Fprintln(stdout, st.Dim("\nchanged or missing files ask for approval the next time a rule runs them"))
	}
	return nil
}

func forgetPins(store *pins.Store, args []string, stdout io.Writer) error {
	all, missing := false, false
	var wanted []string
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		case "--missing":
			missing = true
		default:
			if strings.HasPrefix(a, "-") {
				return errors.New(pinsUsage)
			}
			wanted = append(wanted, a)
		}
	}
	if !all && !missing && len(wanted) == 0 || all && (missing || len(wanted) > 0) {
		return errors.New(pinsUsage)
	}

	paths, err := store.Paths()
	if err != nil {
		return err
	}
	pinned := map[string]bool{}
	for _, p := range paths {
		pinned[p] = true
	}

	var targets []string
	switch {
	case all:
		targets = paths
	case missing:
		for _, p := range paths {
			if state, _, _ := pinState(store, p); state == "missing" {
				targets = append(targets, p)
			}
		}
	default:
		for _, w := range wanted {
			p := w
			if !pinned[p] {
				abs, err := filepath.Abs(w)
				if err != nil || !pinned[abs] {
					return fmt.Errorf("%s is not pinned (see `%s pins list`)", w, ui.Prog)
				}
				p = abs
			}
			targets = append(targets, p)
		}
	}

	st := ui.For(stdout)
	for _, p := range targets {
		if err := store.Forget(p); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s\n", st.BoldGreen("forgot"), p)
	}
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "nothing to forget")
	}
	return nil
}

func shortSum(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
}

// diffPins shows what changed in pinned files since they were approved:
// the files named, or every pin that has changed or is gone.
func diffPins(store *pins.Store, args []string, stdout io.Writer) error {
	paths, err := store.Paths()
	if err != nil {
		return err
	}
	pinned := map[string]bool{}
	for _, p := range paths {
		pinned[p] = true
	}
	explicit := len(args) > 0
	targets := paths
	if explicit {
		targets = nil
		for _, a := range args {
			p := a
			if !pinned[p] {
				abs, err := filepath.Abs(a)
				if err != nil || !pinned[abs] {
					return fmt.Errorf("%s is not pinned (see `%s pins list`)", a, ui.Prog)
				}
				p = abs
			}
			targets = append(targets, p)
		}
	}

	st := ui.For(stdout)
	shown := 0
	for _, p := range targets {
		state, snap, err := pinState(store, p)
		if err != nil || snap == nil {
			continue
		}
		if state == "ok" {
			if explicit {
				fmt.Fprintf(stdout, "%s: unchanged since it was approved\n", st.Bold(p))
				shown++
			}
			continue
		}
		now, _ := pins.Take(p)
		diff, note := pins.DiffFor(p, snap, now)
		fmt.Fprintf(stdout, "%s: %s\n", st.Bold(p), st.BoldYellow(state))
		switch {
		case diff != "":
			for _, l := range strings.Split(strings.TrimRight(approve.Sanitize(diff), "\n"), "\n") {
				fmt.Fprintln(stdout, "  "+colorDiffLine(st, l))
			}
		case note != "":
			fmt.Fprintln(stdout, "  "+st.Dim(note))
		default:
			fmt.Fprintln(stdout, "  "+st.Dim("same content, but the file is not the same one (a link)"))
		}
		fmt.Fprintln(stdout)
		shown++
	}
	if shown == 0 {
		fmt.Fprintln(stdout, "no pinned file has changed")
	}
	return nil
}

func colorDiffLine(st ui.Style, l string) string {
	switch {
	case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		return st.Bold(l)
	case strings.HasPrefix(l, "@@"):
		return st.Cyan(l)
	case strings.HasPrefix(l, "+"):
		return st.Green(l)
	case strings.HasPrefix(l, "-"):
		return st.Red(l)
	}
	return st.Dim(l)
}
