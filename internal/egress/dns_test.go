// SPDX-License-Identifier: GPL-3.0-only

package egress

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func encName(name string) []byte {
	var b []byte
	for _, l := range strings.Split(name, ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

// answerFor builds a response to query with the given records appended: each
// is {owner name pointer or name, type, ttl, rdata}.
func answerFor(query []byte, recs ...[]byte) []byte {
	out := append([]byte(nil), query...)
	out[2], out[3] = 0x81, 0x80
	binary.BigEndian.PutUint16(out[6:8], uint16(len(recs)))
	for _, r := range recs {
		out = append(out, r...)
	}
	return out
}

func rrA(ttl uint32, ip string) []byte {
	b := []byte{0xc0, 0x0c, 0, TypeA, 0, 1}
	b = binary.BigEndian.AppendUint32(b, ttl)
	b = append(b, 0, 4)
	a := netip.MustParseAddr(ip).As4()
	return append(b, a[:]...)
}

func rrCNAME(owner []byte, ttl uint32, target string) []byte {
	b := append([]byte(nil), owner...)
	b = append(b, 0, TypeCNAME, 0, 1)
	b = binary.BigEndian.AppendUint32(b, ttl)
	t := encName(target)
	b = binary.BigEndian.AppendUint16(b, uint16(len(t)))
	return append(b, t...)
}

func TestDNSMessageParsing(t *testing.T) {
	q := BuildQuery(0x1234, "WWW.Example.com", TypeA)
	got, err := ParseQuery(q)
	if err != nil || got.ID != 0x1234 || got.Name != "www.example.com" || got.Type != TypeA {
		t.Fatalf("%+v %v", got, err)
	}
	// A response with a CNAME chain; the owner names use compression pointers.
	resp := answerFor(q,
		rrCNAME([]byte{0xc0, 0x0c}, 300, "edge.cdn.net"),
		append([]byte{0xc0, 0x0c + 0}, rrA(60, "93.184.216.34")[2:]...), // owner: pointer to the question
	)
	_, rcode, recs, err := ParseResponse(resp)
	if err != nil || rcode != 0 || len(recs) != 2 {
		t.Fatalf("%v %d %+v", err, rcode, recs)
	}
	if recs[0].Type != TypeCNAME || recs[0].Alias != "edge.cdn.net" || recs[0].Name != "www.example.com" {
		t.Errorf("cname %+v", recs[0])
	}
	if recs[1].Type != TypeA || recs[1].IP != netip.MustParseAddr("93.184.216.34") || recs[1].TTL != 60 {
		t.Errorf("a %+v", recs[1])
	}
	// Hostile or broken messages never panic.
	for _, bad := range [][]byte{nil, {1, 2, 3}, q[:13], q[:len(q)-2], append(append([]byte(nil), q[:12]...), 0xc0, 0x0c, 0, 1, 0, 1)} {
		ParseQuery(bad)
		ParseResponse(bad)
	}
	loop := append(append([]byte(nil), q[:12]...), 0xc0, 12, 0, 1, 0, 1)
	if _, err := ParseQuery(loop); err == nil {
		t.Error("a compression loop must be an error")
	}
	r := reply(q, rcodeNXDomain)
	if _, rc, recs, err := ParseResponse(r); err != nil || rc != rcodeNXDomain || len(recs) != 0 || r[2]&0x80 == 0 {
		t.Errorf("nxdomain reply: %v %d %v %x", err, rc, recs, r[:4])
	}
}

func TestNameCache(t *testing.T) {
	c := NewNameCache()
	now := time.Now()
	c.now = func() time.Time { return now }
	ip := netip.MustParseAddr("93.184.216.34")
	c.Add("www.example.com", ip, 30*time.Second)
	c.Add("edge.cdn.net", ip, time.Hour)
	if got := c.Names(ip); len(got) != 2 || got[0] != "edge.cdn.net" || got[1] != "www.example.com" {
		t.Errorf("%v", got)
	}
	now = now.Add(6 * time.Minute) // the short TTL is stretched to five minutes, then it is gone
	if got := c.Names(ip); len(got) != 1 || got[0] != "edge.cdn.net" {
		t.Errorf("after the short entry expired: %v", got)
	}
	now = now.Add(2 * time.Hour)
	if got := c.Names(ip); len(got) != 0 {
		t.Errorf("everything expired: %v", got)
	}
	if got := c.Names(netip.MustParseAddr("1.1.1.1")); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

// fakeUpstream answers every query with respond(query).
func fakeUpstream(t *testing.T, respond func(q []byte) []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := respond(append([]byte(nil), buf[:n]...)); r != nil {
				pc.WriteTo(r, addr)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func ask(t *testing.T, addr string, q []byte) []byte {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(q)
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func TestForwarder(t *testing.T) {
	var upstreamQueries atomic.Int32
	up := fakeUpstream(t, func(q []byte) []byte {
		upstreamQueries.Add(1)
		return answerFor(q, rrCNAME([]byte{0xc0, 0x0c}, 300, "edge.cdn.net"), append([]byte{0xc0, 0x0c}, rrA(60, "93.184.216.34")[2:]...))
	})
	cache := NewNameCache()
	f := &Forwarder{Upstreams: []string{up}, Cache: cache, Allow: func(n string) bool { return n != "blocked.example.com" }}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.ServeUDP(ctx, pc)

	resp := ask(t, pc.LocalAddr().String(), BuildQuery(7, "www.example.com", TypeA))
	if _, rcode, recs, err := ParseResponse(resp); err != nil || rcode != 0 || len(recs) != 2 {
		t.Fatalf("answer: %v %d %v", err, rcode, recs)
	}
	ip := netip.MustParseAddr("93.184.216.34")
	if got := cache.Names(ip); len(got) != 2 || got[0] != "edge.cdn.net" || got[1] != "www.example.com" {
		t.Errorf("the cache must hold the name and the cname target: %v", got)
	}

	before := upstreamQueries.Load()
	resp = ask(t, pc.LocalAddr().String(), BuildQuery(8, "blocked.example.com", TypeA))
	if _, rcode, recs, _ := ParseResponse(resp); rcode != rcodeNXDomain || len(recs) != 0 || upstreamQueries.Load() != before {
		t.Errorf("a refused name is NXDOMAIN and never asked upstream: rcode %d, %d upstream queries", rcode, upstreamQueries.Load()-before)
	}
	resp = ask(t, pc.LocalAddr().String(), BuildQuery(9, "www.example.com", TypeAAAA))
	if _, rcode, recs, _ := ParseResponse(resp); rcode != 0 || len(recs) != 0 || upstreamQueries.Load() != before {
		t.Errorf("AAAA is answered with nothing: rcode %d, %d records, %d upstream queries", rcode, len(recs), upstreamQueries.Load()-before)
	}

	// Over TCP too.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go f.ServeTCP(ctx, l)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	writeTCPDNS(c, BuildQuery(10, "tcp.example.com", TypeA))
	r, err := readTCPDNS(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, rcode, recs, _ := ParseResponse(r); rcode != 0 || len(recs) != 2 {
		t.Errorf("tcp answer: %d %v", rcode, recs)
	}
}

func TestForwarderUpstreamDown(t *testing.T) {
	f := &Forwarder{Upstreams: []string{"127.0.0.1:1"}, Timeout: 200 * time.Millisecond}
	if got := f.Answer(context.Background(), BuildQuery(1, "x.example.com", TypeA)); got != nil {
		t.Errorf("with no upstream answering there is no answer: %x", got)
	}
	if got := f.Answer(context.Background(), []byte{1, 2, 3}); got != nil {
		t.Error("garbage gets no answer")
	}
}
