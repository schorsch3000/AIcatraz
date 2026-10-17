package mux

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/schorsch3000/aicatraz/internal/approve"
)

// Socket names inside the session directory.
const (
	AttachSock  = "attach.sock"  // clients: attach, detach, stop
	ApproveSock = "approve.sock" // the jail's launcher: approval requests
)

// Options for Serve.
type Options struct {
	Dir        string    // private directory for the sockets
	Cmd        *exec.Cmd // the program; it is started on a new pty
	Rows, Cols uint16    // size until a client says otherwise
	Label      string    // what runs, for the status bar
}

type clientConn struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *clientConn) send(typ byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return writeFrame(c.conn, typ, payload)
}

type prompt struct {
	id       uint32
	req      approve.Request
	deadline time.Time
	reply    chan string
	timer    *time.Timer
	done     bool
}

type server struct {
	ptmx *os.File
	cmd  *exec.Cmd

	mu         sync.Mutex
	tracker    Tracker
	client     *clientConn
	rows, cols uint16
	queue      []*prompt // waiting for a client
	active     *prompt   // shown to the current client
	nextID     uint32

	redrawMu sync.Mutex

	label   string
	started time.Time

	ptyMu    sync.Mutex // guards ptmx against being used while it closes
	ptyClose bool

	pending  []byte // output from before anyone ever attached, replayed to the first client
	attached bool   // someone has attached at least once
}

// pendingLimit caps the output kept for the first client.
const pendingLimit = 1 << 20

func (s *server) setsize(rows, cols uint16) {
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if !s.ptyClose {
		pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
	}
}

func (s *server) closePty() {
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	s.ptyClose = true
	s.ptmx.Close()
}

// Serve runs o.Cmd on a pty and serves clients until it exits. It returns the
// program's exit code.
func Serve(o Options) (int, error) {
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return 0, err
	}
	attachPath, approvePath := filepath.Join(o.Dir, AttachSock), filepath.Join(o.Dir, ApproveSock)
	os.Remove(attachPath)
	os.Remove(approvePath)
	attachL, err := net.Listen("unix", attachPath)
	if err != nil {
		return 0, err
	}
	defer attachL.Close()
	approveL, err := net.Listen("unix", approvePath)
	if err != nil {
		return 0, err
	}
	defer approveL.Close()
	os.Chmod(attachPath, 0o600)
	os.Chmod(approvePath, 0o600)

	rows, cols := o.Rows, o.Cols
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	ptmx, err := pty.StartWithSize(o.Cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return 0, err
	}
	s := &server{ptmx: ptmx, cmd: o.Cmd, rows: rows, cols: cols, label: o.Label, started: time.Now()}

	go s.acceptClients(attachL)
	go s.acceptApprovals(approveL)

	pumpDone := make(chan struct{})
	go func() {
		s.pump()
		close(pumpDone)
	}()

	waitErr := o.Cmd.Wait()
	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
			if code < 0 { // killed by a signal
				code = 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
			}
		} else {
			code = 1
		}
	}
	select { // let the last output through
	case <-pumpDone:
	case <-time.After(2 * time.Second):
	}

	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c != nil {
		var p [4]byte
		binary.BigEndian.PutUint32(p[:], uint32(int32(code)))
		c.send(fExit, p[:])
		time.Sleep(100 * time.Millisecond) // let it arrive before the socket closes
		c.conn.Close()
	}
	s.closePty()
	s.cancelAll()
	return code, nil
}

// pump copies the program's output to the attached client, and follows its
// terminal modes. With nobody attached the output is dropped.
func (s *server) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.tracker.Feed(buf[:n])
			c := s.client
			if c == nil && !s.attached && len(s.pending)+n <= pendingLimit {
				s.pending = append(s.pending, buf[:n]...)
			}
			s.mu.Unlock()
			if c != nil {
				if c.send(fOutput, buf[:n]) != nil {
					s.detach(c)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *server) acceptClients(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go s.handleClient(conn)
	}
}

func (s *server) handleClient(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, p, err := readFrame(conn)
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	if typ == fStop {
		if s.cmd.Process != nil {
			s.cmd.Process.Signal(syscall.SIGTERM)
		}
		return
	}
	rows, cols, ok := parseSize(p)
	if typ != fHello || !ok {
		return
	}
	c := &clientConn{conn: conn}
	info := binary.BigEndian.AppendUint64(nil, uint64(s.started.Unix()))
	c.send(fInfo, append(info, s.label...))
	s.attach(c, rows, cols)
	defer s.detach(c)

	for {
		typ, p, err := readFrame(conn)
		if err != nil {
			return
		}
		switch typ {
		case fInput:
			s.ptmx.Write(p)
		case fResize:
			if rows, cols, ok := parseSize(p); ok {
				s.resize(rows, cols)
			}
		case fDetach:
			return
		case fAnswer:
			if len(p) >= 4 {
				s.answer(c, binary.BigEndian.Uint32(p[:4]), string(p[4:]))
			}
		case fRedraw:
			go s.redraw()
		}
	}
}

func (s *server) attach(c *clientConn, rows, cols uint16) {
	s.mu.Lock()
	if old := s.client; old != nil {
		old.send(fError, []byte("another client attached to this session"))
		old.conn.Close()
		s.requeueActive()
	}
	// The preamble goes out under the lock: no output can overtake it.
	c.send(fPreamble, s.tracker.Preamble())
	if !s.attached {
		s.attached = true
		for len(s.pending) > 0 { // what the program said before anyone was there
			n := min(len(s.pending), 32<<10)
			c.send(fOutput, s.pending[:n])
			s.pending = s.pending[n:]
		}
		s.pending = nil
	}
	s.client = c
	s.mu.Unlock()

	s.resize(rows, cols)
	go func() {
		time.Sleep(150 * time.Millisecond) // after the client applied the preamble
		s.redraw()
	}()
	s.dispatch()
}

func (s *server) detach(c *clientConn) {
	s.mu.Lock()
	if s.client == c {
		s.client = nil
		s.requeueActive()
	}
	s.mu.Unlock()
	c.conn.Close()
}

// requeueActive puts the prompt that was on screen back in front of the
// queue: whoever attaches next sees it. The caller holds s.mu.
func (s *server) requeueActive() {
	if s.active != nil {
		s.queue = append([]*prompt{s.active}, s.queue...)
		s.active = nil
	}
}

func (s *server) resize(rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	s.mu.Lock()
	s.rows, s.cols = rows, cols
	s.mu.Unlock()
	s.setsize(rows, cols)
}

// redraw makes the program repaint by changing the terminal size and changing
// it back: programs repaint on SIGWINCH.
func (s *server) redraw() {
	s.redrawMu.Lock()
	defer s.redrawMu.Unlock()
	s.mu.Lock()
	rows, cols := s.rows, s.cols
	s.mu.Unlock()
	if cols < 2 {
		return
	}
	s.setsize(rows, cols-1)
	time.Sleep(120 * time.Millisecond)
	s.setsize(rows, cols)
}

// ---- approval prompts ----

// request is what the launcher sends on the approve socket, one JSON line.
type request struct {
	Request  approve.Request `json:"request"`
	TimeoutS float64         `json:"timeout_s"`
}

type response struct {
	Answer string `json:"answer"` // once, session, pin, deny or timeout
}

func (s *server) acceptApprovals(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go s.handleApproval(conn)
	}
}

func (s *server) handleApproval(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReaderSize(conn, 1<<20).ReadBytes('\n')
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	var req request
	if json.Unmarshal(line, &req) != nil {
		return
	}
	timeout := time.Duration(req.TimeoutS * float64(time.Second))
	timeout = min(max(timeout, time.Second), 24*time.Hour)

	p := s.submit(req.Request, timeout)
	gone := make(chan struct{})
	go func() { // the launcher closes the connection when it stops waiting
		io.Copy(io.Discard, conn)
		close(gone)
	}()
	select {
	case ans := <-p.reply:
		out, _ := json.Marshal(response{Answer: ans})
		conn.Write(append(out, '\n'))
	case <-gone:
		s.finish(p, "", true)
	}
}

func (s *server) submit(req approve.Request, timeout time.Duration) *prompt {
	s.mu.Lock()
	s.nextID++
	p := &prompt{id: s.nextID, req: req, deadline: time.Now().Add(timeout), reply: make(chan string, 1)}
	p.timer = time.AfterFunc(timeout, func() { s.finish(p, "timeout", true) })
	s.queue = append(s.queue, p)
	s.mu.Unlock()
	s.dispatch()
	return p
}

// finish ends a prompt with an answer. notify tells the client to take the
// prompt away; it is false when the client itself answered.
func (s *server) finish(p *prompt, answer string, notify bool) {
	s.mu.Lock()
	if p.done {
		s.mu.Unlock()
		return
	}
	p.done = true
	p.timer.Stop()
	for i, q := range s.queue {
		if q == p {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			break
		}
	}
	var c *clientConn
	if s.active == p {
		s.active = nil
		if notify {
			c = s.client
		}
	}
	s.mu.Unlock()
	p.reply <- answer
	if c != nil {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], p.id)
		c.send(fCancel, b[:])
	}
	s.dispatch()
}

// dispatch shows the next waiting prompt, if a client is there to see it.
func (s *server) dispatch() {
	s.mu.Lock()
	if s.active != nil || s.client == nil || len(s.queue) == 0 {
		s.mu.Unlock()
		return
	}
	p := s.queue[0]
	s.queue = s.queue[1:]
	s.active = p
	c := s.client
	left := time.Until(p.deadline)
	s.mu.Unlock()

	body, _ := json.Marshal(p.req)
	payload := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(payload[0:4], p.id)
	binary.BigEndian.PutUint32(payload[4:8], uint32(max(left, 0).Milliseconds()))
	if c.send(fPrompt, append(payload, body...)) != nil {
		s.detach(c)
	}
}

// answer takes the user's answer to the prompt on screen. Anything else, an
// answer to an old prompt or from an old client, is ignored.
func (s *server) answer(c *clientConn, id uint32, text string) {
	s.mu.Lock()
	p := s.active
	valid := s.client == c && p != nil && p.id == id
	s.mu.Unlock()
	if valid {
		if p.req.Kind == approve.KindNetwork {
			s.finish(p, approve.ParseReply(text).Encode(), false)
		} else {
			s.finish(p, approve.ParseAnswer(text).String(), false)
		}
	}
}

// cancelAll denies every prompt that is still waiting.
func (s *server) cancelAll() {
	s.mu.Lock()
	var all []*prompt
	all = append(all, s.queue...)
	if s.active != nil {
		all = append(all, s.active)
	}
	s.mu.Unlock()
	for _, p := range all {
		s.finish(p, "deny", false)
	}
}

// ---- the launcher's side of the approve socket ----

// Approver asks the multiplexer to show the prompt to whoever is attached.
type Approver struct {
	Sock string // the session's approve socket
}

func (a *Approver) Ask(ctx context.Context, req approve.Request, timeout time.Duration) (approve.Answer, error) {
	text, err := a.ask(ctx, req, timeout)
	if err != nil {
		return approve.Deny, err
	}
	return approve.ParseAnswer(text), nil
}

// AskNet shows a network request to whoever is attached.
func (a *Approver) AskNet(ctx context.Context, req approve.Request, timeout time.Duration) (approve.Reply, error) {
	text, err := a.ask(ctx, req, timeout)
	if err != nil {
		return approve.Reply{Verb: approve.VerbDeny}, err
	}
	return approve.ParseReply(text), nil
}

// ask sends the request and returns the text of the answer.
func (a *Approver) ask(ctx context.Context, req approve.Request, timeout time.Duration) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", a.Sock)
	if err != nil {
		return "", approve.ErrUnavailable
	}
	defer conn.Close()
	body, err := json.Marshal(request{Request: req, TimeoutS: timeout.Seconds()})
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return "", approve.ErrUnavailable
	}

	type result struct {
		ans string
		err error
	}
	res := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		var r response
		if err == nil {
			err = json.Unmarshal(line, &r)
		}
		res <- result{r.Answer, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err() // closing the connection withdraws the prompt
	case r := <-res:
		switch {
		case r.err != nil:
			return "", approve.ErrUnavailable
		case r.ans == "timeout":
			return "", approve.ErrTimeout
		}
		return r.ans, nil
	}
}
