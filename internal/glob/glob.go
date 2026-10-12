// Package glob compiles path patterns: `*` and `?` stay within one path
// segment, `**` crosses segments, `[...]` is a class (`[!...]` negated), `\`
// escapes. This is the syntax of .dockerignore and of depends_on patterns.
package glob

import (
	"fmt"
	"regexp"
	"strings"
)

// Compile turns a pattern into an anchored regular expression over
// slash-separated paths.
func Compile(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?") // **/ matches zero or more directories
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(g[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("unterminated character class")
			}
			class := g[i+1 : i+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += j
		case '\\':
			if i+1 < len(g) {
				i++
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// HasMeta reports whether the pattern contains wildcard syntax.
func HasMeta(g string) bool { return strings.ContainsAny(g, "*?[\\") }

// StaticPrefix returns the longest leading run of path segments without
// wildcards, so a walk can start there.
func StaticPrefix(g string) string {
	segs := strings.Split(g, "/")
	var keep []string
	for _, s := range segs {
		if HasMeta(s) {
			break
		}
		keep = append(keep, s)
	}
	p := strings.Join(keep, "/")
	if p == "" && strings.HasPrefix(g, "/") {
		return "/"
	}
	return p
}

// Escape makes s match itself literally when used inside a pattern.
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("*?[\\", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
