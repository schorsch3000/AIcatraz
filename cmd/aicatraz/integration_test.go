//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	t    *testing.T
	home string // fake host home
	dir  string // project directory inside it
}

func newProject(t *testing.T) *project {
	t.Helper()
	root := t.TempDir()
	p := &project{t: t, home: filepath.Join(root, "home"), dir: filepath.Join(root, "home", "p")}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		t.Fatal(err)
	}
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
