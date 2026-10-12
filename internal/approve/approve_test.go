package approve

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The test binary doubles as the popup helper, like the aicatraz binary does.
func TestMain(m *testing.M) {
	if len(os.Args) > 4 && os.Args[1] == PromptCommand {
		secs := 5
		if n, err := time.ParseDuration(os.Args[4] + "s"); err == nil {
			secs = int(n.Seconds())
		}
		os.Exit(RunPrompt(os.Args[2], os.Args[3], time.Duration(secs)*time.Second))
	}
	os.Exit(m.Run())
}

func TestQuoteNeutralisesTerminalInput(t *testing.T) {
	hostile := "ok\x1b[2J\x1b[Hdeny? press y\r\x07\u202e"
	q := Quote(hostile)
	for _, r := range q {
		if r < 0x20 || r == 0x7f || r > 0x7e {
			t.Fatalf("raw control or non-ASCII character %U survived in %q", r, q)
		}
	}
	if !strings.Contains(q, `\x1b`) {
		t.Errorf("escape should be visible: %s", q)
	}
}

func TestQuoteTruncates(t *testing.T) {
	q := Quote(strings.Repeat("a", 1000))
	if len(q) > 400 || !strings.Contains(q, "+700 bytes") {
		t.Errorf("%d bytes: %s", len(q), q)
	}
	args := make([]string, 100)
	if got := QuoteArgs("x", args); !strings.Contains(got, "+60 more arguments") {
		t.Errorf("%s", got)
	}
}

func TestRenderIsSafeAndComplete(t *testing.T) {
	r := Request{
		Name: "make", Args: []string{"deploy", "\x1b[31mred", "a b"},
		Cwd: "/home/u/p", Binary: "/usr/bin/make", EnvPassed: []string{"SSH_AUTH_SOCK"},
		Rule: 2, RuleText: "rules[2]: make * (ask)", Warnings: []string{"Makefile changed since it was last approved"},
	}
	out := Render(r)
	for _, want := range []string{`"make" "deploy"`, `\x1b[31mred`, `"a b"`, "/usr/bin/make", "SSH_AUTH_SOCK", "rules[2]", "!! ", "Makefile changed", "[y]", "[s]", "[n]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, c := range out {
		if (c < 0x20 && c != '\n') || c == 0x7f {
			t.Fatalf("control character %U in prompt", c)
		}
	}
}

func TestAnswerParsing(t *testing.T) {
	for _, a := range []Answer{Deny, AllowOnce, AllowSession} {
		if got := ParseAnswer(a.String()); got != a {
			t.Errorf("%v round trip: %v", a, got)
		}
	}
	if ParseAnswer("yes please") != Deny || ParseAnswer("") != Deny {
		t.Error("unknown answers must deny")
	}
}

// ptyPair returns a terminal for Prompt and the controlling side the test types on.
func ptyPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(); s.Close() })
	return m, s
}

func readUntil(t *testing.T, f *os.File, want string, d time.Duration) string {
	t.Helper()
	var got bytes.Buffer
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			got.Write(buf[:n])
			if strings.Contains(got.String(), want) || err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %q, got %q", want, got.String())
	}
	return got.String()
}

func TestPromptKeys(t *testing.T) {
	tests := []struct {
		keys string
		want Answer
	}{
		{"y", AllowOnce},
		{"Y", AllowOnce},
		{"s", AllowSession},
		{"n", Deny},
		{"\r", Deny},
		{"\x1b", Deny},
		{"\x03", Deny},
		{"x?!z y", AllowOnce}, // unknown keys are ignored, a later y still counts
		{"x\r", Deny},
	}
	for _, tc := range tests {
		m, s := ptyPair(t)
		res := make(chan Answer, 1)
		go func() {
			a, _ := Prompt(context.Background(), s, s, Request{Name: "make"}, 5*time.Second)
			res <- a
		}()
		readUntil(t, m, "[n] deny", 3*time.Second) // the prompt is up
		m.Write([]byte(tc.keys))
		select {
		case got := <-res:
			if got != tc.want {
				t.Errorf("keys %q: got %v, want %v", tc.keys, got, tc.want)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("keys %q: no answer", tc.keys)
		}
	}
}

func TestPromptTimeoutAndCancel(t *testing.T) {
	m, s := ptyPair(t)
	_ = m
	start := time.Now()
	a, err := Prompt(context.Background(), s, s, Request{Name: "x"}, 300*time.Millisecond)
	if a != Deny || err != ErrTimeout || time.Since(start) > 2*time.Second {
		t.Errorf("timeout: %v %v after %v", a, err, time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	a, err = Prompt(ctx, s, s, Request{Name: "x"}, 10*time.Second)
	if a != Deny || err != context.Canceled {
		t.Errorf("cancel: %v %v", a, err)
	}
}

func TestPromptDiscardsTypeAhead(t *testing.T) {
	m, s := ptyPair(t)
	m.Write([]byte("y")) // typed before the prompt existed
	time.Sleep(100 * time.Millisecond)
	a, err := Prompt(context.Background(), s, s, Request{Name: "x"}, 400*time.Millisecond)
	if a != Deny {
		t.Errorf("a key typed before the prompt appeared answered it: %v %v", a, err)
	}
}

func TestPromptWithoutTerminal(t *testing.T) {
	r, w, _ := os.Pipe()
	w.Write([]byte("y"))
	var out bytes.Buffer
	if a, err := Prompt(context.Background(), r, &out, Request{Name: "x"}, time.Second); a != AllowOnce || err != nil {
		t.Errorf("%v %v", a, err)
	}
	r2, w2, _ := os.Pipe()
	w2.Close() // nobody there
	if a, _ := Prompt(context.Background(), r2, &out, Request{Name: "x"}, time.Second); a != Deny {
		t.Errorf("EOF must deny: %v", a)
	}
}

func TestTTYApprover(t *testing.T) {
	m, s := ptyPair(t)
	a := &TTY{Path: s.Name()}
	res := make(chan Answer, 1)
	go func() {
		ans, _ := a.Ask(context.Background(), Request{Name: "make", Args: []string{"deploy"}}, 5*time.Second)
		res <- ans
	}()
	out := readUntil(t, m, "[n] deny", 3*time.Second)
	if !strings.Contains(out, `"make" "deploy"`) {
		t.Errorf("prompt: %q", out)
	}
	m.Write([]byte("s"))
	if got := <-res; got != AllowSession {
		t.Errorf("got %v", got)
	}

	_, err := (&TTY{Path: "/nonexistent/tty"}).Ask(context.Background(), Request{}, time.Second)
	if err == nil || !strings.Contains(err.Error(), ErrUnavailable.Error()) {
		t.Errorf("missing terminal: %v", err)
	}
}

// fakeTmux writes a stand-in for the tmux binary: it logs its arguments and
// runs the popup command with `answer` typed on its stdin.
func fakeTmux(t *testing.T, popupBehaviour string) (script, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	script = filepath.Join(dir, "tmux")
	body := `#!/bin/sh
echo "$*" >> '` + log + `'
[ "$1" = display-popup ] || exit 0
[ "$2" = -C ] && exit 0
for last; do :; done
` + popupBehaviour + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, log
}

func TestTmuxApprover(t *testing.T) {
	self, _ := os.Executable()
	tests := []struct {
		behaviour string
		want      Answer
		wantErr   error
	}{
		{`printf y | sh -c "$last"`, AllowOnce, nil},
		{`printf s | sh -c "$last"`, AllowSession, nil},
		{`printf n | sh -c "$last"`, Deny, nil},
		{`: | sh -c "$last"`, Deny, nil}, // popup closed with no key
	}
	for _, tc := range tests {
		script, logf := fakeTmux(t, tc.behaviour)
		a := &Tmux{Tmux: script, Pane: "%7", Self: self, Dir: t.TempDir()}
		got, err := a.Ask(context.Background(), Request{Name: "make", Args: []string{"x"}}, 5*time.Second)
		if got != tc.want || err != tc.wantErr {
			t.Errorf("%q: got %v %v", tc.behaviour, got, err)
		}
		calls, _ := os.ReadFile(logf)
		for _, want := range []string{"display-popup", "-t %7", "-E"} {
			if !strings.Contains(string(calls), want) {
				t.Errorf("tmux call lacks %q: %s", want, calls)
			}
		}
		if entries, _ := os.ReadDir(a.Dir); len(entries) != 0 {
			t.Errorf("request/answer files left behind: %v", entries)
		}
	}
}

func TestTmuxApproverCancelClosesThePopup(t *testing.T) {
	script, logf := fakeTmux(t, `exec sleep 30`)
	self, _ := os.Executable()
	a := &Tmux{Tmux: script, Pane: "%1", Self: self, Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	got, err := a.Ask(ctx, Request{Name: "x"}, 30*time.Second)
	if got != Deny || err != context.Canceled || time.Since(start) > 5*time.Second {
		t.Errorf("%v %v after %v", got, err, time.Since(start))
	}
	if calls, _ := os.ReadFile(logf); !strings.Contains(string(calls), "display-popup -C -t %1") {
		t.Errorf("the popup must be closed explicitly: %s", calls)
	}
}

func TestTmuxApproverPopupTimeout(t *testing.T) {
	self, _ := os.Executable()
	// Nobody types: stdin stays open but silent, so the helper's own timeout fires.
	script, _ := fakeTmux(t, `f=$(mktemp -u); mkfifo "$f"; exec 3<>"$f"; rm -f "$f"; sh -c "$last" <&3`)
	a := &Tmux{Tmux: script, Pane: "%1", Self: self, Dir: t.TempDir()}
	start := time.Now()
	got, err := a.Ask(context.Background(), Request{Name: "x"}, 1*time.Second)
	if got != Deny || err != ErrTimeout {
		t.Errorf("%v %v", got, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestTmuxApproverTmuxFailure(t *testing.T) {
	script, _ := fakeTmux(t, `echo "no current client" >&2; exit 1`)
	a := &Tmux{Tmux: script, Pane: "%1", Self: "/bin/true", Dir: t.TempDir()}
	got, err := a.Ask(context.Background(), Request{Name: "x"}, time.Second)
	if got != Deny || err == nil || !strings.Contains(err.Error(), "no current client") {
		t.Errorf("%v %v", got, err)
	}
}

func TestSanitizeKeepsContentReadableButNeverRaw(t *testing.T) {
	got := Sanitize("echo \"hi\" \\\n\tx\x1b[2Jy\u202ez\r\x00")
	if !strings.Contains(got, `echo "hi" \`) || !strings.Contains(got, "    x") {
		t.Errorf("readable text mangled: %q", got)
	}
	for _, r := range got {
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x202e {
			t.Fatalf("unprintable %U survived: %q", r, got)
		}
	}
	for _, want := range []string{`\u001b`, `\u202e`, `\u000d`, `\u0000`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing visible escape %s in %q", want, got)
		}
	}
}

func TestRenderFileChanges(t *testing.T) {
	diff := "--- approved: Makefile\n+++ now: Makefile\n@@ -1 +1 @@\n-all: ok\n+all:\x1b[31m rm -rf ~\n"
	r := Request{
		Name: "make", Args: []string{"deploy"}, Cwd: "/p", Binary: "/usr/bin/make", CanPin: true,
		Files: []FileChange{
			{Path: "/p/Makefile", Status: "changed since last approved", Diff: diff},
			{Path: "/p/new.mk", Status: "never approved before", Note: "binary file"},
		},
	}
	out := Render(r)
	for _, want := range []string{"/p/Makefile", "changed since last approved", "| -all: ok", `\u001b[31m rm -rf ~`, "never approved before", "binary file", "[p]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, c := range out {
		if (c < 0x20 && c != '\n') || c == 0x7f {
			t.Fatalf("control character %U in prompt", c)
		}
	}
	if strings.Contains(Render(Request{Name: "x"}), "[p]") {
		t.Error("no pin option without pinnable files")
	}

	// Long diffs are cut and say so.
	var long strings.Builder
	for i := 0; i < 100; i++ {
		long.WriteString("+line\n")
	}
	cut := Render(Request{Name: "x", Files: []FileChange{{Path: "f", Status: "x", Diff: long.String()}}})
	if !strings.Contains(cut, "60 more lines") {
		t.Errorf("long diff not cut:\n%s", cut)
	}
}

func TestRenderBuild(t *testing.T) {
	out := Render(Request{Kind: KindBuild, Name: "docker build", Cwd: "/p", CanPin: true,
		Files: []FileChange{{Path: "/p/Dockerfile.az", Status: "changed since last approved"}}})
	if !strings.Contains(out, "before the image is built") || strings.Contains(out, "ON THE HOST") || strings.Contains(out, "[s]") {
		t.Errorf("%s", out)
	}
}

func TestPromptPinKey(t *testing.T) {
	for _, tc := range []struct {
		req  Request
		keys string
		want Answer
	}{
		{Request{Name: "x", CanPin: true}, "p", AllowPin},
		{Request{Name: "x"}, "py", AllowOnce},                                // p is ignored when there is nothing to pin
		{Request{Name: "x", Kind: KindBuild, CanPin: true}, "sy", AllowOnce}, // no session grants for builds
		{Request{Name: "x", Kind: KindBuild, CanPin: true}, "P", AllowPin},
	} {
		m, s := ptyPair(t)
		res := make(chan Answer, 1)
		go func() {
			a, _ := Prompt(context.Background(), s, s, tc.req, 5*time.Second)
			res <- a
		}()
		marker := "deny"
		if tc.req.Kind == KindBuild {
			marker = "cancel"
		}
		readUntil(t, m, marker, 3*time.Second)
		time.Sleep(100 * time.Millisecond)
		m.Write([]byte(tc.keys))
		if got := <-res; got != tc.want {
			t.Errorf("%+v keys %q: got %v, want %v", tc.req.Kind, tc.keys, got, tc.want)
		}
	}
	if ParseAnswer(AllowPin.String()) != AllowPin {
		t.Error("pin round trip")
	}
}
