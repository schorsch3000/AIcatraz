// SPDX-License-Identifier: GPL-3.0-only

package devices

import (
	"context"
	"sync"
	"time"
)

// Ops makes and removes device nodes in the jail.
type Ops interface {
	Add(ctx context.Context, n Node) error
	Remove(ctx context.Context, n Node) error
}

// Event is something the watcher did, for the log.
type Event struct {
	Kind string // attached, detached, error
	Node Node
	Err  error
}

// Watcher keeps the nodes in the jail the same as the matching devices of the
// host: a device that appears is added, one that disappears is removed, and
// one that comes back under another name (ttyACM0 becomes ttyACM1 after a
// reset) is the old node removed and the new one added.
type Watcher struct {
	Specs []Spec
	Scan  func(specs []Spec) []Node
	Ops   Ops
	Log   func(Event)

	mu      sync.Mutex
	applied map[string]Node
	failed  map[string]string // the last error told for a path, so a retry is not told again
}

func (w *Watcher) log(e Event) {
	if w.Log != nil {
		w.Log(e)
	}
}

// Tick makes one pass: it compares the host with what the jail has and
// fixes the differences. A step that fails (the jail is not up yet) is tried
// again on the next pass.
func (w *Watcher) Tick(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.applied == nil {
		w.applied = map[string]Node{}
		w.failed = map[string]string{}
	}
	current := map[string]Node{}
	for _, n := range w.Scan(w.Specs) {
		if !Reserved(n.Path) {
			current[n.Path] = n
		}
	}

	for path, have := range w.applied {
		if now, ok := current[path]; ok && now.Same(have) {
			continue
		}
		if err := w.Ops.Remove(ctx, have); err != nil {
			w.fail(path, err, have)
			continue
		}
		delete(w.applied, path)
		w.log(Event{Kind: "detached", Node: have})
	}
	for path, n := range current {
		if _, ok := w.applied[path]; ok {
			continue
		}
		if err := w.Ops.Add(ctx, n); err != nil {
			w.fail(path, err, n)
			continue
		}
		w.applied[path] = n
		delete(w.failed, path)
		w.log(Event{Kind: "attached", Node: n})
	}
}

func (w *Watcher) fail(path string, err error, n Node) {
	if w.failed[path] != err.Error() {
		w.failed[path] = err.Error()
		w.log(Event{Kind: "error", Node: n, Err: err})
	}
}

// Run ticks until ctx ends: quickly for the first seconds, so what is plugged
// in at the start is in the jail as soon as it runs, then calmly.
func (w *Watcher) Run(ctx context.Context) {
	start := time.Now()
	for {
		w.Tick(ctx)
		wait := 300 * time.Millisecond
		if time.Since(start) < 10*time.Second {
			wait = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
