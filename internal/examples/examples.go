// SPDX-License-Identifier: GPL-3.0-only

// Package examples holds the starter configurations `aicatraz init` writes.
package examples

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed data
var data embed.FS

// ConfigFile is the name of the main file of every example.
const ConfigFile = "aicatraz.yaml"

// Example is one starter configuration.
type Example struct {
	Name        string
	Description string
	Files       []string // file names, ConfigFile first
}

var descriptions = map[string]string{
	"starter":          "the project writable, with every option explained in comments (default)",
	"secrets":          "hide .env and .secrets/ from the agent, but let `make deploy` use them",
	"deploy-with-make": "build and test in the jail, deploy-test from the host, deploy-prod asks first",
	"claude":           "Claude Code in a jail, with git push going through you; includes Dockerfile.az",
	"opencode":         "opencode in a jail, with git push going through you; includes Dockerfile.az",
}

// List returns the examples, the default first.
func List() []Example {
	entries, _ := fs.ReadDir(data, "data")
	var out []Example
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ex := Example{Name: e.Name(), Description: descriptions[e.Name()]}
		files, _ := fs.ReadDir(data, "data/"+e.Name())
		for _, f := range files {
			if f.Name() != ConfigFile {
				ex.Files = append(ex.Files, f.Name())
			}
		}
		sort.Strings(ex.Files)
		ex.Files = append([]string{ConfigFile}, ex.Files...)
		out = append(out, ex)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Name == "starter") != (out[j].Name == "starter") {
			return out[i].Name == "starter"
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Render returns the files of an example. The configs mount the project as
// `.`, relative to the config file, so they work wherever it is checked out.
func Render(name string) (map[string]string, error) {
	files, err := fs.ReadDir(data, "data/"+name)
	if err != nil {
		var names []string
		for _, e := range List() {
			names = append(names, e.Name)
		}
		return nil, fmt.Errorf("unknown example %q (available: %s)", name, strings.Join(names, ", "))
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := fs.ReadFile(data, "data/"+name+"/"+f.Name())
		if err != nil {
			return nil, err
		}
		out[f.Name()] = string(b)
	}
	return out, nil
}

// CheckProjectDir refuses a directory that can't be the project: the home
// directory itself, one of its parents, or the root.
func CheckProjectDir(dir, home string) error {
	dir = filepath.Clean(dir)
	if dir == "/" || dir == home || strings.HasPrefix(home+"/", dir+"/") {
		return fmt.Errorf("%s contains your home directory, so it can't be the project; run this in a project directory", dir)
	}
	return nil
}

// Home returns the user's home directory.
func Home() (string, error) { return os.UserHomeDir() }
