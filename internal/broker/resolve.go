// SPDX-License-Identifier: GPL-3.0-only

package broker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// under reports whether p is dir or inside it.
func under(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func underAny(dirs []string, p string) (string, bool) {
	for _, d := range dirs {
		if under(d, p) {
			return d, true
		}
	}
	return "", false
}

// resolveBinary finds name on the broker's own PATH. The container never says
// which file to run. Relative PATH entries are ignored, and so is any
// candidate that lives in (or links into) a writable volume: the agent could
// have planted it there.
func resolveBinary(name, pathEnv string, writable []string) (string, error) {
	var skipped string
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) {
			continue
		}
		cand := filepath.Join(dir, name)
		st, err := os.Stat(cand)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(cand)
		if err != nil {
			continue
		}
		if vol, bad := underAny(writable, real); bad {
			skipped = fmt.Sprintf("%s is in the writable volume %s", real, vol)
			continue
		}
		if vol, bad := underAny(writable, cand); bad {
			skipped = fmt.Sprintf("%s is in the writable volume %s", cand, vol)
			continue
		}
		return cand, nil
	}
	if skipped != "" {
		return "", fmt.Errorf("refusing to run %s: %s", name, skipped)
	}
	return "", errors.New("command not found on the host")
}
