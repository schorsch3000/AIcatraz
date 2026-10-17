// SPDX-License-Identifier: GPL-3.0-only

// Package proto is the wire protocol between the in-container shim and the
// host broker: length-prefixed frames over a unix stream socket.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Version is bumped on incompatible protocol changes.
const Version = 2

// Frame types. Control frames carry JSON, data frames raw bytes.
type Type byte

const (
	THello    Type = 'H' // shim -> broker: Hello
	TDecision Type = 'D' // broker -> shim: Decision
	TStdin    Type = 'i' // shim -> broker: data
	TStdinEOF Type = 'e' // shim -> broker
	TStdout   Type = 'o' // broker -> shim: data (also the pty stream)
	TStderr   Type = 'x' // broker -> shim: data
	TSignal   Type = 's' // shim -> broker: one byte, the signal number
	TResize   Type = 'r' // shim -> broker: rows uint16, cols uint16
	TPing     Type = 'p' // both ways
	TWindow   Type = 'w' // broker -> shim: credit, uint32 bytes of stdin it may send
	TExit     Type = 'X' // broker -> shim: Exit
)

const (
	// MaxFrame bounds a single frame so a peer can't make the other allocate without limit.
	MaxFrame = 1 << 20
	// ChunkSize is the size data is split into.
	ChunkSize = 32 << 10
	// StdinWindow is how much stdin the shim may have in flight. The broker
	// grants more as the command consumes it, so a command that doesn't read
	// its stdin can never make the broker block or buffer without limit.
	StdinWindow = 256 << 10
)

// Heartbeat timing. Both sides send a ping every Interval; a peer that has
// been silent for Timeout is considered gone. Variables so tests can shorten them.
var (
	Interval = 2 * time.Second
	Timeout  = 7 * time.Second
)

// Hello is the shim's request. Everything in it is a request, never a fact:
// the broker validates and decides.
type Hello struct {
	Version int      `json:"version"`
	Name    string   `json:"name"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
	Env     []string `json:"env"`
	TTY     bool     `json:"tty"`
	Rows    uint16   `json:"rows"`
	Cols    uint16   `json:"cols"`
}

// Decision actions.
const (
	ActionRun   = "run"   // the broker runs the command on the host
	ActionLocal = "local" // the shim runs the container's own binary
	ActionDeny  = "deny"
)

// Decision is the broker's answer to a Hello.
type Decision struct {
	Action  string `json:"action"`
	Message string `json:"message,omitempty"`
}

// Exit reports how the command ended.
type Exit struct {
	Code   int `json:"code"`
	Signal int `json:"signal,omitempty"` // non-zero if killed by a signal
}

// Conn wraps a connection with frame reading and writing.
type Conn struct {
	c        net.Conn
	wmu      sync.Mutex
	lastRead atomic.Int64 // unix nanoseconds of the last frame received
}

func New(c net.Conn) *Conn {
	x := &Conn{c: c}
	x.lastRead.Store(time.Now().UnixNano())
	return x
}

func (c *Conn) Close() error { return c.c.Close() }

// Write sends one frame. Safe for concurrent use.
func (c *Conn) Write(t Type, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("frame of %d bytes exceeds the limit", len(payload))
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = byte(t)
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)

	c.wmu.Lock()
	defer c.wmu.Unlock()
	for len(buf) > 0 {
		_ = c.c.SetWriteDeadline(time.Now().Add(Timeout))
		n, err := c.c.Write(buf)
		buf = buf[n:]
		if err == nil {
			continue
		}
		// A full socket buffer means the peer is slow, not dead, as long as we
		// keep hearing from it (it pings): keep waiting. Silence is a failure.
		if IsTimeout(err) && time.Since(time.Unix(0, c.lastRead.Load())) < Timeout {
			continue
		}
		return err
	}
	return nil
}

// WriteJSON sends a control frame.
func (c *Conn) WriteJSON(t Type, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(t, b)
}

// Read returns the next frame. It fails if the peer is silent for longer than
// Timeout, which is how a dead peer is noticed.
func (c *Conn) Read() (Type, []byte, error) {
	_ = c.c.SetReadDeadline(time.Now().Add(Timeout))
	var hdr [5]byte
	if _, err := io.ReadFull(c.c, hdr[:]); err != nil {
		return 0, nil, err
	}
	c.lastRead.Store(time.Now().UnixNano())
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the limit", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.c, payload); err != nil {
		return 0, nil, err
	}
	c.lastRead.Store(time.Now().UnixNano())
	return Type(hdr[0]), payload, nil
}

// ReadJSON reads a frame that must be of type want and decodes it into v.
func (c *Conn) ReadJSON(want Type, v any) error {
	t, p, err := c.Read()
	if err != nil {
		return err
	}
	if t != want {
		return fmt.Errorf("unexpected frame %q, want %q", t, want)
	}
	return json.Unmarshal(p, v)
}

// KeepAlive sends pings until stop is closed.
func (c *Conn) KeepAlive(stop <-chan struct{}) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := c.Write(TPing, nil); err != nil {
				return
			}
		}
	}
}

// IsTimeout reports whether err is a read deadline expiring.
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ResizePayload encodes a terminal size.
func ResizePayload(rows, cols uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], rows)
	binary.BigEndian.PutUint16(b[2:4], cols)
	return b
}

// ParseResize decodes a terminal size.
func ParseResize(b []byte) (rows, cols uint16, ok bool) {
	if len(b) != 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(b[0:2]), binary.BigEndian.Uint16(b[2:4]), true
}

// WindowPayload encodes a stdin credit.
func WindowPayload(n uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return b
}

// ParseWindow decodes a stdin credit.
func ParseWindow(b []byte) (uint32, bool) {
	if len(b) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b), true
}

// Credit counts how many bytes may still be sent. Take blocks until some are
// available.
type Credit struct {
	mu    sync.Mutex
	cond  *sync.Cond
	avail int
	done  bool
}

func NewCredit() *Credit {
	c := &Credit{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Add grants n more bytes.
func (c *Credit) Add(n int) {
	c.mu.Lock()
	c.avail += n
	c.mu.Unlock()
	c.cond.Broadcast()
}

// Take waits for credit and takes up to max bytes of it. It returns 0 once the
// credit is closed.
func (c *Credit) Take(max int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.avail == 0 && !c.done {
		c.cond.Wait()
	}
	if c.done {
		return 0
	}
	n := min(c.avail, max)
	c.avail -= n
	return n
}

// Close wakes up and releases every waiter.
func (c *Credit) Close() {
	c.mu.Lock()
	c.done = true
	c.mu.Unlock()
	c.cond.Broadcast()
}
