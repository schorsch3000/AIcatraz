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

	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/pins"
)

// Options for Launch.
type Options struct {
	ConfigPath   string
	Command      []string // run this instead of an interactive shell
	Print        bool     // show what would run, change nothing
	ApprovalTTY  string   // prompt for `ask` rules on this terminal device instead of tmux
	ApproveBuild bool     // pin changed build files without asking
	Stdout       io.Writer
	Stderr       io.Writer
}

// Launch builds the image if needed and runs the jail. It returns the exit
// code of the command inside the container.
func Launch(o Options) (int, error) {
	cfg, problems, err := config.LoadFile(o.ConfigPath, config.Options{})
	if err != nil {
		return 0, err
	}
	for _, p := range problems {
		fmt.Fprintln(o.Stderr, p.Format(o.ConfigPath))
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
	img, err := ResolveImage(cfg, cfgDir, id)
	if err != nil {
		return 0, err
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
		if err := CheckHidden(*img.Build, hidden); err != nil {
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
	runArgs, err := RunArgs(in)
	if err != nil {
		return 0, err
	}

	if o.Print {
		if img.Build != nil {
			fmt.Fprintln(o.Stdout, shellLine("docker", img.Build.Args(img.Ref)))
		}
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
		cmd := exec.Command("docker", img.Build.Args(img.Ref)...)
		cmd.Stdout, cmd.Stderr = o.Stderr, o.Stderr // keep stdout clean for the command's own output
		if err := cmd.Run(); err != nil {
			return 0, fmt.Errorf("docker build failed: %w", err)
		}
	}
	if err := writeIdentity(img.Ref, state, uid, gid, cur.Username, home); err != nil {
		return 0, err
	}
	if err := WriteShimDir(ShimDir(state), ShimCommands(cfg.Rules)); err != nil {
		return 0, err
	}

	// The image is local now (built or pulled), so its PATH can be read.
	in.ImagePath = imagePath("docker", img.Ref)
	if runArgs, err = RunArgs(in); err != nil {
		return 0, err
	}

	stop, err := startBroker(cfg, state, runBase, runDir, id, uid, store, chooseApprover(cfg, o, runDir), o.Stderr)
	if err != nil {
		return 0, err
	}
	defer stop()
	return runDocker(runArgs, o)
}

// startBroker starts this session's broker on a socket in a private
// directory and returns a function that stops it and removes the socket.
func startBroker(cfg *config.Config, state, runBase, runDir, id string, uid int, store *pins.Store, approver approve.Approver, stderr io.Writer) (func(), error) {
	cleanStaleRunDirs(runBase)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	audit, err := broker.OpenAudit(filepath.Join(state, "audit.log"))
	if err != nil {
		return nil, err
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
		return nil, err
	}
	l, err := net.Listen("unix", filepath.Join(runDir, "broker.sock"))
	if err != nil {
		return nil, err
	}
	go func() {
		if err := b.Serve(l); err != nil {
			fmt.Fprintf(stderr, "aicatraz: broker stopped: %v\n", err)
		}
	}()
	return func() {
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
func runDocker(args []string, o Options) (int, error) {
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
			fmt.Fprintf(o.Stderr, "aicatraz: "+format+"\n", a...)
		}
	}

	if o.ApprovalTTY != "" {
		say("approvals are shown on %s", o.ApprovalTTY)
		return &approve.TTY{Path: o.ApprovalTTY}
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
	say("no approval terminal: `ask` rules will be denied (start with `aicatraz up` inside tmux, or pass --approval-tty)")
	return nil
}
