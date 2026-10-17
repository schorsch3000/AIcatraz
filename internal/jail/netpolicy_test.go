package jail

import (
	"bytes"
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/egress"
)

type fakeNetApprover struct {
	mu      sync.Mutex
	reqs    []approve.Request
	replies []approve.Reply
	delay   time.Duration
}

func (f *fakeNetApprover) Ask(ctx context.Context, req approve.Request, timeout time.Duration) (approve.Answer, error) {
	return approve.Deny, nil
}
func (f *fakeNetApprover) AskNet(ctx context.Context, req approve.Request, timeout time.Duration) (approve.Reply, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if len(f.replies) == 0 {
		return approve.Reply{Verb: approve.VerbDeny}, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func testPolicy(t *testing.T, yaml string, ap approve.Approver) (*netPolicy, *bytes.Buffer) {
	t.Helper()
	cfg, ps := config.Parse([]byte(yaml), config.Options{Home: "/home/u", Dir: "/p"})
	if config.HasErrors(ps) {
		t.Fatalf("config: %+v", ps)
	}
	var errb bytes.Buffer
	store := egress.OpenStore(filepath.Join(t.TempDir(), "network.json"))
	return newNetPolicy(cfg, store, ap, nil, "t", &errb), &errb
}

func dst(ip string, port int, names ...string) egress.Dest {
	return egress.Dest{IP: netip.MustParseAddr(ip), Port: port, Proto: egress.TCP, Names: names}
}

const netCfg = `
network:
  rules:
    - {to: "*.anthropic.com", ports: 443, decision: allow}
    - {to: evil.example.com, decision: deny}
    - {to: 10.20.0.0/16, ports: "5432", decision: allow}
`

func TestPolicyRulesDecideWithoutAsking(t *testing.T) {
	ap := &fakeNetApprover{}
	p, _ := testPolicy(t, netCfg, ap)
	ctx := context.Background()
	if !p.DecideTCP(ctx, dst("160.79.104.10", 443, "api.anthropic.com")) {
		t.Error("allowed by a rule")
	}
	if p.DecideTCP(ctx, dst("1.2.3.4", 443, "evil.example.com")) {
		t.Error("denied by a rule")
	}
	if !p.DecideTCP(ctx, dst("10.20.1.1", 5432)) {
		t.Error("allowed by an address rule")
	}
	if len(ap.reqs) != 0 {
		t.Errorf("rules must decide without a question: %d asked", len(ap.reqs))
	}
}

func TestPolicyAsksForTheRest(t *testing.T) {
	ap := &fakeNetApprover{replies: []approve.Reply{
		{Verb: approve.VerbOnce},
		{Verb: approve.VerbDeny},
		{Verb: approve.VerbSession, Ports: "22, 443", Target: "github.com"},
		{Verb: approve.VerbBlock, Ports: "*", Target: "ads.example.net"},
	}}
	p, _ := testPolicy(t, netCfg, ap)
	ctx := context.Background()
	gh := dst("140.82.121.4", 22, "github.com")

	if !p.DecideTCP(ctx, gh) { // once
		t.Error("once allows this connection")
	}
	if p.DecideTCP(ctx, gh) { // asked again: deny
		t.Error("deny")
	}
	if !p.DecideTCP(ctx, gh) { // session rule from now on
		t.Error("session allows")
	}
	n := len(ap.reqs)
	if !p.DecideTCP(ctx, dst("140.82.121.4", 443, "github.com")) || !p.DecideTCP(ctx, gh) {
		t.Error("the session rule covers both ports")
	}
	if len(ap.reqs) != n {
		t.Error("a session rule must stop the questions")
	}
	if p.DecideTCP(ctx, dst("140.82.121.4", 8080, "github.com")) {
		t.Error("another port is asked again (and answered with the empty reply: deny)")
	}

	ap.replies = append(ap.replies, approve.Reply{Verb: approve.VerbBlock, Ports: "*", Target: "ads.example.net"})
	ads := dst("203.0.113.5", 443, "ads.example.net")
	if p.DecideTCP(ctx, ads) {
		t.Error("block")
	}
	n = len(ap.reqs)
	if p.DecideTCP(ctx, ads) || len(ap.reqs) != n {
		t.Error("a block rule stops the questions and keeps refusing")
	}

	// The question carries what the person needs.
	r := ap.reqs[0]
	if r.Kind != approve.KindNetwork || r.Net.IP != "140.82.121.4" || r.Net.Port != 22 || r.Net.Names[0] != "github.com" || len(r.Warnings) != 0 {
		t.Errorf("request %+v", r)
	}
}

func TestPolicyRememberedRulesSurviveASession(t *testing.T) {
	ap := &fakeNetApprover{replies: []approve.Reply{{Verb: approve.VerbRemember, Ports: "22", Target: "github.com"}}}
	p, _ := testPolicy(t, netCfg, ap)
	if !p.DecideTCP(context.Background(), dst("140.82.121.4", 22, "github.com")) {
		t.Fatal("remember allows")
	}
	// A new session with the same store sees the rule.
	p2, _ := testPolicy(t, netCfg, &fakeNetApprover{})
	p2.store = p.store
	if !p2.DecideTCP(context.Background(), dst("140.82.121.9", 22, "github.com")) {
		t.Error("a remembered rule must apply in the next session")
	}
	// The config comes first: a deny there beats anything remembered.
	if err := p.store.Add(egress.Rule{Target: mustTarget(t, "evil.example.com"), Decision: egress.Allow}); err != nil {
		t.Fatal(err)
	}
	if p2.DecideTCP(context.Background(), dst("1.2.3.4", 443, "evil.example.com")) {
		t.Error("the config's deny must win over a remembered allow")
	}
	// A rule added from outside (aicatraz network allow) is picked up while running.
	if p2.DecideTCP(context.Background(), dst("198.51.100.7", 443, "late.example.org")) {
		t.Fatal("not allowed yet")
	}
	time.Sleep(10 * time.Millisecond)
	p.store.Add(egress.Rule{Target: mustTarget(t, "late.example.org"), Decision: egress.Allow})
	if !p2.DecideTCP(context.Background(), dst("198.51.100.7", 443, "late.example.org")) {
		t.Error("a rule added to the store while the session runs must apply")
	}
}

func mustTarget(t *testing.T, s string) egress.Target {
	t.Helper()
	tg, err := egress.ParseTarget(s)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestPolicyDenyDefaultAndNobodyToAsk(t *testing.T) {
	p, _ := testPolicy(t, "network: {default: deny}\n", &fakeNetApprover{})
	if p.DecideTCP(context.Background(), dst("1.2.3.4", 80)) {
		t.Error("default deny")
	}
	p, errb := testPolicy(t, netCfg, nil)
	for i := 0; i < 3; i++ {
		if p.DecideTCP(context.Background(), dst("1.2.3.4", 80)) {
			t.Error("with nobody to ask an unmatched connection is refused")
		}
	}
	if n := strings.Count(errb.String(), "nobody can be asked"); n != 1 {
		t.Errorf("the reason is said once, not %d times: %q", n, errb.String())
	}
}

func TestPolicySpecialAddressGetsAWarning(t *testing.T) {
	ap := &fakeNetApprover{replies: []approve.Reply{{Verb: approve.VerbDeny}}}
	p, _ := testPolicy(t, netCfg, ap)
	p.DecideTCP(context.Background(), dst("169.254.169.254", 80, "metadata.example.com"))
	if len(ap.reqs) != 1 || len(ap.reqs[0].Warnings) == 0 {
		t.Errorf("a special address must be flagged: %+v", ap.reqs)
	}
}

func TestPolicyQuestionsAreSequentialAndShareAnswers(t *testing.T) {
	ap := &fakeNetApprover{delay: 30 * time.Millisecond, replies: []approve.Reply{{Verb: approve.VerbSession, Ports: "443", Target: "burst.example.com"}}}
	p, _ := testPolicy(t, netCfg, ap)
	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.DecideTCP(context.Background(), dst("198.51.100.20", 443, "burst.example.com"))
		}()
	}
	wg.Wait()
	for i, r := range results {
		if !r {
			t.Errorf("connection %d of the burst was refused", i)
		}
	}
	if len(ap.reqs) != 1 {
		t.Errorf("a burst to one destination must produce one question, got %d", len(ap.reqs))
	}
}

func TestPolicyDNS(t *testing.T) {
	p, _ := testPolicy(t, netCfg, nil)
	if p.DecideDNS("evil.example.com") || !p.DecideDNS("anything.org") {
		t.Error("open dns refuses only what a rule denies")
	}
	p, _ = testPolicy(t, "network: {dns: restricted, rules: [{to: '*.anthropic.com', decision: allow}]}\n", nil)
	if !p.DecideDNS("api.anthropic.com") || p.DecideDNS("anything.org") {
		t.Error("restricted dns resolves only names a rule covers")
	}
}
