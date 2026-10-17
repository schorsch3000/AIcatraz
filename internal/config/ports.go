// SPDX-License-Identifier: GPL-3.0-only

package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// PortMap publishes a port of the jail on the host: `3000`, `8080:80`,
// `127.0.0.1:8080:80`, `0.0.0.0:8080:80/tcp`, or ranges of equal length such as
// `8000-8010:8000-8010`.
type PortMap struct {
	HostIP    netip.Addr
	HostLo    int
	HostHi    int
	ContainLo int
	ContainHi int
	Proto     string // tcp or udp
	Spec      string // as written, for messages
}

// Loopback reports whether the port is published on the loopback address only.
func (p PortMap) Loopback() bool { return p.HostIP.IsLoopback() }

// DockerFlag is the argument of `docker run -p`.
func (p PortMap) DockerFlag() string {
	host, cont := portRange(p.HostLo, p.HostHi), portRange(p.ContainLo, p.ContainHi)
	return fmt.Sprintf("%s:%s:%s/%s", p.HostIP, host, cont, p.Proto)
}

func portRange(lo, hi int) string {
	if lo == hi {
		return strconv.Itoa(lo)
	}
	return fmt.Sprintf("%d-%d", lo, hi)
}

func parsePortRange(s string) (lo, hi int, err error) {
	a, b, isRange := strings.Cut(s, "-")
	if lo, err = strconv.Atoi(a); err != nil {
		return 0, 0, fmt.Errorf("%q is not a port", s)
	}
	hi = lo
	if isRange {
		if hi, err = strconv.Atoi(b); err != nil {
			return 0, 0, fmt.Errorf("%q is not a port range", s)
		}
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, fmt.Errorf("%q: ports go from 1 to 65535 and a range runs upwards", s)
	}
	return lo, hi, nil
}

// ParsePortMap reads one `ports` entry. Without an address the port is
// published on 127.0.0.1 only: reachable from this machine, not from others.
func ParsePortMap(spec string) (PortMap, error) {
	pm := PortMap{HostIP: netip.MustParseAddr("127.0.0.1"), Proto: "tcp", Spec: spec}
	body := strings.TrimSpace(spec)
	if i := strings.LastIndex(body, "/"); i >= 0 {
		switch p := strings.ToLower(body[i+1:]); p {
		case "tcp", "udp":
			pm.Proto = p
		default:
			return pm, fmt.Errorf("unknown protocol %q (use tcp or udp)", body[i+1:])
		}
		body = body[:i]
	}
	parts := strings.Split(body, ":")
	var host, cont string
	switch len(parts) {
	case 1:
		host, cont = parts[0], parts[0]
	case 2:
		host, cont = parts[0], parts[1]
	case 3:
		a, err := netip.ParseAddr(parts[0])
		if err != nil || !a.Is4() {
			return pm, fmt.Errorf("%q is not an IPv4 address (IPv6 is switched off)", parts[0])
		}
		pm.HostIP = a
		host, cont = parts[1], parts[2]
	default:
		return pm, fmt.Errorf("%q: write it as port, host:container or address:host:container", spec)
	}
	var err error
	if pm.HostLo, pm.HostHi, err = parsePortRange(host); err != nil {
		return pm, err
	}
	if pm.ContainLo, pm.ContainHi, err = parsePortRange(cont); err != nil {
		return pm, err
	}
	if pm.HostHi-pm.HostLo != pm.ContainHi-pm.ContainLo {
		return pm, fmt.Errorf("%q: the host and container ranges must be as long as each other", spec)
	}
	return pm, nil
}
