// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
)

const (
	defaultPids = 1024

	// Paths inside the container.
	ContainerBinary = "/aicatraz/aicatraz"
	ContainerBinDir = "/aicatraz/bin"
	ContainerSocket = "/aicatraz/broker.sock"

	// fallbackPath is used when the image doesn't define PATH.
	fallbackPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// defaultShell starts bash when the image has it and sh otherwise.
var defaultShell = []string{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"}

// RunInput is everything `docker run` needs. Building the arguments is a pure
// function of it, so the result can be printed and tested.
type RunInput struct {
	Cfg        *config.Config
	ID         string
	Name       string // container name
	Image      string
	UID, GID   int
	UserName   string
	Home       string // host home directory, same path inside the container
	Cwd        string // working directory inside the container
	Command    []string
	TTY        bool
	HostBinary string // this executable, mounted read-only
	StateDir   string
	HideIsDir  map[string]bool // hidden path -> is a directory
	SocketPath string          // the broker's socket on the host, mounted into the container
	ImagePath  string          // the image's own PATH ("" if it has none)
	ExtraArgs  []string        // more docker flags (an agent's mounts and environment)
	ExtraPath  []string        // directories added to PATH after the shim directory
	// NetworkContainer is the sidecar whose network the jail joins ("" for the
	// normal one). The jail then shares the sidecar's hostname and DNS too.
	NetworkContainer string
	// DeviceRules are the device-cgroup rules for the hardware the config names.
	DeviceRules []string
	// ScratchPrefix names the session's scratch volumes ("" means Name).
	ScratchPrefix string
}

// Scratch is one writable, disk-backed directory of the jail.
type Scratch struct {
	Suffix string // volume name suffix
	Path   func(in RunInput) string
	Mode   string // for chmod
}

// ScratchDirs are /tmp, /var/tmp and the home directory. They are docker
// volumes, not tmpfs, so they don't take RAM.
var ScratchDirs = []Scratch{
	{"tmp", func(RunInput) string { return "/tmp" }, "1777"},
	{"vartmp", func(RunInput) string { return "/var/tmp" }, "1777"},
	{"home", func(in RunInput) string { return in.Home }, "0700"},
}

// ScratchVolume is the name of the volume behind one scratch directory.
func (in RunInput) ScratchVolume(sc Scratch) string {
	p := in.ScratchPrefix
	if p == "" {
		p = in.Name
	}
	return p + "-" + sc.Suffix
}

func PasswdPath(stateDir string) string { return filepath.Join(stateDir, "passwd") }
func GroupPath(stateDir string) string  { return filepath.Join(stateDir, "group") }
func ShimDir(stateDir string) string    { return filepath.Join(stateDir, "bin") }

// RunArgs builds the `docker run` argument list for the jail.
func RunArgs(in RunInput) ([]string, error) {
	cfg := in.Cfg
	pids := cfg.Limits.Pids
	if pids == 0 {
		pids = defaultPids
	}

	args := []string{"run", "--rm", "--init", "-i"}
	if in.TTY {
		args = append(args, "-t")
	}
	args = append(args, "--name", in.Name)
	switch {
	case in.NetworkContainer != "":
		// The sidecar's network, and with it its hostname.
		args = append(args, "--network", "container:"+in.NetworkContainer)
	case cfg.NetworkMode() == config.NetOff:
		args = append(args, "--network", "none", "--hostname", hostname(in.ID))
	default:
		args = append(args, "--hostname", hostname(in.ID))
		// Published ports belong to the container that owns the network: with
		// rules that is the sidecar, which publishes them itself.
		for _, p := range cfg.PortMaps {
			args = append(args, "-p", p.DockerFlag())
		}
	}
	args = append(args,
		"--label", "aicatraz.project="+in.ID,
		"--user", fmt.Sprintf("%d:%d", in.UID, in.GID),
		"--cap-drop=ALL",
	)
	if cfg.Sudo {
		// sudo is a setuid program that needs a few capabilities, and
		// installing packages needs a writable root filesystem. The container
		// is removed on exit, so nothing written there survives.
		for _, c := range SudoCaps {
			args = append(args, "--cap-add="+c)
		}
	} else {
		args = append(args, "--security-opt=no-new-privileges", "--read-only")
	}
	args = append(args, "--pids-limit", strconv.Itoa(pids))
	if cfg.Limits.Memory != "" {
		args = append(args, "--memory", cfg.Limits.Memory)
	}

	// Writable scratch space; the root filesystem is read-only. The volumes
	// are created (and made writable for the user) before the jail starts and
	// removed when it ends. A volume, unlike a tmpfs, is not mounted noexec.
	// volume-nocopy stops docker from copying the image's (root-owned) mount
	// point over the ownership createScratch set.
	for _, sc := range ScratchDirs {
		args = append(args, "--mount", "type=volume,src="+in.ScratchVolume(sc)+",dst="+sc.Path(in)+",volume-nocopy")
	}
	args = append(args, "--tmpfs", "/run:rw,nosuid,nodev,size=16m,mode=1777")

	env := []string{
		"HOME=" + in.Home,
		"USER=" + in.UserName,
		"LOGNAME=" + in.UserName,
	}
	for _, k := range sortedKeys(cfg.Env.Static) {
		if k == "PATH" {
			continue // handled below: the shim directory always comes first
		}
		env = append(env, k+"="+cfg.Env.Static[k])
	}
	basePath := in.ImagePath
	if p, ok := cfg.Env.Static["PATH"]; ok {
		basePath = p
	}
	if basePath == "" {
		basePath = fallbackPath
	}
	// The shims shadow the real commands, so a call goes through the broker.
	env = append(env, "PATH="+strings.Join(append(append([]string{ContainerBinDir}, in.ExtraPath...), basePath), ":"))
	for _, e := range env {
		args = append(args, "-e", e)
	}
	// Name only: docker takes the value from our environment, so secrets never
	// show up in the process list or in --print output.
	if in.TTY {
		args = append(args, "-e", "TERM")
	}
	for _, name := range cfg.Env.FromHost {
		args = append(args, "-e", name)
	}

	mounts, err := volumeArgs(cfg.Resolved, in.HideIsDir)
	if err != nil {
		return nil, err
	}
	args = append(args, mounts...)
	if in.SocketPath != "" {
		// Only the socket file is mounted, read-only: the agent can't replace
		// or unlink it (connecting still works, sockets are exempt from ro).
		args = append(args, "--mount", bind(in.SocketPath, ContainerSocket, true))
	}
	args = append(args,
		"--mount", bind(in.HostBinary, ContainerBinary, true),
		"--mount", bind(ShimDir(in.StateDir), ContainerBinDir, true),
		"--mount", bind(PasswdPath(in.StateDir), "/etc/passwd", true),
		"--mount", bind(GroupPath(in.StateDir), "/etc/group", true),
	)

	for _, r := range in.DeviceRules {
		args = append(args, "--device-cgroup-rule", r)
	}
	args = append(args, in.ExtraArgs...)
	args = append(args, "-w", in.Cwd, "--entrypoint", "", in.Image)
	if len(in.Command) == 0 {
		return append(args, defaultShell...), nil
	}
	return append(args, in.Command...), nil
}

func bind(source, target string, readonly bool) string {
	s := "type=bind,source=" + source + ",target=" + target
	if readonly {
		s += ",readonly"
	}
	return s
}

// volumeArgs turns volume entries into mounts, parents before children.
func volumeArgs(vols []config.Volume, hideIsDir map[string]bool) ([]string, error) {
	sorted := append([]config.Volume(nil), vols...)
	// Parents before children, so the deepest entry wins; siblings by path, so
	// the result doesn't depend on the order in the config.
	sort.Slice(sorted, func(i, j int) bool {
		di, dj := strings.Count(sorted[i].Path, "/"), strings.Count(sorted[j].Path, "/")
		if di != dj {
			return di < dj
		}
		return sorted[i].Path < sorted[j].Path
	})

	var args []string
	for _, v := range sorted {
		switch v.Mode {
		case config.ModeRW:
			args = append(args, "--mount", bind(v.Path, v.Path, false))
		case config.ModeRO:
			args = append(args, "--mount", bind(v.Path, v.Path, true))
		case config.ModeHide:
			isDir, known := hideIsDir[v.Path]
			if !known {
				return nil, fmt.Errorf("hidden path %s was not inspected", v.Path)
			}
			if isDir {
				// An empty, read-only directory laid over the real one.
				args = append(args, "--tmpfs", v.Path+":ro,nosuid,nodev,noexec,size=4k,mode=0555")
			} else {
				// /dev/null over a file: reads as empty, can't be removed or renamed.
				args = append(args, "--mount", bind("/dev/null", v.Path, true))
			}
		}
	}
	return args, nil
}

// PickCwd chooses the container's working directory: the current directory if
// it is inside a visible volume, else the first rw volume, else the home.
func PickCwd(vols []config.Volume, cwd, home string) string {
	usable := false
	for _, v := range vols {
		within := cwd == v.Path || inside(v.Path, cwd)
		if v.Mode == config.ModeHide && within {
			usable = false // a hidden path is no place to work, whatever its parent is
			break
		}
		if within {
			usable = true
		}
	}
	if usable {
		return cwd
	}
	for _, v := range vols {
		if v.Mode == config.ModeRW {
			return v.Path
		}
	}
	return home
}

func inside(parent, child string) bool {
	return strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hostname keeps the container hostname within the 64-character limit.
func hostname(id string) string {
	h := "aicatraz-" + id
	if len(h) > 63 {
		h = h[:63]
	}
	return strings.TrimRight(h, "-.")
}
