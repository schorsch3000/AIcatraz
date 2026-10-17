package egress

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer replies "echo:" plus what it reads, then closes.
func echoServer(t *testing.T) netip.AddrPort {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("echo:"), b...))
			}()
		}
	}()
	return netip.MustParseAddrPort(l.Addr().String())
}

func startProxy(t *testing.T, target netip.AddrPort, decide func(ctx context.Context, d Dest) bool, cache *NameCache) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	p := &Proxy{
		Cache: cache, Decide: decide,
		OrigDst: func(net.Conn) (netip.AddrPort, error) { return target, nil },
		Self:    netip.MustParseAddrPort("127.0.0.1:1"),
	}
	go p.Serve(context.Background(), l)
	return l.Addr().String()
}

func TestProxyAllowsAndPassesBytes(t *testing.T) {
	target := echoServer(t)
	cache := NewNameCache()
	cache.Add("service.example.com", target.Addr(), time.Hour)
	seen := make(chan Dest, 1)
	addr := startProxy(t, target, func(ctx context.Context, d Dest) bool { seen <- d; return true }, cache)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("hello"))
	c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	if string(b) != "echo:hello" {
		t.Errorf("got %q", b)
	}
	got := <-seen
	if got.IP != target.Addr() || got.Port != int(target.Port()) || got.Proto != TCP || len(got.Names) != 1 || got.Names[0] != "service.example.com" {
		t.Errorf("the decision saw %+v", got)
	}
}

func TestProxyResetsARefusedConnection(t *testing.T) {
	target := echoServer(t)
	addr := startProxy(t, target, func(ctx context.Context, d Dest) bool { return false }, nil)
	c, err := net.Dial("tcp", addr)
	if err != nil { // the reset can beat the end of the handshake
		if !strings.Contains(err.Error(), "reset") {
			t.Errorf("dial: %v", err)
		}
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("hello"))
	// Usually a reset; on a fast loopback sometimes a plain end of stream. Never data, never a hang.
	b, err := io.ReadAll(c)
	if len(b) != 0 || (err != nil && !strings.Contains(err.Error(), "reset")) {
		t.Errorf("a refused connection must end at once without data: %q %v", b, err)
	}
}

func TestProxyHoldsTheConnectionWhileAsking(t *testing.T) {
	target := echoServer(t)
	release := make(chan bool)
	asked := make(chan struct{}, 1)
	addr := startProxy(t, target, func(ctx context.Context, d Dest) bool { asked <- struct{}{}; return <-release }, nil)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("early bytes sent while the human thinks"))
	c.(*net.TCPConn).CloseWrite()
	<-asked
	time.Sleep(200 * time.Millisecond)
	release <- true
	b, _ := io.ReadAll(c)
	if string(b) != "echo:early bytes sent while the human thinks" {
		t.Errorf("got %q", b)
	}
}

func TestProxyRefusesConnectionsToItself(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	self := netip.MustParseAddrPort(l.Addr().String())
	var asked atomic.Bool
	p := &Proxy{Decide: func(ctx context.Context, d Dest) bool { asked.Store(true); return true },
		OrigDst: func(net.Conn) (netip.AddrPort, error) { return self, nil }, Self: self}
	go p.Serve(context.Background(), l)
	c, err := net.Dial("tcp", l.Addr().String())
	if err == nil {
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		io.ReadAll(c) // reset or end of stream
	}
	time.Sleep(100 * time.Millisecond)
	if asked.Load() {
		t.Error("a connection to the proxy itself must be refused without asking")
	}
}

type fakeHandler struct {
	mu   sync.Mutex
	tcp  []Dest
	dns  []string
	deny map[string]bool
}

func (h *fakeHandler) DecideTCP(ctx context.Context, d Dest) bool {
	h.mu.Lock()
	h.tcp = append(h.tcp, d)
	h.mu.Unlock()
	if d.Port == 99 { // slow
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
	}
	return d.Port != 25
}
func (h *fakeHandler) DecideDNS(name string) bool {
	h.mu.Lock()
	h.dns = append(h.dns, name)
	h.mu.Unlock()
	return !h.deny[name]
}

func TestControlServerAndClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHandler{deny: map[string]bool{"bad.example.com": true}}
	srv := NewServer(h)
	go srv.Serve(l)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-srv.Ready():
		t.Fatal("not ready before hello")
	default:
	}
	c.Hello()
	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("hello was not noticed")
	}

	ctx := context.Background()
	d := Dest{IP: netip.MustParseAddr("93.184.216.34"), Port: 443, Proto: TCP, Names: []string{"a.example.com"}}
	if !c.DecideTCP(ctx, d) {
		t.Error("allowed")
	}
	d.Port = 25
	if c.DecideTCP(ctx, d) {
		t.Error("refused")
	}
	if !c.DecideDNS("ok.example.com") || c.DecideDNS("bad.example.com") {
		t.Error("dns verdicts")
	}
	h.mu.Lock()
	if len(h.tcp) != 2 || h.tcp[0].IP != d.IP || h.tcp[0].Names[0] != "a.example.com" || h.tcp[0].Port != 443 {
		t.Errorf("the host saw %+v", h.tcp)
	}
	h.mu.Unlock()

	// Many questions at once, answered out of order.
	var wg sync.WaitGroup
	results := make([]bool, 20)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dd := d
			dd.Port = 80 + i%2*19 // 80 or 99 (slow)
			results[i] = c.DecideTCP(ctx, dd)
		}()
	}
	wg.Wait()
	for i, r := range results {
		if !r {
			t.Errorf("question %d was refused", i)
		}
	}

	// The host's connection ending refuses everything.
	d.Port = 99
	l.Close()
	c.conn.Close()
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done must close when the connection ends")
	}
	if c.DecideTCP(ctx, d) || c.DecideDNS("x") {
		t.Error("with the host gone everything is refused")
	}
}

func TestRulesetText(t *testing.T) {
	r := func(to, ports string, d Decision, protos ...Proto) Rule { return rule(t, to, ports, d, protos...) }
	text := Ruleset([]Rule{
		r("10.0.0.5", "53", Deny, UDP),
		r("10.0.0.0/8", "53, 8000-8010", Allow, UDP),
		r("203.0.113.9", "", Allow, UDP, ICMP),
		r("example.com", "", Allow, UDP), // names can't be allowed for UDP: skipped
		r("example.org", "", Allow),      // by default a name is tcp only
		r("198.51.100.0/24", "", Ask, UDP),
		r("*", "", Allow, UDP),
	})
	for _, want := range []string{
		"type nat hook output priority -100",
		"meta mark 0x1 return",
		"meta l4proto tcp redirect to :15001",
		"meta mark 0x1 accept",
		"ip daddr 127.0.0.0/8 accept",
		"ct state established,related accept",
		"meta nfproto ipv6 drop",
		"ip daddr 10.0.0.5 udp dport { 53 } reject",
		"ip daddr 10.0.0.0/8 udp dport { 53, 8000-8010 } accept",
		"ip daddr 203.0.113.9 meta l4proto udp accept",
		"ip daddr 203.0.113.9 icmp type echo-request accept",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ruleset lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"example.com", "198.51.100", "ip daddr 0.0.0.0"} {
		if strings.Contains(text, not) {
			t.Errorf("ruleset must not mention %q:\n%s", not, text)
		}
	}
	// The order is the order of the rules: the deny comes before the allow that covers it.
	if strings.Index(text, "10.0.0.5") > strings.Index(text, "10.0.0.0/8") {
		t.Error("rules must keep their order")
	}
	// The last rule rejects whatever nothing allowed.
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if strings.TrimSpace(lines[len(lines)-3]) != "reject" {
		t.Errorf("the chain must end in reject:\n%s", text)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "r.nft"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
