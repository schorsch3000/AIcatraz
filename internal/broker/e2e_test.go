package broker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/proto"
	"github.com/schorsch3000/aicatraz/internal/shim"
)

// The tests start a real broker on a unix socket and run the shim as a
// subprocess (this test binary re-executed), so signals, exit codes and
// terminals behave as they would in the container.

func TestMain(m *testing.M) {
	// Short heartbeat so that tests can show a long approval wait needs pings.
	proto.Interval, proto.Timeout = 100*time.Millisecond, 600*time.Millisecond
	if name := os.Getenv("AICATRAZ_TEST_SHIM"); name != "" {
		shim.SocketPath = os.Getenv("AICATRAZ_TEST_SOCK")
		shim.BinDir = "/nonexistent"
		os.Exit(shim.Run(name, os.Args[1:], shim.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
	}
	os.Exit(m.Run())
}

type env struct {
	t     *testing.T
	b     *broker.Broker
	sock  string
	dir   string // the rw volume
	audit string
}

func setup(t *testing.T, rulesYAML string, opts ...func(*broker.Options)) *env {
	t.Helper()
	root, err := os.MkdirTemp("", "bk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, ps := config.Parse([]byte("volumes:\n  - "+dir+":rw\n"+rulesYAML), config.Options{Home: "/home/nobody"})
	if config.HasErrors(ps) {
		t.Fatalf("config: %+v", ps)
	}
	auditPath := filepath.Join(root, "audit.log")
	audit, err := broker.OpenAudit(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	o := broker.Options{
		UID:     os.Getuid(),
		HostEnv: []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/hosty", "USER=hosty", "HOST_ONLY_SECRET=1"},
		Session: "test",
		Audit:   audit,
	}
	for _, f := range opts {
		f(&o)
	}
	b, err := broker.New(cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go b.Serve(l)
	t.Cleanup(func() { b.Close(); audit.Close() })
	return &env{t: t, b: b, sock: sock, dir: dir, audit: auditPath}
}

type result struct {
	stdout, stderr string
	code           int
}

// shimCmd prepares the shim subprocess for `name args...`, running in the rw volume.
func (e *env) shimCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = e.dir
	cmd.Env = []string{"AICATRAZ_TEST_SHIM=" + name, "AICATRAZ_TEST_SOCK=" + e.sock, "PATH=/usr/bin:/bin", "TERM=xterm"}
	return cmd
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func (e *env) run(stdin, name string, args ...string) result {
	e.t.Helper()
	cmd := e.shimCmd(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = strings.NewReader(stdin)
	err := cmd.Run()
	return result{out.String(), errb.String(), exitCode(e.t, err)}
}

func (e *env) auditEntries() []broker.Entry {
	e.t.Helper()
	data, err := os.ReadFile(e.audit)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []broker.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var en broker.Entry
		if err := json.Unmarshal([]byte(line), &en); err != nil {
			e.t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, en)
	}
	return out
}

const allowAll = `
rules:
  - command: "*"
    decision: allow
`

func TestRunsAllowedCommand(t *testing.T) {
	e := setup(t, allowAll)
	r := e.run("", "echo", "hello", "world")
	if r.code != 0 || r.stdout != "hello world\n" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
}

func TestRunsInTheRequestedDirectory(t *testing.T) {
	e := setup(t, allowAll)
	r := e.run("", "pwd")
	if want, _ := filepath.EvalSymlinks(e.dir); strings.TrimSpace(r.stdout) != want {
		t.Errorf("pwd %q, want %q", r.stdout, want)
	}
}

func TestExitCodeAndStreams(t *testing.T) {
	e := setup(t, allowAll)
	r := e.run("", "sh", "-c", "echo out; echo err >&2; exit 3")
	if r.code != 3 || r.stdout != "out\n" || r.stderr != "err\n" {
		t.Fatalf("%+v", r)
	}
}

func TestStdinIsForwarded(t *testing.T) {
	e := setup(t, allowAll)
	big := strings.Repeat("0123456789abcdef", 20000) // 320 KB: several frames
	r := e.run(big, "cat")
	if r.code != 0 || r.stdout != big {
		t.Fatalf("stdin round trip failed: code %d, %d bytes back", r.code, len(r.stdout))
	}
}

func TestLargeOutput(t *testing.T) {
	e := setup(t, allowAll)
	r := e.run("", "sh", "-c", "head -c 3000000 /dev/zero")
	if r.code != 0 || len(r.stdout) != 3000000 {
		t.Fatalf("code %d, %d bytes", r.code, len(r.stdout))
	}
}

func TestDeniedWithoutRule(t *testing.T) {
	e := setup(t, "rules:\n  - command: echo\n    decision: allow\n")
	r := e.run("", "rm", "-rf", "x")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "does not allow") {
		t.Fatalf("%+v", r)
	}
	entries := e.auditEntries()
	last := entries[len(entries)-1]
	if last.Decision != "deny" || last.Name != "rm" || last.Rule != nil {
		t.Errorf("audit: %+v", last)
	}
}

func TestAskWithoutAnApproverIsDenied(t *testing.T) {
	e := setup(t, "rules:\n  - command: echo\n    decision: ask\n")
	r := e.run("", "echo", "x")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "approval") {
		t.Fatalf("%+v", r)
	}
}

func TestDenyFlagsStopTheRule(t *testing.T) {
	e := setup(t, `
rules:
  - command: sh
    args: ["*"]
    deny_flags: [-c]
    decision: allow
`)
	if r := e.run("", "sh", "-c", "echo pwned"); r.code != shim.ExitDenied || strings.Contains(r.stdout, "pwned") {
		t.Fatalf("%+v", r)
	}
}

func TestLocalDecisionRunsTheContainersBinary(t *testing.T) {
	e := setup(t, "rules:\n  - command: echo\n    decision: local\n")
	// "Local" means the shim execs its own PATH's echo, never the broker's.
	r := e.run("", "echo", "from-the-container")
	if r.code != 0 || r.stdout != "from-the-container\n" {
		t.Fatalf("%+v", r)
	}
	for _, en := range e.auditEntries() {
		if en.Event == "request" && en.Decision != "local" {
			t.Errorf("audit: %+v", en)
		}
		if en.Event == "exit" {
			t.Errorf("a local command must not appear as run on the host: %+v", en)
		}
	}
}

func TestEnvironmentIsWhitelisted(t *testing.T) {
	e := setup(t, `
global:
  env_strip: [STRIPPED_*]
rules:
  - command: env
    decision: allow
    env_pass: [FOO, STRIPPED_ONE]
`)
	cmd := e.shimCmd("env")
	cmd.Env = append(cmd.Env, "FOO=1", "BAR=2", "LD_PRELOAD=/evil.so", "STRIPPED_ONE=x", "BASH_ENV=/evil", "TERM=xterm-256color")
	cmd.Env = append(cmd.Env, "PATH=/evil") // the container's PATH must not reach the command
	out, err := cmd.Output()
	if exitCode(t, err) != 0 {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	for k, want := range map[string]string{"FOO": "1", "HOME": "/home/hosty", "USER": "hosty", "TERM": "xterm-256color"} {
		if got[k] != want {
			t.Errorf("%s=%q, want %q (env: %v)", k, got[k], want, got)
		}
	}
	for _, k := range []string{"BAR", "LD_PRELOAD", "BASH_ENV", "STRIPPED_ONE", "HOST_ONLY_SECRET", "AICATRAZ_TEST_SHIM"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must not reach the command (env: %v)", k, got)
		}
	}
	if strings.Contains(got["PATH"], "evil") || got["PATH"] == "" {
		t.Errorf("PATH must come from the broker: %q", got["PATH"])
	}
}

func TestCwdMustBeInAVolume(t *testing.T) {
	e := setup(t, allowAll)
	cmd := e.shimCmd("pwd")
	cmd.Dir = "/"
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	if exitCode(t, err) != shim.ExitDenied || !strings.Contains(errb.String(), "outside the mounted volumes") {
		t.Fatalf("%v %s", err, errb.String())
	}
}

func TestCwdInsideHiddenPathIsRefused(t *testing.T) {
	root, _ := os.MkdirTemp("", "bk")
	defer os.RemoveAll(root)
	work := filepath.Join(root, "w")
	os.MkdirAll(filepath.Join(work, "secret"), 0o755)
	cfg, ps := config.Parse([]byte("volumes:\n  - "+work+":rw\n  - "+work+"/secret:hi\nrules:\n  - command: pwd\n    decision: allow\n"), config.Options{Home: "/home/nobody"})
	if config.HasErrors(ps) {
		t.Fatal(ps)
	}
	b, err := broker.New(cfg, broker.Options{UID: os.Getuid(), HostEnv: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "s")
	l, _ := net.Listen("unix", sock)
	go b.Serve(l)
	defer b.Close()
	cmd := exec.Command(os.Args[0])
	cmd.Dir = filepath.Join(work, "secret")
	cmd.Env = []string{"AICATRAZ_TEST_SHIM=pwd", "AICATRAZ_TEST_SOCK=" + sock}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if exitCode(t, cmd.Run()) != shim.ExitDenied || !strings.Contains(errb.String(), "hidden") {
		t.Fatalf("%s", errb.String())
	}
}

func TestBinaryInWritableVolumeIsNotRun(t *testing.T) {
	root, _ := os.MkdirTemp("", "bk")
	defer os.RemoveAll(root)
	work := filepath.Join(root, "w")
	os.MkdirAll(filepath.Join(work, "bin"), 0o755)
	planted := filepath.Join(work, "bin", "deploy")
	os.WriteFile(planted, []byte("#!/bin/sh\necho PLANTED\n"), 0o755)

	cfg, _ := config.Parse([]byte("volumes:\n  - "+work+":rw\nrules:\n  - command: deploy\n    decision: allow\n"), config.Options{Home: "/home/nobody"})
	b, err := broker.New(cfg, broker.Options{UID: os.Getuid(), HostEnv: []string{"PATH=" + filepath.Join(work, "bin") + ":/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "s")
	l, _ := net.Listen("unix", sock)
	go b.Serve(l)
	defer b.Close()

	cmd := exec.Command(os.Args[0])
	cmd.Dir = work
	cmd.Env = []string{"AICATRAZ_TEST_SHIM=deploy", "AICATRAZ_TEST_SOCK=" + sock}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := exitCode(t, cmd.Run())
	if code != shim.ExitDenied || strings.Contains(out.String(), "PLANTED") || !strings.Contains(errb.String(), "writable volume") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
}

func TestRelativePathEntriesAreIgnored(t *testing.T) {
	e := setup(t, allowAll, func(o *broker.Options) { o.HostEnv = []string{"PATH=.:bin:/usr/bin"} })
	os.MkdirAll(filepath.Join(e.dir, "bin"), 0o755)
	os.WriteFile(filepath.Join(e.dir, "echo"), []byte("#!/bin/sh\necho PLANTED\n"), 0o755)
	os.WriteFile(filepath.Join(e.dir, "bin", "echo"), []byte("#!/bin/sh\necho PLANTED\n"), 0o755)
	if r := e.run("", "echo", "real"); r.stdout != "real\n" {
		t.Fatalf("%+v", r)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	e := setup(t, allowAll, func(o *broker.Options) { o.MaxConcurrent = 1 })
	first := e.shimCmd("sleep", "30")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { first.Process.Kill(); first.Wait() }()
	waitFor(t, "first command to start", func() bool { return len(e.auditEntries()) >= 1 })

	r := e.run("", "echo", "second")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "too many") {
		t.Fatalf("%+v", r)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestDisconnectKillsTheProcessGroup(t *testing.T) {
	e := setup(t, allowAll)
	pidfile := filepath.Join(e.dir, "pid")
	// The shell and its child (a sleep in the same group) must both die.
	shimc := e.shimCmd("sh", "-c", "echo $$ > pid; sleep 60 & echo $! >> pid; wait")
	if err := shimc.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pid file", func() bool {
		b, _ := os.ReadFile(pidfile)
		return strings.Count(string(b), "\n") >= 2
	})
	b, _ := os.ReadFile(pidfile)
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		p, _ := strconv.Atoi(f)
		pids = append(pids, p)
	}

	shimc.Process.Kill() // the agent side vanishes without a word
	shimc.Wait()
	waitFor(t, "host processes to be killed", func() bool {
		for _, p := range pids {
			if alive(p) {
				// zombies of already-reaped children report alive until reaped by init; check state
				if st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p)); !strings.Contains(string(st), ") Z ") {
					return false
				}
			}
		}
		return true
	})
}

func TestSignalIsForwarded(t *testing.T) {
	e := setup(t, allowAll)
	shimc := e.shimCmd("sleep", "60")
	var out bytes.Buffer
	shimc.Stdout = &out
	if err := shimc.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "command to start", func() bool { return len(e.auditEntries()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	shimc.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- shimc.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(t, err); code != 128+int(syscall.SIGTERM) {
			t.Errorf("exit code %d, want 143", code)
		}
	case <-time.After(5 * time.Second):
		shimc.Process.Kill()
		t.Fatal("signal was not forwarded")
	}
	waitFor(t, "exit audit entry", func() bool {
		for _, en := range e.auditEntries() {
			if en.Event == "exit" && en.Signal == int(syscall.SIGTERM) {
				return true
			}
		}
		return false
	})
}

func TestBrokerGoneEndsTheShim(t *testing.T) {
	e := setup(t, allowAll)
	shimc := e.shimCmd("sleep", "60")
	var errb bytes.Buffer
	shimc.Stderr = &errb
	if err := shimc.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "command to start", func() bool { return len(e.auditEntries()) >= 1 })
	go e.b.Close()
	done := make(chan error, 1)
	go func() { done <- shimc.Wait() }()
	select {
	case err := <-done:
		// Closing the broker kills the command, so the shim sees an exit (or a lost connection).
		if code := exitCode(t, err); code == 0 {
			t.Errorf("exit code 0 after the broker went away; stderr %q", errb.String())
		}
	case <-time.After(8 * time.Second):
		shimc.Process.Kill()
		t.Fatal("shim hung after the broker went away")
	}
}

func TestShimWithoutBrokerFailsClearly(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = []string{"AICATRAZ_TEST_SHIM=echo", "AICATRAZ_TEST_SOCK=/nonexistent/s.sock"}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	start := time.Now()
	code := exitCode(t, cmd.Run())
	if code != shim.ExitUnavailable || !strings.Contains(errb.String(), "cannot reach the broker") {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if time.Since(start) > 15*time.Second {
		t.Error("retrying took too long")
	}
}

func TestProtocolViolations(t *testing.T) {
	e := setup(t, allowAll)
	try := func(h proto.Hello) proto.Decision {
		t.Helper()
		c, err := net.Dial("unix", e.sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conn := proto.New(c)
		conn.WriteJSON(proto.THello, h)
		var d proto.Decision
		if err := conn.ReadJSON(proto.TDecision, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	ok := proto.Hello{Version: proto.Version, Name: "echo", Cwd: e.dir}
	for name, mutate := range map[string]func(*proto.Hello){
		"path as name":  func(h *proto.Hello) { h.Name = "/bin/echo" },
		"dotdot":        func(h *proto.Hello) { h.Name = ".." },
		"empty name":    func(h *proto.Hello) { h.Name = "" },
		"wrong version": func(h *proto.Hello) { h.Version = 99 },
		"relative cwd":  func(h *proto.Hello) { h.Cwd = "work" },
		"NUL in arg":    func(h *proto.Hello) { h.Args = []string{"a\x00b"} },
		"leading dash":  func(h *proto.Hello) { h.Name = "-rf" },
	} {
		h := ok
		mutate(&h)
		if d := try(h); d.Action != proto.ActionDeny {
			t.Errorf("%s: got %+v", name, d)
		}
	}
}

func TestAuditLog(t *testing.T) {
	e := setup(t, allowAll)
	e.run("", "sh", "-c", "exit 4")
	var req, exit *broker.Entry
	for _, en := range e.auditEntries() {
		en := en
		switch en.Event {
		case "request":
			req = &en
		case "exit":
			exit = &en
		}
	}
	if req == nil || exit == nil {
		t.Fatal("need a request and an exit entry")
	}
	if req.Decision != "allow" || req.Name != "sh" || req.Session != "test" || !strings.HasSuffix(req.Binary, "/sh") || req.Rule == nil || *req.Rule != 0 {
		t.Errorf("request: %+v", req)
	}
	if exit.ExitCode == nil || *exit.ExitCode != 4 {
		t.Errorf("exit: %+v", exit)
	}
}

func TestAuditFailureBlocksExecution(t *testing.T) {
	e := setup(t, allowAll)
	// Break the log by closing its file through a second, read-only broker option is awkward;
	// instead point a broker at a closed audit.
	audit, err := broker.OpenAudit(filepath.Join(t.TempDir(), "a.log"))
	if err != nil {
		t.Fatal(err)
	}
	audit.Close()
	_ = e
	e2 := setup(t, allowAll, func(o *broker.Options) { o.Audit = audit })
	r := e2.run("", "echo", "should-not-run")
	if r.code != shim.ExitDenied || strings.Contains(r.stdout, "should-not-run") || !strings.Contains(r.stderr, "audit") {
		t.Fatalf("%+v", r)
	}
}

// syncBuf is a bytes.Buffer safe for one writer and polling readers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestTerminalSession(t *testing.T) {
	e := setup(t, allowAll)
	cmd := e.shimCmd("sh", "-c", "tty; stty size; cat")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 31, Cols: 101})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()

	var out syncBuf
	readDone := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				close(readDone)
				return
			}
		}
	}()
	waitForOutput := func(substr string) {
		t.Helper()
		waitFor(t, fmt.Sprintf("%q in output", substr), func() bool { return strings.Contains(out.String(), substr) })
	}
	waitForOutput("31 101") // the host pty got the container terminal's size
	if !strings.Contains(out.String(), "/dev/pts/") {
		t.Errorf("command should run on a pty: %q", out.String())
	}
	ptmx.Write([]byte("typed-text\n"))
	waitForOutput("typed-text")
	ptmx.Write([]byte{4}) // Ctrl-D ends cat
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(t, err); code != 0 {
			t.Errorf("exit %d, output %q", code, out.String())
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("session did not end; output %q", out.String())
	}
	<-readDone
}

func TestTerminalResizeAndCtrlC(t *testing.T) {
	e := setup(t, allowAll)
	cmd := e.shimCmd("sh", "-c", `trap 'echo caught-int; exit 9' INT; echo ready; while :; do sleep 0.05; stty size; done`)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 20, Cols: 70})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	var out syncBuf
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}()
	waitFor(t, "ready", func() bool { return strings.Contains(out.String(), "ready") })
	pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120}) // sends SIGWINCH to the shim
	waitFor(t, "resized terminal", func() bool { return strings.Contains(out.String(), "40 120") })
	ptmx.Write([]byte{3}) // Ctrl-C travels as a byte through the pty to the host terminal
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(t, err); code != 9 {
			t.Errorf("exit %d, output %q", code, out.String())
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("Ctrl-C did not reach the command; output %q", out.String())
	}
}

// fakeApprover stands in for the human.
type fakeApprover struct {
	mu        sync.Mutex
	answer    approve.Answer
	err       error
	delay     time.Duration
	asked     []approve.Request
	cancelled bool
}

func (f *fakeApprover) Ask(ctx context.Context, req approve.Request, timeout time.Duration) (approve.Answer, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req)
	delay, answer, err := f.delay, f.answer, f.err
	f.mu.Unlock()
	select {
	case <-time.After(delay):
		return answer, err
	case <-ctx.Done():
		f.mu.Lock()
		f.cancelled = true
		f.mu.Unlock()
		return approve.Deny, ctx.Err()
	}
}

func (f *fakeApprover) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

func (f *fakeApprover) request(i int) approve.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[i]
}

func (f *fakeApprover) wasCancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

const askEcho = `
rules:
  - command: echo
    decision: ask
    env_pass: [FOO]
  - command: cat
    decision: ask
`

func withApprover(a approve.Approver) func(*broker.Options) {
	return func(o *broker.Options) { o.Approver = a }
}

func TestAskApprovedOnce(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowOnce}
	e := setup(t, askEcho, withApprover(f))
	for i := 0; i < 2; i++ {
		if r := e.run("", "echo", "hi"); r.code != 0 || r.stdout != "hi\n" {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
	if f.count() != 2 {
		t.Errorf("approval once must ask every time, asked %d times", f.count())
	}
	req := f.request(0)
	if req.Name != "echo" || req.Args[0] != "hi" || !strings.HasSuffix(req.Binary, "/echo") || !strings.Contains(req.RuleText, "rules[0]") {
		t.Errorf("request shown to the human: %+v", req)
	}
	var sawAsk, sawHuman bool
	for _, en := range e.auditEntries() {
		sawAsk = sawAsk || en.Event == "ask"
		sawHuman = sawHuman || (en.Event == "request" && en.Decision == "allow" && en.DecidedBy == "human")
	}
	if !sawAsk || !sawHuman {
		t.Errorf("audit lacks ask/human entries: %+v", e.auditEntries())
	}
}

func TestAskSessionGrantCoversTheExactCallOnly(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowSession}
	e := setup(t, askEcho, withApprover(f))
	e.run("", "echo", "a")
	e.run("", "echo", "a") // same call: no prompt
	if f.count() != 1 {
		t.Fatalf("asked %d times, want 1", f.count())
	}
	e.run("", "echo", "b") // different arguments: prompt again
	if f.count() != 2 {
		t.Errorf("asked %d times, want 2", f.count())
	}
	var grants int
	for _, en := range e.auditEntries() {
		if en.DecidedBy == "session-grant" {
			grants++
		}
	}
	if grants != 1 {
		t.Errorf("session-grant entries: %d", grants)
	}
}

func TestAskDeniedByTheUser(t *testing.T) {
	f := &fakeApprover{answer: approve.Deny}
	e := setup(t, askEcho, withApprover(f))
	r := e.run("", "echo", "x")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "denied by the user") || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
}

func TestAskTimeoutAndUnavailable(t *testing.T) {
	e := setup(t, askEcho, withApprover(&fakeApprover{err: approve.ErrTimeout}))
	r := e.run("", "echo", "x")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "in time") {
		t.Fatalf("timeout: %+v", r)
	}
	var by string
	for _, en := range e.auditEntries() {
		if en.Event == "request" {
			by = en.DecidedBy
		}
	}
	if by != "timeout" {
		t.Errorf("decided_by %q", by)
	}

	e = setup(t, askEcho, withApprover(&fakeApprover{err: fmt.Errorf("%w: no tmux", approve.ErrUnavailable)}))
	if r := e.run("", "echo", "x"); r.code != shim.ExitDenied || !strings.Contains(r.stderr, "not available") {
		t.Fatalf("unavailable: %+v", r)
	}
}

func TestAskLongWaitKeepsTheShimAlive(t *testing.T) {
	// The heartbeat timeout is 600ms in these tests; the human takes 2s.
	f := &fakeApprover{answer: approve.AllowOnce, delay: 2 * time.Second}
	e := setup(t, askEcho, withApprover(f))
	r := e.run("", "echo", "patient")
	if r.code != 0 || r.stdout != "patient\n" {
		t.Fatalf("%+v", r)
	}
}

func TestAskWithdrawnWhenTheAgentGivesUp(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowOnce, delay: 30 * time.Second}
	e := setup(t, askEcho, withApprover(f))
	shimc := e.shimCmd("echo", "never")
	if err := shimc.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prompt", func() bool { return f.count() == 1 })
	shimc.Process.Kill() // Ctrl-C / the agent abandons the call
	shimc.Wait()
	waitFor(t, "prompt to be withdrawn", f.wasCancelled)
}

func TestAskStdinStillWorksAfterApproval(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowOnce, delay: 300 * time.Millisecond}
	e := setup(t, askEcho, withApprover(f))
	big := strings.Repeat("x", 200000)
	if r := e.run(big, "cat"); r.code != 0 || r.stdout != big {
		t.Fatalf("code %d, %d bytes back", r.code, len(r.stdout))
	}
}

func TestTooManyPendingApprovals(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowOnce, delay: time.Minute}
	e := setup(t, askEcho, withApprover(f))
	var procs []*exec.Cmd
	for i := 0; i < 4; i++ {
		c := e.shimCmd("echo", fmt.Sprint(i))
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, c)
	}
	defer func() {
		for _, c := range procs {
			c.Process.Kill()
			c.Wait()
		}
	}()
	waitFor(t, "four prompts", func() bool { return f.count() == 4 })
	r := e.run("", "echo", "fifth")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "too many approvals") {
		t.Fatalf("%+v", r)
	}
}

func TestAskPassesOnlyNamedEnvToTheHuman(t *testing.T) {
	f := &fakeApprover{answer: approve.Deny}
	e := setup(t, askEcho, withApprover(f))
	cmd := e.shimCmd("echo", "x")
	cmd.Env = append(cmd.Env, "FOO=1", "BAR=2")
	cmd.Run()
	if got := f.request(0).EnvPassed; len(got) != 1 || got[0] != "FOO" {
		t.Errorf("env shown for approval: %v", got)
	}
}
