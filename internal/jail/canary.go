// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/canary"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// ExitExposed is the launcher's exit code when a hidden path was exposed and
// the session was ended for it.
const ExitExposed = 70

// CanaryCommand is the hidden subcommand run inside the container.
const CanaryCommand = "__canary"

// exposedError says which hidden paths the jail can see.
type exposedError struct{ paths []string }

func (e *exposedError) Error() string {
	return fmt.Sprintf("hidden path %s is visible inside the jail", strings.Join(e.paths, ", "))
}

// watcher re-checks, from inside the container, that every hidden path is
// still hidden. See package canary for why this can stop being true.
type watcher struct {
	container string
	entries   []canary.Entry
	action    string // "stop" or "warn"
	interval  time.Duration
	broker    *broker.Broker
	out       io.Writer

	once    sync.Once
	mu      sync.Mutex
	tripped bool
}

func newWatcher(container string, hideIsDir map[string]bool, action string, interval time.Duration, b *broker.Broker, out io.Writer) *watcher {
	w := &watcher{container: container, action: action, interval: interval, broker: b, out: out}
	for p, isDir := range hideIsDir {
		w.entries = append(w.entries, canary.Entry{Path: p, Dir: isDir})
	}
	sort.Slice(w.entries, func(i, j int) bool { return w.entries[i].Path < w.entries[j].Path })
	return w
}

// check asks the container. Anything that isn't a clear "exposed" answer (the
// container isn't up yet, docker hiccups) is not treated as exposure.
func (w *watcher) check() error {
	if len(w.entries) == 0 {
		return nil
	}
	args := []string{"exec", w.container, ContainerBinary, CanaryCommand}
	for _, e := range w.entries {
		args = append(args, e.Arg())
	}
	out, _ := exec.Command("docker", args...).Output()
	var exposed []string
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, canary.Marker); ok {
			exposed = append(exposed, p)
		}
	}
	if len(exposed) == 0 {
		return nil
	}
	return &exposedError{paths: exposed}
}

// run checks on a timer until stop is closed.
func (w *watcher) run(stop <-chan struct{}) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := w.check(); err != nil {
				w.trip(err)
			}
		}
	}
}

// trip reacts to exposure, once: say so loudly, stop host commands, and (by
// default) end the session, because the agent may already have read the file.
func (w *watcher) trip(err error) {
	w.once.Do(func() {
		st := ui.For(w.out)
		fmt.Fprintln(w.out)
		fmt.Fprintln(w.out, st.BoldRed("!! aicatraz: a HIDDEN path is no longer hidden"))
		ui.Error(w.out, "%v", err)
		ui.Error(w.out, "It was probably replaced on the host, for example by an editor that saves by renaming a new file over the old one.")
		ui.Error(w.out, "Tip: edit hidden files in place, or hide a directory (such as .secrets/) instead of a single file.")
		if w.action == "stop" {
			w.broker.Freeze(err.Error())
			w.mu.Lock()
			w.tripped = true
			w.mu.Unlock()
			ui.Error(w.out, "The session is being stopped: the agent could have read the file.")
			_ = exec.Command("docker", "kill", w.container).Run()
		} else {
			ui.Error(w.out, "Continuing, because canary.action is warn.")
		}
	})
}

func (w *watcher) wasTripped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tripped
}
