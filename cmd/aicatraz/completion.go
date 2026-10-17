package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/examples"
	"github.com/schorsch3000/aicatraz/internal/mux"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// cflag is a command line flag as the completion scripts describe it. kind
// says what its value is: "" for none (a switch), "file", "tool", "example"
// or "value" (free text, nothing to suggest).
type cflag struct {
	short, long, kind, desc string
}

// cverb is one sub-command. first lists the words valid as its first
// positional argument; later is the kind of every positional after that.
type cverb struct {
	name, desc string
	flags      []cflag
	first      []string
	later      string
}

var completionTools = []string{"claude", "opencode"}

var (
	flagConfig  = cflag{"c", "config", "file", "config file to use"}
	flagPrint   = cflag{"", "print", "", "show the docker commands, change nothing"}
	flagBuild   = cflag{"", "approve-build", "", "approve changed build files without asking"}
	flagTTY     = cflag{"", "approval-tty", "file", "terminal that answers approval prompts"}
	flagAgent   = cflag{"", "agent", "tool", "start this agent"}
	flagResume  = cflag{"", "resume", "value", "continue this agent session"}
	flagPick    = cflag{"", "pick", "", "choose a session from a list"}
	flagFromHst = cflag{"", "from-host", "", "copy the agent from this machine"}
	flagDetach  = cflag{"", "detach-key", "value", "key that detaches, such as ctrl-y"}
	flagBar     = cflag{"", "status-bar", "bar", "bottom or off"}
	flagTheme   = cflag{"", "bar-theme", "theme", "color theme of the status bar"}
	flagBarFG   = cflag{"", "bar-fg", "value", "text color of the status bar"}
	flagBarBG   = cflag{"", "bar-bg", "value", "background color of the status bar"}
	flagTmux    = cflag{"", "tmux", "", "use a tmux session instead of the built-in one"}
)

var completionVerbs = []cverb{
	{name: "up", desc: "start the jail as a session you can detach from", flags: []cflag{flagConfig, flagPrint, flagTmux, flagDetach, flagBar, flagTheme, flagBarFG, flagBarBG, flagTTY, flagBuild, flagAgent, flagResume}},
	{name: "bar-colors", desc: "show the status bar in every color theme"},
	{name: "down", desc: "stop the session of this project", flags: []cflag{flagConfig}},
	{name: "run", desc: "start the jail in this terminal", flags: []cflag{flagConfig, flagPrint, flagTTY, flagBuild, flagAgent, flagResume}},
	{name: "shell", desc: "open another shell in the running jail", flags: []cflag{flagConfig}},
	{name: "claude", desc: "start Claude Code in the jail", flags: []cflag{flagConfig, flagPrint, flagTTY, flagBuild}},
	{name: "opencode", desc: "start opencode in the jail", flags: []cflag{flagConfig, flagPrint, flagTTY, flagBuild}},
	{name: "agent", desc: "start the agent named in the config", flags: []cflag{flagConfig, flagPrint, flagTTY, flagBuild}},
	{name: "sessions", desc: "list the agent sessions of this project", flags: []cflag{flagConfig}},
	{name: "resume", desc: "continue an agent session", flags: []cflag{flagConfig, flagPrint, flagTTY, flagBuild, flagPick}},
	{name: "tools", desc: "manage the cached agent executables", flags: []cflag{flagFromHst}, first: []string{"list", "install", "update"}, later: "tool"},
	{name: "pins", desc: "list or forget approved file versions", flags: []cflag{flagConfig, {"", "all", "", "forget every pin"}, {"", "missing", "", "forget pins of files that are gone"}}, first: []string{"list", "forget"}, later: "file"},
	{name: "init", desc: "write a starter config or Dockerfile.az", flags: []cflag{
		{"e", "example", "example", "starter to write"},
		{"", "list", "", "list the starters"},
		{"", "force", "", "overwrite existing files"},
	}, first: []string{"dockerfile"}},
	{name: "config", desc: "validate a config", first: []string{"check"}, later: "file"},
	{name: "version", desc: "print the version"},
	{name: "help", desc: "show the usage"},
}

// completionPrograms are the names the scripts register for.
var completionPrograms = []string{"aicatraz", "az"}

var completionShells = []string{"bash", "zsh", "fish"}

// runCompletion prints a script to source: `source <(aicatraz completion bash)`.
func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		ui.Error(stderr, "usage: %s completion <%s>", ui.Prog, strings.Join(completionShells, "|"))
		return 2
	}
	var gen func() string
	switch args[0] {
	case "bash", "sh": // POSIX sh has no programmable completion; people mean bash
		gen = bashCompletion
	case "zsh":
		gen = zshCompletion
	case "fish":
		gen = fishCompletion
	default:
		ui.Error(stderr, "unknown shell %q (use %s)", args[0], strings.Join(completionShells, ", "))
		return 2
	}
	io.WriteString(stdout, gen())
	return 0
}

func themeNames() []string {
	var n []string
	for _, t := range mux.Themes {
		n = append(n, t.Name)
	}
	return n
}

func exampleNames() []string {
	var n []string
	for _, e := range examples.List() {
		n = append(n, e.Name)
	}
	return n
}

func flagWords(v cverb) []string {
	var w []string
	for _, f := range v.flags {
		if f.short != "" {
			w = append(w, "-"+f.short)
		}
		w = append(w, "--"+f.long)
	}
	return w
}

func verbNames() []string {
	var n []string
	for _, v := range completionVerbs {
		n = append(n, v.name)
	}
	return n
}

func bashCompletion() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	tools := strings.Join(completionTools, " ")

	w("# bash completion for %s. Load it with: source <(%s completion bash)\n", strings.Join(completionPrograms, " and "), ui.Prog)
	w("_aicatraz_complete() {\n")
	w("    local cur prev verb i word npos=0\n")
	w("    COMPREPLY=()\n")
	w("    cur=${COMP_WORDS[COMP_CWORD]}\n")
	w("    prev=${COMP_WORDS[COMP_CWORD-1]}\n")
	w("    if [ \"$COMP_CWORD\" -eq 1 ]; then\n")
	w("        COMPREPLY=($(compgen -W %q -- \"$cur\"))\n", strings.Join(verbNames(), " "))
	w("        return\n")
	w("    fi\n")
	w("    verb=${COMP_WORDS[1]}\n")
	w("    # after `--` the words belong to the command inside the jail\n")
	w("    for ((i = 2; i < COMP_CWORD; i++)); do\n")
	w("        [ \"${COMP_WORDS[i]}\" = \"--\" ] && return\n")
	w("    done\n")
	w("    case \"$prev\" in\n")
	w("    -c | --config | --approval-tty)\n")
	w("        compopt -o filenames 2>/dev/null\n")
	w("        COMPREPLY=($(compgen -f -- \"$cur\"))\n")
	w("        return ;;\n")
	w("    --agent)\n")
	w("        COMPREPLY=($(compgen -W %q -- \"$cur\"))\n", tools)
	w("        return ;;\n")
	w("    -e | --example)\n")
	w("        COMPREPLY=($(compgen -W %q -- \"$cur\"))\n", strings.Join(exampleNames(), " "))
	w("        return ;;\n")
	w("    --status-bar)\n")
	w("        COMPREPLY=($(compgen -W 'bottom off' -- \"$cur\"))\n")
	w("        return ;;\n")
	w("    --bar-theme)\n")
	w("        COMPREPLY=($(compgen -W %q -- \"$cur\"))\n", strings.Join(themeNames(), " "))
	w("        return ;;\n")
	w("    --resume | --detach-key | --bar-fg | --bar-bg)\n")
	w("        return ;;\n")
	w("    esac\n")
	w("    if [[ $cur == -* ]]; then\n")
	w("        case \"$verb\" in\n")
	for _, v := range completionVerbs {
		if len(v.flags) > 0 {
			w("        %s) COMPREPLY=($(compgen -W %q -- \"$cur\")) ;;\n", v.name, strings.Join(flagWords(v), " "))
		}
	}
	w("        esac\n")
	w("        return\n")
	w("    fi\n")
	w("    # count the positional words so far, skipping flag values\n")
	w("    for ((i = 2; i < COMP_CWORD; i++)); do\n")
	w("        word=${COMP_WORDS[i]}\n")
	w("        case \"$word\" in\n")
	w("        -c | --config | --approval-tty | --agent | --resume | --status-bar | --bar-theme | --bar-fg | --bar-bg | --detach-key | -e | --example) ((i++)) ;;\n")
	w("        -*) ;;\n")
	w("        *) ((npos++)) ;;\n")
	w("        esac\n")
	w("    done\n")
	w("    case \"$verb\" in\n")
	for _, v := range completionVerbs {
		if len(v.first) == 0 {
			continue
		}
		later := ""
		switch v.later {
		case "tool":
			later = fmt.Sprintf("COMPREPLY=($(compgen -W %q -- \"$cur\"))", tools)
		case "file":
			later = "compopt -o filenames 2>/dev/null; COMPREPLY=($(compgen -f -- \"$cur\"))"
		default:
			later = ":"
		}
		w("    %s)\n", v.name)
		w("        if [ \"$npos\" -eq 0 ]; then\n")
		w("            COMPREPLY=($(compgen -W %q -- \"$cur\"))\n", strings.Join(v.first, " "))
		w("        else\n")
		w("            %s\n", later)
		w("        fi ;;\n")
	}
	w("    esac\n")
	w("}\n")
	w("complete -o default -F _aicatraz_complete %s\n", strings.Join(completionPrograms, " "))
	return b.String()
}

// zshSpec renders one flag as an _arguments spec.
func zshSpec(f cflag) string {
	action := ""
	switch f.kind {
	case "file":
		action = ":file:_files"
	case "tool":
		action = ":agent:(" + strings.Join(completionTools, " ") + ")"
	case "example":
		action = ":example:(" + strings.Join(exampleNames(), " ") + ")"
	case "value":
		action = ":value: "
	case "bar":
		action = ":position:(bottom off)"
	case "theme":
		action = ":theme:(" + strings.Join(themeNames(), " ") + ")"
	}
	if f.short != "" {
		return fmt.Sprintf("'(-%s --%s)'{-%s,--%s}'[%s]%s'", f.short, f.long, f.short, f.long, f.desc, action)
	}
	return fmt.Sprintf("'--%s[%s]%s'", f.long, f.desc, action)
}

func zshCompletion() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("#compdef %s\n", strings.Join(completionPrograms, " "))
	w("# zsh completion for %s. Load it with: source <(%s completion zsh)\n", strings.Join(completionPrograms, " and "), ui.Prog)
	w("# (the completion system must be loaded first: autoload -U compinit && compinit)\n")
	w("_aicatraz() {\n")
	w("    local -a verbs\n")
	w("    verbs=(\n")
	for _, v := range completionVerbs {
		w("        '%s:%s'\n", v.name, v.desc)
	}
	w("    )\n")
	w("    if (( CURRENT == 2 )); then\n")
	w("        _describe -t commands command verbs\n")
	w("        return\n")
	w("    fi\n")
	w("    local verb=$words[2]\n")
	w("    # make the verb the command word, so numbered arguments count from it\n")
	w("    shift words\n")
	w("    (( CURRENT-- ))\n")
	w("    case $verb in\n")
	for _, v := range completionVerbs {
		if len(v.flags) == 0 && len(v.first) == 0 {
			continue
		}
		w("    %s)\n", v.name)
		w("        _arguments \\\n")
		for _, f := range v.flags {
			w("            %s \\\n", zshSpec(f))
		}
		if len(v.first) > 0 {
			w("            '1:%s:(%s)' \\\n", "action", strings.Join(v.first, " "))
			switch v.later {
			case "tool":
				w("            '*:agent:(%s)'\n", strings.Join(completionTools, " "))
			case "file":
				w("            '2:file:_files'\n")
			default:
				w("            '*: :'\n")
			}
		} else {
			w("            '*: :'\n")
		}
		w("        ;;\n")
	}
	w("    esac\n")
	w("}\n")
	w("if [ \"$funcstack[1]\" = \"_aicatraz\" ]; then\n")
	w("    _aicatraz \"$@\"\n")
	w("else\n")
	w("    compdef _aicatraz %s\n", strings.Join(completionPrograms, " "))
	w("fi\n")
	return b.String()
}

func fishCompletion() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# fish completion for %s. Load it with: %s completion fish | source\n", strings.Join(completionPrograms, " and "), ui.Prog)
	w("for prog in %s\n", strings.Join(completionPrograms, " "))
	w("    complete -c $prog -f\n")
	for _, v := range completionVerbs {
		w("    complete -c $prog -n __fish_use_subcommand -a %s -d %q\n", v.name, v.desc)
	}
	for _, v := range completionVerbs {
		for _, f := range v.flags {
			line := fmt.Sprintf("    complete -c $prog -n '__fish_seen_subcommand_from %s' -l %s", v.name, f.long)
			if f.short != "" {
				line += " -s " + f.short
			}
			switch f.kind {
			case "file":
				line += " -r -F"
			case "tool":
				line += " -x -a '" + strings.Join(completionTools, " ") + "'"
			case "example":
				line += " -x -a '" + strings.Join(exampleNames(), " ") + "'"
			case "value":
				line += " -x"
			case "bar":
				line += " -x -a 'bottom off'"
			case "theme":
				line += " -x -a '" + strings.Join(themeNames(), " ") + "'"
			}
			w("%s -d %q\n", line, f.desc)
		}
		if len(v.first) > 0 {
			w("    complete -c $prog -n '__fish_seen_subcommand_from %s; and not __fish_seen_subcommand_from %s' -a '%s'\n",
				v.name, strings.Join(v.first, " "), strings.Join(v.first, " "))
			switch v.later {
			case "tool":
				w("    complete -c $prog -n '__fish_seen_subcommand_from install update' -a '%s'\n", strings.Join(completionTools, " "))
			case "file":
				w("    complete -c $prog -n '__fish_seen_subcommand_from %s' -F\n", v.first[0])
			}
		}
	}
	w("end\n")
	return b.String()
}
