// Package broker is the host side of the command proxy. It accepts requests
// from the in-container shim over a unix socket, applies the policy, and runs
// allowed commands on the host.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/policy"
	"github.com/schorsch3000/aicatraz/internal/proto"
)

const (
	defaultMaxConcurrent = 8
	maxArgs              = 4096
	killGrace            = 3 * time.Second

	maxPendingApprovals = 4
	// The prompt enforces the timeout itself; the broker waits a little longer
	// so the prompt's own timeout message wins the race.
	approvalSlack = 2 * time.Second
)

var commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Options configures a Broker.
type Options struct {
	UID           int      // only this user may connect
	HostEnv       []string // the broker's own environment (os.Environ())
	Session       string
	Audit         *Audit
	MaxConcurrent int

	// Approver is asked about `ask` rules. Nil means approval is unavailable
	// and such commands are denied.
	Approver        approve.Approver
	ApprovalTimeout time.Duration // default 2 minutes
}

// Broker serves host command requests.
type Broker struct {
	pol      *policy.Policy
	cfg      *config.Config
	opts     Options
	visible  []string // real paths of volumes the agent can see (rw and ro)
	hidden   []string // real paths of hidden volumes
	writable []string // real paths of rw volumes
	sem      chan struct{}
	pending  chan struct{} // bounds prompts waiting for the human

	gmu    sync.Mutex
	grants map[string]bool // "allow for this session" keys

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	ln     net.Listener
}

// New builds a broker from a validated config.
func New(cfg *config.Config, opts Options) (*Broker, error) {
	pol, err := policy.New(cfg.Rules)
	if err != nil {
		return nil, err
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = defaultMaxConcurrent
	}
	if opts.ApprovalTimeout <= 0 {
		opts.ApprovalTimeout = cfg.Approval.TimeoutOrDefault()
	}
	b := &Broker{
		pol: pol, cfg: cfg, opts: opts,
		sem:     make(chan struct{}, opts.MaxConcurrent),
		pending: make(chan struct{}, maxPendingApprovals),
		grants:  map[string]bool{},
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	for _, v := range cfg.Resolved {
		real, err := filepath.EvalSymlinks(v.Path)
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", v.Spec, err)
		}
		switch v.Mode {
		case config.ModeHide:
			b.hidden = append(b.hidden, real)
		case config.ModeRW:
			b.writable = append(b.writable, real)
			b.visible = append(b.visible, real)
		default:
			b.visible = append(b.visible, real)
		}
	}
	return b, nil
}

// Serve accepts connections until the listener is closed.
func (b *Broker) Serve(l net.Listener) error {
	b.mu.Lock()
	b.ln = l
	b.mu.Unlock()
	for {
		c, err := l.Accept()
		if err != nil {
			if b.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handle(c)
		}()
	}
}

// Close stops accepting, terminates running commands and waits for them.
func (b *Broker) Close() {
	b.cancel()
	b.mu.Lock()
	if b.ln != nil {
		b.ln.Close()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func peerCred(c net.Conn) (uid uint32, pid int32, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if cerr != nil {
		return 0, 0, cerr
	}
	return cred.Uid, cred.Pid, nil
}

func (b *Broker) handle(nc net.Conn) {
	defer nc.Close()
	uid, pid, err := peerCred(nc)
	if err != nil || int(uid) != b.opts.UID {
		b.audit(Entry{Event: "request", PeerPID: int(pid), Decision: "deny", Message: fmt.Sprintf("peer rejected (uid %d)", uid)})
		return
	}
	conn := proto.New(nc)

	var h proto.Hello
	if err := conn.ReadJSON(proto.THello, &h); err != nil {
		return
	}
	entry := Entry{Event: "request", PeerPID: int(pid), Name: h.Name, Args: h.Args, Cwd: h.Cwd}

	// One goroutine reads the connection for the whole request, so the
	// approval wait and the command's I/O both see the same frames.
	done := make(chan struct{})
	defer close(done)
	in := pump(conn, done)

	deny := func(msg string) {
		entry.Decision, entry.Message = "deny", msg
		b.audit(entry)
		conn.WriteJSON(proto.TDecision, proto.Decision{Action: proto.ActionDeny, Message: msg})
	}

	if err := validate(&h); err != nil {
		deny(err.Error())
		return
	}
	d := b.pol.Decide(h.Name, h.Args)
	if d.Rule != policy.NoRule {
		r := d.Rule
		entry.Rule = &r
	}

	switch d.Action {
	case policy.Local:
		entry.Decision = "local"
		if b.audit(entry) != nil {
			deny("audit log unavailable")
			return
		}
		conn.WriteJSON(proto.TDecision, proto.Decision{Action: proto.ActionLocal})
		return
	case policy.Allow, policy.Ask:
		// handled below
	default:
		deny("the policy does not allow this command with these arguments")
		return
	}

	cwd, err := b.checkCwd(h.Cwd)
	if err != nil {
		deny(err.Error())
		return
	}
	bin, err := resolveBinary(h.Name, pathOf(b.opts.HostEnv), b.writable)
	if err != nil {
		deny(err.Error())
		return
	}
	env, passed := BuildEnv(b.opts.HostEnv, h.Env, d.Config, b.cfg.Global.EnvStrip)

	if d.Action == policy.Ask {
		ok, by, msg := b.askHuman(conn, in, &h, d, bin, cwd, passed)
		entry.DecidedBy = by
		if !ok {
			deny(msg)
			return
		}
	}

	select {
	case b.sem <- struct{}{}:
		defer func() { <-b.sem }()
	default:
		deny("too many host commands running at once")
		return
	}

	entry.Decision, entry.Binary, entry.EnvPassed = "allow", bin, passed
	if b.audit(entry) != nil {
		// The audit log is part of the contract: no log, no execution.
		conn.WriteJSON(proto.TDecision, proto.Decision{Action: proto.ActionDeny, Message: "audit log unavailable"})
		return
	}
	if err := conn.WriteJSON(proto.TDecision, proto.Decision{Action: proto.ActionRun}); err != nil {
		return
	}

	start := time.Now()
	exit, err := b.execute(conn, in, &h, bin, cwd, env)
	fin := Entry{Event: "exit", PeerPID: int(pid), Name: h.Name, Args: h.Args, DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		fin.Message = err.Error()
		code := 127
		fin.ExitCode = &code
		conn.Write(proto.TStderr, []byte("aicatraz: could not start "+h.Name+" on the host: "+err.Error()+"\n"))
		conn.WriteJSON(proto.TExit, proto.Exit{Code: code})
	} else {
		fin.ExitCode, fin.Signal = &exit.Code, exit.Signal
	}
	b.audit(fin)
}

func (b *Broker) audit(e Entry) error {
	e.Session = b.opts.Session
	return b.opts.Audit.Log(e)
}

func pathOf(env []string) string {
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			return kv[5:]
		}
	}
	return ""
}

func validate(h *proto.Hello) error {
	switch {
	case h.Version != proto.Version:
		return fmt.Errorf("protocol version mismatch (shim %d, broker %d)", h.Version, proto.Version)
	case !commandName.MatchString(h.Name):
		return fmt.Errorf("invalid command name %q", h.Name)
	case len(h.Args) > maxArgs:
		return errors.New("too many arguments")
	case !filepath.IsAbs(h.Cwd):
		return errors.New("working directory must be absolute")
	}
	for _, a := range append([]string{h.Cwd}, h.Args...) {
		for i := 0; i < len(a); i++ {
			if a[i] == 0 {
				return errors.New("NUL byte in request")
			}
		}
	}
	return nil
}

// checkCwd resolves the working directory and requires it to be inside a
// visible volume and not inside a hidden one.
func (b *Broker) checkCwd(cwd string) (string, error) {
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("working directory %s does not exist on the host", cwd)
	}
	if _, bad := underAny(b.hidden, real); bad {
		return "", errors.New("working directory is inside a hidden path")
	}
	if _, ok := underAny(b.visible, real); !ok {
		return "", errors.New("working directory is outside the mounted volumes")
	}
	return real, nil
}

// execute runs the command and streams its I/O until it exits or the client
// goes away.
func (b *Broker) execute(conn *proto.Conn, in <-chan frame, h *proto.Hello, bin, dir string, env []string) (proto.Exit, error) {
	cmd := &exec.Cmd{Path: bin, Args: append([]string{h.Name}, h.Args...), Dir: dir, Env: env, WaitDelay: 2 * time.Second}

	var stdin io.WriteCloser
	var ptmx *os.File
	outDone := make(chan struct{})

	if h.TTY {
		ws := &pty.Winsize{Rows: h.Rows, Cols: h.Cols}
		if ws.Rows == 0 || ws.Cols == 0 {
			ws.Rows, ws.Cols = 24, 80
		}
		var err error
		if ptmx, err = pty.StartWithSize(cmd, ws); err != nil {
			return proto.Exit{}, err
		}
		stdin = nopCloser{ptmx}
		go func() {
			defer close(outDone)
			copyFrames(conn, proto.TStdout, ptmx)
		}()
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = &frameWriter{conn, proto.TStdout}
		cmd.Stderr = &frameWriter{conn, proto.TStderr}
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			return proto.Exit{}, err
		}
		if err := cmd.Start(); err != nil {
			return proto.Exit{}, err
		}
		close(outDone)
	}
	pgid := cmd.Process.Pid // both modes make the command a group leader
	killGroup := func(sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }

	gone := make(chan struct{}) // the client vanished or timed out
	go func() {
		defer close(gone)
		for f := range in {
			if f.err != nil {
				return
			}
			switch f.t {
			case proto.TStdin:
				_, _ = stdin.Write(f.p)
			case proto.TStdinEOF:
				_ = stdin.Close()
			case proto.TSignal:
				if len(f.p) == 1 && forwardable(syscall.Signal(f.p[0])) {
					killGroup(syscall.Signal(f.p[0]))
				}
			case proto.TResize:
				if rows, cols, ok := proto.ParseResize(f.p); ok && ptmx != nil {
					_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
				}
			case proto.TPing:
			default:
				return // a protocol violation ends the session like a disconnect
			}
		}
	}()

	stopPing := make(chan struct{})
	defer close(stopPing)
	go conn.KeepAlive(stopPing)

	exited := make(chan struct{})
	go func() {
		select {
		case <-gone:
		case <-b.ctx.Done():
		case <-exited:
			return
		}
		killGroup(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(killGrace):
			killGroup(syscall.SIGKILL)
		}
	}()

	waitErr := cmd.Wait()
	close(exited)
	if ptmx != nil {
		select {
		case <-outDone:
		case <-time.After(time.Second):
		}
		ptmx.Close()
	}

	exit := proto.Exit{}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			exit.Signal = int(ws.Signal())
			exit.Code = 128 + exit.Signal
		} else {
			exit.Code = ws.ExitStatus()
		}
	} else if waitErr != nil {
		return proto.Exit{}, waitErr
	}
	_ = conn.WriteJSON(proto.TExit, exit)
	return exit, nil
}

func forwardable(s syscall.Signal) bool {
	switch s {
	case syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM:
		return true
	}
	return false
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// frameWriter sends everything written to it as data frames.
type frameWriter struct {
	c *proto.Conn
	t proto.Type
}

func (w *frameWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), proto.ChunkSize)
		if err := w.c.Write(w.t, p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func copyFrames(c *proto.Conn, t proto.Type, r io.Reader) {
	_, _ = io.Copy(&frameWriter{c, t}, r)
}

// frame is one received message, or the error that ended the connection.
type frame struct {
	t   proto.Type
	p   []byte
	err error
}

// pump reads the connection until it fails or done is closed. The final
// frame carries the error.
func pump(conn *proto.Conn, done <-chan struct{}) <-chan frame {
	ch := make(chan frame, 16)
	go func() {
		defer close(ch)
		for {
			t, p, err := conn.Read()
			f := frame{t: t, p: p, err: err}
			select {
			case ch <- f:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func grantKey(name string, args []string, cwd string) string {
	return strings.Join(append([]string{name, cwd}, args...), "\x00")
}

func (b *Broker) granted(key string) bool {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	return b.grants[key]
}

func (b *Broker) grant(key string) {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	b.grants[key] = true
}

func ruleText(i int, r *config.Rule) string {
	if r == nil {
		return ""
	}
	txt := fmt.Sprintf("rules[%d]: %s", i, r.Command)
	if r.Args != nil {
		txt += " " + strings.Join(r.Args, " ")
	}
	return txt + " (" + r.Decision + ")"
}

// askHuman handles an `ask` decision. It returns whether the command may run,
// who decided, and the message for the agent if not. While the human thinks it
// keeps the connection alive and notices if the agent gives up.
func (b *Broker) askHuman(conn *proto.Conn, in <-chan frame, h *proto.Hello, d policy.Decision, bin, cwd string, passed []string) (bool, string, string) {
	key := grantKey(h.Name, h.Args, cwd)
	if b.granted(key) {
		return true, "session-grant", ""
	}
	if b.opts.Approver == nil {
		return false, "", "approval is not available: start the session with `aicatraz up` (tmux) or --approval-tty"
	}
	select {
	case b.pending <- struct{}{}:
		defer func() { <-b.pending }()
	default:
		return false, "", "too many approvals are already waiting"
	}

	req := approve.Request{
		Name: h.Name, Args: h.Args, Cwd: cwd, Binary: bin, EnvPassed: passed,
		Rule: d.Rule, RuleText: ruleText(d.Rule, d.Config),
	}
	rule := d.Rule
	b.audit(Entry{Event: "ask", PeerPID: 0, Name: h.Name, Args: h.Args, Cwd: cwd, Rule: &rule, Binary: bin, EnvPassed: passed})

	timeout := b.opts.ApprovalTimeout
	ctx, cancel := context.WithTimeout(b.ctx, timeout+approvalSlack)
	defer cancel()

	stopPing := make(chan struct{})
	watcherDone := make(chan struct{})
	go conn.KeepAlive(stopPing)
	go func() {
		defer close(watcherDone)
		for {
			select {
			case f, ok := <-in:
				if !ok || f.err != nil {
					cancel() // the agent's command was interrupted: withdraw the prompt
					return
				}
			case <-stopPing:
				return
			}
		}
	}()
	ans, err := b.opts.Approver.Ask(ctx, req, timeout)
	close(stopPing)
	<-watcherDone

	switch {
	case err == nil && ans == approve.AllowOnce:
		return true, "human", ""
	case err == nil && ans == approve.AllowSession:
		b.grant(key)
		return true, "human", ""
	case err == nil:
		return false, "human", "denied by the user"
	case errors.Is(err, approve.ErrTimeout) || errors.Is(err, context.DeadlineExceeded):
		return false, "timeout", "nobody answered the approval prompt in time"
	case errors.Is(err, context.Canceled):
		return false, "cancelled", "the request was cancelled"
	case errors.Is(err, approve.ErrUnavailable):
		return false, "", "approval is not available: " + err.Error()
	}
	return false, "", "approval failed: " + err.Error()
}
