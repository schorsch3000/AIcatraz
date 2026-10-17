//go:build integration

package main

import (
	"bytes"
	"fmt"
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
	// reviewBuild makes run/runWithApproval leave build files for the launcher
	// to check (by default tests pass --approve-build, since they test other things).
	reviewBuild bool
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

// runArgs is `run [flags] --` as the tests call it.
func (p *project) runArgs(flags ...string) []string {
	a := []string{"run"}
	if !p.reviewBuild {
		a = append(a, "--approve-build")
	}
	a = append(a, flags...)
	return append(a, "--")
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
	cmd := exec.Command(bin, append(p.runArgs(), args...)...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(),
		"HOME="+p.home,
		"DOCKER_CONFIG="+realDocker,
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"),
		"XDG_DATA_HOME="+filepath.Join(p.home, ".data"),
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

func TestSudo(t *testing.T) {
	p := newProject(t)
	p.write(".env", "SECRET=1\n")
	p.write("aicatraz.yaml", "project: {id: it-sudo}\nimage: debian:stable-slim\nsudo: true\nvolumes:\n  - ~/p:rw\n  - ~/p/.env:hi\n")
	exec.Command("docker", "rmi", "-f", "aicatraz/it-sudo:sudo").Run()
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", "aicatraz/it-sudo:sudo").Run() })

	// The user stays the host user; sudo is root, passwordless, and can install packages.
	stdout, stderr, code := p.run("sh", "-c", "id -u; sudo -n id -u; sudo -n sh -c 'apt-get update -qq && apt-get install -y -qq cowsay >/dev/null && echo installed'")
	if code != 0 || stdout != fmt.Sprintf("%d\n0\ninstalled\n", os.Getuid()) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}

	// Root still can't see or undo what the jail hides or protects.
	stdout, _, _ = p.run("sudo", "-n", "sh", "-c", "wc -c < .env; umount .env 2>&1; echo x > /etc/passwd 2>&1; touch /aicatraz/bin/x 2>&1")
	if !strings.Contains(stdout, "0\n") || !strings.Contains(stdout, "must be superuser") ||
		!strings.Contains(stdout, "Read-only file system") {
		t.Errorf("root escaped a protection: %s", stdout)
	}
	if b, _ := os.ReadFile(filepath.Join(p.dir, ".env")); string(b) != "SECRET=1\n" {
		t.Errorf(".env changed on the host: %q", b)
	}
}

func TestSudoOffByDefault(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	stdout, _, _ := p.run("sh", "-c", "command -v sudo || echo no-sudo; touch /etc/x 2>&1; grep NoNewPrivs /proc/self/status")
	if !strings.Contains(stdout, "Read-only file system") || !strings.Contains(stdout, "NoNewPrivs:\t1") {
		t.Errorf("hardening must stay on without sudo: %s", stdout)
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
	cmd := exec.Command(bin, append(p.runArgs("--approval-tty", tty), args...)...)
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

const makeProject = `
volumes:
  - ~/p:rw
approval:
  timeout: 30s
rules:
  - command: make
    args: [deploy-test]
    decision: allow
    depends_on: auto
`

func TestEditedMakefileTurnsAnAllowIntoAnAsk(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed on the host")
	}
	p := newProject(t)
	p.write("Makefile", "deploy-test:\n\t@echo deployed\n")
	p.write("aicatraz.yaml", makeProject)
	term := newApprovalTerminal(t)

	script := `
make deploy-test; echo "rc1=$?"
make deploy-test; echo "rc2=$?"
printf '\t@echo extra\n' >> Makefile
make deploy-test; echo "rc3=$?"
`
	// The Makefile was never approved: the first call asks, and the human pins it.
	// The second is silent. After the agent's edit the third asks again, and is refused.
	go func() {
		term.answerWhenAsked(1, "p")
		term.answerWhenAsked(2, "n")
	}()
	stdout, stderr, code := p.runWithApproval(term.slave.Name(), "sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if strings.Count(stdout, "deployed") != 2 || !strings.Contains(stdout, "rc1=0") || !strings.Contains(stdout, "rc2=0") || !strings.Contains(stdout, "rc3=126") {
		t.Errorf("unexpected output:\n%s\n%s", stdout, stderr)
	}
	if strings.Contains(stdout, "extra") {
		t.Errorf("the edited Makefile ran:\n%s", stdout)
	}
	shown := term.text()
	if n := strings.Count(shown, "[n] deny"); n != 2 {
		t.Errorf("expected exactly 2 prompts, saw %d", n)
	}
	for _, want := range []string{"never approved before", "changed since last approved", "@echo extra", "[p]"} {
		if !strings.Contains(shown, want) {
			t.Errorf("approval terminal lacks %q:\n%s", want, shown)
		}
	}
	// The pin store is private to the user and outside the project.
	pinDirs, _ := filepath.Glob(filepath.Join(p.stateDir(), "*", "pins"))
	if len(pinDirs) != 1 {
		t.Fatalf("pin store: %v", pinDirs)
	}
	if st, _ := os.Stat(pinDirs[0]); st.Mode().Perm() != 0o700 {
		t.Errorf("pin store mode %v", st.Mode().Perm())
	}

	// A new session remembers: the edited Makefile still needs approval, and then it is pinned.
	go term.answerWhenAsked(3, "p")
	stdout, stderr, code = p.runWithApproval(term.slave.Name(), "sh", "-c", "make deploy-test; make deploy-test")
	if code != 0 || strings.Count(stdout, "extra") != 2 {
		t.Fatalf("second session: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if n := strings.Count(term.text(), "[n] deny"); n != 3 {
		t.Errorf("pinning once must be enough: %d prompts in total", n)
	}

	// Another project with the identical Makefile pins independently.
	q := newProject(t)
	q.write("Makefile", "deploy-test:\n\t@echo deployed\n\t@echo extra\n")
	q.write("aicatraz.yaml", makeProject)
	termQ := newApprovalTerminal(t)
	go termQ.answerWhenAsked(1, "n")
	_, stderrQ, codeQ := q.runWithApproval(termQ.slave.Name(), "make", "deploy-test")
	if codeQ != 126 || !strings.Contains(termQ.text(), "never approved before") {
		t.Errorf("other project: exit %d\n%s\n%s", codeQ, stderrQ, termQ.text())
	}
}

func TestBuildFilesNeedApprovalBeforeBuilding(t *testing.T) {
	p := newProject(t)
	p.write("Dockerfile.az", "FROM debian:stable-slim\n")
	p.reviewBuild = true
	p.write("aicatraz.yaml", "project: {id: it-buildpin}\nvolumes:\n  - ~/p:rw\n")
	p.removeImage("it-buildpin")

	// Never approved and nobody to ask: refused, with the way out.
	_, stderr, code := p.run("true")
	if code != 2 || !strings.Contains(stderr, "--approve-build") {
		t.Fatalf("first run: exit %d: %s", code, stderr)
	}
	// Explicit, non-interactive approval.
	runFlag := func(flag string, args ...string) (string, int) {
		cmd := exec.Command(bin, append([]string{"run", flag, "--"}, args...)...)
		cmd.Dir = p.dir
		cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"),
			"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	if out, code := runFlag("--approve-build", "true"); code != 0 {
		t.Fatalf("--approve-build: exit %d: %s", code, out)
	}
	if _, stderr, code := p.run("true"); code != 0 {
		t.Fatalf("pinned build must pass silently: exit %d: %s", code, stderr)
	}

	// The agent (or anyone) changes the Dockerfile.
	p.write("Dockerfile.az", "FROM debian:stable-slim\nRUN echo changed\n")
	_, stderr, code = p.run("true")
	if code != 2 || !strings.Contains(stderr, "changed since") {
		t.Fatalf("changed Dockerfile must stop the build: exit %d: %s", code, stderr)
	}

	// In a terminal the launcher shows the diff and asks.
	cmd := exec.Command(bin, "run", "--", "true")
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"),
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	var out syncOut
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			out.write(buf[:n])
			if err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(out.text(), "[n] cancel") {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("no build prompt; terminal shows %q", out.text())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, want := range []string{"before the image is built", "changed since last approved", "RUN echo changed"} {
		if !strings.Contains(out.text(), want) {
			t.Errorf("prompt lacks %q:\n%s", want, out.text())
		}
	}
	time.Sleep(150 * time.Millisecond)
	ptmx.Write([]byte("p"))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("after approving: %v\n%s", err, out.text())
	}
	if _, stderr, code := p.run("true"); code != 0 {
		t.Errorf("re-pinned build must pass silently: exit %d: %s", code, stderr)
	}
}

type syncOut struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncOut) write(p []byte) { s.mu.Lock(); s.b.Write(p); s.mu.Unlock() }
func (s *syncOut) text() string   { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// ---- coding agents ----

// installFake puts a stand-in for an agent into the project's tool cache, so
// these tests need neither network nor credentials.
func (p *project) installFake(tool, version, script string) {
	p.t.Helper()
	dir := filepath.Join(p.home, ".data", "aicatraz", "tools", tool, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
		p.t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(dir), "current")
	os.Remove(link)
	if err := os.Symlink(version, link); err != nil {
		p.t.Fatal(err)
	}
}

// aicatraz runs a subcommand with the project's environment.
func (p *project) aicatraz(args ...string) (string, string, int) {
	p.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"),
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_DATA_HOME="+filepath.Join(p.home, ".data"),
		"XDG_RUNTIME_DIR="+p.runtime)
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

const fakeClaude = `#!/bin/sh
echo "ARGV: $*"
echo "CONFIG_DIR: $CLAUDE_CONFIG_DIR"
echo "PATH: $PATH"
echo "SETTINGS: $(cat "$CLAUDE_CONFIG_DIR/settings.json" 2>&1)"
echo "TOPLEVEL: $(cat "$CLAUDE_CONFIG_DIR/.claude.json" 2>&1)"
echo "SKILL: $(cat "$CLAUDE_CONFIG_DIR/skills/hello/SKILL.md" 2>&1)"
echo "CREDMODE: $(stat -c %a "$CLAUDE_CONFIG_DIR/.credentials.json" 2>&1)"
echo "PROJECTS: $(ls "$CLAUDE_CONFIG_DIR/projects" 2>&1 | tr '\n' ',')"
echo "HISTORY: $(cat "$CLAUDE_CONFIG_DIR/history.jsonl" 2>&1)"
echo "PARENT_WRITABLE: $(touch "$HOME/.writable-test" 2>&1 && echo yes)"
# The agent changes a synced file and writes its own state.
echo '{"evil":true}' > "$CLAUDE_CONFIG_DIR/settings.json"
echo "line from run" >> "$CLAUDE_CONFIG_DIR/history.jsonl"
# Record a transcript the way Claude Code does, under the id it was given.
id=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--session-id" ] || [ "$prev" = "--resume" ]; then id="$a"; fi
  prev="$a"
done
if [ -n "$id" ] && [ -z "$NO_TRANSCRIPT" ]; then
  mkdir -p "$CLAUDE_CONFIG_DIR/projects/-work"
  echo '{}' > "$CLAUDE_CONFIG_DIR/projects/-work/$id.jsonl"
fi
exit 0
`

func TestClaudeRunsInTheJailWithYourConfig(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	p.installFake("claude", "1.0.0", fakeClaude)

	// The user's real Claude configuration.
	real := filepath.Join(p.home, ".claude")
	write := func(rel, content string, mode os.FileMode) {
		path := filepath.Join(p.home, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/settings.json", `{"theme":"dark"}`, 0o644)
	write(".claude/.credentials.json", `{"token":"abc"}`, 0o600)
	write(".claude/skills/hello/SKILL.md", "say hello", 0o644)
	write(".claude/projects/old-secret-history/x.jsonl", "other project's transcript", 0o644)
	write(".claude.json", `{"hasCompletedOnboarding":true}`, 0o600)

	stdout, stderr, code := p.aicatraz("claude", "--approve-build", "--", "--model", "sonnet")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		`ARGV: --dangerously-skip-permissions --settings {"skipDangerousModePermissionPrompt":true} --session-id `, "--model sonnet",
		"CONFIG_DIR: " + real,
		"/aicatraz/bin:/aicatraz/tools:",
		`SETTINGS: {"theme":"dark"}`,
		`TOPLEVEL: {"hasCompletedOnboarding":true}`,
		"SKILL: say hello",
		"CREDMODE: 600",
		"PARENT_WRITABLE: yes",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "old-secret-history") {
		t.Errorf("another project's history leaked into the jail:\n%s", stdout)
	}
	// What the agent did never touches the real files.
	if b, _ := os.ReadFile(filepath.Join(real, "settings.json")); string(b) != `{"theme":"dark"}` {
		t.Errorf("the real settings.json changed: %s", b)
	}
	if _, err := os.Stat(filepath.Join(real, "history.jsonl")); err == nil {
		t.Error("the agent's history appeared in the real ~/.claude")
	}

	// The session was recorded outside the jail's reach, and the agent's own id is in it.
	out, _, _ := p.aicatraz("sessions")
	fields := strings.Fields(out)
	if len(fields) < 4 || fields[2] != "claude" || len(fields[3]) != 36 {
		t.Fatalf("sessions output: %q", out)
	}
	id := fields[3]
	if !strings.Contains(stderr, id) || !strings.Contains(stderr, "aicatraz resume") {
		t.Errorf("the session id should be announced on exit: %q", stderr)
	}

	// Second launch: your edits carry over, the agent's edits to synced files are gone,
	// its own state (history) survived.
	write(".claude/settings.json", `{"theme":"light"}`, 0o644)
	stdout, stderr, code = p.aicatraz("claude", "--approve-build")
	if code != 0 {
		t.Fatalf("second run: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `SETTINGS: {"theme":"light"}`) {
		t.Errorf("host change not carried over:\n%s", stdout)
	}
	if !strings.Contains(stdout, "HISTORY: line from run") {
		t.Errorf("the agent's own state must persist:\n%s", stdout)
	}

	// resume continues the most recent session with the agent's own option.
	stdout, stderr, code = p.aicatraz("resume", "--approve-build")
	if code != 0 || !strings.Contains(stdout, "--resume ") {
		t.Fatalf("resume: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "--session-id") {
		t.Errorf("a resumed session must not get a new id:\n%s", stdout)
	}
	out, _, _ = p.aicatraz("sessions")
	if strings.Count(out, "claude") != 2 {
		t.Errorf("two sessions expected (the first and the second launch), got:\n%s", out)
	}
	// Resuming a specific id.
	stdout, _, code = p.aicatraz("resume", id, "--approve-build")
	if code != 0 || !strings.Contains(stdout, "--resume "+id) {
		t.Errorf("resume by id: exit %d\n%s", code, stdout)
	}
	if _, stderr, code = p.aicatraz("resume", "no-such-id", "--approve-build"); code == 0 || !strings.Contains(stderr, "no recorded session") {
		t.Errorf("unknown id: exit %d %s", code, stderr)
	}
}

func TestSessionWithoutATranscriptIsNotRecorded(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\nagent:\n  tool: claude\n  skip_permissions: false\n")
	p.installFake("claude", "1.0.0", "#!/bin/sh\necho \"ARGV: $*\"\n") // exits without saving anything
	stdout, _, code := p.aicatraz("agent", "--approve-build")
	if code != 0 || strings.Contains(stdout, "--dangerously-skip-permissions") || !strings.Contains(stdout, "--session-id") {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if out, _, _ := p.aicatraz("sessions"); !strings.Contains(out, "no sessions") {
		t.Errorf("an empty session can't be resumed, so it must not be listed: %q", out)
	}
	if _, stderr, code := p.aicatraz("resume", "--approve-build"); code == 0 || !strings.Contains(stderr, "no sessions") {
		t.Errorf("resume without sessions: exit %d %s", code, stderr)
	}
}

const fakeOpencode = `#!/bin/sh
if [ "$1" = session ]; then
  now=$(date +%s)000
  if [ -f "$HOME/.local/share/opencode/ran" ]; then
    echo "[{\"id\":\"ses_fake1\",\"title\":\"fake work\",\"updated\":$now,\"created\":$now,\"projectId\":\"p\",\"directory\":\"$PWD\"},{\"id\":\"ses_elsewhere\",\"title\":\"x\",\"updated\":$now,\"created\":$now,\"projectId\":\"q\",\"directory\":\"/somewhere/else\"}]"
  else
    echo "[]"
  fi
  exit 0
fi
echo "OC_ARGV: $*"
echo "OC_PERMISSION: $OPENCODE_PERMISSION"
echo "OC_CONFIG: $(cat "$HOME/.config/opencode/opencode.json" 2>&1)"
echo "OC_AUTH: $(cat "$HOME/.local/share/opencode/auth.json" 2>&1)"
echo "OC_NODE_MODULES: $(ls "$HOME/.config/opencode/node_modules" 2>&1 | head -1)"
echo "OC_DB: $(ls "$HOME/.local/share/opencode/" 2>&1 | tr '\n' ',')"
echo "OC_PARENTS: $(touch "$HOME/.config/other" "$HOME/.local/other" 2>&1 && echo writable)"
touch "$HOME/.local/share/opencode/ran"
`

func TestOpencodeRunsAndItsSessionIsFoundAfterwards(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	p.installFake("opencode", "1.2.3", fakeOpencode)

	write := func(rel, content string) {
		path := filepath.Join(p.home, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o600)
	}
	write(".config/opencode/opencode.json", `{"model":"x"}`)
	write(".config/opencode/node_modules/dep/index.js", "huge")
	write(".local/share/opencode/auth.json", `{"provider":"key"}`)
	write(".local/share/opencode/opencode.db", "11 GB of every project's history")

	stdout, stderr, code := p.aicatraz("opencode", "--approve-build", "--", "--model", "m")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"OC_ARGV: --model m",
		`OC_PERMISSION: {"*":"allow"}`,
		`OC_CONFIG: {"model":"x"}`,
		`OC_AUTH: {"provider":"key"}`,
		"OC_PARENTS: writable",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	// Big or history-like host state is not copied: the jail starts with its own.
	if strings.Contains(stdout, "opencode.db") || strings.Contains(stdout, "OC_NODE_MODULES: dep") {
		t.Errorf("host state leaked into the jail:\n%s", stdout)
	}
	if !strings.Contains(stderr, "ses_fake1") {
		t.Errorf("the session id should be announced: %q", stderr)
	}
	out, _, _ := p.aicatraz("sessions")
	if !strings.Contains(out, "ses_fake1") || !strings.Contains(out, "fake work") || strings.Contains(out, "ses_elsewhere") {
		t.Errorf("sessions: %q", out)
	}
	stdout, _, code = p.aicatraz("resume", "--approve-build")
	if code != 0 || !strings.Contains(stdout, "OC_ARGV: --session ses_fake1") {
		t.Errorf("resume: exit %d\n%s", code, stdout)
	}
}

func TestAgentPrintShowsTheMountsWithoutInstalling(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	stdout, stderr, code := p.aicatraz("claude", "--print", "--approve-build")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"target=/aicatraz/tools,readonly", "CLAUDE_CONFIG_DIR=" + filepath.Join(p.home, ".claude"), "--dangerously-skip-permissions", "<not installed yet>"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(p.home, ".data")); err == nil {
		t.Error("--print must not install or create anything")
	}
}

func TestToolsFromHost(t *testing.T) {
	p := newProject(t)
	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, "claude"), []byte("#!/bin/sh\necho '3.4.5 (Claude Code)'\n"), 0o755)
	cmd := exec.Command(bin, "tools", "install", "claude", "--from-host")
	cmd.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"), "XDG_DATA_HOME="+filepath.Join(p.home, ".data"), "HOME="+p.home)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "claude 3.4.5 installed") {
		t.Fatalf("%v: %s", err, out)
	}
	list, _, _ := p.aicatraz("tools", "list")
	if !strings.Contains(list, "* claude    3.4.5") {
		t.Errorf("list: %q", list)
	}
}

func TestAgentRefusesAnImageWithoutGlibc(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "image: alpine:3\nvolumes:\n  - ~/p:rw\n")
	p.installFake("claude", "1.0.0", "#!/bin/sh\necho should-not-run\n")
	stdout, stderr, code := p.aicatraz("claude", "--approve-build")
	if code != 2 || strings.Contains(stdout, "should-not-run") || !strings.Contains(stderr, "glibc") {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

// ---- hidden-path canary ----

// startRun starts `aicatraz run -- args` in the background.
func (p *project) startRun(args ...string) (*exec.Cmd, *syncOut) {
	p.t.Helper()
	cmd := exec.Command(bin, append(p.runArgs(), args...)...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"),
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_DATA_HOME="+filepath.Join(p.home, ".data"),
		"XDG_RUNTIME_DIR="+p.runtime)
	var out syncOut
	cmd.Stdout = writerFunc(out.write)
	cmd.Stderr = writerFunc(out.write)
	if err := cmd.Start(); err != nil {
		p.t.Fatal(err)
	}
	return cmd, &out
}

type writerFunc func([]byte)

func (f writerFunc) Write(b []byte) (int, error) { f(b); return len(b), nil }

func waitContainer(t *testing.T, projectID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("docker", "ps", "-q", "--filter", "label=aicatraz.project="+projectID).Output()
		if strings.TrimSpace(string(out)) != "" {
			time.Sleep(700 * time.Millisecond) // let the command inside start
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the jail did not start")
}

func waitExit(t *testing.T, cmd *exec.Cmd, d time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 0
	case <-time.After(d):
		cmd.Process.Kill()
		t.Fatal("the launcher did not exit in time")
		return -1
	}
}

func containersFor(projectID string) int {
	out, _ := exec.Command("docker", "ps", "-q", "--filter", "label=aicatraz.project="+projectID).Output()
	return len(strings.Fields(string(out)))
}

const canaryConfig = `
project: {id: %s}
volumes:
  - ~/p:rw
  - ~/p/.env:hi
  - ~/p/secrets:hi
canary:
  interval: %s
rules:
  - command: hostname
    decision: allow
`

func newCanaryProject(t *testing.T, id, interval string) *project {
	p := newProject(t)
	p.write(".env", "SECRET=1\n")
	p.write("secrets/key", "top secret\n")
	p.write("aicatraz.yaml", fmt.Sprintf(canaryConfig, id, interval))
	return p
}

func TestRenameOverAHiddenFileEndsTheSession(t *testing.T) {
	p := newCanaryProject(t, "it-canary-file", "500ms")
	cmd, out := p.startRun("sleep", "60")
	waitContainer(t, "it-canary-file")

	// What most editors do when saving: write a new file and rename it over the old one.
	os.WriteFile(filepath.Join(p.dir, ".env.new"), []byte("SECRET=2\n"), 0o600)
	if err := os.Rename(filepath.Join(p.dir, ".env.new"), filepath.Join(p.dir, ".env")); err != nil {
		t.Fatal(err)
	}

	if code := waitExit(t, cmd, 30*time.Second); code != 70 {
		t.Errorf("exit code %d, want 70\n%s", code, out.text())
	}
	for _, want := range []string{"no longer hidden", ".env", "stopped"} {
		if !strings.Contains(out.text(), want) {
			t.Errorf("warning lacks %q:\n%s", want, out.text())
		}
	}
	if n := containersFor("it-canary-file"); n != 0 {
		t.Errorf("%d containers still running", n)
	}
}

func TestReplacedHiddenDirectoryIsDetected(t *testing.T) {
	p := newCanaryProject(t, "it-canary-dir", "500ms")
	cmd, out := p.startRun("sleep", "60")
	waitContainer(t, "it-canary-dir")

	os.RemoveAll(filepath.Join(p.dir, "secrets"))
	os.MkdirAll(filepath.Join(p.dir, "secrets"), 0o755)
	os.WriteFile(filepath.Join(p.dir, "secrets", "key"), []byte("new\n"), 0o600)

	if code := waitExit(t, cmd, 30*time.Second); code != 70 || !strings.Contains(out.text(), "secrets") {
		t.Errorf("exit %d\n%s", code, out.text())
	}
}

func TestEditingAHiddenFileInPlaceIsFine(t *testing.T) {
	p := newCanaryProject(t, "it-canary-inplace", "300ms")
	cmd, out := p.startRun("sleep", "6")
	waitContainer(t, "it-canary-inplace")

	// Same inode, new content: the mount still covers it.
	f, err := os.OpenFile(filepath.Join(p.dir, ".env"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("SECRET=changed\n")
	f.Close()

	if code := waitExit(t, cmd, 40*time.Second); code != 0 {
		t.Errorf("an in-place edit must not end the session: exit %d\n%s", code, out.text())
	}
	if strings.Contains(out.text(), "no longer hidden") {
		t.Errorf("false alarm:\n%s", out.text())
	}
}

func TestHostCommandAfterExposureIsRefused(t *testing.T) {
	// The timer is an hour away: only the per-command check can notice.
	p := newCanaryProject(t, "it-canary-cmd", "1h")
	cmd, out := p.startRun("sh", "-c", "sleep 3; hostname; echo reached-the-end")
	waitContainer(t, "it-canary-cmd")

	os.WriteFile(filepath.Join(p.dir, ".env.new"), []byte("x\n"), 0o600)
	os.Rename(filepath.Join(p.dir, ".env.new"), filepath.Join(p.dir, ".env"))

	code := waitExit(t, cmd, 40*time.Second)
	hostName, _ := os.Hostname()
	if code != 70 || strings.Contains(out.text(), hostName+"\n") && !strings.Contains(out.text(), "no longer hidden") {
		t.Errorf("exit %d\n%s", code, out.text())
	}
	if !strings.Contains(out.text(), "no longer hidden") || strings.Contains(out.text(), "reached-the-end") {
		t.Errorf("the host command must not run, and the session must end:\n%s", out.text())
	}
}

func TestCanaryCanBeSwitchedOff(t *testing.T) {
	p := newCanaryProject(t, "it-canary-off", "300ms")
	p.write("aicatraz.yaml", strings.Replace(fmt.Sprintf(canaryConfig, "it-canary-off", "300ms"), "canary:\n", "canary:\n  action: off\n", 1))
	cmd, out := p.startRun("sleep", "4")
	waitContainer(t, "it-canary-off")
	os.WriteFile(filepath.Join(p.dir, ".env.new"), []byte("x\n"), 0o600)
	os.Rename(filepath.Join(p.dir, ".env.new"), filepath.Join(p.dir, ".env"))
	if code := waitExit(t, cmd, 40*time.Second); code != 0 {
		t.Errorf("exit %d\n%s", code, out.text())
	}
}

// ---- teardown ----

func TestLauncherSignalsLeaveNothingBehind(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			id := "it-teardown-" + strings.ToLower(sig.String()[:4])
			p := newProject(t)
			p.write("aicatraz.yaml", "project: {id: "+id+"}\nvolumes:\n  - ~/p:rw\nrules:\n  - command: hostname\n    decision: allow\n")
			cmd, out := p.startRun("sleep", "120")
			waitContainer(t, id)
			if ents, _ := os.ReadDir(filepath.Join(p.runtime, "aicatraz")); len(ents) != 1 {
				t.Fatalf("expected one session directory while running: %v", ents)
			}
			cmd.Process.Signal(sig)
			waitExit(t, cmd, 60*time.Second)

			if n := containersFor(id); n != 0 {
				t.Errorf("%d containers left running\n%s", n, out.text())
			}
			if ents, _ := os.ReadDir(filepath.Join(p.runtime, "aicatraz")); len(ents) != 0 {
				t.Errorf("session directory (with the broker socket) left behind: %v", ents)
			}
		})
	}
}

func TestStaleSessionDirectoriesAreCleanedUp(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	// A launcher that was killed with SIGKILL can't clean up after itself.
	stale := filepath.Join(p.runtime, "aicatraz", "999999") // no such process
	os.MkdirAll(stale, 0o700)
	os.WriteFile(filepath.Join(stale, "broker.sock"), nil, 0o600)
	alive := filepath.Join(p.runtime, "aicatraz", strconv.Itoa(os.Getpid())) // this test process is alive
	os.MkdirAll(alive, 0o700)

	if _, stderr, code := p.run("true"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("the directory of a dead launcher must be removed")
	}
	if _, err := os.Stat(alive); err != nil {
		t.Error("the directory of a live process must be left alone")
	}
}
