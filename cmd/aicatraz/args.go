// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// errHelp is what normalizeArgs returns when the user asked for help.
var errHelp = errors.New("help requested")

// usageError is a mistake in the command line. It is shown with a hint where
// to read more, not with the whole usage text.
type usageError struct {
	verb string
	msg  string
}

func (e *usageError) Error() string { return e.msg }

func findVerb(name string) *cverb {
	for i := range completionVerbs {
		if completionVerbs[i].name == name {
			return &completionVerbs[i]
		}
	}
	return nil
}

func (v *cverb) flag(arg string) *cflag {
	for i, f := range v.flags {
		if arg == "--"+f.long || (f.short != "" && arg == "-"+f.short) {
			return &v.flags[i]
		}
	}
	return nil
}

func (v *cverb) has(f cflag) bool {
	for _, x := range v.flags {
		if x.long == f.long {
			return true
		}
	}
	return false
}

// normalizeArgs checks the flags of a command against the table the completion
// scripts are made from, so what the program accepts, what `help` lists and
// what completes are one list. It rewrites `--flag=value` as `--flag value`
// and moves -c/--config to the front, where every command looks for it.
// Everything after `--` belongs to the program in the jail and is left alone.
func normalizeArgs(v *cverb, args []string) ([]string, error) {
	var out, tail []string
	var cfg []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			tail = args[i:]
			break
		}
		if a == "--help" || a == "-h" {
			return nil, errHelp
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, a)
			continue
		}
		name, value, hasValue := a, "", false
		if strings.HasPrefix(a, "--") {
			name, value, hasValue = strings.Cut(a, "=")
		}
		f := v.flag(name)
		if f == nil {
			return nil, &usageError{v.name, fmt.Sprintf("unknown flag %s for `%s %s`", name, ui.Prog, v.name)}
		}
		if f.kind == "" {
			if hasValue {
				return nil, &usageError{v.name, fmt.Sprintf("%s does not take a value", name)}
			}
			out = append(out, name)
			continue
		}
		if !hasValue {
			if i+1 >= len(args) || args[i+1] == "--" {
				return nil, &usageError{v.name, fmt.Sprintf("%s needs a value (%s)", name, f.valueName())}
			}
			i++
			value = args[i]
		}
		if f.long == "config" {
			cfg = []string{"--config", value}
			continue
		}
		out = append(out, name, value)
	}
	out = append(cfg, out...)
	return append(out, tail...), nil
}

func (f cflag) valueName() string {
	switch f.kind {
	case "file", "tool", "example", "theme", "bar":
		return "<" + f.kind + ">"
	}
	return "<value>"
}

// configArg returns the config file named in normalized args (default
// aicatraz.yaml) and whether it was named.
func configArg(args []string) (string, bool) {
	if len(args) >= 2 && args[0] == "--config" {
		return args[1], true
	}
	return config.DefaultPath(), false
}

// needsConfig lists the commands that read the project's config.
var needsConfig = map[string]bool{
	"up": true, "down": true, "run": true, "shell": true, "claude": true, "opencode": true,
	"agent": true, "resume": true, "sessions": true, "network": true, "pins": true, "log": true,
}

// checkConfigExists says what to do when there is no config, instead of
// letting the first command fail with "open aicatraz.yaml: no such file".
func checkConfigExists(args []string) error {
	path, named := configArg(args)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if named {
		return fmt.Errorf("config file %s not found", path)
	}
	return fmt.Errorf("no %s in this directory; run `aicatraz init` to write one, or name a file with -c <file>", config.DefaultFile)
}

// badArgs reports a mistake in the command line and returns the exit code.
func badArgs(stderr io.Writer, err error) int {
	ui.Error(stderr, "%v", err)
	var ue *usageError
	if errors.As(err, &ue) && ue.verb != "" {
		ui.Note(stderr, "see `aicatraz help %s`", ue.verb)
	} else {
		ui.Note(stderr, "see `aicatraz help`")
	}
	return 2
}

// unexpected is the error for a positional argument a command doesn't take.
func unexpected(verb, arg string) error {
	return &usageError{verb, fmt.Sprintf("unexpected argument %q for `%s %s`", arg, ui.Prog, verb)}
}

// printVerbHelp writes the help of one command.
func printVerbHelp(w io.Writer, v *cverb) {
	st := ui.For(w)
	fmt.Fprintf(w, "%s %s\n\n", st.BoldCyan(ui.Prog+" "+v.name), v.desc)
	line := ui.Prog + " " + v.name
	if v.args != "" {
		line += " " + v.args
	}
	fmt.Fprintf(w, "%s %s\n", st.Bold("usage:"), line)
	if len(v.flags) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", st.Bold("flags:"))
	labels := make([]string, len(v.flags))
	width := 0
	for i, f := range v.flags {
		l := "      --" + f.long
		if f.short != "" {
			l = "  -" + f.short + ", --" + f.long
		}
		if f.kind != "" {
			l += " " + f.valueName()
		}
		labels[i] = l
		width = max(width, len(l))
	}
	for i, f := range v.flags {
		fmt.Fprintf(w, "%s%s  %s\n", st.Yellow(labels[i]), strings.Repeat(" ", width-len(labels[i])), f.desc)
	}
}

// runHelp implements `aicatraz help [command]`.
func runHelp(args []string, stdout, stderr io.Writer) int {
	switch len(args) {
	case 0:
		fmt.Fprint(stdout, styledUsage(ui.For(stdout)))
		return 0
	case 1:
		if v := findVerb(args[0]); v != nil {
			printVerbHelp(stdout, v)
			return 0
		}
		return badArgs(stderr, unknownCommand(args[0]))
	}
	return badArgs(stderr, &usageError{"help", "help takes one command name"})
}

// unknownCommand builds the error for a command that does not exist, with the
// closest names if there are any.
func unknownCommand(name string) error {
	var near []string
	for _, v := range completionVerbs {
		if strings.HasPrefix(v.name, name) || strings.HasPrefix(name, v.name) || editDistance(v.name, name) <= 2 {
			near = append(near, v.name)
		}
	}
	sort.Strings(near)
	msg := fmt.Sprintf("unknown command %q", name)
	if len(near) > 0 {
		msg += "; did you mean " + strings.Join(near, " or ") + "?"
	}
	return &usageError{"", msg}
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
