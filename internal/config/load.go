package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Options tune loading. Home is the host home directory used to expand `~`;
// Dir is the directory relative volume paths (`.`, `./x`, `../x`) are
// resolved against, normally the directory holding the config file.
type Options struct {
	Home string
	Dir  string
}

// LoadFile reads and validates a config file.
func LoadFile(path string, opts Options) (*Config, []Problem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if opts.Home == "" {
		opts.Home, err = os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("cannot determine home directory: %w", err)
		}
	}
	if opts.Dir == "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, nil, err
		}
		opts.Dir = filepath.Dir(abs)
	}
	cfg, problems := Parse(data, opts)
	return cfg, problems, nil
}

var yamlLine = regexp.MustCompile(`^line (\d+): (.*)$`)

// Parse decodes strictly (unknown keys are errors) and validates. The returned
// Config is nil if the YAML itself could not be decoded.
func Parse(data []byte, opts Options) (*Config, []Problem) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, []Problem{yamlProblem(err)}
	}

	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			var ps []Problem
			for _, m := range te.Errors {
				ps = append(ps, textProblem(m))
			}
			return nil, ps
		}
		return nil, []Problem{yamlProblem(err)}
	}

	v := &validator{root: &root, home: opts.Home, dir: opts.Dir}
	v.validate(cfg)
	sort.SliceStable(v.problems, func(i, j int) bool { return v.problems[i].Line < v.problems[j].Line })
	return cfg, v.problems
}

// HasErrors reports whether any problem is an error (warnings don't count).
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if p.Severity == Error {
			return true
		}
	}
	return false
}

func yamlProblem(err error) Problem {
	return textProblem(strings.TrimPrefix(err.Error(), "yaml: "))
}

func textProblem(msg string) Problem {
	msg = strings.TrimPrefix(msg, "yaml: ")
	if m := yamlLine.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return Problem{Line: n, Msg: m[2]}
	}
	if i := strings.Index(msg, "line "); i >= 0 {
		rest := msg[i+len("line "):]
		if j := strings.Index(rest, ":"); j > 0 {
			if n, err := strconv.Atoi(rest[:j]); err == nil {
				return Problem{Line: n, Msg: strings.TrimSpace(rest[j+1:])}
			}
		}
	}
	return Problem{Msg: msg}
}

// lineOf walks the YAML tree along path (string keys, int indexes) and returns
// the line of the deepest node found. 0 if the document is empty.
func lineOf(root *yaml.Node, path ...any) int {
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	line := n.Line
	for _, p := range path {
		switch k := p.(type) {
		case string:
			if n.Kind != yaml.MappingNode {
				return line
			}
			found := false
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == k {
					line = n.Content[i].Line
					n = n.Content[i+1]
					found = true
					break
				}
			}
			if !found {
				return line
			}
		case int:
			if n.Kind != yaml.SequenceNode || k >= len(n.Content) {
				return line
			}
			n = n.Content[k]
			line = n.Line
		}
	}
	return line
}

func pathString(path []any) string {
	var b strings.Builder
	for _, p := range path {
		switch k := p.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(k)
		case int:
			fmt.Fprintf(&b, "[%d]", k)
		}
	}
	return b.String()
}
