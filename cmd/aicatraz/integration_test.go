// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package main

import (
	"bytes"
	"fmt"
	"io"
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
	// The tests drive terminals; answer the user config question up front.
	cfgDir := filepath.Join(p.home, ".config", "aicatraz")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, declinedMarker), nil, 0o644); err != nil {
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
	// The static binary starts on Alpine (musl); inside a jail the CLI refuses on purpose.
	stdout, stderr, code := p.run("sh", "-c", "/aicatraz/aicatraz version 2>&1; echo refused-rc=$?; whoami; echo $HOME")
	if code != 0 || !strings.Contains(stdout, "refusing to run inside a jail") || !strings.Contains(stdout, "refused-rc=1") || !strings.Contains(stdout, p.home) {
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
	p.write("aicatraz.yaml", "project: {id: it-sudo}\nimage: debian:stable-slim\nsudo: true\nnetwork: open # apt needs it\nvolumes:\n  - ~/p:rw\n  - ~/p/.env:hi\n")
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

	cmd := exec.Command(bin, "up", "--tmux", "--print", "--", "claude")
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

func TestUpPrintsTheSessionServerCommand(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "project: {id: demo}\nvolumes:\n  - ~/p:rw\n")
	cmd := exec.Command(bin, "up", "--print", "--", "claude")
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+p.runtime, "HOME="+p.home, "MY_SECRET_TOKEN=hunter2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{"__mux-serve", "run -c " + filepath.Join(p.dir, "aicatraz.yaml"), "--approval-sock", "approve.sock", "-- claude"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(string(out), "hunter2") {
		t.Error("environment leaked into the command line")
	}
	if entries, _ := os.ReadDir(filepath.Join(p.runtime, "aicatraz")); len(entries) != 0 {
		t.Errorf("--print left files behind: %v", entries)
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

	// `pins list` shows what was approved and how the file looks now; `pins forget` makes the next call ask again.
	listed, _, lcode := p.aicatraz("pins", "list")
	if lcode != 0 || !strings.Contains(listed, "Makefile") || !strings.HasPrefix(strings.Split(listed, "\n")[1], "ok") {
		t.Errorf("pins list: exit %d\n%s", lcode, listed)
	}
	p.write("Makefile", "deploy-test:\n\t@echo changed-again\n")
	if listed, _, _ = p.aicatraz("pins", "list"); !strings.Contains(listed, "changed") {
		t.Errorf("pins list after an edit:\n%s", listed)
	}
	if out, _, c := p.aicatraz("pins", "forget", "Makefile"); c != 0 || !strings.Contains(out, "forgot") {
		t.Errorf("pins forget: exit %d\n%s", c, out)
	}
	if listed, _, _ = p.aicatraz("pins", "list"); strings.Contains(listed, "Makefile") {
		t.Errorf("a forgotten pin is still listed:\n%s", listed)
	}
	go term.answerWhenAsked(4, "n")
	if _, _, c := p.runWithApproval(term.slave.Name(), "make", "deploy-test"); c != 126 {
		t.Errorf("after forgetting, the call must ask (and was refused): exit %d", c)
	}
	if n := strings.Count(term.text(), "never approved before"); n < 2 {
		t.Errorf("the forgotten file should show as never approved:\n%s", term.text())
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
		`"hasCompletedOnboarding": true`,
		`"hasTrustDialogAccepted": true`, // the workspace is marked as trusted in the jail's copy
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

// userTerm is a user's terminal for driving `up`: a pty with everything it shows
// collected.
type userTerm struct {
	t    *testing.T
	pty  *os.File
	cmd  *exec.Cmd
	mu   sync.Mutex
	seen strings.Builder
}

func (p *project) startTerm(args ...string) *userTerm {
	p.t.Helper()
	realDocker := filepath.Join(realHome(), ".docker")
	cmd := exec.Command(bin, args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+realDocker,
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_DATA_HOME="+filepath.Join(p.home, ".data"),
		"XDG_RUNTIME_DIR="+p.runtime)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		p.t.Fatal(err)
	}
	tm := &userTerm{t: p.t, pty: ptmx, cmd: cmd}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			tm.mu.Lock()
			tm.seen.Write(buf[:n])
			tm.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	p.t.Cleanup(func() { ptmx.Close(); cmd.Process.Kill(); cmd.Wait() })
	return tm
}

func (tm *userTerm) screen() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.seen.String()
}

func (tm *userTerm) expect(what string, within time.Duration) {
	tm.t.Helper()
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(tm.screen(), what) {
			return
		}
	}
	tm.t.Fatalf("never saw %q in\n%q", what, tm.screen())
}

func (tm *userTerm) send(s string) { tm.pty.WriteString(s) }

func (tm *userTerm) wait(within time.Duration) int {
	tm.t.Helper()
	done := make(chan error, 1)
	go func() { done <- tm.cmd.Wait() }()
	select {
	case <-done:
		return tm.cmd.ProcessState.ExitCode()
	case <-time.After(within):
		tm.t.Fatalf("process did not end; screen:\n%q", tm.screen())
		return -1
	}
}

// The built-in multiplexer end to end: an ask prompt over the agent's screen,
// detach and reattach, a prompt that waits for you, and `down`.
func TestUpSession(t *testing.T) {
	p := newProject(t)
	p.write("Makefile", "deploy:\n\t@echo DEPLOYED-ON-HOST\n")
	p.write("aicatraz.yaml", "project: {id: it-up}\nvolumes:\n  - ~/p:rw\nrules:\n  - command: make\n    args: [deploy]\n    decision: ask\n")
	down := func() ([]byte, error) {
		cmd := exec.Command(bin, "down", "-c", filepath.Join(p.dir, "aicatraz.yaml"))
		cmd.Env = append(os.Environ(), "HOME="+p.home, "XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() { down() })
	p.removeImage("it-up")

	tm := p.startTerm("up", "--approve-build", "--", "sh")
	tm.expect("$ ", 120*time.Second)
	tm.send("(make deploy; echo make-rc=$?)\r")
	tm.expect("ON THE HOST", 20*time.Second)
	tm.send("y") // too early: keys are ignored while the prompt arms
	time.Sleep(900 * time.Millisecond)
	if strings.Contains(tm.screen(), "DEPLOYED") {
		t.Fatal("a key typed as the prompt appeared answered it")
	}
	tm.send("y")
	tm.expect("make-rc=0", 20*time.Second)
	tm.expect("DEPLOYED-ON-HOST", 5*time.Second)

	// Detach: the session and the jail stay.
	tm.send("(sleep 2; make deploy; echo late-rc=$?) &\r")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x19")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("detach exit code %d", code)
	}
	if out, _ := exec.Command("docker", "ps", "-q", "--filter", "label=aicatraz.project=it-up").Output(); len(bytes.TrimSpace(out)) == 0 {
		t.Fatal("the jail stopped when the client detached")
	}
	time.Sleep(3 * time.Second) // the background make asks, with nobody attached

	tm2 := p.startTerm("up")
	tm2.expect("ON THE HOST", 20*time.Second) // the waiting prompt is shown on attach
	time.Sleep(900 * time.Millisecond)
	tm2.send("n")
	tm2.expect("late-rc=", 10*time.Second)
	if strings.Contains(tm2.screen(), "late-rc=0") {
		t.Error("a denied command ran")
	}

	if out, err := down(); err != nil {
		t.Fatalf("down: %v %s", err, out)
	}
	tm2.wait(15 * time.Second)
	if out, _ := exec.Command("docker", "ps", "-q", "--filter", "label=aicatraz.project=it-up").Output(); len(bytes.TrimSpace(out)) != 0 {
		t.Error("the jail still runs after down")
	}
}

// ---- network rules ----

// bridgeGateway is the address the sidecar reaches this host at.
func bridgeGateway(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	gw := strings.TrimSpace(string(out))
	if err != nil || gw == "" {
		t.Skip("cannot find the docker bridge gateway")
	}
	return gw
}

// hello starts a server that says "hello-<port>" and closes; it returns the port.
func hello(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	port := l.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			fmt.Fprintf(c, "hello-%d\n", port)
			c.Close()
		}
	}()
	return port
}

// fetch is the jail-side script: connect, read a line.
func fetch(gw string, port int) string {
	return fmt.Sprintf(`l=; exec 3<>/dev/tcp/%s/%d 2>/dev/null && read -t 8 l <&3; echo "port%d=[$l]"; exec 3>&-;`, gw, port, port)
}

func egressContainers(t *testing.T) []string {
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "label=aicatraz.egress=1", "--format", "{{.Names}}").Output()
	return strings.Fields(string(out))
}

func TestNetworkRulesAllowAndRefuse(t *testing.T) {
	gw := bridgeGateway(t)
	open, closed := hello(t), hello(t)
	p := newProject(t)
	p.write("aicatraz.yaml", fmt.Sprintf(`
project: {id: it-net}
volumes:
  - ~/p:rw
network:
  default: deny
  rules:
    - {to: %s, ports: "%d", decision: allow}
`, gw, open))
	before := len(egressContainers(t))

	script := fetch(gw, open) + fetch(gw, closed) + fmt.Sprintf(`printf x >/dev/udp/%s/53 2>/dev/null || echo udp-blocked;`, gw) +
		`grep -c . /etc/resolv.conf >/dev/null; grep -h nameserver /etc/resolv.conf;`
	stdout, stderr, code := p.run("bash", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("port%d=[hello-%d]", open, open)) {
		t.Errorf("the allowed port must work:\n%s", stdout)
	}
	if !strings.Contains(stdout, fmt.Sprintf("port%d=[]", closed)) {
		t.Errorf("a port no rule allows must give nothing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "udp-blocked") {
		t.Errorf("udp must be blocked:\n%s", stdout)
	}
	if !strings.Contains(stdout, "nameserver 127.0.0.1") {
		t.Errorf("the jail must use the helper's resolver:\n%s", stdout)
	}
	if len(egressContainers(t)) != before {
		t.Errorf("the sidecar must be gone after the session: %v", egressContainers(t))
	}

	// Both decisions are in the audit log.
	logs, _ := filepath.Glob(filepath.Join(p.stateDir(), "*", "audit.log"))
	if len(logs) != 1 {
		t.Fatalf("audit logs: %v", logs)
	}
	data, _ := os.ReadFile(logs[0])
	if !strings.Contains(string(data), `"event":"net"`) || !strings.Contains(string(data), fmt.Sprintf("%s:%d", gw, closed)) ||
		!strings.Contains(string(data), `"decision":"deny"`) || !strings.Contains(string(data), `"decision":"allow"`) {
		t.Errorf("the audit log lacks the network decisions:\n%s", data)
	}
}

func TestNetworkAskAnsweredOnTheApprovalTerminal(t *testing.T) {
	gw := bridgeGateway(t)
	first, second := hello(t), hello(t)
	p := newProject(t)
	p.write("aicatraz.yaml", "project: {id: it-netask}\nvolumes:\n  - ~/p:rw\nnetwork:\n  rules:\n    - {to: 203.0.113.0/24, decision: deny}\napproval:\n  timeout: 30s\n")
	term := newApprovalTerminal(t)

	// The first connection is allowed once (y), the second is refused (n),
	// the third is allowed for the session (s) and the fourth then needs no answer.
	go func() {
		term.answerWhenAsked(1, "y")
		term.answerWhenAsked(2, "n")
		term.answerWhenAsked(3, "s")
	}()
	script := fetch(gw, first) + fetch(gw, second) + fetch(gw, first) + fetch(gw, first)
	stdout, stderr, code := p.runWithApproval(term.slave.Name(), "bash", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	want := []string{
		fmt.Sprintf("port%d=[hello-%d]", first, first), // y
		fmt.Sprintf("port%d=[]", second),               // n
	}
	for _, w := range want {
		if !strings.Contains(stdout, w) {
			t.Errorf("missing %q:\n%s", w, stdout)
		}
	}
	if n := strings.Count(stdout, fmt.Sprintf("port%d=[hello-%d]", first, first)); n != 3 {
		t.Errorf("the first port should have worked three times (y, s, then without asking), got %d:\n%s", n, stdout)
	}
	shown := term.text()
	if n := strings.Count(shown, "OUT OF THE JAIL"); n != 3 {
		t.Errorf("expected 3 questions, saw %d:\n%s", n, shown)
	}
	for _, w := range []string{fmt.Sprintf("%s:%d", gw, first), "[e]", "allow this connection once", "only a rule naming the address"} {
		if !strings.Contains(shown, w) {
			t.Errorf("the prompt lacks %q:\n%s", w, shown)
		}
	}
}

func TestNetworkRememberedRuleAndTheNetworkCommand(t *testing.T) {
	gw := bridgeGateway(t)
	port := hello(t)
	p := newProject(t)
	p.write("aicatraz.yaml", "project: {id: it-netmem}\nvolumes:\n  - ~/p:rw\nnetwork: {}\napproval:\n  timeout: 30s\n")
	term := newApprovalTerminal(t)

	// Edit the ports to a range around the real one, then remember.
	go term.answerWhenAsked(1, portEdit(port))
	stdout, stderr, code := p.runWithApproval(term.slave.Name(), "bash", "-c", fetch(gw, port))
	if code != 0 || !strings.Contains(stdout, fmt.Sprintf("port%d=[hello-%d]", port, port)) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}

	// A new session, nobody to ask: the remembered rule decides.
	stdout, stderr, code = p.run("bash", "-c", fetch(gw, port))
	if code != 0 || !strings.Contains(stdout, fmt.Sprintf("port%d=[hello-%d]", port, port)) {
		t.Fatalf("the remembered rule must apply in a new session: exit %d\n%s\n%s", code, stdout, stderr)
	}

	// `network list` shows it, `network forget` takes it away.
	out, _, c := p.aicatraz("network", "list")
	if c != 0 || !strings.Contains(out, gw) || !strings.Contains(out, fmt.Sprintf("%d-%d", port-1, port+1)) {
		t.Errorf("network list: exit %d\n%s", c, out)
	}
	if _, _, c := p.aicatraz("network", "forget", "1"); c != 0 {
		t.Errorf("network forget: exit %d", c)
	}
	stdout, _, _ = p.run("bash", "-c", fetch(gw, port))
	if !strings.Contains(stdout, fmt.Sprintf("port%d=[]", port)) {
		t.Errorf("after forgetting, nobody to ask means refused:\n%s", stdout)
	}
}

// portEdit types keys that replace the prefilled port by the range
// port-1 to port+1 (the prefilled text is the port itself) and remember.
func portEdit(port int) string {
	return "e" + strings.Repeat("\x7f", len(fmt.Sprint(port))) + fmt.Sprintf("%d-%d\rp", port-1, port+1)
}

func TestNetworkNamesGoThroughTheHelperResolver(t *testing.T) {
	if out, err := exec.Command("getent", "hosts", "example.com").Output(); err != nil || len(out) == 0 {
		t.Skip("no DNS from this machine")
	}
	p := newProject(t)
	p.write("aicatraz.yaml", `
project: {id: it-netnames}
volumes:
  - ~/p:rw
network:
  default: deny
  rules:
    - {to: example.org, decision: deny}
    - {to: example.com, ports: 80, decision: allow}
`)
	script := `
getent hosts example.com >/dev/null && echo resolved-allowed
getent hosts example.org >/dev/null || echo refused-denied-name
exec 3<>/dev/tcp/example.com/80 && printf 'HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n' >&3 && read -t 10 l <&3 && echo "http: $l"
l=; exec 4<>/dev/tcp/example.com/443 2>/dev/null && { printf x >&4; read -t 5 l <&4; echo "port443=[$l]"; }
`
	stdout, stderr, code := p.run("bash", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, w := range []string{"resolved-allowed", "refused-denied-name", "http: HTTP/1.", "port443=[]"} {
		if !strings.Contains(stdout, w) {
			t.Errorf("missing %q:\n%s", w, stdout)
		}
	}
}

func TestNetworkPrintShowsTheSidecar(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\nnetwork: {resolvers: [9.9.9.9]}\n")
	out, _, code := p.aicatraz("run", "--print", "--", "true")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var side, jailLine string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "--label aicatraz.egress=1"):
			side = l
		case strings.HasPrefix(l, "docker run") && strings.Contains(l, "--network container:"):
			jailLine = l
		}
	}
	if side == "" || jailLine == "" {
		t.Fatalf("both containers must be shown:\n%s", out)
	}
	if !strings.Contains(side, "--cap-add=NET_ADMIN") || strings.Contains(jailLine, "NET_ADMIN") || !strings.Contains(jailLine, "--cap-drop=ALL") {
		t.Errorf("only the sidecar gets NET_ADMIN:\nsidecar: %s\njail: %s", side, jailLine)
	}
	if strings.Contains(jailLine, "--hostname") || !strings.Contains(side, "--hostname") || !strings.Contains(side, "disable_ipv6=1") || !strings.HasSuffix(side, "9.9.9.9") {
		t.Errorf("hostname, ipv6 and resolvers:\n%s\n%s", side, jailLine)
	}
}

// In a terminal an agent runs as a session: approval prompts appear over its
// screen, and it can be detached from.
func TestAgentVerbRunsAsASession(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed on the host")
	}
	p := newProject(t)
	p.write("Makefile", "deploy:\n\t@echo DEPLOYED-ON-HOST\n")
	p.write("aicatraz.yaml", "project: {id: it-agentsession}\nvolumes:\n  - ~/p:rw\nrules:\n  - command: make\n    args: [deploy]\n    decision: ask\n")
	p.installFake("claude", "1.0.0", "#!/bin/sh\necho \"FAKE-AGENT-UP\"\nmake deploy; echo \"make-rc=$?\"\nread line; echo \"FAKE-GOT:$line\"\nexit 5\n")
	p.removeImage("it-agentsession")
	down := func() {
		cmd := exec.Command(bin, "down", "-c", filepath.Join(p.dir, "aicatraz.yaml"))
		cmd.Env = append(os.Environ(), "HOME="+p.home, "XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
		cmd.Run()
	}
	t.Cleanup(down)

	tm := p.startTerm("claude", "--approve-build")
	tm.expect("FAKE-AGENT-UP", 120*time.Second)
	tm.expect("detach with Ctrl-Y", 5*time.Second)
	tm.expect("ON THE HOST", 20*time.Second) // the agent's `make deploy` asks, over its screen
	time.Sleep(900 * time.Millisecond)
	tm.send("y")
	tm.expect("DEPLOYED-ON-HOST", 20*time.Second)
	tm.expect("make-rc=0", 5*time.Second)
	tm.send("hi\r")
	tm.expect("FAKE-GOT:hi", 5*time.Second)
	if code := tm.wait(20 * time.Second); code != 5 {
		t.Errorf("the agent's exit code must come through the session: %d", code)
	}
}

func TestAgentVerbNoSessionRunsInTheTerminal(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	p.installFake("claude", "1.0.0", "#!/bin/sh\necho DIRECT-RUN\n")
	tm := p.startTerm("claude", "--no-session", "--approve-build")
	tm.expect("DIRECT-RUN", 120*time.Second)
	if code := tm.wait(20 * time.Second); code != 0 {
		t.Errorf("exit %d", code)
	}
	if strings.Contains(tm.screen(), "detach with") {
		t.Errorf("--no-session must not start a session:\n%s", tm.screen())
	}
}

// Without a network section the jail has no network at all; `network: open`
// is the normal Docker network.
func TestNetworkIsOffUnlessConfigured(t *testing.T) {
	gw := bridgeGateway(t)
	port := hello(t)
	script := fetch(gw, port) + `echo "interfaces=$(ls /sys/class/net | tr '\n' ' ')";`

	p := newProject(t)
	p.write("aicatraz.yaml", "volumes:\n  - ~/p:rw\n")
	stdout, stderr, code := p.run("bash", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("port%d=[]", port)) || !strings.Contains(stdout, "interfaces=lo ") {
		t.Errorf("with no network section the jail must have only the loopback:\n%s", stdout)
	}
	if out, _, c := p.aicatraz("network", "list"); c != 0 || !strings.Contains(out, "the network is off") {
		t.Errorf("network list: %d %s", c, out)
	}
	if out, _, _ := p.aicatraz("run", "--print", "--", "true"); !strings.Contains(out, "--network none") {
		t.Errorf("--print must show --network none:\n%s", out)
	}

	q := newProject(t)
	q.write("aicatraz.yaml", "network: open\nvolumes:\n  - ~/p:rw\n")
	stdout, _, code = q.run("bash", "-c", script)
	if code != 0 || !strings.Contains(stdout, fmt.Sprintf("port%d=[hello-%d]", port, port)) || !strings.Contains(stdout, "eth0") {
		t.Errorf("network: open is Docker's normal network: exit %d\n%s", code, stdout)
	}
}

// ---- published ports ----

func freeHostPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serveFromTheJail starts a jail whose command listens on port 8080 and says
// hello to the first client, and connects to it from the host.
func serveFromTheJail(t *testing.T, p *project, hostPort int) string {
	t.Helper()
	cmd := exec.Command(bin, append(p.runArgs(), "sh", "-c", "echo hello-from-jail | nc -l -p 8080; echo served")...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "HOME="+p.home, "DOCKER_CONFIG="+filepath.Join(realHome(), ".docker"),
		"XDG_STATE_HOME="+filepath.Join(p.home, ".state"), "XDG_RUNTIME_DIR="+p.runtime)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })

	var got string
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 2*time.Second)
		if err != nil {
			continue
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		b, _ := io.ReadAll(c)
		c.Close()
		if len(b) > 0 {
			got = strings.TrimSpace(string(b))
			break
		}
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
	}
	if got == "" {
		t.Fatalf("nothing came back through the published port; the jail said:\n%s", out.String())
	}
	return got
}

func TestPublishedPortReachesAServerInTheJail(t *testing.T) {
	port := freeHostPort(t)
	p := newProject(t)
	p.write("aicatraz.yaml", fmt.Sprintf("image: alpine:3\nnetwork: open\nports: [\"%d:8080\"]\nvolumes:\n  - ~/p:rw\n", port))
	if got := serveFromTheJail(t, p, port); got != "hello-from-jail" {
		t.Errorf("got %q", got)
	}
}

func TestPublishedPortWorksWithNetworkRules(t *testing.T) {
	port := freeHostPort(t)
	p := newProject(t)
	// default deny: the jail can't connect anywhere, but a browser can still reach it.
	p.write("aicatraz.yaml", fmt.Sprintf("image: alpine:3\nnetwork: {default: deny}\nports: [\"%d:8080\"]\nvolumes:\n  - ~/p:rw\n", port))
	if got := serveFromTheJail(t, p, port); got != "hello-from-jail" {
		t.Errorf("got %q", got)
	}
}

func TestPortsNeedANetwork(t *testing.T) {
	p := newProject(t)
	p.write("aicatraz.yaml", "ports: [3000]\nvolumes:\n  - ~/p:rw\n")
	_, stderr, code := p.aicatraz("config", "check")
	if code == 0 || !strings.Contains(stderr, "the jail has none") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

// ---- devices ----

func TestDeviceNodeIsMadeInTheJail(t *testing.T) {
	if st, err := os.Stat("/dev/fuse"); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		t.Skip("no /dev/fuse on this machine")
	}
	p := newProject(t)
	p.write("aicatraz.yaml", "devices:\n  - path: /dev/fuse\nvolumes:\n  - ~/p:rw\n")
	script := `i=0; while [ ! -e /dev/fuse ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done; ls -ln /dev/fuse; : < /dev/fuse && echo opened; ls /dev/mem 2>&1; id -u`
	stdout, stderr, code := p.run("sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	owner := fmt.Sprintf("%d %d", os.Getuid(), os.Getgid())
	if !strings.Contains(stdout, "crw-rw----") || !strings.Contains(stdout, owner) || !strings.Contains(stdout, "10, 229") || !strings.Contains(stdout, "opened") {
		t.Errorf("the node must exist, be owned by the jail user and open:\n%s", stdout)
	}
	if strings.Contains(stdout, "/dev/mem\n") && !strings.Contains(stdout, "No such file") {
		t.Errorf("only the named device may appear:\n%s", stdout)
	}

	// Nodes of other devices can't be made by the jail itself.
	out, _, _ := p.run("sh", "-c", "mknod /dev/mynull c 1 3 2>&1; echo rc=$?")
	if strings.Contains(out, "rc=0") {
		t.Errorf("the jail made a device node itself:\n%s", out)
	}
}

func TestSerialPortsOfTheMachineAppearByTheirStableName(t *testing.T) {
	entries, _ := os.ReadDir("/dev/serial/by-id")
	if len(entries) == 0 {
		t.Skip("no serial adapter plugged in")
	}
	p := newProject(t)
	p.write("aicatraz.yaml", "devices:\n  - serial: '*'\nvolumes:\n  - ~/p:rw\n")
	name := entries[0].Name()
	// Only looked at, never opened: opening a serial port can reset the board behind it.
	script := fmt.Sprintf(`i=0; while [ ! -L '/dev/serial/by-id/%s' ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done; ls -l '/dev/serial/by-id/%s'; test -c "$(readlink '/dev/serial/by-id/%s')" && echo node-ok`, name, name, name)
	stdout, stderr, code := p.run("sh", "-c", script)
	if code != 0 || !strings.Contains(stdout, name) || !strings.Contains(stdout, "node-ok") {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}
