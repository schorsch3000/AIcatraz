// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/agent"
	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/canary"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/devices"
	"github.com/schorsch3000/aicatraz/internal/egress"
	"github.com/schorsch3000/aicatraz/internal/jail"
	"github.com/schorsch3000/aicatraz/internal/mux"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

const defaultConfig = "aicatraz.yaml"

const usage = `usage: {prog} <command> [flags]      ({prog} help <command> lists the flags of a command)

Start a jail
  run [-- cmd...]            the jail in this terminal (default: a shell)
  up [-- cmd...]             the jail as a session you can detach from (Ctrl-Y) and re-attach to
  claude | opencode          start that coding agent in the jail (as a session, in a terminal)
  agent                      start the agent named by agent.tool in the config
  resume [id] [--pick]       continue the latest (or the given) agent session
  shell [-- cmd...]          another shell in the running jail
  down                       stop the session of this project

Look and change
  status                     the running jails of all projects
  log [-f] [-n N]            the audit log: what the agent asked for, and what was decided
  sessions                   the agent sessions of this project
  network list|allow|deny|forget
                             the network rules, and the ones remembered from prompts
  pins list|diff|forget      files whose approved version is remembered
  devices                    USB devices of this machine, and which the config hands over
  tools [list|install|update]
                             the cached agent executables

Set up and maintain
  init [--example name]      write a starter ` + defaultConfig + ` and a Dockerfile.az here (--list shows the starters)
  config check [file]        validate a config (default: ` + defaultConfig + `)
  doctor                     check that Docker and the rest are in order
  clean                      remove state of projects whose directory is gone
  bar-colors                 show the status bar in every color theme
  completion bash|zsh|fish   print shell completion code to source
  version

Commands that use a project take -c <file> to name the config (default: ` + defaultConfig + `).
Exit codes: 0 ok; 1 config problems; 2 anything else; run, up and the agents pass on the
exit code of the program they ran.
`

func runCLI(args []string) int {
	return cli(args, os.Stdout, os.Stderr)
}

func cli(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, styledUsage(ui.For(stderr)))
		return 2
	}
	switch args[0] {
	case "--help", "-h":
		args = []string{"help"}
	case "--version":
		args = []string{"version"}
	}
	if v := findVerb(args[0]); v != nil && args[0] != "help" && args[0] != "completion" {
		norm, err := normalizeArgs(v, args[1:])
		if errors.Is(err, errHelp) {
			printVerbHelp(stdout, v)
			return 0
		}
		if err != nil {
			return badArgs(stderr, err)
		}
		args = append([]string{args[0]}, norm...)
		if needsConfig[args[0]] {
			if err := checkConfigExists(args[1:]); err != nil {
				ui.Error(stderr, "%v", err)
				return 2
			}
		}
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "aicatraz %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return 0
	case "run", "up", "down", "shell", "claude", "opencode", "agent", "resume":
		return launch(args[0], args[1:], stdout, stderr)
	case jail.CanaryCommand: // used inside the container by the hidden-path check
		return canary.Run(args[1:], stdout)
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "devices":
		if err := jail.Devices(args[1:], stdout, stderr); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return 0
	case "network":
		if err := jail.Network(args[1:], stdout, stderr); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return 0
	case "pins":
		if err := jail.Pins(args[1:], stdout, stderr); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return 0
	case "status":
		if len(args) > 1 {
			return badArgs(stderr, unexpected("status", args[1]))
		}
		if err := jail.Status(stdout); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return 0
	case "log":
		return runLog(args[1:], stdout, stderr)
	case "doctor":
		file, _ := configArg(args[1:])
		if jail.Doctor(file, stdout) {
			return 0
		}
		return 1
	case "clean":
		yes := len(args) > 1 && (args[1] == "--yes" || args[1] == "-y")
		if len(args) > 1 && !yes {
			return badArgs(stderr, unexpected("clean", args[1]))
		}
		if err := jail.Clean(yes, os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), stdout, stderr); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return 0
	case "bar-colors":
		w := 80
		if f, ok := stdout.(*os.File); ok {
			if cols, _, err := term.GetSize(int(f.Fd())); err == nil {
				w = cols
			}
		}
		mux.Preview(stdout, w, ui.For(stdout).Enabled())
		return 0
	case "completion":
		return runCompletion(args[1:], stdout, stderr)
	case "sessions":
		file, _ := configArg(args[1:])
		if len(args) > 1 && args[1] != "--config" {
			return badArgs(stderr, unexpected("sessions", args[1]))
		}
		if err := jail.ListSessions(file, stdout); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return 0
	case "tools":
		if err := jail.Tools(args[1:], stdout, stderr); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return 0
	case approve.PromptCommand: // used inside the tmux popup
		if len(args) == 4 {
			secs, err := strconv.Atoi(args[3])
			if err == nil {
				return approve.RunPrompt(args[1], args[2], time.Duration(secs)*time.Second)
			}
		}
		return 2
	case devices.NodeCommand: // runs inside the jail, to make or remove a device node
		return devices.RunNodeCommand(args[1:], stderr)
	case egress.SidecarCommand: // the network helper container
		return egress.RunSidecar(args[1:], stderr)
	case jail.MuxServeCommand: // the detached process behind `up`
		code, err := jail.ServeMux(args[1:])
		if err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return code
	case jail.UpEnvCommand: // used inside the tmux session: restore the saved environment, then run
		if len(args) < 3 {
			return 2
		}
		if err := jail.RestoreEnv(args[1]); err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		return cli(args[2:], stdout, stderr)
	case "config":
		file, named := configArg(args[1:])
		rest := args[1:]
		if named {
			rest = rest[2:]
		}
		if len(rest) == 0 || rest[0] != "check" {
			return badArgs(stderr, &usageError{"config", "config needs a sub-command: check"})
		}
		switch {
		case len(rest) > 2:
			return badArgs(stderr, unexpected("config", rest[2]))
		case len(rest) == 2 && named:
			return badArgs(stderr, &usageError{"config", "name the file once, as -c <file> or as an argument"})
		case len(rest) == 2:
			file = rest[1]
		}
		return configCheck(file, stdout, stderr)
	case "help":
		return runHelp(args[1:], stdout, stderr)
	}
	return badArgs(stderr, unknownCommand(args[0]))
}

func configCheck(file string, stdout, stderr io.Writer) int {
	_, problems, err := config.Load(file, config.Options{})
	if err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	st := ui.For(stderr)
	for _, p := range problems {
		fmt.Fprintln(stderr, p.FormatStyled(file, st))
	}
	if config.HasErrors(problems) {
		return 1
	}
	fmt.Fprintf(stdout, "%s: %s\n", file, ui.For(stdout).BoldGreen("ok"))
	return 0
}

func launch(verb string, args []string, stdout, stderr io.Writer) int {
	opts := jail.Options{ConfigPath: defaultConfig, Stdout: stdout, Stderr: stderr}
	agentVerb := verb == "claude" || verb == "opencode" || verb == "agent" || verb == "resume"
	sessionVerb := verb == "up" || verb == "run" || agentVerb // verbs that can run as a session you can detach from
	noSession := false
	switch verb {
	case "claude", "opencode":
		opts.Agent = verb
	case "agent":
		opts.Agent = "@config"
	}
	pick := false
	var rest []string // arguments after `--`
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--":
			rest = args[1:]
			args = nil
		case a == "--print":
			opts.Print = true
			args = args[1:]
		case (a == "-c" || a == "--config") && len(args) >= 2:
			opts.ConfigPath = args[1]
			args = args[2:]
		case a == "--detach-key" && len(args) >= 2 && sessionVerb:
			opts.DetachKey = args[1]
			args = args[2:]
		case a == "--info-key" && len(args) >= 2 && sessionVerb:
			opts.InfoKey = args[1]
			args = args[2:]
		case a == "--bar-theme" && len(args) >= 2 && sessionVerb:
			opts.BarTheme = args[1]
			args = args[2:]
		case a == "--bar-fg" && len(args) >= 2 && sessionVerb:
			opts.BarFG = args[1]
			args = args[2:]
		case a == "--bar-bg" && len(args) >= 2 && sessionVerb:
			opts.BarBG = args[1]
			args = args[2:]
		case a == "--status-bar" && len(args) >= 2 && sessionVerb:
			opts.StatusBar = args[1]
			args = args[2:]
		case a == "--no-session" && (agentVerb || verb == "run"):
			noSession = true
			args = args[1:]
		case a == "--tmux" && verb == "up":
			opts.Tmux = true
			args = args[1:]
		case a == "--approval-sock" && len(args) >= 2 && verb == "run":
			opts.ApprovalSock = args[1]
			args = args[2:]
		case a == "--approve-build" && verb != "shell" && verb != "down":
			opts.ApproveBuild = true
			args = args[1:]
		case a == "--approval-tty" && len(args) >= 2 && verb != "shell":
			opts.ApprovalTTY = args[1]
			args = args[2:]
		case a == "--agent" && len(args) >= 2 && (verb == "run" || verb == "up"):
			opts.Agent = args[1]
			args = args[2:]
		case a == "--resume" && len(args) >= 2 && (verb == "run" || verb == "up"):
			opts.Resume = args[1]
			args = args[2:]
		case a == "--pick" && verb == "resume":
			pick = true
			args = args[1:]
		case verb == "resume" && !strings.HasPrefix(a, "-") && opts.Resume == "":
			opts.Resume = a // a session id
			args = args[1:]
		default:
			return badArgs(stderr, unexpected(verb, a))
		}
	}
	if opts.Agent != "" || agentVerb {
		opts.AgentArgs = rest
	} else {
		opts.Command = rest
	}

	if !opts.Print { // --print changes nothing
		askUserConfig(stderr)
	}

	if verb == "resume" {
		s, err := chooseSession(opts, pick, os.Stdin, stderr)
		if err != nil {
			ui.Error(stderr, "%v", err)
			return 2
		}
		opts.Agent, opts.Resume = s.Tool, s.ID
	}

	var (
		code int
		err  error
	)
	switch verb {
	case "up":
		code, err = jail.Up(opts)
	case "down":
		err = jail.Down(opts)
	case "shell":
		opts.Command = rest
		code, err = jail.Shell(opts)
	default:
		// An agent, and `run`, run as a session (you can detach, and approval
		// prompts have somewhere to appear) unless that is switched off, there is
		// no terminal to attach, or you named a terminal or a socket for the
		// prompts yourself. A session starts its program with `run --approval-sock`.
		if (agentVerb || verb == "run") && !noSession && opts.ApprovalTTY == "" && opts.ApprovalSock == "" && isTerminalPair(stdout) {
			code, err = jail.Up(opts)
		} else {
			code, err = jail.Launch(opts)
		}
	}
	if err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	return code
}

// isTerminalPair reports whether stdin and out are both terminals.
func isTerminalPair(out io.Writer) bool {
	f, ok := out.(*os.File)
	return ok && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(f.Fd()))
}

// chooseSession finds the session `resume` continues.
func chooseSession(opts jail.Options, pick bool, in io.Reader, stderr io.Writer) (*agent.Session, error) {
	if !pick {
		return jail.FindSession(opts.ConfigPath, opts.Resume)
	}
	var b strings.Builder
	if err := jail.ListSessions(opts.ConfigPath, &b); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for i, l := range lines {
		fmt.Fprintf(stderr, "%2d) %s\n", i+1, l)
	}
	fmt.Fprint(stderr, "session number (Enter for 1): ")
	text, _ := bufio.NewReader(in).ReadString('\n')
	n := 1
	if t := strings.TrimSpace(text); t != "" {
		var err error
		if n, err = strconv.Atoi(t); err != nil || n < 1 || n > len(lines) {
			return nil, fmt.Errorf("not a session number: %q", t)
		}
	}
	fields := strings.Fields(lines[n-1])
	if len(fields) < 4 {
		return nil, fmt.Errorf("no sessions recorded for this project yet")
	}
	return jail.FindSession(opts.ConfigPath, fields[3])
}

// usageText is the usage with the name the program was started under.
func usageText() string {
	return strings.ReplaceAll(usage, "{prog}", ui.Prog)
}

// styledUsage colors the usage text: headings bold, command names bold cyan,
// descriptions dimmed.
func styledUsage(st ui.Style) string {
	text := usageText()
	if !st.Enabled() {
		return text
	}
	var b strings.Builder
	for i, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		body := strings.TrimLeft(line, " ")
		switch {
		case i == 0 || line == "":
			b.WriteString(line)
		case strings.HasPrefix(line, "Commands that") || strings.HasPrefix(line, "Exit codes") || strings.HasPrefix(line, "exit code"):
			b.WriteString(st.Dim(line))
		case !strings.HasPrefix(line, " "):
			b.WriteString(st.Bold(line))
		case len(line)-len(body) > 2: // a description continued from the line above
			b.WriteString(st.Dim(line))
		default:
			name, rest, found := strings.Cut(body, "   ")
			b.WriteString("  " + st.BoldCyan(name))
			if found {
				desc := strings.TrimLeft(rest, " ")
				b.WriteString(strings.Repeat(" ", 3+len(rest)-len(desc)) + st.Dim(desc))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// runLog parses the flags of `aicatraz log`.
func runLog(args []string, stdout, stderr io.Writer) int {
	o := jail.LogOptions{ConfigPath: defaultConfig, Lines: 20}
	if len(args) >= 2 && args[0] == "--config" {
		o.ConfigPath, args = args[1], args[2:]
	}
	for len(args) > 0 {
		switch a := args[0]; a {
		case "-f", "--follow":
			o.Follow = true
			args = args[1:]
		case "-n", "--lines":
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 0 {
				return badArgs(stderr, &usageError{"log", fmt.Sprintf("%s needs a number of entries, not %q", a, args[1])})
			}
			o.Lines = n
			args = args[2:]
		default:
			return badArgs(stderr, unexpected("log", a))
		}
	}
	if err := jail.Log(o, stdout); err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	return 0
}
