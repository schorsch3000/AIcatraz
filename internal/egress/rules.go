package egress

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Decision is what a rule says about a connection.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

// Proto is a transport protocol a rule applies to.
type Proto string

const (
	TCP  Proto = "tcp"
	UDP  Proto = "udp"
	ICMP Proto = "icmp"
)

// Rule is one line of the network policy.
type Rule struct {
	Target   Target
	Ports    PortSet // empty means every port
	Protos   []Proto // empty means tcp and udp for an address, tcp for a name or *
	Decision Decision
}

// Dest is a connection the jail wants to make.
type Dest struct {
	IP    netip.Addr
	Port  int
	Proto Proto
	Names []string // names the DNS forwarder resolved to IP
}

// ParseProtos reads "tcp", "udp", "icmp" or a list of them.
func ParseProtos(in []string) ([]Proto, error) {
	var out []Proto
	for _, s := range in {
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
			switch p := Proto(strings.ToLower(f)); p {
			case TCP, UDP, ICMP:
				out = append(out, p)
			default:
				return nil, fmt.Errorf("unknown protocol %q (use tcp, udp or icmp)", f)
			}
		}
	}
	return out, nil
}

// IsSpecial reports whether an address is not a public one: loopback, the
// private ranges, link-local (which holds the cloud metadata address), the
// shared address space, multicast and the like. A host name can never be
// used to allow such an address, only an explicit IP or CIDR rule can, so DNS
// rebinding can't point an allowed name at the local network.
func IsSpecial(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return true
	}
	if a.Is4() {
		b := a.As4()
		switch {
		case b[0] == 100 && b[1]&0xc0 == 64: // 100.64.0.0/10 shared address space
			return true
		case b[0] == 192 && b[1] == 0 && b[2] == 0: // 192.0.0.0/24
			return true
		case b[0] >= 240: // reserved and broadcast
			return true
		}
	}
	return false
}

func (r Rule) protos() []Proto {
	switch {
	case len(r.Protos) > 0:
		return r.Protos
	case r.Target.IsAddress():
		return []Proto{TCP, UDP}
	}
	return []Proto{TCP} // names and * only mean something for TCP: that is where a name is known
}

// AppliesTo reports whether the rule covers the protocol.
func (r Rule) AppliesTo(p Proto) bool {
	for _, x := range r.protos() {
		if x == p {
			return true
		}
	}
	return false
}

// Matches reports whether the rule covers the connection.
func (r Rule) Matches(d Dest) bool {
	if !r.AppliesTo(d.Proto) {
		return false
	}
	if d.Proto != ICMP && !r.Ports.IsEmpty() && !r.Ports.Contains(d.Port) {
		return false
	}
	t := r.Target
	switch {
	case t.IsAddress():
		return t.ContainsIP(d.IP)
	case IsSpecial(d.IP):
		return false // only an address rule may allow (or deny) these
	case t.IsAny():
		return true
	}
	for _, n := range d.Names {
		if t.MatchesName(n) {
			return true
		}
	}
	return false
}

// Evaluate returns the decision of the first rule that matches, and that
// rule, or def and nil if none does.
func Evaluate(rules []Rule, d Dest, def Decision) (Decision, *Rule) {
	for i := range rules {
		if rules[i].Matches(d) {
			return rules[i].Decision, &rules[i]
		}
	}
	return def, nil
}

// ResolveName decides whether the DNS forwarder resolves a name. Only an
// explicit deny rule for the name (on every port) refuses it; with restricted
// set, a name must also be covered by an allow or ask rule.
func ResolveName(rules []Rule, name string, restricted bool) bool {
	covered := false
	for _, r := range rules {
		if !r.Target.IsName() && !r.Target.IsAny() || !r.Target.MatchesName(name) {
			continue
		}
		if r.Decision == Deny {
			if r.Ports.IsEmpty() || r.Ports.IsAny() {
				return false
			}
			continue
		}
		covered = true
	}
	return covered || !restricted
}

// FormatRule shows a rule the way it is written in the config.
func FormatRule(r Rule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Decision, r.Target)
	if !r.Ports.IsEmpty() && !r.Ports.IsAny() {
		fmt.Fprintf(&b, " ports %s", r.Ports)
	}
	if len(r.Protos) > 0 {
		ps := make([]string, len(r.Protos))
		for i, p := range r.Protos {
			ps[i] = string(p)
		}
		sort.Strings(ps)
		fmt.Fprintf(&b, " proto %s", strings.Join(ps, ","))
	}
	return b.String()
}
