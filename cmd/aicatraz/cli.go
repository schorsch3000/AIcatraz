// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/agent"
	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/canary"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/egress"
	"github.com/schorsch3000/aicatraz/internal/jail"
	"github.com/schorsch3000/aicatraz/internal/mux"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

const defaultConfig = "aicatraz.yaml"

const usage = `usage:
  aicatraz up [-c file] [--print] [--tmux] [--detach-key key] [--approve-build] [-- command...]
              [--status-bar bottom|off] [--bar-theme name] [--bar-fg color] [--bar-bg color]
                                 start the jail as a session that keeps running when you
                                 detach (Ctrl-Y; or --detach-key ctrl-<key>, or
                                 $AICATRAZ_DETACH_KEY), with a status bar on the bottom
                                 row (colors: see bar-colors); approval prompts for 'ask'
                                 rules appear over the screen; attaches if it already runs
  aicatraz down [-c file]        stop the session of this project
  aicatraz bar-colors            show the status bar in every color theme
  aicatraz run [-c file] [--print] [--approval-tty dev] [-- command...]
                                 start the jail in this terminal (default: an
                                 interactive shell); --print shows the docker
                                 commands and changes nothing
  aicatraz shell [-c file] [-- command...]
                                 open another shell in the running jail
  aicatraz claude|opencode [flags] [-- agent arguments...]
                                 start a coding agent in the jail (installed on first
                                 use, with your configuration copied in); in a terminal it
                                 runs as an up session (flags as for up, --no-session to
                                 run it directly)
  aicatraz agent [flags] [-- agent arguments...]
                                 start the agent named by agent.tool in the config
  aicatraz sessions              list the agent sessions of this project
  aicatraz resume [flags] [id] [--pick]
                                 continue the latest (or the given) agent session
  aicatraz network [-c file] list
                                 show the network rules and the remembered ones
  aicatraz network [-c file] allow|deny <host|ip|cidr|*> [--ports 22,443,8000-8080|*]
  aicatraz network [-c file] forget <number> | --all
                                 change the remembered rules (also in a running session)
  aicatraz pins [-c file] list   show the files whose approved version is remembered
  aicatraz pins [-c file] diff [<path>...]
                                 show what changed in pinned files since they were approved
  aicatraz pins [-c file] forget <path>... | --missing | --all
                                 forget pins; the next run asks again
  aicatraz tools [list | install <tool> [version] [--from-host] | update [tool...]]
                                 manage the cached agent executables
  aicatraz init [--example name] [--list] [--force]
                                 write a starter ` + defaultConfig + ` (and Dockerfile.az) here
  aicatraz init dockerfile [--force]
                                 write a one-line Dockerfile.az (FROM the distribution
                                 this machine runs)
  aicatraz completion bash|zsh|fish
                                 print shell completion code to source
  aicatraz config check [file]   validate a config (default: ` + defaultConfig + `)
  aicatraz version
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
	case "version", "--version":
		fmt.Fprintf(stdout, "aicatraz %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return 0
	case "run", "up", "down", "shell", "claude", "opencode", "agent", "resume":
		return launch(args[0], args[1:], stdout, stderr)
	case jail.CanaryCommand: // used inside the container by the hidden-path check
		return canary.Run(args[1:], stdout)
	case "init":
		return runInit(args[1:], stdout, stderr)
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
		file := defaultConfig
		if len(args) == 3 && (args[1] == "-c" || args[1] == "--config") {
			file = args[2]
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
		if len(args) >= 2 && args[1] == "check" && len(args) <= 3 {
			file := defaultConfig
			if len(args) == 3 {
				file = args[2]
			}
			return configCheck(file, stdout, stderr)
		}
	case "help", "--help", "-h":
		fmt.Fprint(stdout, styledUsage(ui.For(stdout)))
		return 0
	}
	fmt.Fprint(stderr, styledUsage(ui.For(stderr)))
	return 2
}

func configCheck(file string, stdout, stderr io.Writer) int {
	_, problems, err := config.LoadFile(file, config.Options{})
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
	sessionVerb := verb == "up" || agentVerb // verbs that can run as a session you can detach from
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
		case a == "--no-session" && agentVerb:
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
			fmt.Fprint(stderr, styledUsage(ui.For(stderr)))
			return 2
		}
	}
	if opts.Agent != "" || agentVerb {
		opts.AgentArgs = rest
	} else {
		opts.Command = rest
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
		// An agent runs as a session (you can detach, and approval prompts have
		// somewhere to appear) unless that is switched off, there is no terminal
		// to attach, or you named a terminal for the prompts yourself.
		if agentVerb && !noSession && opts.ApprovalTTY == "" && isTerminalPair(stdout) {
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

var (
	usageVerb = regexp.MustCompile(`^(  \S+ )([a-z|]+)`)
	usageFlag = regexp.MustCompile(`(\s|\[)(--?[a-z][a-z-]*)`)
)

var usageGap = regexp.MustCompile(`(\S)( {2,})(\S)`)

// renameUsage swaps the program name in the usage text for ui.Prog, widening
// the description gap on a line so the descriptions stay aligned.
func renameUsage(text string) string {
	diff := len("aicatraz") - len(ui.Prog)
	if diff == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "  aicatraz ") {
			continue
		}
		l = "  " + ui.Prog + l[len("  aicatraz"):]
		if diff > 0 {
			l = usageGap.ReplaceAllString(l, "${1}${2}"+strings.Repeat(" ", diff)+"${3}")
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// styledUsage colors the usage text: command names bold cyan, flags yellow,
// descriptions dimmed.
func styledUsage(st ui.Style) string {
	text := renameUsage(usage)
	if !st.Enabled() {
		return text
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		switch {
		case line == "usage:":
			b.WriteString(st.Bold(line))
		case strings.HasPrefix(line, "  "+ui.Prog+" "):
			line = usageFlag.ReplaceAllStringFunc(line, func(m string) string {
				return m[:1] + st.Yellow(m[1:])
			})
			line = usageVerb.ReplaceAllString(line, "$1"+st.BoldCyan("$2"))
			b.WriteString(line)
		default:
			b.WriteString(st.Dim(line)) // continuation lines describe the command above
		}
		b.WriteString("\n")
	}
	return b.String()
}
