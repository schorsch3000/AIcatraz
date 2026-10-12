package main

import (
	"fmt"
	"io"
	"os"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/jail"
)

const defaultConfig = "aicatraz.yaml"

const usage = `usage:
  aicatraz run [-c file] [--print] [-- command...]
                                 start the jail (default: an interactive shell);
                                 --print shows the docker commands and changes nothing
  aicatraz config check [file]   validate a config (default: ` + defaultConfig + `)
  aicatraz version
`

func runCLI(args []string) int {
	return cli(args, os.Stdout, os.Stderr)
}

func cli(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, "aicatraz", version)
		return 0
	case "run":
		return runJail(args[1:], stdout, stderr)
	case "config":
		if len(args) >= 2 && args[1] == "check" && len(args) <= 3 {
			file := defaultConfig
			if len(args) == 3 {
				file = args[2]
			}
			return configCheck(file, stdout, stderr)
		}
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func configCheck(file string, stdout, stderr io.Writer) int {
	_, problems, err := config.LoadFile(file, config.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "aicatraz: %v\n", err)
		return 2
	}
	for _, p := range problems {
		fmt.Fprintln(stderr, p.Format(file))
	}
	if config.HasErrors(problems) {
		return 1
	}
	fmt.Fprintf(stdout, "%s: ok\n", file)
	return 0
}

func runJail(args []string, stdout, stderr io.Writer) int {
	opts := jail.Options{ConfigPath: defaultConfig, Stdout: stdout, Stderr: stderr}
	for len(args) > 0 {
		switch a := args[0]; {
		case a == "--":
			opts.Command = args[1:]
			args = nil
		case a == "--print":
			opts.Print = true
			args = args[1:]
		case a == "-c" || a == "--config":
			if len(args) < 2 {
				fmt.Fprint(stderr, usage)
				return 2
			}
			opts.ConfigPath = args[1]
			args = args[2:]
		default:
			fmt.Fprint(stderr, usage)
			return 2
		}
	}
	code, err := jail.Launch(opts)
	if err != nil {
		fmt.Fprintf(stderr, "aicatraz: %v\n", err)
		return 2
	}
	return code
}
