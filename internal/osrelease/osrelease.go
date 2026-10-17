// Package osrelease reads os-release files, which describe a Linux distribution.
package osrelease

import (
	"bufio"
	"io"
	"strings"
)

// Parse reads the KEY=value lines of an os-release file. Quotes around values
// are removed.
func Parse(r io.Reader) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		m[k] = strings.Trim(v, `"'`)
	}
	return m
}

// Like returns the distribution's own ID followed by the IDs it says it is
// like (ID_LIKE), in order.
func Like(rel map[string]string) []string {
	var ids []string
	if rel["ID"] != "" {
		ids = append(ids, rel["ID"])
	}
	return append(ids, strings.Fields(rel["ID_LIKE"])...)
}
