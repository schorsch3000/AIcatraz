package mux

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/schorsch3000/aicatraz/internal/approve"
)

// session runs a program under Serve in a temp directory.
type session struct {
	t    *testing.T
	dir  string
	code chan int
}

func startSession(t *testing.T, script string) *session {
	t.Helper()
	dir, err := os.MkdirTemp("", "mux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &session{t: t, dir: dir, code: make(chan int, 1)}
	cmd := exec.Command("sh", "-c", script)
	go func() {
		code, err := Serve(Options{Dir: dir, Cmd: cmd, Rows: 24, Cols: 80, Label: "testprog"})
		if err != nil {
			t.Error(err)
		}
		s.code <- code
	}()
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, ApproveSock))
		return err == nil
	})
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *session) sock() string { return filepath.Join(s.dir, AttachSock) }

func (s *session) stop() {
	if conn, err := net.Dial("unix", s.sock()); err == nil {
		writeFrame(conn, fStop, nil)
		conn.Close()
	}
}

func (s *session) wait() int {
	s.t.Helper()
	select {
	case c := <-s.code:
		s.code <- c
		return c
	case <-time.After(10 * time.Second):
		s.t.Fatal("session did not end")
		return -1
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// terminal is the user's terminal: a pty whose slave the client uses.
type terminal struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	seen   bytes.Buffer
	res    chan Result
}

func attach(t *testing.T, s *session) *terminal { return attachBar(t, s, BarOff) }

func attachBar(t *testing.T, s *session, bar Bar) *terminal {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	pty.Setsize(master, &pty.Winsize{Rows: 30, Cols: 100})
	tm := &terminal{t: t, master: master, slave: slave, res: make(chan Result, 1)}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			tm.mu.Lock()
			tm.seen.Write(buf[:n])
			tm.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		r, err := Attach(s.sock(), slave, slave, AttachOptions{Detach: DefaultKey, Bar: bar, Project: "proj"})
		if err != nil {
			t.Errorf("attach: %v", err)
		}
		tm.res <- r
	}()
	t.Cleanup(func() { master.Close(); slave.Close() })
	return tm
}

func (tm *terminal) screen() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.seen.String()
}

func (tm *terminal) expect(what string) {
	tm.t.Helper()
	for i := 0; i < 300; i++ {
		if strings.Contains(tm.screen(), what) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	tm.t.Fatalf("never saw %q in %q", what, tm.screen())
}

func (tm *terminal) type_(s string) { tm.master.WriteString(s) }

func (tm *terminal) result() Result {
	tm.t.Helper()
	select {
	case r := <-tm.res:
		return r
	case <-time.After(10 * time.Second):
		tm.t.Fatal("attach did not return")
		return Result{}
	}
}

func TestInputOutputAndExitCode(t *testing.T) {
	s := startSession(t, `read a; echo "got:$a"; exit 7`)
	tm := attach(t, s)
	tm.type_("hello\r")
	tm.expect("got:hello")
	r := tm.result()
	if !r.Exited || r.Code != 7 {
		t.Errorf("result %+v", r)
	}
	if c := s.wait(); c != 7 {
		t.Errorf("server exit code %d", c)
	}
}

func TestDetachKeepsTheProgramAndReattachWorks(t *testing.T) {
	s := startSession(t, `i=0; while true; do i=$((i+1)); echo tick$i; sleep 0.1; done`)
	tm := attach(t, s)
	tm.expect("tick1")
	tm.type_(string([]byte{DefaultKey.Byte()}))
	if r := tm.result(); !r.Detached {
		t.Fatalf("result %+v", r)
	}
	time.Sleep(600 * time.Millisecond) // the program runs on, unseen
	tm2 := attach(t, s)
	tm2.expect("tick1") // late enough to have missed tick1 .. tick5, not tick6
	if strings.Contains(tm2.screen(), "tick1\r") {
		t.Errorf("output from while nobody was attached must not come back: %q", tm2.screen())
	}
	s.stop()
	s.wait()
}

func TestTerminalModesSurviveReattachAndAreResetOnDetach(t *testing.T) {
	s := startSession(t, `printf '\033[?1000h\033[?2004h'; echo ready; sleep 30`)
	tm := attach(t, s)
	tm.expect("ready")
	tm.type_(string([]byte{DefaultKey.Byte()}))
	tm.result()
	tm.expect("\x1b[?1000l") // detach puts the terminal back to normal
	tm.expect("\x1b[?2004l")
	tm2 := attach(t, s)
	tm2.expect("\x1b[?1000h")
	tm2.expect("\x1b[?2004h")
}

func TestPromptAnsweredOnTheClientAndKeysDoNotReachTheProgram(t *testing.T) {
	s := startSession(t, `while read l; do echo "typed:$l"; done`)
	tm := attach(t, s)
	tm.type_("one\r")
	tm.expect("typed:one")

	ap := &Approver{Sock: filepath.Join(s.dir, ApproveSock)}
	type res struct {
		a   approve.Answer
		err error
	}
	got := make(chan res, 1)
	go func() {
		a, err := ap.Ask(context.Background(), approve.Request{Name: "make", Args: []string{"deploy"}, Cwd: "/p"}, 30*time.Second)
		got <- res{a, err}
	}()
	tm.expect("ON THE HOST")
	tm.expect("deploy")

	tm.type_("y") // inside the arm delay: ignored
	time.Sleep(armDelay + 200*time.Millisecond)
	select {
	case r := <-got:
		t.Fatalf("a key typed before the prompt was armed answered it: %+v", r)
	default:
	}
	tm.type_("s")
	r := <-got
	if r.err != nil || r.a != approve.AllowSession {
		t.Errorf("answer %+v", r)
	}

	// The program saw nothing of it, and works again afterwards.
	tm.type_("two\r")
	tm.expect("typed:two")
	if strings.Contains(tm.screen(), "typed:y") || strings.Contains(tm.screen(), "typed:s") {
		t.Errorf("a prompt key reached the program: %q", tm.screen())
	}
	s.stop()
}

func TestPromptWaitsForAClientAndTimesOutWithoutOne(t *testing.T) {
	s := startSession(t, `sleep 30`)
	ap := &Approver{Sock: filepath.Join(s.dir, ApproveSock)}

	a, err := ap.Ask(context.Background(), approve.Request{Name: "x"}, 1200*time.Millisecond)
	if err != approve.ErrTimeout || a != approve.Deny {
		t.Errorf("nobody attached: %v %v", a, err)
	}

	got := make(chan approve.Answer, 1)
	go func() {
		a, _ := ap.Ask(context.Background(), approve.Request{Name: "waiting-cmd"}, 30*time.Second)
		got <- a
	}()
	time.Sleep(300 * time.Millisecond) // queued, nobody to show it to
	tm := attach(t, s)
	tm.expect("waiting-cmd")
	time.Sleep(armDelay + 100*time.Millisecond)
	tm.type_("y")
	if a := <-got; a != approve.AllowOnce {
		t.Errorf("answer %v", a)
	}
	s.stop()
}

func TestWithdrawnPromptDisappears(t *testing.T) {
	s := startSession(t, `sleep 30`)
	tm := attach(t, s)
	ap := &Approver{Sock: filepath.Join(s.dir, ApproveSock)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ap.Ask(ctx, approve.Request{Name: "gone-cmd"}, 30*time.Second)
		done <- err
	}()
	tm.expect("gone-cmd")
	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("err %v", err)
	}
	tm.expect("\x1b[?1049l") // the overlay was taken away without an answer
	s.stop()
}

func TestSecondClientTakesOver(t *testing.T) {
	s := startSession(t, `sleep 30`)
	tm1 := attach(t, s)
	time.Sleep(200 * time.Millisecond)
	tm2 := attach(t, s)
	r := tm1.result()
	if !strings.Contains(r.Message, "another client") {
		t.Errorf("first client result %+v", r)
	}
	_ = tm2
	s.stop()
}

func TestPromptRequestIsJSONSafe(t *testing.T) {
	// A hostile command line can contain anything; it must survive the trip.
	s := startSession(t, `sleep 30`)
	tm := attach(t, s)
	ap := &Approver{Sock: filepath.Join(s.dir, ApproveSock)}
	go ap.Ask(context.Background(), approve.Request{Name: "evil", Args: []string{"\x1b[2J\n\"quote\"", "ünïcode"}}, 5*time.Second)
	tm.expect(`\u00fcn\u00efcode`) // shown escaped: nothing the agent sends is drawn as it is
	s.stop()
}

func TestOutputBeforeTheFirstAttachIsReplayed(t *testing.T) {
	s := startSession(t, `echo early-output; sleep 30`)
	time.Sleep(300 * time.Millisecond) // the program speaks, nobody is attached
	tm := attach(t, s)
	tm.expect("early-output")
	tm.type_(string([]byte{DefaultKey.Byte()}))
	tm.result()
	tm2 := attach(t, s)
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(tm2.screen(), "early-output") {
		t.Error("only the first client gets what was said before anyone attached")
	}
	s.stop()
}

func TestParseKey(t *testing.T) {
	for spec, want := range map[string]byte{"ctrl-y": 0x19, "Ctrl+Q": 0x11, "^g": 0x07, "ctrl-space": 0, `ctrl-\`: 0x1c, "ctrl-]": 0x1d} {
		k, err := ParseKey(spec)
		if err != nil || k.Byte() != want {
			t.Errorf("%q: %v %v", spec, k, err)
		}
	}
	for _, bad := range []string{"", "y", "ctrl-", "ctrl-yy", "ctrl-1", "alt-y", "ctrl-c", "ctrl-d", "ctrl-z", "ctrl-m", "ctrl-i", "ctrl-["} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	k, _ := ParseKey("ctrl-y")
	if k.Name != "Ctrl-Y" || k.find([]byte("ab\x19cd")) != 2 || k.find([]byte("\x1b[121;5u")) != 0 || k.find([]byte("abc")) != -1 {
		t.Errorf("key %+v", k)
	}
}
