package jail

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/mux"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// DetachKeyEnv names the environment variable that sets the detach key, for
// those who don't want to pass --detach-key every time.
const DetachKeyEnv = "AICATRAZ_DETACH_KEY"

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Environment variables for the status bar.
const (
	StatusBarEnv = "AICATRAZ_STATUS_BAR" // bottom or off
	BarThemeEnv  = "AICATRAZ_BAR_THEME"
	BarFGEnv     = "AICATRAZ_BAR_FG"
	BarBGEnv     = "AICATRAZ_BAR_BG"
)

// MuxServeCommand is the hidden subcommand that runs a session server.
const MuxServeCommand = "__mux-serve"

// MuxDir is the private directory of a project's session: its sockets. It is
// short because socket paths are limited to about 100 bytes.
func MuxDir(id string) (string, error) {
	base, err := RunBase()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(base, "m-"+hex.EncodeToString(sum[:5])), nil
}

// sessionAlive reports whether a session server answers on the attach socket.
func sessionAlive(dir string) bool {
	c, err := net.DialTimeout("unix", filepath.Join(dir, mux.AttachSock), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// muxRunArgs is the `run` command line the session runs: the same options as
// `up`, with approvals going to the session.
func muxRunArgs(cfgPath, dir string, o Options) []string {
	run := []string{"run", "-c", cfgPath, "--approval-sock", filepath.Join(dir, mux.ApproveSock)}
	if o.ApproveBuild {
		run = append(run, "--approve-build")
	}
	if o.Agent != "" {
		run = append(run, "--agent", o.Agent)
	}
	if o.Resume != "" {
		run = append(run, "--resume", o.Resume)
	}
	if rest := append(append([]string(nil), o.Command...), o.AgentArgs...); len(rest) > 0 {
		run = append(run, "--")
		run = append(run, rest...)
	}
	return run
}

// UpMux starts the project's session (or attaches to the one that runs) with
// the built-in multiplexer.
func UpMux(o Options) (int, error) {
	bar, err := mux.ParseBar(firstNonEmpty(o.StatusBar, os.Getenv(StatusBarEnv)))
	if err != nil {
		return 0, err
	}
	colors, err := mux.ResolveColors(firstNonEmpty(o.BarTheme, os.Getenv(BarThemeEnv)),
		firstNonEmpty(o.BarFG, os.Getenv(BarFGEnv)), firstNonEmpty(o.BarBG, os.Getenv(BarBGEnv)))
	if err != nil {
		return 0, err
	}
	key := mux.DefaultKey
	if spec := firstNonEmpty(o.DetachKey, os.Getenv(DetachKeyEnv)); spec != "" {
		var err error
		if key, err = mux.ParseKey(spec); err != nil {
			return 0, err
		}
	}
	cfgAbs, err := filepath.Abs(o.ConfigPath)
	if err != nil {
		return 0, err
	}
	cfg, id, err := loadForID(cfgAbs)
	if err != nil {
		return 0, err
	}
	dir, err := MuxDir(id)
	if err != nil {
		return 0, err
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	runArgs := muxRunArgs(cfgAbs, dir, o)
	cols, rows, _ := term.GetSize(int(os.Stdout.Fd()))
	rows = mux.NewScreen(bar, rows, cols).AgentRows()
	serveArgs := append([]string{MuxServeCommand, dir, strconv.Itoa(rows), strconv.Itoa(cols), sessionLabel(o, cfg.Agent.Tool), "--"}, runArgs...)

	alive := sessionAlive(dir)
	if o.Print {
		if alive {
			fmt.Fprintf(o.Stdout, "# a session is running: attach to %s\n", filepath.Join(dir, mux.AttachSock))
		} else {
			fmt.Fprintln(o.Stdout, shellLine(self, serveArgs))
		}
		return 0, nil
	}

	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return 0, errors.New("`up` needs a terminal to attach to (use `run` in scripts)")
	}
	if alive {
		if len(o.Command)+len(o.AgentArgs) > 0 || o.Agent != "" || o.Resume != "" {
			ui.Note(o.Stderr, "a session is already running; attaching to it (the command and options you gave are ignored)")
		}
	} else if err := startSessionServer(self, serveArgs, dir); err != nil {
		return 0, err
	}

	ui.Note(o.Stderr, "detach with %s (the session keeps running), stop it with `%s down`", key.Name, ui.Prog)
	res, err := mux.Attach(filepath.Join(dir, mux.AttachSock), os.Stdin, os.Stdout, mux.AttachOptions{Detach: key, Bar: bar, Project: id, Colors: colors})
	if err != nil {
		return 0, err
	}
	switch {
	case res.Exited:
		return res.Code, nil
	case res.Detached:
		ui.Note(o.Stderr, "detached; the session keeps running. `%s up` attaches again, `%s down` stops it", ui.Prog, ui.Prog)
		return 0, nil
	}
	return 0, errors.New(res.Message)
}

// sessionLabel names what the session runs, for the status bar.
func sessionLabel(o Options, configured string) string {
	switch {
	case o.Agent == "@config" && configured != "":
		return configured
	case o.Agent != "" && o.Agent != "@config":
		return o.Agent
	case o.Agent == "@config":
		return "agent"
	case len(o.Command) > 0:
		return filepath.Base(o.Command[0])
	}
	return "shell"
}

// startSessionServer starts the detached server process and waits for its
// socket.
func startSessionServer(self string, args []string, dir string) error {
	os.RemoveAll(dir) // a stale socket of a server that is gone
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()

	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives this terminal
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	for i := 0; i < 200; i++ {
		if sessionAlive(dir) {
			return nil
		}
		select {
		case <-exited:
			msg, _ := os.ReadFile(logPath)
			return fmt.Errorf("the session server stopped at once: %s", strings.TrimSpace(string(msg)))
		case <-time.After(25 * time.Millisecond):
		}
	}
	return errors.New("the session server did not start")
}

// ServeMux is the body of the hidden `__mux-serve <dir> <rows> <cols> <label> -- run-args...`
// command: it runs `aicatraz run ...` on a pty and serves clients.
func ServeMux(args []string) (int, error) {
	if len(args) < 6 || args[4] != "--" {
		return 2, errors.New("usage: " + MuxServeCommand + " dir rows cols label -- args...")
	}
	rows, _ := strconv.Atoi(args[1])
	cols, _ := strconv.Atoi(args[2])
	self, err := os.Executable()
	if err != nil {
		return 2, err
	}
	cwd, _ := os.Getwd()
	cmd := exec.Command(self, args[5:]...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	code, err := mux.Serve(mux.Options{Dir: args[0], Cmd: cmd, Rows: uint16(rows), Cols: uint16(cols), Label: args[3]})
	os.RemoveAll(args[0])
	if err == nil {
		os.Remove(args[0] + ".log") // nothing went wrong, nothing to read
	}
	return code, err
}

// Down stops the project's session: the program gets SIGTERM, so the jail shuts
// down in order.
func Down(o Options) error {
	_, id, err := loadForID(o.ConfigPath)
	if err != nil {
		return err
	}
	dir, err := MuxDir(id)
	if err != nil {
		return err
	}
	if !sessionAlive(dir) {
		return errors.New("no session is running for this project")
	}
	if err := mux.Stop(filepath.Join(dir, mux.AttachSock)); err != nil {
		return err
	}
	for i := 0; i < 200; i++ { // up to 10 s: the container needs a moment
		if !sessionAlive(dir) {
			ui.Note(o.Stderr, "session stopped")
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("the session did not stop in time")
}
