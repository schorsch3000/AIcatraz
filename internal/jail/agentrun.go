// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/schorsch3000/aicatraz/internal/agent"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// ContainerTools is where the agent's executable directory is mounted.
const ContainerTools = "/aicatraz/tools"

// agentRun is everything specific to starting a coding agent in the jail.
type agentRun struct {
	profile   *agent.Profile
	version   string
	toolDir   string   // host directory with the executable (read-only in the jail)
	extraArgs []string // docker flags: tool mount, config roots, env
	command   []string
	sessionID string // the id given to a fresh session (agents that take one)
	resume    string
	state     string // the project's state directory
	home      string
	roots     map[string]string // root path -> its directory on the host
}

// planAgent works out how to start the agent. With dry set nothing is
// installed or copied; the result is only good for --print.
func planAgent(o Options, cfg *config.Config, home, state string, uid, gid int, dry bool) (*agentRun, error) {
	name := o.Agent
	if name == "@config" {
		if name = cfg.Agent.Tool; name == "" {
			return nil, fmt.Errorf("no agent configured: set agent.tool in the config, or start one with `aicatraz claude` or `aicatraz opencode`")
		}
	}
	prof, err := agent.Lookup(name)
	if err != nil {
		return nil, err
	}
	if len(o.Command) > 0 {
		return nil, fmt.Errorf("a command and an agent can't both be given; pass agent arguments after `--` to `aicatraz %s`", name)
	}

	a := &agentRun{profile: prof, state: state, home: home, resume: o.Resume, roots: map[string]string{}}
	version := ""
	if cfg.Agent.Tool == name {
		version = cfg.Agent.Version
	}
	if a.resume != "" && !agent.ValidID(a.resume) {
		return nil, fmt.Errorf("invalid session id %q", a.resume)
	}

	cache, err := agent.CacheDir()
	if err != nil {
		return nil, err
	}
	dir, resolved, err := agent.Resolve(cache, prof, version)
	if err != nil {
		if !isNotInstalled(err) {
			return nil, err
		}
		if dry {
			dir, resolved = filepath.Join(cache, prof.Name, "<not installed yet>"), "<not installed yet>"
		} else {
			ui.Note(o.Stderr, "%s is not installed yet; installing it (first use only)", prof.Name)
			if _, err := agent.InstallInContainer(cache, prof, version, uid, gid, o.Stderr); err != nil {
				return nil, fmt.Errorf("%w\n(if %s is installed on this machine, `aicatraz tools install %s --from-host` copies it instead)", err, prof.Name, prof.Name)
			}
			if dir, resolved, err = agent.Resolve(cache, prof, version); err != nil {
				return nil, err
			}
		}
	}
	a.toolDir, a.version = dir, resolved

	lo := agent.LaunchOptions{SkipPermissions: cfg.Agent.SkipPermissionsOrDefault(), Resume: a.resume, Args: o.AgentArgs}
	if a.resume == "" && prof.SupportsSessionID() {
		a.sessionID = agent.NewSessionID()
		lo.SessionID = a.sessionID
	}
	argv, env := prof.Command(home, lo)
	a.command = argv

	a.extraArgs = append(a.extraArgs, "--mount", bind(dir, ContainerTools, true))
	for _, r := range prof.Roots {
		host := filepath.Join(state, "agents", prof.Name, strings.NewReplacer("/", "_", ".", "").Replace(r.Path))
		a.roots[r.Path] = host
		a.extraArgs = append(a.extraArgs, "--mount", bind(host, filepath.Join(home, r.Path), false))
	}
	for _, p := range prof.ParentDirs() {
		// Parents of the mounted roots (~/.config, ~/.local/share, ...) are
		// created owned by the user, so tools can write next to the roots.
		a.extraArgs = append(a.extraArgs, "--tmpfs",
			fmt.Sprintf("%s:rw,exec,nosuid,nodev,size=64m,mode=0755,uid=%d,gid=%d", filepath.Join(home, p), uid, gid))
	}
	for _, k := range sortedKeys(env) {
		a.extraArgs = append(a.extraArgs, "-e", k+"="+env[k])
	}
	return a, nil
}

func isNotInstalled(err error) bool {
	for ; err != nil; err = unwrap(err) {
		if err == agent.ErrNotInstalled {
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}

// prepare creates the per-project config directories and copies the user's
// configuration into them (Sync entries overwrite, so changes carry over).
func (a *agentRun) prepare(image, cwd string) error {
	// The agents are dynamically linked against glibc. On Alpine (musl) the
	// kernel's "no such file" for the missing loader is all one would see.
	if !imageHasFile("docker", image, "/lib64/ld-linux-x86-64.so.2") && !imageHasFile("docker", image, "/lib/ld-linux-aarch64.so.1") {
		return fmt.Errorf("the image %s has no glibc dynamic loader, so %s can't run in it (is it Alpine or musl-based?); use a Debian- or Ubuntu-based image", image, a.profile.Name)
	}
	for _, r := range a.profile.Roots {
		if err := agent.SyncRoot(a.home, r, a.roots[r.Path]); err != nil {
			return fmt.Errorf("copying your %s configuration (%s): %w", a.profile.Name, r.Path, err)
		}
	}
	if a.profile.Name == "claude" {
		if err := agent.TrustClaudeDir(a.roots[".claude"], cwd); err != nil {
			return fmt.Errorf("marking %s as trusted for claude: %w", cwd, err)
		}
	}
	return nil
}

// apply adds the agent to the container run.
func (a *agentRun) apply(in *RunInput) {
	in.ExtraArgs = append(in.ExtraArgs, a.extraArgs...)
	in.ExtraPath = append(in.ExtraPath, ContainerTools)
	in.Command = a.command
}

// finish records the session the run produced so `aicatraz resume` can find it.
func (a *agentRun) finish(in RunInput, started time.Time, exitCode int, o Options) {
	id, title := a.resume, ""
	switch a.profile.Name {
	case "claude":
		if id == "" {
			id = a.sessionID
		}
		cfgDir := a.roots[".claude"]
		if id == "" || !agent.ClaudeSessionExists(cfgDir, id) {
			return // nothing was said, so there is nothing to resume
		}
	case "opencode":
		found, err := a.newestOpencodeSession(in, started)
		if err != nil {
			ui.Error(o.Stderr, "could not look up the opencode session: %v", err)
			return
		}
		if found == nil {
			return
		}
		id, title = found.ID, found.Title
	}
	if id == "" {
		return
	}
	err := agent.OpenStore(a.state).Add(agent.Session{
		Tool: a.profile.Name, ID: id, Title: title, Started: started, Ended: time.Now(),
		ExitCode: exitCode, Image: in.Image, Directory: in.Cwd,
	})
	if err != nil {
		ui.Error(o.Stderr, "could not record the session: %v", err)
		return
	}
	fmt.Fprintln(o.Stderr)
	ui.Good(o.Stderr, "session %s saved; continue it with `aicatraz resume`", id)
}

// newestOpencodeSession asks opencode, in a throwaway container with the same
// mounts, which session it just wrote. opencode keeps sessions in a database,
// so its own CLI is the reliable way to read them.
func (a *agentRun) newestOpencodeSession(in RunInput, started time.Time) (*agent.OpencodeSession, error) {
	q := in
	q.Name += "-ls"
	q.TTY = false
	q.SocketPath = ""
	q.Command = []string{"opencode", "session", "list", "--format", "json", "-n", "20"}
	args, err := RunArgs(q)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	list, err := agent.ParseOpencodeSessions(out.Bytes())
	if err != nil {
		return nil, err
	}
	// Clock skew between the container's database and this process is possible; allow a little.
	if s, ok := agent.PickOpencodeSession(list, in.Cwd, started.Add(-5*time.Second)); ok {
		return &s, nil
	}
	return nil, nil
}

// ListSessions writes the project's recorded sessions.
func ListSessions(configPath string, w io.Writer) error {
	_, id, err := loadForID(configPath)
	if err != nil {
		return err
	}
	state, err := StateDir(id)
	if err != nil {
		return err
	}
	list, err := agent.OpenStore(state).List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "no sessions recorded for this project yet")
		return nil
	}
	st := ui.For(w)
	for _, s := range list {
		title := s.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		// Plain columns when not a terminal: `resume --pick` parses this output.
		fmt.Fprintf(w, "%s  %s  %s  %s\n", st.Dim(s.Ended.Local().Format("2006-01-02 15:04")),
			st.Cyan(fmt.Sprintf("%-8s", s.Tool)), st.Yellow(s.ID), title)
	}
	return nil
}

// FindSession picks the session `resume` should continue: the given id, else
// the most recent one.
func FindSession(configPath, id string) (*agent.Session, error) {
	_, pid, err := loadForID(configPath)
	if err != nil {
		return nil, err
	}
	state, err := StateDir(pid)
	if err != nil {
		return nil, err
	}
	store := agent.OpenStore(state)
	if id != "" {
		s, err := store.Find(id)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, fmt.Errorf("no recorded session %q (see `aicatraz sessions`)", id)
		}
		return s, nil
	}
	list, err := store.List()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no sessions recorded for this project yet")
	}
	return &list[0], nil
}

// Tools manages the tool cache.
func Tools(args []string, stdout, stderr io.Writer) error {
	cache, err := agent.CacheDir()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		list, err := agent.List(cache)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintf(stdout, "nothing installed (cache: %s)\n", cache)
		}
		for _, t := range list {
			st := ui.For(stdout)
			mark := " "
			if t.Current {
				mark = st.BoldGreen("*")
			}
			fmt.Fprintf(stdout, "%s %s %s\n", mark, st.Cyan(fmt.Sprintf("%-9s", t.Tool)), t.Version)
		}
		return nil
	case "install", "update":
		fromHost := false
		var rest []string
		for _, a := range args[1:] {
			if a == "--from-host" {
				fromHost = true
			} else {
				rest = append(rest, a)
			}
		}
		names := agent.Names()
		version := ""
		if args[0] == "install" {
			if len(rest) == 0 {
				return fmt.Errorf("usage: aicatraz tools install <%s> [version] [--from-host]", strings.Join(names, "|"))
			}
			names = rest[:1]
			if len(rest) > 1 {
				version = rest[1]
			}
		} else if len(rest) > 0 {
			names = rest
		}
		uid, gid := os.Getuid(), os.Getgid()
		for _, n := range names {
			p, err := agent.Lookup(n)
			if err != nil {
				return err
			}
			if args[0] == "update" {
				if _, _, err := agent.Resolve(cache, p, ""); err != nil && len(rest) == 0 {
					continue // `update` without names only touches what is installed
				}
			}
			var v string
			if fromHost {
				v, err = agent.InstallFromHost(cache, p)
			} else {
				v, err = agent.InstallInContainer(cache, p, version, uid, gid, stderr)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s %s %s\n", ui.For(stdout).Cyan(n), v, ui.For(stdout).BoldGreen("installed"))
		}
		return nil
	}
	return fmt.Errorf("usage: aicatraz tools [list | install <tool> [version] [--from-host] | update [tool...]]")
}
