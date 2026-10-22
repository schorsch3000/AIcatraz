// SPDX-License-Identifier: GPL-3.0-only

package config

import (
	"os"
	"path/filepath"
)

// AzDir is the directory in a project that holds AIcatraz's own files.
const AzDir = ".az"

// FileName is the name of the config file, in AzDir (or, in older projects,
// next to the project).
const FileName = "aicatraz.yaml"

// DefaultFile is where a project's config lives.
var DefaultFile = filepath.Join(AzDir, FileName)

// DefaultPath is the config the commands use when none is named: .az/aicatraz.yaml,
// or an aicatraz.yaml in the current directory from before .az existed.
// With neither, it is the new location.
func DefaultPath() string {
	if _, err := os.Stat(DefaultFile); err != nil {
		if _, err := os.Stat(FileName); err == nil {
			return FileName
		}
	}
	return DefaultFile
}

// ProjectDir is the directory relative paths in the config at path (absolute)
// are resolved from: the config's own directory, or the project directory
// when the config lives in its .az directory.
func ProjectDir(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == AzDir {
		return filepath.Dir(dir)
	}
	return dir
}
