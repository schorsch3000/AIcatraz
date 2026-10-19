// SPDX-License-Identifier: GPL-3.0-only

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// userSections are the top-level keys the user-wide config may hold. They are
// the settings that describe how you work (prompts, bar, limits, network
// defaults), not what a project is or what its agent may touch: volumes, image,
// host command rules, ports, devices and sudo stay in the project's file, so a
// setting you forgot about can never widen a jail.
var userSections = []string{"approval", "canary", "global", "agent", "limits", "env", "network", "bar", "init"}

// UserConfigPath is where the user-wide config lives: $XDG_CONFIG_HOME/aicatraz/config.yaml,
// or ~/.config/aicatraz/config.yaml. It is on the host and never mounted into a jail.
func UserConfigPath(home string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "aicatraz", "config.yaml")
}

// Load reads a project's config file like LoadFile and fills in what it leaves
// out from the user-wide config, if there is one. A section or key the project
// writes replaces the user's; `network` is replaced as a whole section.
func Load(path string, opts Options) (*Config, []Problem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if opts.Home == "" {
		if opts.Home, err = os.UserHomeDir(); err != nil {
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
	if cfg == nil {
		return cfg, problems, nil
	}
	user, err := loadUser(opts.Home)
	if err != nil || user == nil {
		return cfg, problems, err
	}
	var root yaml.Node
	_ = yaml.Unmarshal(data, &root)
	applyUser(cfg, user, &root)
	return cfg, problems, nil
}

// LoadUser reads the user-wide config of home for commands that run without a
// project config. Nil, nil: there is none.
func LoadUser(home string) (*Config, error) { return loadUser(home) }

// loadUser reads and validates the user-wide config. Nil, nil: there is none.
func loadUser(home string) (*Config, error) {
	path := UserConfigPath(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("user config %s: %w", path, err)
	}
	cfg, problems := Parse(data, Options{Home: home, Dir: filepath.Dir(path)})
	var msgs []string
	for _, p := range problems {
		if p.Severity == Error {
			where := ""
			if p.Path != "" {
				where = p.Path + ": "
			}
			msgs = append(msgs, fmt.Sprintf("line %d: %s%s", p.Line, where, p.Msg))
		}
	}
	if cfg != nil {
		var root yaml.Node
		_ = yaml.Unmarshal(data, &root)
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 && root.Content[0].Kind == yaml.MappingNode {
			m := root.Content[0]
			for i := 0; i+1 < len(m.Content); i += 2 {
				if k := m.Content[i]; !contains(userSections, k.Value) {
					msgs = append(msgs, fmt.Sprintf("line %d: %s belongs in the project's aicatraz.yaml; the user config may hold %s", k.Line, k.Value, strings.Join(userSections, ", ")))
				}
			}
		}
	}
	if len(msgs) > 0 {
		return nil, fmt.Errorf("user config %s:\n  %s", path, strings.Join(msgs, "\n  "))
	}
	return cfg, nil
}

// present reports whether the YAML document has a key at path.
func present(root *yaml.Node, path ...string) bool {
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	for _, k := range path {
		if n.Kind != yaml.MappingNode {
			return false
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
				break
			}
		}
		if next == nil {
			return false
		}
		n = next
	}
	return true
}

// applyUser copies the user's settings into every place the project's YAML
// (root) does not set.
func applyUser(dst, src *Config, root *yaml.Node) {
	if !present(root, "network") && src.Network != nil {
		dst.Network, dst.NetRules = src.Network, src.NetRules
	}
	d, s := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	for _, section := range userSections {
		if section == "network" {
			continue
		}
		for i := 0; i < d.NumField(); i++ {
			if name, _, _ := strings.Cut(d.Type().Field(i).Tag.Get("yaml"), ","); name == section {
				fillStruct(d.Field(i), s.Field(i), root, section)
			}
		}
	}
}

// fillStruct sets each field of dst the project's YAML lacks from src.
func fillStruct(dst, src reflect.Value, root *yaml.Node, path ...string) {
	for i := 0; i < dst.NumField(); i++ {
		name, _, _ := strings.Cut(dst.Type().Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		if !present(root, append(append([]string(nil), path...), name)...) {
			dst.Field(i).Set(src.Field(i))
		}
	}
}
