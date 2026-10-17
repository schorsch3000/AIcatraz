// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/ui"
)

// askIgnore returns the question asked for every hidden path that an ignore
// file does not exclude. Only an explicit yes counts; anything else, end of
// input included, is a no.
func askIgnore(in io.Reader, out io.Writer) func(IgnoreGap) bool {
	st := ui.For(out)
	return func(g IgnoreGap) bool {
		fmt.Fprintf(out, "%s hidden path %s is inside the build context, ", st.BoldYellow(ui.Prog+":"), st.Bold(g.Rel))
		if g.Exists {
			fmt.Fprintf(out, "but %s does not exclude it.\n", filepath.Base(g.File))
			fmt.Fprintf(out, "  Add %s to %s so it can't end up in the image? [y/N] ", st.Bold(g.Rel), st.Bold(g.File))
		} else {
			fmt.Fprintf(out, "but there is no %s.\n", filepath.Base(g.File))
			fmt.Fprintf(out, "  Create %s containing %s so it can't end up in the image? [y/N] ", st.Bold(g.File), st.Bold(g.Rel))
		}
		ans := readLine(in)
		fmt.Fprintln(out)
		return strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes")
	}
}

// readLine reads up to a newline without reading past it, so input meant for
// the session that follows stays unread.
func readLine(r io.Reader) string {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
