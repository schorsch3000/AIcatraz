// Package config loads and validates the AIcatraz YAML configuration.
package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/schorsch3000/aicatraz/internal/egress"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// Config is the parsed configuration file.
type Config struct {
	Project  Project   `yaml:"project"`
	Image    ImageSpec `yaml:"image"`
	Volumes  []string  `yaml:"volumes"`
	Env      Env       `yaml:"env"`
	Limits   Limits    `yaml:"limits"`
	Approval Approval  `yaml:"approval"`
	Canary   Canary    `yaml:"canary"`
	Global   Global    `yaml:"global"`
	Rules    []Rule    `yaml:"rules"`
	Agent    Agent     `yaml:"agent"`
	Network  *Network  `yaml:"network"`

	// Sudo gives the user passwordless sudo in the jail (see docs/security.md
	// for what that costs). Default off.
	Sudo bool `yaml:"sudo"`

	// Resolved is filled in by validation: volumes with ~ expanded and the mode parsed.
	Resolved []Volume `yaml:"-"`
	// NetRules is filled in by validation: the network rules, parsed.
	NetRules []egress.Rule `yaml:"-"`
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

// Approval tunes the approval prompt for `ask` rules.
type Approval struct {
	Timeout string `yaml:"timeout"` // a Go duration such as "90s" or "2m"; default 2m
}

// DefaultApprovalTimeout is how long a prompt waits before it counts as a deny.
const DefaultApprovalTimeout = 2 * time.Minute

// TimeoutOrDefault returns the prompt timeout. Validation guarantees it parses.
func (a Approval) TimeoutOrDefault() time.Duration {
	if a.Timeout == "" {
		return DefaultApprovalTimeout
	}
	d, err := time.ParseDuration(a.Timeout)
	if err != nil || d <= 0 {
		return DefaultApprovalTimeout
	}
	return d
}

// Canary controls the check that hidden paths are still hidden.
type Canary struct {
	Action   string `yaml:"action"`   // stop (default), warn or off
	Interval string `yaml:"interval"` // how often to check, default 3s
}

// DefaultCanaryInterval is how often hidden paths are re-checked.
const DefaultCanaryInterval = 3 * time.Second

// ActionOrDefault returns what to do when a hidden path is exposed.
func (c Canary) ActionOrDefault() string {
	if c.Action == "" {
		return "stop"
	}
	return c.Action
}

// IntervalOrDefault returns the check interval. Validation guarantees it parses.
func (c Canary) IntervalOrDefault() time.Duration {
	if d, err := time.ParseDuration(c.Interval); err == nil && d > 0 {
		return d
	}
	return DefaultCanaryInterval
}

// Limits caps the container's resources. Zero values mean the default
// (pids) or no limit (memory).
type Limits struct {
	Pids   int    `yaml:"pids"`
	Memory string `yaml:"memory"`
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

// Format renders a problem as "file:line: error: message".
func (p Problem) Format(file string) string { return p.FormatStyled(file, ui.Style{}) }

// FormatStyled is Format with colors: the location bold, the label red for
// errors and yellow for warnings.
func (p Problem) FormatStyled(file string, st ui.Style) string {
	label := st.BoldRed("error")
	if p.Severity == Warning {
		label = st.BoldYellow("warning")
	}
	loc := file
	if p.Line > 0 {
		loc = fmt.Sprintf("%s:%d", file, p.Line)
	}
	return fmt.Sprintf("%s: %s: %s", st.Bold(loc), label, p.Msg)
}

// Network limits what the jail may connect to. Without this section the
// network is open.
type Network struct {
	Default   string    `yaml:"default"`   // what a connection no rule covers does: ask (default) or deny
	DNS       string    `yaml:"dns"`       // open (default): names resolve unless a rule refuses them; restricted: only names a rule covers
	Resolvers []string  `yaml:"resolvers"` // upstream DNS servers (default: the host's)
	Rules     []NetRule `yaml:"rules"`
}

// DefaultOrAsk is the decision for connections no rule covers.
func (n *Network) DefaultOrAsk() egress.Decision {
	if n.Default == "deny" {
		return egress.Deny
	}
	return egress.Ask
}

// NetRule is one line of the network policy.
type NetRule struct {
	To       string     `yaml:"to"`
	Ports    FlexString `yaml:"ports"`
	Proto    FlexString `yaml:"proto"`
	Decision string     `yaml:"decision"`
}

// FlexString is a value written either as one string (`ports: "80, 443"`,
// `ports: 443`, `ports: "*"`) or as a list (`ports: [80, 443, 8000-8080]`);
// a list is joined with commas.
type FlexString string

func (f *FlexString) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*f = FlexString(n.Value)
		return nil
	case yaml.SequenceNode:
		var parts []string
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: expected a value or a list of values", c.Line)
			}
			parts = append(parts, c.Value)
		}
		*f = FlexString(strings.Join(parts, ","))
		return nil
	}
	return fmt.Errorf("line %d: expected a value or a list of values", n.Line)
}
