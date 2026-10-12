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
	"time"
)

// Version is bumped on incompatible protocol changes.
const Version = 1

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
	TExit     Type = 'X' // broker -> shim: Exit
)

const (
	// MaxFrame bounds a single frame so a peer can't make the other allocate without limit.
	MaxFrame = 1 << 20
	// ChunkSize is the size data is split into.
	ChunkSize = 32 << 10
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
	c   net.Conn
	wmu sync.Mutex
}

func New(c net.Conn) *Conn { return &Conn{c: c} }

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
	_ = c.c.SetWriteDeadline(time.Now().Add(Timeout))
	_, err := c.c.Write(buf)
	return err
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
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the limit", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.c, payload); err != nil {
		return 0, nil, err
	}
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
