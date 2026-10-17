package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"sync"
)

// The sidecar is a data plane without opinions: for every TCP connection and
// every DNS question it asks the host, over a unix socket, and the host
// decides. The messages are JSON lines.
type msg struct {
	Op    string   `json:"op"` // hello, tcp, dns (sidecar to host); reply (host to sidecar)
	ID    uint64   `json:"id,omitempty"`
	IP    string   `json:"ip,omitempty"`
	Port  int      `json:"port,omitempty"`
	Names []string `json:"names,omitempty"`
	Name  string   `json:"name,omitempty"`
	Allow bool     `json:"allow,omitempty"`
}

// Handler is the host's side: the policy.
type Handler interface {
	// DecideTCP says whether a TCP connection may be made. It may take as
	// long as the human takes to answer; ctx ends when the sidecar gives up.
	DecideTCP(ctx context.Context, d Dest) bool
	// DecideDNS says whether a name may be resolved.
	DecideDNS(name string) bool
}

// Server answers the sidecar's questions on the host.
type Server struct {
	Handler Handler
	ready   chan struct{}
	once    sync.Once
}

// NewServer creates a server for h.
func NewServer(h Handler) *Server { return &Server{Handler: h, ready: make(chan struct{})} }

// Ready is closed when the sidecar has said hello: its firewall rules are in
// place and its proxy and DNS forwarder are listening.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Serve handles sidecar connections until l is closed.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wmu sync.Mutex
	reply := func(id uint64, allow bool) {
		b, _ := json.Marshal(msg{Op: "reply", ID: id, Allow: allow})
		wmu.Lock()
		c.Write(append(b, '\n'))
		wmu.Unlock()
	}
	r := bufio.NewReaderSize(c, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var m msg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch m.Op {
		case "hello":
			s.once.Do(func() { close(s.ready) })
		case "tcp":
			ip, err := netip.ParseAddr(m.IP)
			if err != nil {
				reply(m.ID, false)
				continue
			}
			go func() {
				reply(m.ID, s.Handler.DecideTCP(ctx, Dest{IP: ip, Port: m.Port, Proto: TCP, Names: m.Names}))
			}()
		case "dns":
			reply(m.ID, s.Handler.DecideDNS(m.Name))
		}
	}
}

// Client is the sidecar's side. If the connection to the host is lost,
// everything is refused and Done is closed.
type Client struct {
	conn    net.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan bool
	done    chan struct{}
}

// Dial connects to the host's control socket and starts reading replies.
func Dial(sock string) (*Client, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, pending: map[uint64]chan bool{}, done: make(chan struct{})}
	go c.read()
	return c, nil
}

// Done is closed when the host has gone.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close hangs up.
func (c *Client) Close() { c.conn.Close() }

func (c *Client) read() {
	r := bufio.NewReaderSize(c.conn, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break
		}
		var m msg
		if json.Unmarshal(line, &m) != nil || m.Op != "reply" {
			continue
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m.Allow
		}
	}
	close(c.done)
	c.mu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- false
	}
	c.mu.Unlock()
}

func (c *Client) send(m msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.conn.Write(append(b, '\n'))
	return err
}

// Hello tells the host that the sidecar is ready.
func (c *Client) Hello() error { return c.send(msg{Op: "hello"}) }

// ask sends a question and waits for the verdict. A lost connection, or ctx
// ending, is a refusal.
func (c *Client) ask(ctx context.Context, m msg) bool {
	ch := make(chan bool, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return false
	default:
	}
	c.next++
	m.ID = c.next
	c.pending[m.ID] = ch
	c.mu.Unlock()
	if err := c.send(m); err != nil {
		return false
	}
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		return false
	case <-c.done:
		return false
	}
}

// DecideTCP asks the host about a connection.
func (c *Client) DecideTCP(ctx context.Context, d Dest) bool {
	return c.ask(ctx, msg{Op: "tcp", IP: d.IP.String(), Port: d.Port, Names: d.Names})
}

// DecideDNS asks the host about a name.
func (c *Client) DecideDNS(name string) bool {
	return c.ask(context.Background(), msg{Op: "dns", Name: name})
}
