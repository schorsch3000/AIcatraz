// SPDX-License-Identifier: GPL-3.0-only

package broker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"github.com/schorsch3000/aicatraz/internal/pins"
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
	store, err := pins.Open(filepath.Join(root, "pins"))
	if err != nil {
		t.Fatal(err)
	}
	o := broker.Options{
		Pins:    store,
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

// ---- pinned dependent files ----

// depRules: `cat <file>` runs without asking as long as that file is the one
// a human approved; `*.sh` files too, as a glob.
const depRules = `
rules:
  - command: cat
    args: ["*"]
    decision: allow
    depends_on: ["{args[0]}"]
  - command: sh
    args: ["*"]
    decision: allow
    depends_on: ["scripts/*.sh"]
`

func (e *env) writeFile(rel, content string) string {
	e.t.Helper()
	p := filepath.Join(e.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestAllowRuleWithPinnedFilesRunsSilently(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowPin}
	e := setup(t, depRules, withApprover(f))
	e.writeFile("data.txt", "v1\n")

	// Never approved: the allow rule asks first, and the human pins the file.
	if r := e.run("", "cat", "data.txt"); r.code != 0 || r.stdout != "v1\n" {
		t.Fatalf("first run: %+v", r)
	}
	req := f.request(0)
	if !req.CanPin || len(req.Files) != 1 || req.Files[0].Status != "never approved before" ||
		!strings.Contains(req.Files[0].Diff, "+v1") || len(req.Warnings) == 0 {
		t.Errorf("first prompt: %+v", req)
	}

	// Pinned now: the same call runs without a prompt.
	if r := e.run("", "cat", "data.txt"); r.code != 0 || r.stdout != "v1\n" {
		t.Fatalf("second run: %+v", r)
	}
	if f.count() != 1 {
		t.Errorf("a pinned file must not prompt again, asked %d times", f.count())
	}

	// The agent edits the file: the next call asks again, with the diff.
	e.writeFile("data.txt", "v2 changed\n")
	f.mu.Lock()
	f.answer = approve.AllowOnce
	f.mu.Unlock()
	if r := e.run("", "cat", "data.txt"); r.code != 0 || r.stdout != "v2 changed\n" {
		t.Fatalf("after edit: %+v", r)
	}
	req = f.request(1)
	if len(req.Files) != 1 || req.Files[0].Status != "changed since last approved" ||
		!strings.Contains(req.Files[0].Diff, "-v1") || !strings.Contains(req.Files[0].Diff, "+v2 changed") {
		t.Errorf("changed prompt: %+v", req)
	}

	// Approving once does not move the pin: it asks again.
	e.run("", "cat", "data.txt")
	if f.count() != 3 {
		t.Errorf("run-once must not re-pin; asked %d times", f.count())
	}

	// Approving with the pin option moves it.
	f.mu.Lock()
	f.answer = approve.AllowPin
	f.mu.Unlock()
	e.run("", "cat", "data.txt")
	e.run("", "cat", "data.txt")
	if f.count() != 4 {
		t.Errorf("after re-pinning there must be no more prompts; asked %d times", f.count())
	}

	var sawAttention, sawPinned bool
	for _, en := range e.auditEntries() {
		sawAttention = sawAttention || (en.Pins == "attention" && strings.Contains(en.PinNotes, "data.txt"))
		sawPinned = sawPinned || strings.Contains(en.PinNotes, "[pinned]")
	}
	if !sawAttention || !sawPinned {
		t.Errorf("audit lacks pin results: %+v", e.auditEntries())
	}
}

func TestChangedFileWithoutApproverIsDeniedWithTheReason(t *testing.T) {
	e := setup(t, depRules) // no approver
	e.writeFile("data.txt", "v1\n")
	r := e.run("", "cat", "data.txt")
	if r.code != shim.ExitDenied || r.stdout != "" || !strings.Contains(r.stderr, "data.txt: never approved before") {
		t.Fatalf("%+v", r)
	}
}

func TestOnlyAHumanAnswerMovesAPin(t *testing.T) {
	e := setup(t, depRules, withApprover(&fakeApprover{answer: approve.Deny}))
	e.writeFile("data.txt", "v1\n")
	for i := 0; i < 3; i++ {
		if r := e.run("", "cat", "data.txt"); r.code != shim.ExitDenied {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
	// Nothing in the agent's reach can create a pin either: its own writes are just changes.
	e2 := setup(t, depRules)
	e2.writeFile("data.txt", "v1\n")
	e2.run("", "cat", "data.txt")
	e2.run("", "cat", "data.txt")
	if r := e2.run("", "cat", "data.txt"); r.code != shim.ExitDenied {
		t.Errorf("repetition must not create trust: %+v", r)
	}
}

func TestSessionGrantDoesNotSurviveAFileChange(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowSession}
	e := setup(t, depRules, withApprover(f))
	e.writeFile("data.txt", "v1\n")
	e.run("", "cat", "data.txt")
	e.run("", "cat", "data.txt") // granted for exactly this content
	if f.count() != 1 {
		t.Fatalf("asked %d times", f.count())
	}
	e.writeFile("data.txt", "v2\n")
	if r := e.run("", "cat", "data.txt"); r.code != 0 || r.stdout != "v2\n" {
		t.Fatalf("%+v", r)
	}
	if f.count() != 2 {
		t.Errorf("a changed file must void the grant; asked %d times", f.count())
	}
}

func TestNewAndDeletedFilesMatchingAGlobAreNoticed(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowPin}
	e := setup(t, depRules, withApprover(f))
	e.writeFile("scripts/a.sh", "echo a\n")
	e.writeFile("scripts/b.sh", "echo b\n")
	if r := e.run("", "sh", "scripts/a.sh"); r.code != 0 || r.stdout != "a\n" {
		t.Fatalf("%+v", r)
	}
	if got := f.request(0).Files; len(got) != 2 {
		t.Fatalf("both scripts are dependencies: %+v", got)
	}
	e.run("", "sh", "scripts/a.sh")
	if f.count() != 1 {
		t.Fatalf("pinned: asked %d times", f.count())
	}

	e.writeFile("scripts/evil.sh", "echo evil\n") // a new file appears
	e.run("", "sh", "scripts/a.sh")
	if f.count() != 2 || len(f.request(1).Files) != 1 || f.request(1).Files[0].Status != "never approved before" {
		t.Fatalf("new file not noticed: %+v", f.request(1))
	}

	os.Remove(filepath.Join(e.dir, "scripts", "b.sh")) // one disappears
	e.run("", "sh", "scripts/a.sh")
	if f.count() != 3 || f.request(2).Files[0].Status != "deleted since last approved" {
		t.Fatalf("deleted file not noticed: %+v", f.request(2))
	}
}

func TestFileChangedWhileTheHumanDecides(t *testing.T) {
	// An approver that approves, but only after the agent has swapped the file.
	swap := &swappingApprover{}
	e := setup(t, depRules, withApprover(swap))
	swap.set(e.writeFile("data.txt", "v1\n"))
	r := e.run("", "cat", "data.txt")
	if r.code != shim.ExitDenied || strings.Contains(r.stdout, "evil") || !strings.Contains(r.stderr, "changed while") {
		t.Fatalf("%+v", r)
	}
}

type swappingApprover struct {
	mu   sync.Mutex
	path string
}

func (s *swappingApprover) set(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = p
}

func (s *swappingApprover) Ask(ctx context.Context, req approve.Request, timeout time.Duration) (approve.Answer, error) {
	s.mu.Lock()
	p := s.path
	s.mu.Unlock()
	os.WriteFile(p, []byte("evil\n"), 0o644)
	return approve.AllowOnce, nil
}

func TestDependentFilesMustBeInVisibleVolumes(t *testing.T) {
	e := setup(t, depRules, withApprover(&fakeApprover{answer: approve.AllowPin}))
	r := e.run("", "cat", "/etc/passwd")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "outside the mounted volumes") {
		t.Fatalf("%+v", r)
	}
}

func TestHiddenFileCannotBeADependency(t *testing.T) {
	root, _ := os.MkdirTemp("", "bk")
	defer os.RemoveAll(root)
	work := filepath.Join(root, "w")
	os.MkdirAll(work, 0o755)
	os.WriteFile(filepath.Join(work, ".env"), []byte("SECRET=1\n"), 0o644)
	cfg, ps := config.Parse([]byte("volumes:\n  - "+work+":rw\n  - "+work+"/.env:hi\n"+depRules), config.Options{Home: "/home/nobody"})
	if config.HasErrors(ps) {
		t.Fatal(ps)
	}
	store, _ := pins.Open(filepath.Join(root, "pins"))
	b, err := broker.New(cfg, broker.Options{UID: os.Getuid(), HostEnv: os.Environ(), Pins: store, Approver: &fakeApprover{answer: approve.AllowPin}})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "s")
	l, _ := net.Listen("unix", sock)
	go b.Serve(l)
	defer b.Close()
	cmd := exec.Command(os.Args[0], ".env")
	cmd.Dir = work
	cmd.Env = []string{"AICATRAZ_TEST_SHIM=cat", "AICATRAZ_TEST_SOCK=" + sock}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := exitCode(t, cmd.Run())
	if code != shim.ExitDenied || strings.Contains(out.String(), "SECRET") || !strings.Contains(errb.String(), "hidden") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
}

func TestAskRuleStillAsksWhenFilesAreClean(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowPin}
	e := setup(t, `
rules:
  - command: cat
    args: ["*"]
    decision: ask
    depends_on: ["{args[0]}"]
`, withApprover(f))
	e.writeFile("data.txt", "v1\n")
	e.run("", "cat", "data.txt") // asks, pins
	e.run("", "cat", "data.txt") // clean, but the rule says ask
	if f.count() != 2 {
		t.Fatalf("asked %d times", f.count())
	}
	if req := f.request(1); len(req.Files) != 0 || req.CanPin {
		t.Errorf("clean files need no warning or pin option: %+v", req)
	}
}

func TestTooManyDependenciesAlwaysAsk(t *testing.T) {
	f := &fakeApprover{answer: approve.AllowPin}
	e := setup(t, `
rules:
  - command: cat
    args: ["*"]
    decision: allow
    depends_on: ["many/*"]
`, withApprover(f))
	for i := 0; i < pins.MaxFiles+3; i++ {
		e.writeFile(fmt.Sprintf("many/f%03d", i), "x")
	}
	e.writeFile("data.txt", "v")
	e.run("", "cat", "data.txt")
	e.run("", "cat", "data.txt")
	if f.count() != 2 {
		t.Errorf("an unpinnable rule must ask every time; asked %d", f.count())
	}
	if req := f.request(0); req.CanPin || len(req.Files) != 1 || !strings.Contains(req.Files[0].Status, "too many") {
		t.Errorf("%+v", req)
	}
}

func TestDependsOnWithoutAPinStoreFailsClosed(t *testing.T) {
	e := setup(t, depRules, func(o *broker.Options) { o.Pins = nil })
	e.writeFile("data.txt", "v1\n")
	r := e.run("", "cat", "data.txt")
	if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "pin store") {
		t.Fatalf("%+v", r)
	}
}

// ---- confine_paths ----

const confineRules = `
rules:
  - command: cat
    args: ["*"]
    decision: allow
    confine_paths: true
`

func TestConfinePaths(t *testing.T) {
	e := setup(t, confineRules)
	e.writeFile("data.txt", "inside\n")
	e.writeFile("sub/deep.txt", "deep\n")
	os.Symlink("/etc/hostname", filepath.Join(e.dir, "link-out"))
	os.Symlink(filepath.Join(e.dir, "data.txt"), filepath.Join(e.dir, "link-in"))
	os.Symlink("/", filepath.Join(e.dir, "rootlink"))

	allowed := [][]string{
		{"data.txt"},
		{"./data.txt"},
		{"sub/deep.txt"},
		{"sub/../data.txt"},
		{"link-in"},
		{"-n", "data.txt"},
		{"new/dir/not-yet.txt"}, // does not exist, but would be inside
		{"https://example.com/a/b"},
		{"origin/main"}, // looks like a ref; resolves inside the workspace
		{e.dir + "/data.txt"},
	}
	for _, args := range allowed {
		r := e.run("", "cat", args...)
		if r.code == shim.ExitDenied && strings.Contains(r.stderr, "volumes") {
			t.Errorf("%v must be allowed: %s", args, r.stderr)
		}
	}
	if r := e.run("", "cat", "data.txt"); r.stdout != "inside\n" {
		t.Errorf("the allowed call must really run: %+v", r)
	}

	denied := [][]string{
		{"/etc/passwd"},
		{"../outside.txt"},
		{"sub/../../outside.txt"},
		{"link-out"},                 // a symlink pointing out
		{"rootlink/etc/passwd"},      // a symlinked directory pointing out
		{"--file=/etc/passwd"},       // value of a flag
		{"-f", "data.txt", "/tmp/x"}, // one bad argument among good ones
		{"~/secret"},
		{"/"},
		{".."},
	}
	for _, args := range denied {
		r := e.run("", "cat", args...)
		if r.code != shim.ExitDenied || !strings.Contains(r.stderr, "outside the mounted volumes") {
			t.Errorf("%v must be denied: %+v", args, r)
		}
	}
	// Nothing denied reached the host command.
	for _, en := range e.auditEntries() {
		if en.Event == "exit" && len(en.Args) > 0 && (en.Args[0] == "/etc/passwd" || en.Args[0] == "link-out") {
			t.Errorf("a confined call ran: %+v", en)
		}
	}
}

func TestConfinePathsRefusesHiddenPaths(t *testing.T) {
	root, _ := os.MkdirTemp("", "bk")
	defer os.RemoveAll(root)
	work := filepath.Join(root, "w")
	os.MkdirAll(work, 0o755)
	os.WriteFile(filepath.Join(work, ".env"), []byte("SECRET=1\n"), 0o644)
	cfg, ps := config.Parse([]byte("volumes:\n  - "+work+":rw\n  - "+work+"/.env:hi\n"+confineRules), config.Options{Home: "/home/nobody"})
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
	cmd := exec.Command(os.Args[0], ".env")
	cmd.Dir = work
	cmd.Env = []string{"AICATRAZ_TEST_SHIM=cat", "AICATRAZ_TEST_SOCK=" + sock}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if code := exitCode(t, cmd.Run()); code != shim.ExitDenied || strings.Contains(out.String(), "SECRET") || !strings.Contains(errb.String(), "hidden") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
}

// ---- stress and teardown ----

func openFDs() int {
	entries, _ := os.ReadDir("/proc/self/fd")
	return len(entries)
}

// hostProcessesWith counts processes whose command line contains marker.
func hostProcessesWith(marker string) int {
	n := 0
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err == nil && strings.Contains(string(cmdline), marker) {
			if st, _ := os.ReadFile("/proc/" + e.Name() + "/stat"); !strings.Contains(string(st), ") Z ") {
				n++
			}
		}
	}
	return n
}

func TestManyConcurrentCommands(t *testing.T) {
	e := setup(t, allowAll, func(o *broker.Options) { o.MaxConcurrent = 8 })
	const total = 40
	type res struct {
		r   result
		arg string
	}
	results := make(chan res, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arg := fmt.Sprintf("n%d", i)
			results <- res{e.run("", "sh", "-c", "sleep 0.3; echo "+arg), arg}
		}(i)
	}
	wg.Wait()
	close(results)
	ran, refused := 0, 0
	for x := range results {
		switch {
		case x.r.code == 0 && x.r.stdout == x.arg+"\n":
			ran++
		case x.r.code == shim.ExitDenied && strings.Contains(x.r.stderr, "too many"):
			refused++
		default:
			t.Errorf("%s: neither run nor cleanly refused: %+v", x.arg, x.r)
		}
	}
	if ran < 8 || ran+refused != total {
		t.Errorf("ran %d, refused %d of %d", ran, refused, total)
	}
	if refused == 0 {
		t.Errorf("40 overlapping 300ms commands against a limit of 8 should have been refused in part")
	}
	// The limit is a ceiling, not a leak: afterwards everything runs again.
	if r := e.run("", "echo", "after"); r.code != 0 || r.stdout != "after\n" {
		t.Errorf("broker not usable after the burst: %+v", r)
	}
}

func TestHugeStdinAndStdout(t *testing.T) {
	e := setup(t, allowAll)
	const size = 64 << 20
	cmd := e.shimCmd("cat")
	var out sha256Writer
	cmd.Stdout = &out
	var errb bytes.Buffer
	cmd.Stderr = &errb
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	want := sha256.New()
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	go func() {
		for sent := 0; sent < size; sent += len(chunk) {
			want.Write(chunk)
			if _, err := in.Write(chunk); err != nil {
				return
			}
		}
		in.Close()
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if exitCode(t, err) != 0 {
			t.Fatalf("%v: %s", err, errb.String())
		}
	case <-time.After(90 * time.Second):
		cmd.Process.Kill()
		t.Fatal("64 MB did not make it through in time")
	}
	if out.n != size || !bytes.Equal(out.sum(), want.Sum(nil)) {
		t.Errorf("data corrupted or truncated: %d bytes back, want %d", out.n, size)
	}
}

type sha256Writer struct {
	h hash.Hash
	n int
}

func (w *sha256Writer) Write(p []byte) (int, error) {
	if w.h == nil {
		w.h = sha256.New()
	}
	w.n += len(p)
	return w.h.Write(p)
}

func (w *sha256Writer) sum() []byte { return w.h.Sum(nil) }

func TestAbruptDisconnectsLeakNothing(t *testing.T) {
	e := setup(t, allowAll)
	baseFDs := openFDs()
	const rounds = 25
	marker := "61.4242"
	for i := 0; i < rounds; i++ {
		c := e.shimCmd("sh", "-c", "sleep "+marker+" & wait")
		if i%2 == 0 {
			c.Stdin = strings.NewReader(strings.Repeat("x", 100000)) // data in flight when it dies
		}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond) // sometimes before the broker started the command, sometimes after
		c.Process.Kill()
		c.Wait()
	}
	deadline := time.Now().Add(6 * time.Second)
	for hostProcessesWith("sleep\x00"+marker) != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := hostProcessesWith("sleep\x00" + marker); n != 0 {
		ps, _ := exec.Command("ps", "-eo", "pid,ppid,pgid,stat,args", "--sort=pid").Output()
		var lines []string
		for _, l := range strings.Split(string(ps), "\n") {
			if strings.Contains(l, marker) {
				lines = append(lines, l)
			}
		}
		t.Fatalf("%d host processes survived their client:\n%s", n, strings.Join(lines, "\n"))
	}
	// The broker must still work, and not hold on to descriptors.
	if r := e.run("", "echo", "alive"); r.code != 0 || r.stdout != "alive\n" {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "descriptors to be released", func() bool { return openFDs() <= baseFDs+6 })
}

func TestBrokerCloseWaitsForCommandsAndLeavesNothing(t *testing.T) {
	e := setup(t, allowAll)
	marker := "62.4343"
	var procs []*exec.Cmd
	for i := 0; i < 5; i++ {
		c := e.shimCmd("sh", "-c", "sleep "+marker+"; echo never")
		var out bytes.Buffer
		c.Stdout = &out
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, c)
	}
	waitFor(t, "commands to start", func() bool { return hostProcessesWith("sleep\x00"+marker) == 5 })
	done := make(chan struct{})
	go func() { e.b.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close hung")
	}
	for _, c := range procs {
		c.Wait()
	}
	if n := hostProcessesWith("sleep\x00" + marker); n != 0 {
		t.Errorf("%d host processes left after Close", n)
	}
}

func TestBrokerRestart(t *testing.T) {
	// A shim that starts while the broker is down gets through once it is back.
	e := setup(t, allowAll)
	e.b.Close() // takes the listener, and its socket, away

	shimc := e.shimCmd("echo", "after-restart")
	var out, errb bytes.Buffer
	shimc.Stdout, shimc.Stderr = &out, &errb
	if err := shimc.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // the shim is retrying with backoff

	cfg, ps := config.Parse([]byte("volumes:\n  - "+e.dir+":rw\n"+allowAll), config.Options{Home: "/home/nobody"})
	if config.HasErrors(ps) {
		t.Fatal(ps)
	}
	b2, err := broker.New(cfg, broker.Options{UID: os.Getuid(), HostEnv: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", e.sock)
	if err != nil {
		t.Fatal(err)
	}
	go b2.Serve(l)
	defer b2.Close()

	if code := exitCode(t, shimc.Wait()); code != 0 || out.String() != "after-restart\n" {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
}

func TestNoGoroutineLeakAfterManyRequests(t *testing.T) {
	e := setup(t, allowAll)
	e.run("", "echo", "warm up")
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()
	for i := 0; i < 60; i++ {
		e.run("", "echo", "x")
	}
	waitFor(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= base+4 })
}

func TestSignalsGetThroughWhileStdinIsBackedUp(t *testing.T) {
	// The command never reads its stdin, and the client keeps sending it. That
	// must not stop a Ctrl-C (SIGTERM here) from reaching the command.
	e := setup(t, allowAll)
	cmd := e.shimCmd("sh", "-c", "sleep 60")
	cmd.Stdin = strings.NewReader(strings.Repeat("x", 4<<20))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to start", func() bool { return len(e.auditEntries()) >= 1 })
	time.Sleep(500 * time.Millisecond) // the pipe is full by now
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(t, err); code != 128+int(syscall.SIGTERM) {
			t.Errorf("exit code %d, want 143", code)
		}
	case <-time.After(8 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the signal was stuck behind the stdin data")
	}
}

func TestSlowOutputConsumerIsNotKilled(t *testing.T) {
	// Heartbeat timeout is 600ms in these tests. A consumer that doesn't read the
	// output for 2s is slow, not dead, and must get all of it.
	e := setup(t, allowAll)
	cmd := e.shimCmd("sh", "-c", "head -c 20000000 /dev/zero")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	n, err := io.Copy(io.Discard, out)
	if err != nil || n != 20000000 {
		t.Fatalf("got %d bytes, err %v", n, err)
	}
	if code := exitCode(t, cmd.Wait()); code != 0 {
		t.Errorf("exit %d", code)
	}
}

func TestStdinFlowControlDeliversEverythingToASlowReader(t *testing.T) {
	e := setup(t, allowAll)
	data := strings.Repeat("0123456789", 100000) // 1 MB, many times the window
	r := e.run(data, "sh", "-c", "sleep 1; cat") // the command starts reading late
	if r.code != 0 || r.stdout != data {
		t.Fatalf("code %d, %d bytes back of %d", r.code, len(r.stdout), len(data))
	}
}
