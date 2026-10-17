// SPDX-License-Identifier: GPL-3.0-only

// Package agent knows how to run coding agents (Claude Code, opencode) inside
// the jail: where they are installed, which of the user's configuration they
// get, how they are started, and how their sessions are tracked.
package agent

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Mode says how a config entry gets into the jail.
type Mode int

const (
	// Sync entries are copied from the host on every launch and overwrite what
	// the jail has: changes to your own config carry over, anything the agent
	// changed in them is discarded. An entry that no longer exists on the host
	// is removed from the jail.
	Sync Mode = iota
	// Seed entries are copied only if the jail doesn't have them yet.
	Seed
)

// Entry is one file or directory of host configuration.
type Entry struct {
	Mode Mode
	// Name is the path inside the root (and inside the host's copy of it).
	Name string
	// HostPath, if set, is where the entry lives on the host instead, relative
	// to the home directory (for files that sit next to the root, not in it).
	HostPath string
	// Exclude lists base names that are not copied from a directory.
	Exclude []string
}

// Root is a directory the agent keeps its configuration and state in. The same
// path exists in the container, backed by a per-project directory on the host,
// so the agent never touches your real files. Everything in it that is not a
// Sync/Seed entry is the agent's own state and survives between launches.
type Root struct {
	Path    string // relative to the home directory
	Entries []Entry
}

// Profile describes an agent.
type Profile struct {
	Name    string
	Binary  string // executable name
	Roots   []Root
	Install Install
}

// Install says how to fetch the agent into the tool cache.
type Install struct {
	URL string // official installer script
	// VersionArgs turns a version into arguments for the installer.
	VersionArgs func(version string) string
	// BinaryPath is where the installer puts the executable, relative to $HOME.
	BinaryPath string
}

// LaunchOptions shape the agent's command line.
type LaunchOptions struct {
	SkipPermissions bool
	SessionID       string   // start with this session id (agents that support it)
	Resume          string   // continue this session
	Args            []string // passed through
}

var profiles = map[string]*Profile{
	"claude": {
		Name:   "claude",
		Binary: "claude",
		Roots: []Root{{
			Path: ".claude",
			Entries: []Entry{
				{Name: "settings.json"},
				{Name: "CLAUDE.md"},
				{Name: "keybindings.json"},
				{Name: ".credentials.json"},
				{Name: ".claude.json", HostPath: ".claude.json"}, // lives next to .claude on the host, inside it for the jail (CLAUDE_CONFIG_DIR)
				{Name: "skills"},
				{Name: "agents"},
				{Name: "commands"},
				{Name: "plugins"},
			},
		}},
		Install: Install{
			URL: "https://claude.ai/install.sh",
			VersionArgs: func(v string) string {
				if v == "" || v == "latest" {
					return ""
				}
				return v
			},
			BinaryPath: ".local/bin/claude",
		},
	},
	"opencode": {
		Name:   "opencode",
		Binary: "opencode",
		Roots: []Root{
			{Path: ".config/opencode", Entries: []Entry{
				{Name: "opencode.json"},
				{Name: "opencode.jsonc"},
				{Name: "AGENTS.md"},
				{Name: "agents"},
				{Name: "commands"},
				{Name: "plugins"},
				{Name: "skills"},
				{Name: "tool"},
				{Name: "themes"},
			}},
			{Path: ".local/share/opencode", Entries: []Entry{
				{Name: "auth.json"},
				{Name: "mcp-auth.json"},
			}},
			{Path: ".local/state/opencode", Entries: []Entry{
				{Name: "kv.json", Mode: Seed},
			}},
		},
		Install: Install{
			URL: "https://opencode.ai/install",
			VersionArgs: func(v string) string {
				if v == "" || v == "latest" {
					return ""
				}
				return "--version " + v
			},
			BinaryPath: ".opencode/bin/opencode",
		},
	},
}

// Lookup returns the profile for an agent name.
func Lookup(name string) (*Profile, error) {
	if p, ok := profiles[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown agent %q (known: %s)", name, strings.Join(Names(), ", "))
}

// Names lists the known agents.
func Names() []string {
	var n []string
	for k := range profiles {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Command returns the agent's argv and the environment it needs inside the
// jail. home is the home directory (the same path on host and in the container).
func (p *Profile) Command(home string, o LaunchOptions) (argv []string, env map[string]string) {
	argv = []string{p.Binary}
	env = map[string]string{}
	switch p.Name {
	case "claude":
		env["CLAUDE_CONFIG_DIR"] = path.Join(home, ".claude")
		env["DISABLE_AUTOUPDATER"] = "1"
		if o.SkipPermissions {
			// The flag alone still shows a confirmation dialog on every start. It is
			// skipped when skipDangerousModePermissionPrompt is set in any settings
			// layer; passing it on the command line leaves your settings.json alone.
			argv = append(argv, "--dangerously-skip-permissions", "--settings", `{"skipDangerousModePermissionPrompt":true}`)
		}
		switch {
		case o.Resume != "":
			argv = append(argv, "--resume", o.Resume)
		case o.SessionID != "":
			argv = append(argv, "--session-id", o.SessionID)
		}
	case "opencode":
		env["OPENCODE_DISABLE_AUTOUPDATE"] = "true"
		if o.SkipPermissions {
			env["OPENCODE_PERMISSION"] = `{"*":"allow"}`
		}
		if o.Resume != "" {
			argv = append(argv, "--session", o.Resume)
		}
	}
	return append(argv, o.Args...), env
}

// SupportsSessionID reports whether the agent can be told its session id up front.
func (p *Profile) SupportsSessionID() bool { return p.Name == "claude" }

// ParentDirs returns the directories between home and each root (exclusive of
// the root), which the jail has to create as user-owned directories so that
// tools can write next to the mounted root (for example ~/.config/git).
func (p *Profile) ParentDirs() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range p.Roots {
		dir := path.Dir(r.Path)
		for dir != "." && dir != "/" {
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
			dir = path.Dir(dir)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Count(out[i], "/") < strings.Count(out[j], "/") || (strings.Count(out[i], "/") == strings.Count(out[j], "/") && out[i] < out[j])
	})
	return out
}
