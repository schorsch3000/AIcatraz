package egress

import (
	"fmt"
	"strings"
)

// ProxyPort is the port of the transparent proxy inside the sidecar.
const ProxyPort = 15001

// Ruleset is the nftables ruleset of the sidecar, which is the jail's
// network namespace.
//
// TCP never leaves directly: every connection is redirected to the proxy,
// which asks the host. What goes out as UDP or ICMP is what an address rule
// allows, in the order of the rules; anything else is rejected at once. The
// sidecar's own packets carry the mark and pass. Redirected packets still
// leave through the physical interface when the filter sees them, so
// everything addressed to the loopback range is accepted by address.
// IPv6 is switched off in the namespace; its packets are dropped anyway.
func Ruleset(rules []Rule) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("table inet aicatraz {")
	w("\tchain nat_out {")
	w("\t\ttype nat hook output priority -100; policy accept;")
	w("\t\tmeta mark 0x%x return", Mark)
	w("\t\tip daddr 127.0.0.0/8 return")
	w("\t\tmeta l4proto tcp redirect to :%d", ProxyPort)
	w("\t}")
	w("\tchain filter_out {")
	w("\t\ttype filter hook output priority 0; policy accept;")
	w("\t\tmeta mark 0x%x accept", Mark)
	w("\t\tip daddr 127.0.0.0/8 accept")
	w("\t\tct state established,related accept")
	w("\t\tmeta nfproto ipv6 drop")
	for _, r := range rules {
		if !r.Target.IsAddress() || (r.Decision != Allow && r.Decision != Deny) {
			continue // names need the proxy; asking is only possible for TCP
		}
		verdict := "accept"
		if r.Decision == Deny {
			verdict = "reject"
		}
		addr := "ip daddr " + r.Target.Prefix().String()
		if r.Target.Prefix().Bits() == 32 {
			addr = "ip daddr " + r.Target.Prefix().Addr().String()
		}
		if r.AppliesTo(UDP) {
			match := "meta l4proto udp"
			if !r.Ports.IsEmpty() && !r.Ports.IsAny() {
				match = "udp dport { " + r.Ports.String() + " }"
			}
			w("\t\t%s %s %s", addr, match, verdict)
		}
		if r.AppliesTo(ICMP) {
			w("\t\t%s icmp type echo-request %s", addr, verdict)
		}
	}
	w("\t\treject")
	w("\t}")
	w("}")
	return b.String()
}
