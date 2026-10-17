package approve

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// typed returns a KeySource that plays back s, then reports that nobody is there.
func typed(s string) KeySource {
	b := []byte(s)
	return func(ctx context.Context, deadline time.Time) (byte, error) {
		if len(b) == 0 {
			return 0, io.EOF
		}
		k := b[0]
		b = b[1:]
		return k, nil
	}
}

func netReq(names ...string) Request {
	return Request{Kind: KindNetwork, Net: &NetInfo{IP: "140.82.121.4", Port: 22, Names: names}}
}

func promptNet(t *testing.T, keys string, req Request) (Reply, string) {
	t.Helper()
	var out bytes.Buffer
	r, err := PromptNet(context.Background(), typed(keys), &out, req, time.Minute, false)
	if err != nil {
		t.Fatalf("keys %q: %v", keys, err)
	}
	return r, out.String()
}

func TestPromptNetAnswers(t *testing.T) {
	req := netReq("github.com")
	for keys, want := range map[string]Reply{
		"y":     {Verb: VerbOnce},
		"Y":     {Verb: VerbOnce},
		"s":     {Verb: VerbSession, Ports: "22", Target: "github.com"},
		"p":     {Verb: VerbRemember, Ports: "22", Target: "github.com"},
		"x":     {Verb: VerbBlock, Ports: "22", Target: "github.com"},
		"n":     {Verb: VerbDeny},
		"\r":    {Verb: VerbDeny},
		"\x1b":  {Verb: VerbDeny},
		"\x03":  {Verb: VerbDeny},
		"":      {Verb: VerbDeny}, // nobody there
		"q?! y": {Verb: VerbOnce}, // other keys are ignored
		// t switches between the name and the address
		"ts":  {Verb: VerbSession, Ports: "22", Target: "140.82.121.4"},
		"ttp": {Verb: VerbRemember, Ports: "22", Target: "github.com"},
		// e edits the ports: a list, a range, a mix, or *
		"e, 80, 443, 8000-8080\rs":           {Verb: VerbSession, Ports: "22, 80, 443, 8000-8080", Target: "github.com"}, // the old ports are where the text starts
		"e\x7f\x7f80, 443, 22, 8000-8080\rs": {Verb: VerbSession, Ports: "22, 80, 443, 8000-8080", Target: "github.com"},
		"e\x7f\x7f1000-2000\rp":              {Verb: VerbRemember, Ports: "1000-2000", Target: "github.com"},
		"e\x7f\x7f*\rs":                      {Verb: VerbSession, Ports: "*", Target: "github.com"},
		"e\x7f\x7f\x7ftx443\rs":              {Verb: VerbSession, Ports: "443", Target: "github.com"}, // letters can't be typed
		// Esc leaves the editor with the old ports
		"e999\x1bs": {Verb: VerbSession, Ports: "22", Target: "github.com"},
		// an invalid list is not accepted: the editor stays open
		"e\x7f\x7f99999\r\x7f\x7f\x7f\x7f\x7f443\rs": {Verb: VerbSession, Ports: "443", Target: "github.com"},
	} {
		got, _ := promptNet(t, keys, req)
		if got != want {
			t.Errorf("keys %q: got %+v, want %+v", keys, got, want)
		}
	}

	// Without a name the address is the target, and there is nothing to switch.
	if got, out := promptNet(t, "ts", netReq()); got.Target != "140.82.121.4" || strings.Contains(out, "[t]") {
		t.Errorf("no names: %+v\n%s", got, out)
	}
}

func TestPromptNetShowsWhatIsAsked(t *testing.T) {
	_, out := promptNet(t, "n", netReq("github.com", "www.github.com"))
	for _, want := range []string{"140.82.121.4:22", "github.com", "[y]", "[s]", "[p]", "[x]", "[e]", "[t]", "[n] deny", "default: Enter", "OUT OF THE JAIL"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q:\n%s", want, out)
		}
	}
	// A name the agent made up cannot draw on the terminal.
	_, out = promptNet(t, "n", netReq("evil\x1b[2J.example.com"))
	if strings.Contains(out, "\x1b[2J") || !strings.Contains(out, `\x1b[2J`) {
		t.Errorf("escape sequences from a name must be shown, not sent: %q", out)
	}
}

func TestPromptNetTimeoutAndCancel(t *testing.T) {
	var out bytes.Buffer
	slow := func(ctx context.Context, deadline time.Time) (byte, error) {
		<-time.After(time.Until(deadline))
		return 0, ErrTimeout
	}
	r, err := PromptNet(context.Background(), slow, &out, netReq(), 50*time.Millisecond, false)
	if err != ErrTimeout || r.Verb != VerbDeny || !strings.Contains(out.String(), "timed out") {
		t.Errorf("timeout: %+v %v\n%s", r, err, out.String())
	}
	if _, err := PromptNet(context.Background(), typed(""), &out, Request{Kind: KindNetwork}, time.Second, false); err == nil {
		t.Error("a network request without a connection is an error")
	}
}

func TestReplyEncoding(t *testing.T) {
	for _, r := range []Reply{{Verb: VerbOnce}, {Verb: VerbDeny}, {Verb: VerbSession, Ports: "22, 80", Target: "example.com"},
		{Verb: VerbRemember, Ports: "*", Target: "10.0.0.0/8"}, {Verb: VerbBlock, Ports: "443", Target: "*.evil.org"}} {
		if got := ParseReply(r.Encode()); got != r {
			t.Errorf("%+v became %+v", r, got)
		}
	}
	// Anything wrong is a deny, never an allow.
	for _, bad := range []string{"", "once", "{", `{"verb":"allow"}`, `{"verb":"session"}`, `{"verb":"session","ports":"99999","target":"a.com"}`,
		`{"verb":"session","ports":"22","target":"::1"}`, `{"verb":"remember","ports":"22","target":""}`, `{"verb":"SESSION","ports":"22","target":"a.com"}`} {
		if got := ParseReply(bad); got.Verb != VerbDeny || got.Allows() {
			t.Errorf("%q must be a deny, got %+v", bad, got)
		}
	}
	if !(Reply{Verb: VerbOnce}).Allows() || !(Reply{Verb: VerbSession}).Allows() || (Reply{Verb: VerbBlock}).Allows() || (Reply{Verb: VerbDeny}).Allows() {
		t.Error("Allows")
	}
}

func TestTmuxApproverNetworkRequest(t *testing.T) {
	self, _ := os.Executable()
	req := Request{Kind: KindNetwork, Net: &NetInfo{IP: "140.82.121.4", Port: 22, Names: []string{"github.com"}}}
	for behaviour, want := range map[string]Reply{
		`printf s | sh -c "$last"`:          {Verb: VerbSession, Ports: "22", Target: "github.com"},
		`printf 'e, 80\rp' | sh -c "$last"`: {Verb: VerbRemember, Ports: "22, 80", Target: "github.com"},
		`printf ts | sh -c "$last"`:         {Verb: VerbSession, Ports: "22", Target: "140.82.121.4"},
		`printf y | sh -c "$last"`:          {Verb: VerbOnce},
		`printf n | sh -c "$last"`:          {Verb: VerbDeny},
		`: | sh -c "$last"`:                 {Verb: VerbDeny},
	} {
		script, logf := fakeTmux(t, behaviour)
		a := &Tmux{Tmux: script, Pane: "%7", Self: self, Dir: t.TempDir()}
		got, err := a.AskNet(context.Background(), req, 5*time.Second)
		if err != nil || got != want {
			t.Errorf("%q: got %+v %v, want %+v", behaviour, got, err, want)
		}
		if calls, _ := os.ReadFile(logf); !strings.Contains(string(calls), "the jail wants to connect") {
			t.Errorf("the popup title must say what it is about: %s", calls)
		}
		if entries, _ := os.ReadDir(a.Dir); len(entries) != 0 {
			t.Errorf("files left behind: %v", entries)
		}
	}

	script, _ := fakeTmux(t, `f=$(mktemp -u); mkfifo "$f"; exec 3<>"$f"; rm -f "$f"; sh -c "$last" <&3`)
	a := &Tmux{Tmux: script, Pane: "%1", Self: self, Dir: t.TempDir()}
	if got, err := a.AskNet(context.Background(), req, 1*time.Second); err != ErrTimeout || got.Verb != VerbDeny {
		t.Errorf("popup timeout: %+v %v", got, err)
	}
}
