package jail

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/schorsch3000/aicatraz/internal/config"
)

// loadForID loads the config just far enough to know the project.
func loadForID(path string) (*config.Config, string, error) {
	cfg, problems, err := config.LoadFile(path, config.Options{})
	if err != nil {
		return nil, "", err
	}
	if config.HasErrors(problems) {
		return nil, "", fmt.Errorf("config has errors (see `aicatraz config check %s`)", path)
	}
	id, err := ProjectID(path, cfg)
	return cfg, id, err
}

// SessionName is the tmux session `aicatraz up` uses for a project.
func SessionName(id string) string { return "aicatraz-" + id }

// UpArgs builds the tmux command that starts (or attaches to) the project's
// session. Inside tmux already, `up` is just `run`, because popups work there.
// The launcher's environment travels in a private file, not in tmux's
// arguments, because it may hold secrets (env.from_host) and arguments show up
// in the process list.
func UpArgs(tmux, self, cfgPath, cwd, envFile string, attach bool, o Options) []string {
	name := SessionName(mustID(cfgPath))
	if attach {
		return []string{tmux, "attach-session", "-t", "=" + name}
	}
	run := []string{"run", "-c", cfgPath}
	if o.ApprovalTTY != "" {
		run = append(run, "--approval-tty", o.ApprovalTTY)
	}
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
	// Keep the pane open on failure so the error can be read.
	script := `"$@"; c=$?; if [ "$c" -ne 0 ]; then printf '\naicatraz exited with status %s - press Enter to close\n' "$c"; read -r _; fi; exit "$c"`
	args := []string{tmux, "new-session", "-s", name, "-c", cwd, "sh", "-c", script, "sh", self, UpEnvCommand, envFile}
	return append(args, run...)
}

func mustID(cfgPath string) string {
	_, id, err := loadForID(cfgPath)
	if err != nil {
		return "unknown"
	}
	return id
}

// UpEnvCommand is the hidden subcommand that restores the saved environment
// inside the tmux pane and then runs the rest of its arguments.
const UpEnvCommand = "__with-env"

// Up starts the project's session, or attaches to it. It uses the built-in
// multiplexer, or tmux with o.Tmux.
func Up(o Options) (int, error) {
	if o.Tmux {
		return UpTmux(o)
	}
	return UpMux(o)
}

// UpTmux starts the project's tmux session (or attaches to it), replacing this
// process with tmux. If already inside tmux it simply runs the jail.
func UpTmux(o Options) (int, error) {
	if os.Getenv("TMUX") != "" {
		return Launch(o)
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return 0, fmt.Errorf("tmux is needed for `aicatraz up --tmux` (install it, or drop --tmux to use the built-in multiplexer)")
	}
	cfgAbs, err := filepath.Abs(o.ConfigPath)
	if err != nil {
		return 0, err
	}
	_, id, err := loadForID(cfgAbs)
	if err != nil {
		return 0, err
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 0, err
	}

	attach := exec.Command(tmux, "has-session", "-t", "="+SessionName(id)).Run() == nil
	var envFile string
	if !attach {
		if envFile, err = saveEnv(); err != nil {
			return 0, err
		}
	}
	args := UpArgs(tmux, self, cfgAbs, cwd, envFile, attach, o)
	if o.Print {
		fmt.Fprintln(o.Stdout, shellLine(args[0], args[1:]))
		if envFile != "" {
			os.Remove(envFile)
		}
		return 0, nil
	}
	err = syscall.Exec(tmux, args, os.Environ())
	if envFile != "" {
		os.Remove(envFile)
	}
	return 0, err
}

// saveEnv writes the environment (NUL-separated) to a private file.
func saveEnv() (string, error) {
	base, err := RunBase()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(base, "env-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := f.WriteString(strings.Join(os.Environ(), "\x00")); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// RestoreEnv replaces the process environment with the one saved by `up` and
// deletes the file. TMUX and TMUX_PANE are kept from the current environment:
// they describe the pane we are in now.
func RestoreEnv(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	os.Remove(file)
	keep := map[string]string{}
	for _, k := range []string{"TMUX", "TMUX_PANE"} {
		if v, ok := os.LookupEnv(k); ok {
			keep[k] = v
		}
	}
	os.Clearenv()
	for _, kv := range strings.Split(string(data), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			os.Setenv(k, v)
		}
	}
	for k, v := range keep {
		os.Setenv(k, v)
	}
	return nil
}

// Shell opens another shell in the running jail of this project (docker exec),
// for a second terminal. Host commands from it go through the same broker.
func Shell(o Options) (int, error) {
	cfg, id, err := loadForID(o.ConfigPath)
	if err != nil {
		return 0, err
	}
	out, err := exec.Command("docker", "ps", "--filter", "label=aicatraz.project="+id, "--format", "{{.Names}}").Output()
	if err != nil {
		return 0, fmt.Errorf("docker ps: %w", err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return 0, fmt.Errorf("no running jail for this project; start one with `aicatraz run` or `aicatraz up`")
	}
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	args := []string{"exec", "-i"}
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		args = append(args, "-t")
	}
	args = append(args, "-w", PickCwd(cfg.Resolved, cwd, home), names[0]) // newest first
	if len(o.Command) == 0 {
		args = append(args, defaultShell...)
	} else {
		args = append(args, o.Command...)
	}
	if o.Print {
		fmt.Fprintln(o.Stdout, shellLine("docker", args))
		return 0, nil
	}
	return runDocker(args, o, "")
}
