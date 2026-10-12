package main

import (
	"fmt"
	"io"
	"os"

	"github.com/schorsch3000/aicatraz/internal/config"
)

const defaultConfig = "aicatraz.yaml"

const usage = `usage:
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
		label := "error"
		if p.Severity == config.Warning {
			label = "warning"
		}
		loc := file
		if p.Line > 0 {
			loc = fmt.Sprintf("%s:%d", file, p.Line)
		}
		fmt.Fprintf(stderr, "%s: %s: %s\n", loc, label, p.Msg)
	}
	if config.HasErrors(problems) {
		return 1
	}
	fmt.Fprintf(stdout, "%s: ok\n", file)
	return 0
}
