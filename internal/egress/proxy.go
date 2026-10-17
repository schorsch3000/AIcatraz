package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Proxy is the transparent TCP proxy: the firewall redirects every TCP
// connection of the jail to it. It finds out where the connection was going,
// asks Decide, and either connects there (with the sidecar's mark, so the
// firewall lets it out) and passes the bytes along, or resets the connection.
type Proxy struct {
	Cache   *NameCache
	Decide  func(ctx context.Context, d Dest) bool
	Mark    int
	OrigDst func(c net.Conn) (netip.AddrPort, error) // where the connection was going; default: the kernel's answer
	Self    netip.AddrPort                           // the proxy's own address: a connection meant for it is refused
	Log     func(format string, a ...any)
}

func (p *Proxy) logf(format string, a ...any) {
	if p.Log != nil {
		p.Log(format, a...)
	}
}

// Serve accepts connections until l is closed.
func (p *Proxy) Serve(ctx context.Context, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go p.handle(ctx, c)
	}
}

func reset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0) // close with a reset: the jail sees "connection refused/reset", not a hang
	}
	c.Close()
}

func (p *Proxy) handle(ctx context.Context, c net.Conn) {
	orig := p.OrigDst
	if orig == nil {
		orig = originalDst
	}
	dst, err := orig(c)
	if err != nil || !dst.IsValid() || dst == p.Self {
		reset(c)
		return
	}
	ip := dst.Addr().Unmap()
	var names []string
	if p.Cache != nil {
		names = p.Cache.Names(ip)
	}
	if !p.Decide(ctx, Dest{IP: ip, Port: int(dst.Port()), Proto: TCP, Names: names}) {
		p.logf("tcp: refused %s", dst)
		reset(c)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	up, err := markedDialer(p.Mark).DialContext(dctx, "tcp", dst.String())
	cancel()
	if err != nil {
		p.logf("tcp: %s: %v", dst, err)
		reset(c)
		return
	}
	splice(c, up)
}

// splice copies both ways until both directions have ended.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// originalDst asks the kernel where a redirected IPv4 connection was going
// (SO_ORIGINAL_DST).
func originalDst(c net.Conn) (netip.AddrPort, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return netip.AddrPort{}, errors.New("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	const soOriginalDst = 80
	var addr syscall.RawSockaddrInet4
	var serr error
	if err := raw.Control(func(fd uintptr) {
		size := uint32(unsafe.Sizeof(addr))
		_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&addr)), uintptr(unsafe.Pointer(&size)), 0)
		if e != 0 {
			serr = e
		}
	}); err != nil {
		return netip.AddrPort{}, err
	}
	if serr != nil {
		return netip.AddrPort{}, serr
	}
	port := addr.Port>>8 | addr.Port<<8 // network byte order
	return netip.AddrPortFrom(netip.AddrFrom4(addr.Addr), port), nil
}

// dialTimeout bounds how long the proxy waits for a destination.
const dialTimeout = 15 * time.Second
