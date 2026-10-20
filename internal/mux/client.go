// SPDX-License-Identifier: GPL-3.0-only

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

// DefaultGuard is how long a new prompt ignores keys. The person may be typing
// to the program at the moment the prompt appears, and those keys must not
// answer it.
const DefaultGuard = 500 * time.Millisecond

// DefaultIdle is how long the person must have stopped typing before a prompt
// takes over the screen.
const DefaultIdle = 5 * time.Second

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
	Detach    Key
	Info      Key         // shows Settings in a popup; zero: no popup
	Settings  [][2]string // what the popup lists: name and value
	ThemeName string      // the bar's theme, for the swatch in the popup
	Bar       Bar         // whether there is a status bar
	Project   string      // shown on the bar
	Colors    Colors      // of the bar; zero means DefaultColors
	LogPath   string      // if set, a record of what is sent to the terminal and what is typed

	// A prompt that arrives while the person is typing does not take over the
	// screen: the bar blinks, and the prompt opens when they have been idle for
	// Idle (zero: always at once) or press the Info key. A new prompt then
	// ignores keys for Guard (zero: DefaultGuard).
	Idle  time.Duration
	Guard time.Duration

	// AlertSymbol is drawn before the text of the bar's alert (empty: none) and
	// takes AlertSymbolWidth columns on the terminal.
	AlertSymbol      string
	AlertSymbolWidth int
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
	var tlog *termLog
	if o.LogPath != "" {
		if tlog, err = openTermLog(o.LogPath); err != nil {
			return Result{}, err
		}
		defer tlog.Close()
	}
	c := &client{conn: conn, in: in, out: out, detach: o.Detach, scr: scr, log: tlog,
		info: o.Info, settings: o.Settings, theme: o.ThemeName,
		idleFor: o.Idle, guard: o.Guard,
		bar: BarInfo{Project: o.Project, Key: o.Detach.Name, InfoKey: o.Info.Name, Started: time.Now(), Colors: o.Colors,
			AlertSymbol: o.AlertSymbol, AlertSymbolWidth: o.AlertSymbolWidth}}

	defer func() {
		c.emit(c.leaveOverlayBytes())
		c.emit(c.tracker.Reset())
		c.emitString(FinalReset)
		c.emitString(c.scr.Teardown())
		c.emitString("\x1b[23;0t") // the title the shell had
		term.Restore(int(in.Fd()), old)
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// The session starts where the cursor is, below the command that started
	// it, like any program, and the screen stays as it was: when the session
	// ends or you detach, you are where you were. Only the bottom row is taken
	// for the bar, so the cursor must not be on it: a line feed moves the cursor
	// down, or scrolls the screen if it was on the last row, and the cursor up
	// puts it back one row higher.
	c.emitString("\n\x1b[A")
	c.emitString(scr.Begin())
	// The window title is the session's: saved, set, and given back on leaving.
	// The program's own title commands are dropped, or they would replace it.
	scr.HideTitles = true
	c.emitString("\x1b[22;0t\x1b]0;" + title(o.Project) + "\x07")
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

	log      *termLog // a record of what was shown and typed, if asked for
	detach   Key
	info     Key // zero Name: the popup is off
	settings [][2]string
	theme    string
	scr      *Screen // keeps a row for the status bar
	bar      BarInfo

	lastOutput time.Time // when the program last wrote
	lastBar    string    // the bar as last drawn
	lastDraw   time.Time // when

	tracker Tracker // the modes the real terminal is in because of the program

	overlay *overlay
	held    []byte // output kept back while an overlay is up
	lost    bool   // some of it was dropped

	idleFor time.Duration // how long the person must not have typed before a prompt opens
	guard   time.Duration // how long a prompt that opened ignores keys

	lastKey   time.Time // when the person last typed to the program
	pending   *pending  // a prompt the person has not seen yet
	lastPhase bool      // the look of the alert as last drawn
}

// pending is a prompt held back because the person is typing.
type pending struct {
	id      uint32
	payload []byte
	alert   string // what the bar says about it
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
				c.log.write('<', buf[:n])
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
					c.emitString(c.scr.Margins())
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
			if !isFocusReport(k) {
				c.lastKey = time.Now()
			}
			if c.pending != nil && c.info.Name != "" {
				if i := c.info.find(k); i >= 0 { // the key that shows the settings shows the waiting prompt
					if i > 0 {
						c.sendInput(k[:i])
					}
					c.openPending()
					continue
				}
			}
			if i := c.detach.find(k); i >= 0 {
				if i > 0 {
					c.sendInput(k[:i])
				}
				c.send(fDetach, nil)
				return Result{Detached: true}, nil
			}
			if c.info.Name != "" {
				if i := c.info.find(k); i >= 0 {
					if i > 0 {
						c.sendInput(k[:i])
					}
					c.startInfo()
					continue
				}
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
				c.onPrompt(f.payload)
			case fCancel:
				if len(f.payload) != 4 {
					break
				}
				id := binary.BigEndian.Uint32(f.payload)
				if c.overlay != nil && c.overlay.id == id {
					c.overlay.cancel()
				}
				if c.pending != nil && c.pending.id == id {
					c.setPending(nil)
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

func (c *client) guardOrDefault() time.Duration {
	if c.guard > 0 {
		return c.guard
	}
	return DefaultGuard
}

// isFocusReport reports whether typed bytes are only the terminal saying that
// its window gained or lost focus, which is not typing.
func isFocusReport(k []byte) bool {
	return string(k) == "\x1b[I" || string(k) == "\x1b[O"
}

// onPrompt shows a prompt from the server, at once if the person is not typing,
// else it waits and the bar says so.
func (c *client) onPrompt(payload []byte) {
	if len(payload) < 8 {
		return
	}
	var req approve.Request
	if json.Unmarshal(payload[8:], &req) != nil {
		c.startOverlay(payload) // it answers the malformed request itself
		return
	}
	if c.overlay == nil && time.Since(c.lastKey) >= c.idleFor {
		c.startOverlay(payload)
		return
	}
	c.setPending(&pending{id: binary.BigEndian.Uint32(payload[0:4]), payload: payload, alert: c.alertText(req)})
}

// alertText is the bar's message for a waiting prompt: what it is, and the key
// that opens it.
func (c *client) alertText(req approve.Request) string {
	what := ""
	switch {
	case req.Kind == approve.KindNetwork && req.Net != nil:
		host := req.Net.IP
		if len(req.Net.Names) > 0 {
			host = req.Net.Names[0]
		}
		what = fmt.Sprintf("Network %s:%d", host, req.Net.Port)
	case req.Kind == approve.KindBuild:
		what = "Build"
	default:
		what = strings.TrimSpace("Command " + req.Name + " " + strings.Join(req.Args, " "))
	}
	text := what
	if c.info.Name != "" {
		text += "  " + c.info.Name
	}
	return text
}

// setPending holds a prompt back (or, with nil, lets go of it) and updates the bar.
func (c *client) setPending(p *pending) {
	c.pending = p
	c.bar.Alert = ""
	if p != nil {
		c.bar.Alert = p.alert
	}
	c.drawBar(true)
}

// openPending shows the waiting prompt now.
func (c *client) openPending() {
	p := c.pending
	if p == nil {
		return
	}
	c.pending = nil
	c.bar.Alert = ""
	c.startOverlay(p.payload)
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
	o := &overlay{id: id, cancel: cancel, keys: make(chan byte, 64), armed: time.Now().Add(c.guardOrDefault()), done: make(chan answerMsg, 1)}
	c.overlay = o
	c.send(fOpened, binary.BigEndian.AppendUint32(nil, id)) // its timeout starts now

	c.emit(c.tracker.Reset())
	// The alternate screen first, so it saves the program's cursor; the scroll
	// region reset homes the cursor, and would otherwise be what is saved.
	c.emitString("\x1b[?1049h\x1b[r\x1b[2J\x1b[H\x1b[?25l") // the prompt gets the whole screen
	go func() {
		var text string
		var err error
		if req.Kind == approve.KindNetwork {
			var reply approve.Reply
			reply, err = approve.PromptNet(ctx, o.keySource(), c.out, req, timeout, true)
			text = reply.Encode()
		} else {
			var ans approve.Answer
			ans, err = approve.PromptKeys(ctx, o.keySource(), c.out, req, timeout, true)
			text = ans.String()
		}
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

// startInfo shows the session's settings over everything until a key is
// pressed. It is an overlay like a prompt: the program's output is held back
// and the keys do not reach the program.
func (c *client) startInfo() {
	_ = unix.IoctlSetInt(int(c.in.Fd()), unix.TCFLSH, unix.TCIFLUSH)
	ctx, cancel := context.WithCancel(context.Background())
	o := &overlay{cancel: cancel, keys: make(chan byte, 64), armed: time.Now().Add(150 * time.Millisecond), done: make(chan answerMsg, 1)}
	c.overlay = o
	c.emit(c.tracker.Reset())
	c.emitString("\x1b[?1049h\x1b[r\x1b[2J\x1b[H\x1b[?25l")
	c.emitString(c.infoText(time.Now()))
	go func() {
		select {
		case <-o.keys:
		case <-ctx.Done():
		}
		o.done <- answerMsg{}
	}()
}

// infoText is the popup: the settings as aligned lines, with the bar's colors
// shown as they look.
func (c *client) infoText(now time.Time) string {
	rows := [][2]string{{"project", c.bar.Project}}
	if c.bar.Label != "" {
		rows = append(rows, [2]string{"program", c.bar.Label})
	}
	rows = append(rows, [2]string{"up since", c.bar.Started.Format("2006-01-02 15:04") + " (" + strings.TrimPrefix(uptime(now.Sub(c.bar.Started)), "up ") + ")"})
	rows = append(rows, c.settings...)
	rows = append(rows, [2]string{"keys", c.info.Name + " this popup, " + c.detach.Name + " detach"})
	w := 0
	for _, r := range rows {
		if n := len(r[0]); n > w {
			w = n
		}
	}
	var sb strings.Builder
	sb.WriteString("\x1b[1mAIcatraz session\x1b[0m\r\n\r\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "  %-*s  %s\r\n", w, clean(r[0]), clean(r[1]))
		if r[0] == "bar theme" {
			fmt.Fprintf(&sb, "  %-*s  %s %s \x1b[0m\r\n", w, "", c.bar.style(), clean(c.theme))
		}
	}
	sb.WriteString("\r\n\x1b[2many key closes\x1b[0m")
	return sb.String()
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
	c.emitString("\x1b[?25h\x1b[?1049l")
	c.emitString(c.scr.Margins())
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
	c.emit(c.scr.Out(p))
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
	c.lastDraw = time.Now()
	c.lastPhase = AlertPhase(c.lastDraw)
	c.lastBar = c.bar.Render(c.scr.cols, c.lastDraw)
	c.emitString(c.scr.Draw(c.lastBar))
}

// idle runs on a timer: it lets go of an output sequence that was cut short,
// and refreshes the uptime when the program is quiet.
func (c *client) idle() {
	now := time.Now()
	if c.pending != nil && c.overlay == nil {
		switch {
		case now.Sub(c.lastKey) >= c.idleFor:
			c.openPending()
			return
		case c.scr.Active() && c.bar.Colors.Rainbow:
			c.drawBar(true) // the rainbow turns
		case c.scr.Active() && AlertPhase(now) != c.lastPhase:
			c.drawBar(true) // the alert blinks
		}
	}
	if now.Sub(c.lastOutput) > 150*time.Millisecond {
		if b := c.scr.FlushOut(); len(b) > 0 && c.overlay == nil {
			c.emit(b)
		}
	}
	// Whatever the program did, the bar comes back within a second: a repaint
	// costs a few bytes, and covers anything not foreseen. A program that never
	// goes quiet (an animated spinner) must not keep a damaged bar either.
	if c.scr.Active() && c.overlay == nil && now.Sub(c.lastDraw) > time.Second {
		c.drawBar(true)
	}
}

// emit writes bytes to the terminal.
func (c *client) emit(b []byte) {
	c.log.write('>', b)
	c.out.Write(b)
}

func (c *client) emitString(s string) { c.emit([]byte(s)) }

// title is the window title of a session: "AIcatraz <project>", with nothing
// in the project that could end the command early.
func title(project string) string {
	return "AIcatraz " + strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x9c {
			return -1
		}
		return r
	}, project)
}
