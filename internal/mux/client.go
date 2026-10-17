package mux

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/approve"
)

// armDelay is how long a new prompt ignores keys. The person may be typing
// to the program at the moment the prompt appears, and those keys must not
// answer it.
const armDelay = 500 * time.Millisecond

// holdLimit caps the output kept back while a prompt is on screen.
const holdLimit = 4 << 20

// Result says how an attach ended.
type Result struct {
	Exited   bool   // the program ended; Code is its exit code
	Code     int    // exit code of the program
	Detached bool   // the user detached
	Message  string // why the server closed the connection, if it did
}

// Options for Attach.
type AttachOptions struct {
	Detach  Key
	Bar     Bar    // whether there is a status bar
	Project string // shown on the bar
	Colors  Colors // of the bar; zero means DefaultColors
}

// Attach connects the terminal in/out to the session at sock until the
// program ends, the user detaches, or the server goes away.
func Attach(sock string, in, out *os.File, o AttachOptions) (Result, error) {
	rows, cols, err := termSize(out)
	if err != nil {
		return Result{}, fmt.Errorf("not a terminal: %w", err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	scr := NewScreen(o.Bar, int(rows), int(cols))
	if err := writeFrame(conn, fHello, sizePayload(uint16(scr.AgentRows()), cols)); err != nil {
		return Result{}, err
	}

	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return Result{}, err
	}
	c := &client{conn: conn, in: in, out: out, detach: o.Detach, scr: scr,
		bar: BarInfo{Project: o.Project, Key: o.Detach.Name, Started: time.Now(), Colors: o.Colors}}
	defer func() {
		out.Write(c.leaveOverlayBytes())
		out.Write(c.tracker.Reset())
		out.WriteString(c.scr.Teardown())
		term.Restore(int(in.Fd()), old)
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// Make room without erasing: scroll what is on the screen away into the
	// terminal's scrollback, so the session starts on a blank screen and the
	// shell's earlier output is still there afterwards.
	fmt.Fprintf(out, "\x1b[%d;1H%s\x1b[H", rows, strings.Repeat("\n", int(rows)))
	out.WriteString(scr.Setup())
	return c.run(winch)
}

func termSize(f *os.File) (rows, cols uint16, err error) {
	w, h, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0, err
	}
	return uint16(h), uint16(w), nil
}

type client struct {
	conn net.Conn
	in   *os.File
	out  *os.File
	wmu  sync.Mutex

	detach Key
	scr    *Screen // keeps a row for the status bar
	bar    BarInfo

	lastOutput time.Time // when the program last wrote
	lastBar    string    // the bar as last drawn

	tracker Tracker // the modes the real terminal is in because of the program

	overlay *overlay
	held    []byte // output kept back while an overlay is up
	lost    bool   // some of it was dropped
}

type overlay struct {
	id     uint32
	cancel context.CancelFunc
	keys   chan byte
	armed  time.Time
	done   chan answerMsg
}

type answerMsg struct {
	id   uint32
	text string
}

func (c *client) send(typ byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.conn, typ, payload)
}

type frame struct {
	typ     byte
	payload []byte
	err     error
}

func (c *client) run(winch <-chan os.Signal) (Result, error) {
	frames := make(chan frame)
	go func() {
		for {
			typ, p, err := readFrame(c.conn)
			select {
			case frames <- frame{typ, p, err}:
			case <-time.After(30 * time.Second):
				return
			}
			if err != nil {
				return
			}
		}
	}()
	keys := make(chan []byte, 8)
	stdinGone := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := c.in.Read(buf)
			if n > 0 {
				keys <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(stdinGone)
				return
			}
		}
	}()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		var done <-chan answerMsg
		if c.overlay != nil {
			done = c.overlay.done
		}
		select {
		case <-winch:
			if rows, cols, err := termSize(c.out); err == nil {
				c.scr.Resize(int(rows), int(cols))
				c.send(fResize, sizePayload(uint16(c.scr.AgentRows()), cols))
				if c.overlay == nil {
					c.out.WriteString(c.scr.Margins())
					c.drawBar(true)
				}
			}

		case <-tick.C:
			c.idle()

		case <-stdinGone:
			return Result{Detached: true}, nil

		case k := <-keys:
			if c.overlay != nil {
				c.feedOverlay(k)
				continue
			}
			if i := c.detach.find(k); i >= 0 {
				if i > 0 {
					c.sendInput(k[:i])
				}
				c.send(fDetach, nil)
				return Result{Detached: true}, nil
			}
			c.sendInput(k)

		case a := <-done:
			c.endOverlay()
			if a.text != "" {
				var p [4]byte
				binary.BigEndian.PutUint32(p[:], a.id)
				c.send(fAnswer, append(p[:], a.text...))
			}

		case f := <-frames:
			if f.err != nil {
				return Result{}, errors.New("the session ended without a word (the connection closed)")
			}
			switch f.typ {
			case fOutput:
				c.output(f.payload)
			case fPreamble:
				c.write(f.payload)
				c.tracker.Feed(f.payload)
			case fInfo:
				if len(f.payload) >= 8 {
					c.bar.Started = time.Unix(int64(binary.BigEndian.Uint64(f.payload[:8])), 0)
					c.bar.Label = string(f.payload[8:])
					c.drawBar(true)
				}
			case fPrompt:
				c.startOverlay(f.payload)
			case fCancel:
				if len(f.payload) == 4 && c.overlay != nil && c.overlay.id == binary.BigEndian.Uint32(f.payload) {
					c.overlay.cancel()
				}
			case fExit:
				if len(f.payload) == 4 {
					return Result{Exited: true, Code: int(int32(binary.BigEndian.Uint32(f.payload)))}, nil
				}
				return Result{Exited: true}, nil
			case fError:
				return Result{Message: string(f.payload)}, nil
			}
		}
	}
}

// output writes the program's output to the terminal, or keeps it back while a
// prompt is on screen.
func (c *client) output(p []byte) {
	if c.overlay == nil {
		c.write(p)
		c.tracker.Feed(p)
		c.lastOutput = time.Now()
		return
	}
	if len(c.held)+len(p) > holdLimit {
		c.lost = true
		return
	}
	c.held = append(c.held, p...)
}

// startOverlay draws a prompt over everything and starts reading its answer.
// The terminal first goes back to its normal modes (no mouse reports, no
// extended keys that would arrive as escape sequences and count as Esc), then
// onto the alternate screen, which the program's own screen survives.
func (c *client) startOverlay(payload []byte) {
	if len(payload) < 8 {
		return
	}
	id := binary.BigEndian.Uint32(payload[0:4])
	timeout := time.Duration(binary.BigEndian.Uint32(payload[4:8])) * time.Millisecond
	var req approve.Request
	if json.Unmarshal(payload[8:], &req) != nil {
		c.send(fAnswer, append(binary.BigEndian.AppendUint32(nil, id), "deny"...))
		return
	}
	if c.overlay != nil { // never two at once; the server sends one at a time
		c.overlay.cancel()
	}
	// Whatever was typed before the prompt appeared must not answer it.
	_ = unix.IoctlSetInt(int(c.in.Fd()), unix.TCFLSH, unix.TCIFLUSH)
	ctx, cancel := context.WithCancel(context.Background())
	o := &overlay{id: id, cancel: cancel, keys: make(chan byte, 64), armed: time.Now().Add(armDelay), done: make(chan answerMsg, 1)}
	c.overlay = o

	c.out.Write(c.tracker.Reset())
	// The alternate screen first, so it saves the program's cursor; the scroll
	// region reset homes the cursor, and would otherwise be what is saved.
	c.out.WriteString("\x1b[?1049h\x1b[r\x1b[2J\x1b[H\x1b[?25l") // the prompt gets the whole screen
	go func() {
		ans, err := approve.PromptKeys(ctx, o.keySource(), c.out, req, timeout, true)
		text := ans.String()
		switch {
		case ctx.Err() != nil:
			text = "" // withdrawn: nobody to tell
		case err == approve.ErrTimeout:
			text = "timeout"
		}
		select { // let the result line be read
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
		}
		o.done <- answerMsg{id, text}
	}()
}

func (o *overlay) keySource() approve.KeySource {
	return func(ctx context.Context, deadline time.Time) (byte, error) {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-timer.C:
				return 0, approve.ErrTimeout
			case b := <-o.keys:
				return b, nil
			}
		}
	}
}

// feedOverlay hands typed bytes to the prompt, once it is armed.
func (c *client) feedOverlay(k []byte) {
	if time.Now().Before(c.overlay.armed) {
		return
	}
	for _, b := range k {
		select {
		case c.overlay.keys <- b:
		default:
		}
	}
}

func (c *client) leaveOverlayBytes() []byte {
	if c.overlay == nil {
		return nil
	}
	return []byte("\x1b[?25h\x1b[?1049l")
}

// endOverlay takes the prompt away and puts the terminal back as the program
// had it. If the program is on the alternate screen, which the prompt cleared,
// it is asked to repaint instead of replaying what was held back.
func (c *client) endOverlay() {
	c.overlay = nil
	c.out.WriteString("\x1b[?25h\x1b[?1049l")
	c.out.WriteString(c.scr.Margins())
	c.write(c.tracker.Preamble())
	c.drawBar(true)
	held, lost := c.held, c.lost
	c.held, c.lost = nil, false
	switch {
	case c.tracker.InAlt() || lost:
		c.send(fRedraw, nil)
	case len(held) > 0:
		c.write(held)
		c.tracker.Feed(held)
	}
}

// Stop asks the session at sock to end: its program gets SIGTERM.
func Stop(sock string) error {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	return writeFrame(conn, fStop, nil)
}

// write sends the program's output to the terminal through the screen layer,
// and repairs the bar if the output may have damaged it.
func (c *client) write(p []byte) {
	c.out.Write(c.scr.Out(p))
	c.drawBar(false)
}

// sendInput passes typed bytes on to the program.
func (c *client) sendInput(k []byte) {
	c.send(fInput, k)
}

// drawBar paints the bar if it is damaged (or force), unless a prompt is up.
func (c *client) drawBar(force bool) {
	if !c.scr.Active() || c.overlay != nil {
		return
	}
	if !c.scr.TakeDirty() && !force {
		return
	}
	c.lastBar = c.bar.Render(c.scr.cols, time.Now())
	c.out.WriteString(c.scr.Draw(c.lastBar))
}

// idle runs on a timer: it lets go of an output sequence that was cut short,
// and refreshes the uptime when the program is quiet.
func (c *client) idle() {
	now := time.Now()
	if now.Sub(c.lastOutput) > 150*time.Millisecond {
		if b := c.scr.FlushOut(); len(b) > 0 && c.overlay == nil {
			c.out.Write(b)
		}
	}
	if c.scr.Active() && c.overlay == nil && now.Sub(c.lastOutput) > 400*time.Millisecond &&
		c.bar.Render(c.scr.cols, now) != c.lastBar {
		c.drawBar(true)
	}
}
