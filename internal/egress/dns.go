// SPDX-License-Identifier: GPL-3.0-only

package egress

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// NameCache remembers which names resolved to which addresses, so a
// connection to an address can be shown, and judged, by name.
type NameCache struct {
	mu      sync.Mutex
	entries map[netip.Addr]map[string]time.Time // address -> name -> expiry
	now     func() time.Time
}

const (
	minKeep    = 5 * time.Minute // apps keep addresses longer than the TTL says
	maxKeep    = time.Hour
	maxEntries = 20000
)

// NewNameCache creates an empty cache.
func NewNameCache() *NameCache {
	return &NameCache{entries: map[netip.Addr]map[string]time.Time{}, now: time.Now}
}

// Add records that name resolved to ip, with the TTL of the answer.
func (c *NameCache) Add(name string, ip netip.Addr, ttl time.Duration) {
	ttl = min(max(ttl, minKeep), maxKeep)
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.entries[ip]
	if m == nil {
		if len(c.entries) >= maxEntries {
			c.expire()
			if len(c.entries) >= maxEntries {
				return
			}
		}
		m = map[string]time.Time{}
		c.entries[ip] = m
	}
	m[name] = c.now().Add(ttl)
}

func (c *NameCache) expire() {
	now := c.now()
	for ip, m := range c.entries {
		for n, exp := range m {
			if now.After(exp) {
				delete(m, n)
			}
		}
		if len(m) == 0 {
			delete(c.entries, ip)
		}
	}
}

// Names returns the names that currently resolve to ip, sorted.
func (c *NameCache) Names(ip netip.Addr) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	now := c.now()
	for n, exp := range c.entries[ip] {
		if now.Before(exp) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Forwarder is the jail's DNS server. It forwards questions to the real
// resolvers, remembers what they answer, refuses the names the policy
// refuses, and answers AAAA questions with nothing: the jail has no IPv6.
type Forwarder struct {
	Upstreams []string               // "host:port" of the real resolvers
	Mark      int                    // socket mark for the forwarder's own traffic
	Allow     func(name string) bool // may this name be resolved?
	Cache     *NameCache
	Log       func(format string, a ...any)
	Timeout   time.Duration
}

func (f *Forwarder) logf(format string, a ...any) {
	if f.Log != nil {
		f.Log(format, a...)
	}
}

// Answer produces the response to one query message, or nil if the message
// is not worth answering.
func (f *Forwarder) Answer(ctx context.Context, query []byte) []byte {
	q, err := ParseQuery(query)
	if err != nil {
		return nil
	}
	if f.Allow != nil && !f.Allow(q.Name) {
		f.logf("dns: refused %s", q.Name)
		return reply(query, rcodeNXDomain)
	}
	if q.Type == TypeAAAA {
		return reply(query, rcodeOK)
	}
	resp := f.forward(ctx, query)
	if resp == nil {
		return nil
	}
	if _, rcode, recs, err := ParseResponse(resp); err == nil && rcode == rcodeOK {
		names := map[string]bool{q.Name: true}
		for _, r := range recs {
			if r.Type == TypeCNAME {
				names[r.Name] = true
				names[r.Alias] = true
			}
		}
		for _, r := range recs {
			if r.Type == TypeA && r.IP.IsValid() && f.Cache != nil {
				for n := range names {
					f.Cache.Add(n, r.IP, time.Duration(r.TTL)*time.Second)
				}
			}
		}
	}
	return resp
}

func (f *Forwarder) forward(ctx context.Context, query []byte) []byte {
	timeout := f.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	d := markedDialer(f.Mark)
	for _, up := range f.Upstreams {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := d.DialContext(cctx, "udp", up)
		if err != nil {
			cancel()
			continue
		}
		conn.SetDeadline(time.Now().Add(timeout))
		var resp []byte
		if _, err = conn.Write(query); err == nil {
			buf := make([]byte, 4096)
			var n int
			if n, err = conn.Read(buf); err == nil {
				resp = buf[:n]
			}
		}
		conn.Close()
		cancel()
		if resp != nil && len(resp) >= 12 && resp[0] == query[0] && resp[1] == query[1] {
			if resp[2]&0x02 != 0 { // truncated: ask again over TCP
				if full := f.forwardTCP(ctx, up, query, timeout); full != nil {
					return full
				}
			}
			return resp
		}
	}
	return nil
}

func (f *Forwarder) forwardTCP(ctx context.Context, up string, query []byte, timeout time.Duration) []byte {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := markedDialer(f.Mark).DialContext(cctx, "tcp", up)
	if err != nil {
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if err := writeTCPDNS(conn, query); err != nil {
		return nil
	}
	resp, err := readTCPDNS(conn)
	if err != nil {
		return nil
	}
	return resp
}

func writeTCPDNS(w io.Writer, msg []byte) error {
	b := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(b, uint16(len(msg)))
	copy(b[2:], msg)
	_, err := w.Write(b)
	return err
}

func readTCPDNS(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err := io.ReadFull(r, msg)
	return msg, err
}

// ServeUDP answers queries on pc until it is closed.
func (f *Forwarder) ServeUDP(ctx context.Context, pc net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		query := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := f.Answer(ctx, query); resp != nil {
				pc.WriteTo(resp, addr)
			}
		}()
	}
}

// ServeTCP answers queries on l until it is closed.
func (f *Forwarder) ServeTCP(ctx context.Context, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			for {
				c.SetReadDeadline(time.Now().Add(10 * time.Second))
				query, err := readTCPDNS(c)
				if err != nil {
					return
				}
				resp := f.Answer(ctx, query)
				if resp == nil || writeTCPDNS(c, resp) != nil {
					return
				}
			}
		}()
	}
}
