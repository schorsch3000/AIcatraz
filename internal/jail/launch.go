// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/devices"
	"github.com/schorsch3000/aicatraz/internal/mux"
	"github.com/schorsch3000/aicatraz/internal/pins"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// Options for Launch.
type Options struct {
	ConfigPath   string
	Command      []string // run this instead of an interactive shell
	Print        bool     // show what would run, change nothing
	ApprovalTTY  string   // prompt for `ask` rules on this terminal device instead of tmux
	ApprovalSock string   // prompt for `ask` rules through this session socket (set by `up`)
	Tmux         bool     // `up` uses tmux instead of the built-in multiplexer
	InfoKey      string   // the key that shows the session's settings, such as "ctrl-q"; "off" for none
	DetachKey    string   // the key that detaches from an `up` session, such as "ctrl-y"
	StatusBar    string   // `up`'s status bar: bottom (default) or off
	BarTheme     string   // color theme of the bar
	BarFG        string   // text color of the bar
	BarBG        string   // background color of the bar
	ApproveBuild bool     // pin changed build files without asking
	Agent        string   // start this coding agent ("@config" for the configured one)
	AgentArgs    []string // arguments for the agent
	Resume       string   // continue this agent session
	Stdout       io.Writer
	Stderr       io.Writer
}

// Launch builds the image if needed and runs the jail. It returns the exit
// code of the command inside the container.
func Launch(o Options) (int, error) {
	cfg, problems, err := config.Load(o.ConfigPath, config.Options{})
	if err != nil {
		return 0, err
	}
	for _, p := range problems {
		fmt.Fprintln(o.Stderr, p.FormatStyled(o.ConfigPath, ui.For(o.Stderr)))
	}
	if config.HasErrors(problems) {
		return 0, errors.New("config has errors")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	cur, err := user.Current()
	if err != nil {
		return 0, err
	}
	uid, _ := strconv.Atoi(cur.Uid)
	gid, _ := strconv.Atoi(cur.Gid)

	cfgAbs, err := filepath.Abs(o.ConfigPath)
	if err != nil {
		return 0, err
	}
	cfgDir := filepath.Dir(cfgAbs)
	id, err := ProjectID(o.ConfigPath, cfg)
	if err != nil {
		return 0, err
	}
	state, err := StateDir(id)
	if err != nil {
		return 0, err
	}
	recordProject(state, cfgAbs)
	img, err := ResolveImage(cfg, cfgDir, id)
	if err != nil {
		return 0, err
	}

	baseRef := img.Ref // what the build produces, or the image as given
	if cfg.Sudo {
		img.Ref = SudoRef(id) // the container runs the image with sudo added
	}

	hideIsDir, err := inspectVolumes(cfg.Resolved)
	if err != nil {
		return 0, err
	}
	if img.Build != nil {
		var hidden []string
		for p := range hideIsDir {
			hidden = append(hidden, p)
		}
		var ask func(IgnoreGap) bool
		if !o.Print && isTerminal(os.Stdin) && isTerminal(os.Stderr) {
			ask = askIgnore(os.Stdin, os.Stderr)
			if err := MigrateIgnore(*img.Build, askMigrate(os.Stdin, os.Stderr)); err != nil {
				return 0, err
			}
		}
		if err := EnsureHiddenIgnored(*img.Build, hidden, ask); err != nil {
			return 0, err
		}
	}

	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return 0, err
	}
	if dynamicBinary(self) {
		fmt.Fprintln(o.Stderr, "warning: this aicatraz binary is dynamically linked; the shim will not run on images with a different libc (build with CGO_ENABLED=0)")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	runBase, err := RunBase()
	if err != nil {
		return 0, err
	}
	runDir := filepath.Join(runBase, strconv.Itoa(os.Getpid()))
	in := RunInput{
		Cfg:        cfg,
		ID:         id,
		Name:       fmt.Sprintf("aicatraz-%s-%d", id, os.Getpid()),
		Image:      img.Ref,
		UID:        uid,
		GID:        gid,
		UserName:   cur.Username,
		Home:       home,
		Cwd:        PickCwd(cfg.Resolved, cwd, home),
		Command:    o.Command,
		TTY:        isTerminal(os.Stdin) && isTerminal(os.Stdout),
		HostBinary: self,
		StateDir:   state,
		HideIsDir:  hideIsDir,
		SocketPath: filepath.Join(runDir, "broker.sock"),
		ImagePath:  imagePath("docker", img.Ref),
	}
	if cfg.NetworkMode() == config.NetFiltered {
		in.NetworkContainer = EgressName(id, os.Getpid())
	}
	scanner := devices.NewScanner()
	if len(cfg.DeviceSpecs) > 0 {
		in.DeviceRules = devices.CgroupRules(cfg.DeviceSpecs, scanner.Scan(cfg.DeviceSpecs))
	}
	var ag *agentRun
	if o.Agent != "" {
		if ag, err = planAgent(o, cfg, home, state, uid, gid, o.Print); err != nil {
			return 0, err
		}
		ag.apply(&in)
		if cfg.NetworkMode() == config.NetOff && !o.Print {
			ui.Note(o.Stderr, "the network is off, so the agent can't reach its service: put `network: open`, or rules, in the config")
		}
	}
	runArgs, err := RunArgs(in)
	if err != nil {
		return 0, err
	}

	if o.Print {
		if img.Build != nil {
			fmt.Fprintln(o.Stdout, shellLine("docker", img.Build.Args(baseRef)))
		}
		if cfg.Sudo {
			fmt.Fprintf(o.Stdout, "# then, from %s: a layer with sudo and a sudoers entry for uid %d, tagged %s\n", baseRef, uid, img.Ref)
		}
		if cfg.NetworkMode() == config.NetFiltered {
			fmt.Fprintf(o.Stdout, "# network rules: the sidecar below owns the jail's network; every connection is checked against the rules\n")
			fmt.Fprintln(o.Stdout, shellLine("docker", egressRunArgs(in.NetworkContainer, id, os.Getpid(), self, "<run dir>/egress.sock", "<run dir>/egress.nft", cfg.Network.Resolvers, cfg.PortMaps)))
		}
		fmt.Fprintf(o.Stdout, "# scratch volumes for /tmp, /var/tmp and the home directory are created first (docker volume create %s-{tmp,vartmp,home}) and removed afterwards\n", in.Name)
		fmt.Fprintln(o.Stdout, shellLine("docker", runArgs))
		return 0, nil
	}

	if err := os.MkdirAll(state, 0o700); err != nil {
		return 0, err
	}
	store, err := pins.Open(filepath.Join(state, "pins"))
	if err != nil {
		return 0, err
	}
	if img.Build != nil {
		if err := ApproveBuild(store, *img.Build, o.ApproveBuild, terminalPrompt(os.Stdin, os.Stderr, cfg)); err != nil {
			return 0, err
		}
		cmd := exec.Command("docker", img.Build.Args(baseRef)...)
		cmd.Stdout, cmd.Stderr = o.Stderr, o.Stderr // keep stdout clean for the command's own output
		if err := cmd.Run(); err != nil {
			return 0, fmt.Errorf("docker build failed: %w", err)
		}
	}
	if cfg.Sudo {
		if err := BuildSudoImage(baseRef, img.Ref, uid, o.Stderr); err != nil {
			return 0, err
		}
	}
	if err := writeIdentity(img.Ref, state, uid, gid, cur.Username, home); err != nil {
		return 0, err
	}
	if err := WriteShimDir(ShimDir(state), ShimCommands(cfg.Rules)); err != nil {
		return 0, err
	}

	if ag != nil {
		if err := ag.prepare(img.Ref, in.Cwd); err != nil {
			return 0, err
		}
	}

	// The image is local now (built or pulled), so its PATH can be read.
	in.ImagePath = imagePath("docker", img.Ref)
	if runArgs, err = RunArgs(in); err != nil {
		return 0, err
	}

	in.ScratchPrefix = in.Name // later runs with the same mounts (agent session lookup) share the volumes
	defer removeScratch(in)
	if err := createScratch(in, o.Stderr); err != nil {
		return 0, err
	}

	var w *watcher
	hook := func(b *broker.Broker) {
		if cfg.Canary.ActionOrDefault() == "off" || len(hideIsDir) == 0 {
			return
		}
		w = newWatcher(in.Name, hideIsDir, cfg.Canary.ActionOrDefault(), cfg.Canary.IntervalOrDefault(), b, o.Stderr)
	}
	approver := chooseApprover(cfg, o, runDir)
	b, stop, err := startBroker(cfg, state, runBase, runDir, id, uid, store, approver, o.Stderr, hook)
	if err != nil {
		return 0, err
	}
	defer stop()
	if cfg.NetworkMode() == config.NetFiltered {
		eg, err := startEgress(cfg, id, state, runDir, approver, self, o.Stderr)
		if err != nil {
			return 0, err
		}
		defer eg.stop()
	}
	stopWatch := make(chan struct{})
	if w != nil {
		b.SetCanary(func() error {
			err := w.check()
			if err != nil {
				w.trip(err) // a request that finds the problem ends the session, not just the request
			}
			return err
		})
		go w.run(stopWatch)
	}
	if len(cfg.DeviceSpecs) > 0 {
		daudit, _ := broker.OpenAudit(filepath.Join(state, "audit.log"))
		stopDevices := startDeviceWatcher(in, daudit, fmt.Sprintf("%s-%d", id, os.Getpid()), scanner)
		defer func() {
			stopDevices()
			daudit.Close()
		}()
	}
	started := time.Now()
	code, err := runDocker(runArgs, o, in.Name)
	close(stopWatch)
	if w != nil && w.wasTripped() {
		return ExitExposed, nil
	}
	if ag != nil && err == nil {
		ag.finish(in, started, code, o)
	}
	return code, err
}

// startBroker starts this session's broker on a socket in a private
// directory and returns a function that stops it and removes the socket.
func startBroker(cfg *config.Config, state, runBase, runDir, id string, uid int, store *pins.Store, approver approve.Approver, stderr io.Writer, hook func(*broker.Broker)) (*broker.Broker, func(), error) {
	cleanStaleRunDirs(runBase)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, nil, err
	}
	audit, err := broker.OpenAudit(filepath.Join(state, "audit.log"))
	if err != nil {
		return nil, nil, err
	}
	b, err := broker.New(cfg, broker.Options{
		UID:      uid,
		HostEnv:  os.Environ(),
		Session:  fmt.Sprintf("%s-%d", id, os.Getpid()),
		Audit:    audit,
		Approver: approver,
		Pins:     store,
	})
	if err != nil {
		return nil, nil, err
	}
	l, err := net.Listen("unix", filepath.Join(runDir, "broker.sock"))
	if err != nil {
		return nil, nil, err
	}
	go func() {
		if err := b.Serve(l); err != nil {
			ui.Error(stderr, "broker stopped: %v", err)
		}
	}()
	if hook != nil {
		hook(b)
	}
	return b, func() {
		b.Close()
		audit.Close()
		os.RemoveAll(runDir)
	}, nil
}

// cleanStaleRunDirs removes socket directories left behind by launchers that
// were killed (the directory name is the launcher's PID).
func cleanStaleRunDirs(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// writeIdentity generates /etc/passwd and /etc/group for the container: the
// image's own files plus an entry for the host user, so whoami and $HOME work
// for a UID the image has never heard of.
func writeIdentity(image, state string, uid, gid int, name, home string) error {
	passwd, err := imageFile("docker", image, "/etc/passwd")
	if err != nil {
		return err
	}
	group, err := imageFile("docker", image, "/etc/group")
	if err != nil {
		return err
	}
	if err := os.WriteFile(PasswdPath(state), []byte(MergePasswd(passwd, uid, gid, name, home)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(GroupPath(state), []byte(MergeGroup(group, gid, name)), 0o644)
}

// inspectVolumes checks that every volume path exists (docker would create
// missing bind sources as root-owned directories) and records which hidden
// paths are directories.
func inspectVolumes(vols []config.Volume) (map[string]bool, error) {
	hideIsDir := map[string]bool{}
	for _, v := range vols {
		st, err := os.Stat(v.Path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("volume %s: %s does not exist", v.Spec, v.Path)
			}
			return nil, fmt.Errorf("volume %s: %w", v.Spec, err)
		}
		if v.Mode == config.ModeHide {
			hideIsDir[v.Path] = st.IsDir()
		}
	}
	return hideIsDir, nil
}

// runDocker runs `docker run` attached to our terminal and returns its exit code.
// killGrace is how long a container gets to stop after SIGTERM or SIGHUP
// before it is killed. An interactive shell ignores SIGTERM, so without this
// `down` would leave the jail running.
const killGrace = 3 * time.Second

// runDocker runs docker with our terminal. container, if set, names the
// container to kill when we are told to stop and it doesn't.
func runDocker(args []string, o Options, container string) (int, error) {
	cmd := exec.Command("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, o.Stdout, o.Stderr

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() {
		for s := range sigs {
			// The terminal already delivers SIGINT to docker; forward the rest.
			if s != syscall.SIGINT {
				_ = cmd.Process.Signal(s)
				if container != "" {
					time.AfterFunc(killGrace, func() { exec.Command("docker", "kill", container).Run() })
				}
			}
		}
	}()
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// isTerminal asks the kernel. Checking for a character device is not enough:
// /dev/null is one, and passing -t to docker with it as stdin fails.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// dynamicBinary reports whether path is an ELF executable that needs a dynamic loader.
func dynamicBinary(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return f.Section(".interp") != nil
}

// shellLine renders a command so it can be pasted into a shell.
func shellLine(name string, args []string) string {
	parts := []string{name}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// chooseApprover picks where `ask` prompts appear: an explicit terminal, else a
// tmux popup if the launcher itself runs inside tmux, else nowhere (and `ask`
// rules are denied). It says which, so the user isn't surprised later.
func chooseApprover(cfg *config.Config, o Options, runDir string) approve.Approver {
	hasAsk := false
	for _, r := range cfg.Rules {
		hasAsk = hasAsk || r.Decision == "ask"
	}
	say := func(format string, a ...any) {
		if hasAsk {
			ui.Note(o.Stderr, format, a...)
		}
	}

	if o.ApprovalTTY != "" {
		say("approvals are shown on %s", o.ApprovalTTY)
		return &approve.TTY{Path: o.ApprovalTTY}
	}
	if o.ApprovalSock != "" {
		say("approvals are shown in the session (attach with `aicatraz up`)")
		return &mux.Approver{Sock: o.ApprovalSock}
	}
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") != "" && pane != "" {
		tmux, err := exec.LookPath("tmux")
		self, err2 := os.Executable()
		if err == nil && err2 == nil {
			say("approvals are shown as a tmux popup")
			return &approve.Tmux{Tmux: tmux, Pane: pane, Self: self, Dir: runDir}
		}
	}
	say("no approval terminal: `ask` rules will be denied (start with `aicatraz up`, or pass --approval-tty)")
	return nil
}

// createScratch creates the session's scratch volumes and hands them to the
// user: docker makes a new volume's mount point root-owned unless the image
// already has the directory.
func createScratch(in RunInput, stderr io.Writer) error {
	script := ""
	args := []string{"run", "--rm", "--user", "0:0", "--cap-drop=ALL", "--cap-add=CHOWN", "--cap-add=FOWNER",
		"--network", "none", "--entrypoint", "sh"}
	for i, sc := range ScratchDirs {
		vol := in.ScratchVolume(sc)
		if out, err := exec.Command("docker", "volume", "create", "--label", "aicatraz.project="+in.ID, vol).CombinedOutput(); err != nil {
			return fmt.Errorf("docker volume create: %w: %s", err, strings.TrimSpace(string(out)))
		}
		mnt := fmt.Sprintf("/s%d", i)
		args = append(args, "--mount", "type=volume,src="+vol+",dst="+mnt)
		script += fmt.Sprintf("chown %d:%d %s && chmod %s %s && ", in.UID, in.GID, mnt, sc.Mode, mnt)
	}
	args = append(args, in.Image, "-c", script+"true")
	cmd := exec.Command("docker", args...)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("preparing scratch volumes: %w", err)
	}
	return nil
}

func removeScratch(in RunInput) {
	for _, sc := range ScratchDirs {
		exec.Command("docker", "volume", "rm", "-f", in.ScratchVolume(sc)).Run()
	}
}
