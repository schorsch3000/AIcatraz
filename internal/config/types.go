// Package config loads and validates the AIcatraz YAML configuration.
package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Config is the parsed configuration file.
type Config struct {
	Project Project   `yaml:"project"`
	Image   ImageSpec `yaml:"image"`
	Volumes []string  `yaml:"volumes"`
	Env     Env       `yaml:"env"`
	Global  Global    `yaml:"global"`
	Rules   []Rule    `yaml:"rules"`
	Agent   Agent     `yaml:"agent"`

	// Resolved is filled in by validation: volumes with ~ expanded and the mode parsed.
	Resolved []Volume `yaml:"-"`
}

type Project struct {
	ID string `yaml:"id"`
}

// ImageSpec is either a plain image reference (`image: debian:13`) or a build
// description (`image: {dockerfile: Dockerfile.az, context: .}`).
type ImageSpec struct {
	Ref        string
	Dockerfile string
	Context    string
}

func (i *ImageSpec) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		i.Ref = n.Value
		return nil
	case yaml.MappingNode:
		for j := 0; j+1 < len(n.Content); j += 2 {
			key, val := n.Content[j], n.Content[j+1]
			if val.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: image.%s must be a string", val.Line, key.Value)
			}
			switch key.Value {
			case "dockerfile":
				i.Dockerfile = val.Value
			case "context":
				i.Context = val.Value
			default:
				return fmt.Errorf("line %d: field %s not found in image", key.Line, key.Value)
			}
		}
		return nil
	}
	return fmt.Errorf("line %d: image must be a string or a mapping", n.Line)
}

type Env struct {
	Static   map[string]string `yaml:"static"`
	FromHost []string          `yaml:"from_host"`
}

type Global struct {
	EnvStrip []string `yaml:"env_strip"`
}

type Rule struct {
	Command      string    `yaml:"command"`
	Args         []string  `yaml:"args"`
	DenyFlags    []string  `yaml:"deny_flags"`
	Decision     string    `yaml:"decision"`
	DependsOn    DependsOn `yaml:"depends_on"`
	EnvPass      []string  `yaml:"env_pass"`
	ConfinePaths bool      `yaml:"confine_paths"`
}

// DependsOn is either the word `auto` or a list of path patterns.
type DependsOn struct {
	Auto     bool
	Patterns []string
}

func (d *DependsOn) IsSet() bool { return d.Auto || len(d.Patterns) > 0 }

func (d *DependsOn) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value != "auto" {
			return fmt.Errorf("line %d: depends_on must be `auto` or a list, got %q", n.Line, n.Value)
		}
		d.Auto = true
		return nil
	case yaml.SequenceNode:
		return n.Decode(&d.Patterns)
	}
	return fmt.Errorf("line %d: depends_on must be `auto` or a list", n.Line)
}

type Agent struct {
	Tool            string `yaml:"tool"`
	Version         string `yaml:"version"`
	SkipPermissions *bool  `yaml:"skip_permissions"`
}

// SkipPermissionsOrDefault reports whether the agent runs in skip-permissions mode (default on).
func (a Agent) SkipPermissionsOrDefault() bool {
	return a.SkipPermissions == nil || *a.SkipPermissions
}

// Mode of a volume entry.
type Mode string

const (
	ModeRW   Mode = "rw"
	ModeRO   Mode = "ro"
	ModeHide Mode = "hi"
)

// Volume is a parsed `path:mode` entry.
type Volume struct {
	Path string
	Mode Mode
	Spec string // original text, for messages
}

// Severity of a problem.
type Severity int

const (
	Error Severity = iota
	Warning
)

// Problem is one finding from validation, tied to a line in the file.
type Problem struct {
	Line     int
	Path     string
	Severity Severity
	Msg      string
}
