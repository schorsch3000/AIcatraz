package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/schorsch3000/aicatraz/internal/agent"
	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/canary"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/jail"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

const defaultConfig = "aicatraz.yaml"

const usage = `usage:
  aicatraz up [-c file] [--print] [--approval-tty dev] [--approve-build] [-- command...]
                                 start the jail in a tmux session (prompts for 'ask'
                                 rules appear as popups); attaches if it already runs
  aicatraz run [-c file] [--print] [--approval-tty dev] [-- command...]
                                 start the jail in this terminal (default: an
                                 interactive shell); --print shows the docker
                                 commands and changes nothing
  aicatraz shell [-c file] [-- command...]
                                 open another shell in the running jail
  aicatraz claude|opencode [flags] [-- agent arguments...]
                                 start a coding agent in the jail (installed on first
                                 use, with your configuration copied in)
  aicatraz agent [flags] [-- agent arguments...]
                                 start the agent named by agent.tool in the config
  aicatraz sessions              list the agent sessions of this project
  aicatraz resume [flags] [id] [--pick]
                                 continue the latest (or the given) agent session
  aicatraz tools [list | install <tool> [version] [--from-host] | update [tool...]]
                                 manage the cached agent executables
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
		fmt.Fprintln(stdout, "aicatraz", version)
		return 0
	case "run", "up", "shell", "claude", "opencode", "agent", "resume":
		return launch(args[0], args[1:], stdout, stderr)
	case jail.CanaryCommand: // used inside the container by the hidden-path check
		return canary.Run(args[1:], stdout)
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
		case a == "--approve-build" && verb != "shell":
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
	case "shell":
		opts.Command = rest
		code, err = jail.Shell(opts)
	default:
		code, err = jail.Launch(opts)
	}
	if err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	return code
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
	usageVerb = regexp.MustCompile(`^(  aicatraz )([a-z|]+)`)
	usageFlag = regexp.MustCompile(`(\s|\[)(--?[a-z][a-z-]*)`)
)

// styledUsage colors the usage text: command names bold cyan, flags yellow,
// descriptions dimmed.
func styledUsage(st ui.Style) string {
	if !st.Enabled() {
		return usage
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(usage, "\n"), "\n") {
		switch {
		case line == "usage:":
			b.WriteString(st.Bold(line))
		case strings.HasPrefix(line, "  aicatraz "):
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
