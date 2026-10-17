// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every Go source file carries the license line, so a file taken out of the
// tree still says what it is.
func TestEveryGoFileHasALicenseHeader(t *testing.T) {
	root := filepath.Join("..", "..")
	var missing []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "user_tests" || d.Name() == "bin" || d.Name() == "dist") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(string(b), "// SPDX-License-Identifier: GPL-3.0-only\n\n") {
				missing = append(missing, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("these files lack the SPDX license line (add `// SPDX-License-Identifier: GPL-3.0-only` and a blank line at the top): %v", missing)
	}
}
