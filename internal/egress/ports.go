// SPDX-License-Identifier: GPL-3.0-only

// Package egress limits what the jail may connect to. A sidecar container owns
// the jail's network: nftables rejects everything the rules do not allow
// outright, redirects every TCP connection to a transparent proxy that asks
// the host (and so the human) about each destination, and a DNS forwarder
// remembers which names resolved to which addresses. This package holds the
// pieces that run in the sidecar and the matching that the host uses.
package egress

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PortRange is an inclusive range of ports.
type PortRange struct{ Lo, Hi int }

// PortSet is a set of TCP or UDP ports: a list such as "80, 443, 22",
// ranges such as "8000-8080", any mix of them, or "*" for every port.
type PortSet struct {
	any    bool
	ranges []PortRange
}

// AnyPort is the set of every port.
var AnyPort = PortSet{any: true}

// ParsePorts reads a port set. Entries are separated by commas or spaces.
func ParsePorts(s string) (PortSet, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(fields) == 0 {
		return PortSet{}, fmt.Errorf("no ports given (use a list like 80, 443, a range like 8000-8080, or *)")
	}
	var set PortSet
	for _, f := range fields {
		if f == "*" {
			return AnyPort, nil
		}
		lo, hi, isRange := strings.Cut(f, "-")
		l, err := strconv.Atoi(lo)
		h := l
		if err == nil && isRange {
			h, err = strconv.Atoi(hi)
		}
		if err != nil || l < 1 || h > 65535 || l > h {
			return PortSet{}, fmt.Errorf("bad port or range %q (ports go from 1 to 65535, a range is written 1000-2000)", f)
		}
		set.ranges = append(set.ranges, PortRange{l, h})
	}
	set.normalize()
	return set, nil
}

// SinglePort is the set holding one port.
func SinglePort(p int) PortSet { return PortSet{ranges: []PortRange{{p, p}}} }

// normalize sorts the ranges and merges overlapping and adjacent ones.
func (s *PortSet) normalize() {
	sort.Slice(s.ranges, func(i, j int) bool { return s.ranges[i].Lo < s.ranges[j].Lo })
	var out []PortRange
	for _, r := range s.ranges {
		if n := len(out); n > 0 && r.Lo <= out[n-1].Hi+1 {
			out[n-1].Hi = max(out[n-1].Hi, r.Hi)
			continue
		}
		out = append(out, r)
	}
	s.ranges = out
}

// IsAny reports whether the set is every port.
func (s PortSet) IsAny() bool { return s.any }

// IsEmpty reports whether the set has no port (the zero value).
func (s PortSet) IsEmpty() bool { return !s.any && len(s.ranges) == 0 }

// Contains reports whether port is in the set.
func (s PortSet) Contains(port int) bool {
	if s.any {
		return true
	}
	for _, r := range s.ranges {
		if port >= r.Lo && port <= r.Hi {
			return true
		}
	}
	return false
}

// Ranges returns the ranges of a set that is not "any".
func (s PortSet) Ranges() []PortRange { return s.ranges }

// String is the normalized form: "80, 443, 8000-8080", or "*".
func (s PortSet) String() string {
	if s.any {
		return "*"
	}
	parts := make([]string, len(s.ranges))
	for i, r := range s.ranges {
		if r.Lo == r.Hi {
			parts[i] = strconv.Itoa(r.Lo)
		} else {
			parts[i] = fmt.Sprintf("%d-%d", r.Lo, r.Hi)
		}
	}
	return strings.Join(parts, ", ")
}
