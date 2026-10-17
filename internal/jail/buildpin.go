// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/pins"
)

// The agent can edit Dockerfile.az, and the host builds it on the next
// launch. So the files that define the build are pinned like a rule's
// dependencies: if one is new or changed since you last approved it, the
// launcher shows the diff and asks before building.
//
// Pinned: the Dockerfile, the ignore files, and single files named by
// COPY/ADD. Directories copied wholesale (`COPY . /app`) are not pinned: that
// is the project's own code, which the agent legitimately edits.

// BuildFiles lists the files that define a build.
func BuildFiles(b Build) []string {
	files := map[string]bool{b.Dockerfile: true}
	for _, f := range ignoreFilesFor(b) {
		files[f] = true
	}
	if data, err := os.ReadFile(b.Dockerfile); err == nil && len(data) < 1<<20 {
		for _, src := range copySources(string(data)) {
			p := filepath.Join(b.Context, src)
			if rel, err := filepath.Rel(b.Context, p); err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				files[p] = true
			}
		}
	}
	out := make([]string, 0, len(files))
	for f := range files {
		out = append(out, f)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var instruction = regexp.MustCompile(`(?i)^(COPY|ADD)\s+(.*)$`)

// copySources returns the source paths named by COPY and ADD instructions,
// without flags, URLs, wildcards, variables, or copies from other stages.
func copySources(dockerfile string) []string {
	// Join continuation lines.
	text := strings.ReplaceAll(dockerfile, "\\\r\n", " ")
	text = strings.ReplaceAll(text, "\\\n", " ")

	var out []string
	for _, line := range strings.Split(text, "\n") {
		m := instruction.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		var args []string
		rest := strings.TrimSpace(m[2])
		if strings.HasPrefix(rest, "[") {
			if json.Unmarshal([]byte(rest), &args) != nil {
				continue
			}
		} else {
			args = strings.Fields(rest)
		}
		var srcs []string
		fromStage := false
		for _, a := range args {
			if strings.HasPrefix(a, "--") {
				if strings.HasPrefix(a, "--from") {
					fromStage = true
				}
				continue
			}
			srcs = append(srcs, a)
		}
		if fromStage || len(srcs) < 2 {
			continue
		}
		for _, s := range srcs[:len(srcs)-1] {
			if strings.Contains(s, "://") || strings.ContainsAny(s, "*?[$") || filepath.IsAbs(s) {
				continue
			}
			out = append(out, filepath.Clean(s))
		}
	}
	return out
}

// ApproveBuild checks the build files against the pin store and, if anything
// is new or changed, asks before the build goes ahead. approveAll pins
// without asking (--approve-build, for non-interactive use).
func ApproveBuild(store *pins.Store, b Build, approveAll bool, prompt func(approve.Request) (approve.Answer, error)) error {
	rep, err := pins.Check(store, BuildFiles(b), nil)
	if err != nil {
		return fmt.Errorf("checking the build files: %w", err)
	}
	if !rep.Dirty() {
		return nil
	}
	pinAll := func() error {
		for _, it := range rep.Attention() {
			if err := store.Put(it.Now); err != nil {
				return err
			}
		}
		return nil
	}
	if approveAll {
		return pinAll()
	}
	if prompt == nil {
		return fmt.Errorf("build files changed since they were last approved (%s); run `aicatraz up` or `aicatraz run` in a terminal to review them, or pass --approve-build",
			rep.Summary())
	}

	req := approve.Request{Kind: approve.KindBuild, Name: "docker build", Args: nil, Cwd: b.Context, CanPin: true}
	for _, it := range rep.Attention() {
		req.Files = append(req.Files, approve.FileChange{Path: it.Path, Status: it.Status.String(), Diff: it.Diff, Note: it.Note})
	}
	ans, err := prompt(req)
	switch {
	case err != nil:
		return fmt.Errorf("build not approved: %w", err)
	case ans == approve.AllowPin:
		return pinAll()
	case ans == approve.AllowOnce:
		return nil
	}
	return errors.New("build not approved")
}

// terminalPrompt shows the build prompt on the launcher's own terminal. At this
// point no agent is running yet, so the terminal is safe to use.
func terminalPrompt(in *os.File, out *os.File, cfg *config.Config) func(approve.Request) (approve.Answer, error) {
	if !isTerminal(in) || !isTerminal(out) {
		return nil
	}
	return func(req approve.Request) (approve.Answer, error) {
		return approve.Prompt(context.Background(), in, out, req, cfg.Approval.TimeoutOrDefault())
	}
}
