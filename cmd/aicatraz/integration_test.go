//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// These tests start real containers (debian:stable-slim and alpine:3 are
// pulled if missing). Run them with `make test-integration`.

var bin string

func TestMain(m *testing.M) {
	if err := exec.Command("docker", "version").Run(); err != nil {
		fmt.Println("docker not available, skipping integration tests")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "aicatraz-it")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "aicatraz")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Printf("build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type project struct {
	t       *testing.T
	home    string // fake host home
	dir     string // project directory inside it
	runtime string // stands in for $XDG_RUNTIME_DIR (socket paths must stay short)
}

func newProject(t *testing.T) *project {
	t.Helper()
	root := t.TempDir()
	p := &project{t: t, home: filepath.Join(root, "home"), dir: filepath.Join(root, "home", "p")}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := os.MkdirTemp("", "rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(rt) })
	p.runtime = rt
	return p
}

// removeImage deletes the image a project with a fixed project.id built.
func (p *project) removeImage(id string) {
	p.t.Cleanup(func() { exec.Command("docker", "rmi", "-f", "aicatraz/"+id+":latest").Run() })
}

func (p *project) write(rel, content string) {
	p.t.Helper()
	path := filepath.Join(p.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// run executes `aicatraz run -- args...` in the project and returns stdout, stderr and the exit code.
func (p *project) run(args ...string) (string, string, int) {
	p.t.Helper()
	realDocker := os.Getenv("DOCKER_CONFIG")
	if realDocker == "" {
		h, _ := os.UserHomeDir()
		realDocker = filepath.Join(h, ".docker")
	}
	cmd := exec.Command(bin, append([]string{"run", "--"}, args...)...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(),
		"HOME="+p.home,
		"DOCKER_CONFIG="+realDocker,
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"),
		"XDG_RUNTIME_DIR="+p.runtime,
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		p.t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestJailInDebian(t *testing.T) {
	p := newProject(t)
	p.write(".env", "SECRET=1\n")
	p.write("secrets/key", "top secret\n")
	p.write("Makefile", "all:\n")
	p.write("a.txt", "x\n")
	p.write("aicatraz.yaml", `
volumes:
  - ~/p:rw
  - ~/p/.env:hi
  - ~/p/secrets:hi
  - ~/p/Makefile:ro
env:
  static: {GREETING: hi}
rules:
  - command: make
    decision: local
`)
	script := `
echo "uid=$(id -u) name=$(whoami) home=$HOME pwd=$PWD greeting=$GREETING"
echo "home-entries=$(ls -A "$HOME" | sort | tr '\n' ',')"
echo "env-size=$(wc -c < .env) secrets-entries=$(ls -A secrets | wc -l)"
rm .env 2>/dev/null && echo "env-removed" || echo "env-undeletable"
mv .env x 2>/dev/null && echo "env-renamed" || echo "env-unrenamable"
echo x > secrets/n 2>/dev/null && echo "secrets-writable" || echo "secrets-readonly"
echo y >> Makefile 2>/dev/null && echo "makefile-writable" || echo "makefile-readonly"
echo z > b.txt && echo "workspace-writable"
touch /usr/x 2>/dev/null && echo "rootfs-writable" || echo "rootfs-readonly"
grep -E '^(CapEff|NoNewPrivs)' /proc/self/status | tr -d '\t' | tr '\n' ' '; echo
sudo -n true 2>/dev/null && echo "sudo-works" || echo "sudo-useless"
`
	stdout, stderr, code := p.run("sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"name=" + os.Getenv("USER"),
		"home=" + p.home,
		"pwd=" + p.dir,
		"greeting=hi",
		"home-entries=p,", // the project's mount point only: no dotfiles, no keys
		"env-size=0 secrets-entries=0",
		"env-undeletable",
		"env-unrenamable",
		"secrets-readonly",
		"makefile-readonly",
		"workspace-writable",
		"rootfs-readonly",
		"CapEff:0000000000000000",
		"NoNewPrivs:1",
		"sudo-useless",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in output:\n%s", want, stdout)
		}
	}
	for _, bad := range []string{"env-removed", "env-renamed", "secrets-writable", "makefile-writable", "rootfs-writable", "sudo-works"} {
		if strings.Contains(stdout, bad) {
			t.Errorf("unexpected %q in output:\n%s", bad, stdout)
		}
	}

	// The host side is untouched by anything the jail tried.
	if got, _ := os.ReadFile(filepath.Join(p.dir, ".env")); string(got) != "SECRET=1\n" {
		t.Errorf(".env on the host changed: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(p.dir, "Makefile")); string(got) != "all:\n" {
		t.Errorf("Makefile on the host changed: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(p.dir, "b.txt")); string(got) != "z\n" {
		t.Errorf("rw volume did not receive the write: %q", got)
	}

	// The shim directory has a symlink for the rule's command.
	out, _, _ := p.run("ls", "/aicatraz/bin")
	if strings.TrimSpace(out) != "make" {
		t.Errorf("shim dir: %q", out)
	}
}

func TestExitCodePassesThrough(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	if _, _, code := p.run("sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exit code %d, want 7", code)
	}
}

func TestShimBinaryRunsOnAlpine(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "image: alpine:3\nvolumes:\n  - ~/p:rw\n")
	stdout, stderr, code := p.run("sh", "-c", "/aicatraz/aicatraz version; whoami; echo $HOME")
	if code != 0 || !strings.Contains(stdout, "aicatraz") || !strings.Contains(stdout, p.home) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestImageWithOtherUser(t *testing.T) {
	p := newProject(t)
	p.write("Dockerfile.az", "FROM debian:stable-slim\nRUN useradd -u 2000 app\nUSER app\n")
	p.write("aicatraz.yaml", "project: {id: it-otheruser}\nvolumes:\n  - ~/p:rw\n")
	p.removeImage("it-otheruser")
	stdout, stderr, code := p.run("sh", "-c", "echo name=$(whoami) uid=$(id -u) home=$HOME")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if want := "name=" + os.Getenv("USER") + fmt.Sprintf(" uid=%d home=%s", os.Getuid(), p.home); !strings.Contains(stdout, want) {
		t.Errorf("want %q in %q", want, stdout)
	}
}

func TestBuildRefusesLeakingContext(t *testing.T) {
	p := newProject(t)
	p.write(".env", "SECRET=1\n")
	p.write("Dockerfile.az", "FROM debian:stable-slim\nCOPY . /app\n")
	p.write("aicatraz.yaml", "project: {id: it-leak}\nvolumes:\n  - ~/p:rw\n  - ~/p/.env:hi\n")
	p.removeImage("it-leak")

	_, stderr, code := p.run("true")
	if code == 0 || !strings.Contains(stderr, ".dockerignore") {
		t.Fatalf("build must be refused (exit %d): %s", code, stderr)
	}

	// With the exclusion in place the build goes through and the image has no .env.
	p.write(".dockerignore", ".env\nDockerfile.az\n")
	stdout, stderr, code := p.run("sh", "-c", "ls -A /app | tr '\\n' ','; test ! -e /app/.env && echo no-env-in-image")
	if code != 0 || !strings.Contains(stdout, "no-env-in-image") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestMissingVolumeIsAnError(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n  - ~/p/.env:hi\n")
	_, stderr, code := p.run("true")
	if code != 2 || !strings.Contains(stderr, "does not exist") {
		t.Errorf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(p.dir, ".env")); err == nil {
		t.Error("the launcher must not create the missing file")
	}
}

const proxyRules = `
volumes:
  - ~/p:rw
  - ~/p/.env:hi
rules:
  - command: hostname
    decision: allow
  - command: uname
    decision: local
  - command: cat
    args: [.env]
    decision: allow
  - command: "false"
    decision: allow
  - command: rm
    decision: deny
`

func (p *project) stateDir() string { return filepath.Join(p.home, ".state", "aicatraz") }

func TestHostCommandsThroughTheBroker(t *testing.T) {
	p := newProject(t)
	p.write(".env", "SECRET=1\n")
	p.write("keep.txt", "x\n")
	p.write("aicatraz.yaml", proxyRules)
	hostName, _ := os.Hostname()

	script := `
echo "host-hostname=$(hostname)"
echo "container-uname=$(uname -n)"
echo "jail-sees-env-bytes=$(wc -c < .env)"
echo "host-cat=$(cat .env)"
false; echo "false-exit=$?"
rm keep.txt; echo "rm-exit=$?"
test -e keep.txt && echo "keep-still-there"
rm /aicatraz/broker.sock 2>/dev/null && echo "socket-removed" || echo "socket-protected"
`
	stdout, stderr, code := p.run("sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"host-hostname=" + hostName, // ran on the host
		"container-uname=aicatraz-", // `local`: the container's own uname
		"jail-sees-env-bytes=0",     // the agent can't read the hidden file...
		"host-cat=SECRET=1",         // ...but an allowed host command can
		"false-exit=1",              // exit codes pass through
		"rm-exit=126",               // denied by the policy
		"keep-still-there",
		"socket-protected",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in output:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "does not allow") {
		t.Errorf("the denial message should reach the agent: %q", stderr)
	}

	// The audit log records what happened, and the session's socket is gone.
	matches, _ := filepath.Glob(filepath.Join(p.stateDir(), "*", "audit.log"))
	if len(matches) != 1 {
		t.Fatalf("expected one audit log, found %v", matches)
	}
	audit, _ := os.ReadFile(matches[0])
	for _, want := range []string{`"name":"hostname"`, `"decision":"allow"`, `"decision":"deny"`, `"decision":"local"`, `"event":"exit"`} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("audit log lacks %s:\n%s", want, audit)
		}
	}
	runDirs, _ := filepath.Glob(filepath.Join(p.runtime, "aicatraz", "*"))
	if len(runDirs) != 0 {
		t.Errorf("session socket directory not cleaned up: %v", runDirs)
	}
}

func TestInteractiveTerminalThroughTheBroker(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\nrules:\n  - command: hostname\n    decision: allow\n  - command: read\n    decision: deny\n")
	hostName, _ := os.Hostname()

	cmd := exec.Command(bin, "run", "--", "sh", "-c", `read line; echo "got:$line"; hostname`)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"), "XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()

	var out strings.Builder
	var mu sync.Mutex
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	ptmx.Write([]byte("abc\n"))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("timed out")
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(out.String(), "got:abc") || !strings.Contains(out.String(), hostName) {
		t.Errorf("output through a terminal: %q", out.String())
	}
}

func realHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// approvalTerminal is a pty the test plays the human on. Prompts are written
// to the slave side's path; the test reads them from, and answers on, master.
type approvalTerminal struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	out    strings.Builder
}

func newApprovalTerminal(t *testing.T) *approvalTerminal {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := &approvalTerminal{t: t, master: m, slave: s}
	t.Cleanup(func() { m.Close(); s.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			a.mu.Lock()
			a.out.Write(buf[:n])
			a.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return a
}

func (a *approvalTerminal) text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.out.String()
}

// answerWhenAsked waits for the n-th prompt and types key.
func (a *approvalTerminal) answerWhenAsked(n int, key string) {
	a.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for strings.Count(a.text(), "[n] deny") < n {
		if time.Now().After(deadline) {
			a.t.Fatalf("prompt %d never appeared; terminal shows %q", n, a.text())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // the prompt switches the terminal to raw mode
	a.master.Write([]byte(key))
}

func (p *project) runWithApproval(tty string, args ...string) (string, string, int) {
	p.t.Helper()
	cmd := exec.Command(bin, append([]string{"run", "--approval-tty", tty, "--"}, args...)...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"),
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		p.t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

const askRules = `
volumes:
  - ~/p:rw
approval:
  timeout: 5s
rules:
  - command: hostname
    decision: ask
`

func TestAskPromptsOnTheApprovalTerminal(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", askRules)
	hostName, _ := os.Hostname()
	term := newApprovalTerminal(t)

	// Approved once: the command runs on the host, and the prompt showed what it is.
	go term.answerWhenAsked(1, "y")
	stdout, stderr, code := p.runWithApproval(term.slave.Name(), "sh", "-c", "hostname")
	if code != 0 || strings.TrimSpace(stdout) != hostName {
		t.Fatalf("approved: exit %d\n%s\n%s", code, stdout, stderr)
	}
	shown := term.text()
	for _, want := range []string{`"hostname"`, "ON THE HOST", "/hostname", "rules[0]"} {
		if !strings.Contains(shown, want) {
			t.Errorf("prompt lacks %q:\n%s", want, shown)
		}
	}
	// The agent's own output never contained the prompt.
	if strings.Contains(stdout+stderr, "ON THE HOST") {
		t.Error("the prompt leaked into the agent's terminal")
	}

	// Denied: exit 126 and the message reaches the agent.
	go term.answerWhenAsked(2, "n")
	stdout, stderr, code = p.runWithApproval(term.slave.Name(), "sh", "-c", "hostname")
	if code != 126 || !strings.Contains(stderr, "denied by the user") || stdout != "" {
		t.Errorf("denied: exit %d\n%s\n%s", code, stdout, stderr)
	}

	// Allow for the session: asked once for two identical calls.
	go term.answerWhenAsked(3, "s")
	stdout, stderr, code = p.runWithApproval(term.slave.Name(), "sh", "-c", "hostname; hostname")
	if code != 0 || strings.Count(stdout, hostName) != 2 {
		t.Errorf("session grant: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if n := strings.Count(term.text(), "[n] deny"); n != 3 {
		t.Errorf("expected 3 prompts in total, saw %d", n)
	}
}

func TestAskTimesOutToDeny(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", askRules)
	term := newApprovalTerminal(t)
	start := time.Now()
	stdout, stderr, code := p.runWithApproval(term.slave.Name(), "sh", "-c", "hostname")
	if code != 126 || !strings.Contains(stderr, "in time") || stdout != "" {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if time.Since(start) > 40*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestAskWithoutApprovalTerminalIsDeniedWithAHint(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", askRules)
	stdout, stderr, code := p.run("sh", "-c", "hostname")
	if code != 126 || stdout != "" || !strings.Contains(stderr, "aicatraz up") {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestSecondShellUsesTheSameBroker(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\nrules:\n  - command: hostname\n    decision: allow\n")
	hostName, _ := os.Hostname()

	env := append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"),
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
	first := exec.Command(bin, "run", "--", "sleep", "30")
	first.Dir, first.Env = p.dir, env
	var firstErr bytes.Buffer
	first.Stderr = &firstErr
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { first.Process.Kill(); first.Wait() }()

	var out, errb []byte
	var err error
	for i := 0; i < 100; i++ { // wait for the container to exist
		second := exec.Command(bin, "shell", "--", "hostname")
		second.Dir, second.Env = p.dir, env
		out, err = second.Output()
		if err == nil {
			break
		}
		if ee, ok := err.(*exec.ExitError); ok {
			errb = ee.Stderr
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil || strings.TrimSpace(string(out)) != hostName {
		t.Fatalf("second shell: %v %q %q\nfirst jail stderr: %s", err, out, errb, firstErr.String())
	}
	// Without a running jail the command explains itself.
	first.Process.Kill()
	first.Wait()
}

func TestUpPrintsTheTmuxCommand(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "project: {id: demo}\nvolumes:\n  - ~/p:rw\n")
	// A stand-in tmux: no session exists, so `up` must create one.
	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, "tmux"), []byte("#!/bin/sh\n[ \"$1\" = has-session ] && exit 1\nexit 0\n"), 0o755)

	cmd := exec.Command(bin, "up", "--print", "--", "claude")
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"), "TMUX=", "XDG_RUNTIME_DIR="+p.runtime, "HOME="+p.home)
	cmd.Env = append(cmd.Env, "MY_SECRET_TOKEN=hunter2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	line := string(out)
	for _, want := range []string{"new-session -s aicatraz-demo", "__with-env", "run -c " + filepath.Join(p.dir, "aicatraz.yaml"), "-- claude"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
	if strings.Contains(line, "hunter2") {
		t.Error("environment leaked into the tmux command line")
	}
}
