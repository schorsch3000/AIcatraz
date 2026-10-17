// SPDX-License-Identifier: GPL-3.0-only

package egress

import (
	"fmt"
	"net/netip"
	"strings"
)

// Target is what a rule's `to` names: a host name, a name pattern such as
// *.example.com, one IPv4 address, a CIDR range, or * for any destination.
type Target struct {
	raw     string
	any     bool
	name    string // exact name, lower case
	suffix  string // for *.example.com: ".example.com"
	prefix  netip.Prefix
	isRange bool
}

// ParseTarget reads a rule target.
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	t := Target{raw: s}
	switch {
	case s == "":
		return t, fmt.Errorf("a target is needed: a host name, *.domain, an IP address, a CIDR range or *")
	case s == "*":
		t.any = true
		return t, nil
	case strings.Contains(s, ":") && !strings.Contains(s, "/"):
		return t, fmt.Errorf("target %q: IPv6 is switched off in the jail, and ports go in `ports`", s)
	case strings.Contains(s, "/"):
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			return t, fmt.Errorf("target %q: not an IPv4 CIDR range such as 10.0.0.0/8", s)
		}
		t.prefix, t.isRange = p.Masked(), true
		return t, nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		if !a.Is4() {
			return t, fmt.Errorf("target %q: IPv6 is switched off in the jail", s)
		}
		t.prefix, t.isRange = netip.PrefixFrom(a, 32), true
		return t, nil
	}
	if strings.Count(s, ".") == 3 && strings.Trim(s, "0123456789.") == "" {
		return t, fmt.Errorf("target %q is not a valid IP address", s)
	}
	name := strings.ToLower(s)
	if strings.HasPrefix(name, "*.") {
		rest := name[2:]
		if !validName(rest) {
			return t, fmt.Errorf("target %q: not a valid name pattern (use *.example.com)", s)
		}
		t.suffix = "." + rest
		return t, nil
	}
	if !validName(name) {
		return t, fmt.Errorf("target %q: not a host name, IP address or CIDR range", s)
	}
	t.name = name
	return t, nil
}

func validName(n string) bool {
	if n == "" || len(n) > 253 || strings.HasPrefix(n, ".") || strings.Contains(n, "..") || strings.Contains(n, "*") {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return false
		}
	}
	return true
}

// String is the target as written (names lower-cased).
func (t Target) String() string {
	switch {
	case t.name != "":
		return t.name
	case t.suffix != "":
		return "*" + t.suffix
	case t.isRange && t.prefix.Bits() == 32:
		return t.prefix.Addr().String()
	case t.isRange:
		return t.prefix.String()
	}
	return t.raw
}

// IsAddress reports whether the target is an IP address or CIDR range, which
// is what is needed to allow a private or otherwise special address.
func (t Target) IsAddress() bool { return t.isRange }

// IsName reports whether the target is a name or name pattern.
func (t Target) IsName() bool { return t.name != "" || t.suffix != "" }

// IsAny reports whether the target is *.
func (t Target) IsAny() bool { return t.any }

// MatchesName reports whether the name is the target (or matches the pattern;
// *.example.com does not match example.com itself).
func (t Target) MatchesName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	switch {
	case t.any:
		return true
	case t.name != "":
		return name == t.name
	case t.suffix != "":
		return strings.HasSuffix(name, t.suffix) && len(name) > len(t.suffix)
	}
	return false
}

// ContainsIP reports whether the address is the target address or in its range.
func (t Target) ContainsIP(a netip.Addr) bool {
	return t.isRange && t.prefix.Contains(a)
}

// Prefix returns the range of an address or CIDR target.
func (t Target) Prefix() netip.Prefix { return t.prefix }
