package broker

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/pins"
	"github.com/schorsch3000/aicatraz/internal/proto"
)

// depState is the result of checking a rule's dependent files for one call.
type depState struct {
	checked bool // the rule has depends_on
	tooMany bool // too many files to pin: always needs attention, can't be pinned
	report  pins.Report
}

func (d depState) attention() bool { return d.checked && (d.tooMany || d.report.Dirty()) }
func (d depState) pinnable() bool  { return d.checked && !d.tooMany }

func (d depState) summary() string {
	if d.tooMany {
		return "too many dependent files to pin"
	}
	return d.report.Summary()
}

func (d depState) fingerprint() string {
	if d.tooMany {
		return "too-many"
	}
	return d.report.Fingerprint()
}

// changes describes what needs the human's attention.
func (d depState) changes() []approve.FileChange {
	if d.tooMany {
		return []approve.FileChange{{
			Path:   "(files this rule depends on)",
			Status: fmt.Sprintf("more than %d files, too many to pin: every call needs approval", pins.MaxFiles),
		}}
	}
	var out []approve.FileChange
	for _, it := range d.report.Attention() {
		out = append(out, approve.FileChange{Path: it.Path, Status: it.Status.String(), Diff: it.Diff, Note: it.Note})
	}
	return out
}

// pin remembers the versions that were shown to the human. They come from the
// snapshots taken for the prompt, not from a fresh read, so what is pinned is
// what was approved.
func (d depState) pin(store *pins.Store) error {
	for _, it := range d.report.Attention() {
		if err := store.Put(it.Now); err != nil {
			return err
		}
	}
	return nil
}

func specOf(r *config.Rule) pins.Spec {
	return pins.Spec{Auto: r.DependsOn.Auto, Patterns: r.DependsOn.Patterns}
}

// checkDeps resolves and checks the files the matching rule depends on.
func (b *Broker) checkDeps(rule *config.Rule, h *proto.Hello, cwd string) (depState, error) {
	if rule == nil || !rule.DependsOn.IsSet() {
		return depState{}, nil
	}
	if b.opts.Pins == nil {
		return depState{}, errors.New("this rule depends on files, but there is no pin store to check them against")
	}
	res, err := pins.Resolve(specOf(rule), h.Name, h.Args, cwd)
	if errors.Is(err, pins.ErrTooMany) {
		return depState{checked: true, tooMany: true}, nil
	}
	if err != nil {
		return depState{}, fmt.Errorf("cannot resolve the files this rule depends on: %w", err)
	}
	for _, f := range res.Files {
		real, err := filepath.EvalSymlinks(f)
		if err != nil {
			return depState{}, fmt.Errorf("cannot resolve %s: %w", f, err)
		}
		if _, bad := underAny(b.hidden, real); bad {
			return depState{}, fmt.Errorf("a file this rule depends on is inside a hidden path: %s", f)
		}
		if _, ok := underAny(b.visible, real); !ok {
			return depState{}, fmt.Errorf("a file this rule depends on is outside the mounted volumes: %s", f)
		}
	}
	rep, err := pins.Check(b.opts.Pins, res.Files, res.Matchers)
	if err != nil {
		return depState{}, fmt.Errorf("cannot check the files this rule depends on: %w", err)
	}
	return depState{checked: true, report: rep}, nil
}

// recheckDeps runs just before the command starts and fails if the files are
// no longer what checkDeps saw.
func (b *Broker) recheckDeps(rule *config.Rule, h *proto.Hello, cwd string, before depState) error {
	if !before.checked || before.tooMany {
		return nil
	}
	now, err := b.checkDeps(rule, h, cwd)
	if err != nil {
		return err
	}
	if now.fingerprint() != before.fingerprint() {
		return errors.New("files this command depends on changed while the request was being processed")
	}
	return nil
}
