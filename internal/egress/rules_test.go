package egress

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePorts(t *testing.T) {
	for in, want := range map[string]string{
		"80":                    "80",
		"80, 443, 22":           "22, 80, 443",
		"80,443,22,8000-8080":   "22, 80, 443, 8000-8080",
		"1000-2000":             "1000-2000",
		"*":                     "*",
		"80 443":                "80, 443",
		"80, 81, 82":            "80-82",
		"100-200, 150-300, 301": "100-301",
		"443, 443":              "443",
		"22,*":                  "*",
	} {
		s, err := ParsePorts(in)
		if err != nil || s.String() != want {
			t.Errorf("%q: got %q %v, want %q", in, s.String(), err, want)
		}
	}
	for _, bad := range []string{"", " ", "0", "65536", "abc", "80-", "-80", "200-100", "80;443", "1-2-3", "8o"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	s, _ := ParsePorts("22, 80, 8000-8080")
	for port, want := range map[int]bool{22: true, 80: true, 8000: true, 8050: true, 8080: true, 8081: false, 21: false, 443: false} {
		if s.Contains(port) != want {
			t.Errorf("Contains(%d) = %v", port, !want)
		}
	}
	if !AnyPort.Contains(1) || !AnyPort.Contains(65535) || (PortSet{}).Contains(80) || !(PortSet{}).IsEmpty() {
		t.Error("any and empty")
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]string{
		"api.anthropic.com": "api.anthropic.com", "API.Example.COM": "api.example.com", "*.github.com": "*.github.com",
		"1.2.3.4": "1.2.3.4", "10.0.0.0/8": "10.0.0.0/8", "10.1.2.3/8": "10.0.0.0/8", "*": "*", "localhost": "localhost", "my_host-1.x": "my_host-1.x",
	} {
		tg, err := ParseTarget(in)
		if err != nil || tg.String() != want {
			t.Errorf("%q: got %q %v, want %q", in, tg.String(), err, want)
		}
	}
	for _, bad := range []string{"", "::1", "2001:db8::/32", "1.2.3.256", "999.1.1.1", "a..b", ".example.com", "exa mple.com", "*.", "*.*.com", "foo*.com", "10.0.0.0/33", "host:80", "http://x"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	w, _ := ParseTarget("*.example.com")
	if !w.MatchesName("a.example.com") || !w.MatchesName("A.B.Example.com.") || w.MatchesName("example.com") || w.MatchesName("notexample.com") || w.MatchesName("example.com.evil.org") {
		t.Error("wildcard matching")
	}
	e, _ := ParseTarget("example.com")
	if !e.MatchesName("EXAMPLE.com") || e.MatchesName("www.example.com") {
		t.Error("exact matching")
	}
	c, _ := ParseTarget("10.0.0.0/8")
	if !c.ContainsIP(netip.MustParseAddr("10.9.9.9")) || c.ContainsIP(netip.MustParseAddr("11.0.0.1")) || c.MatchesName("10.0.0.1") {
		t.Error("cidr matching")
	}
}

func rule(t *testing.T, to, ports string, d Decision, protos ...Proto) Rule {
	t.Helper()
	tg, err := ParseTarget(to)
	if err != nil {
		t.Fatal(err)
	}
	r := Rule{Target: tg, Decision: d, Protos: protos}
	if ports != "" {
		if r.Ports, err = ParsePorts(ports); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func dest(ip string, port int, names ...string) Dest {
	return Dest{IP: netip.MustParseAddr(ip), Port: port, Proto: TCP, Names: names}
}

func TestEvaluateFirstMatchWins(t *testing.T) {
	rules := []Rule{
		rule(t, "evil.example.com", "", Deny),
		rule(t, "*.example.com", "443", Allow),
		rule(t, "203.0.113.0/24", "22, 8000-8080", Allow),
		rule(t, "*", "80", Ask),
	}
	for _, c := range []struct {
		d    Dest
		want Decision
	}{
		{dest("93.184.216.34", 443, "www.example.com"), Allow},
		{dest("93.184.216.34", 80, "www.example.com"), Ask},    // port 80 is not in the allow rule; the * rule asks
		{dest("93.184.216.34", 443, "evil.example.com"), Deny}, // the deny rule comes first
		{dest("93.184.216.34", 443), Deny},                     // no name known: nothing matches, default
		{dest("203.0.113.9", 22), Allow},
		{dest("203.0.113.9", 8050), Allow},
		{dest("203.0.113.9", 23), Deny},
		{dest("198.51.100.1", 80), Ask},
		{dest("198.51.100.1", 25), Deny},
	} {
		if got, _ := Evaluate(rules, c.d, Deny); got != c.want {
			t.Errorf("%+v: got %s, want %s", c.d, got, c.want)
		}
	}
	if got, r := Evaluate(rules, dest("1.1.1.1", 25), Ask); got != Ask || r != nil {
		t.Errorf("the default: %s %v", got, r)
	}
}

func TestSpecialAddressesNeedAnAddressRule(t *testing.T) {
	rules := []Rule{
		rule(t, "internal.example.com", "", Allow),
		rule(t, "*", "", Allow),
		rule(t, "10.20.0.0/16", "5432", Allow),
	}
	// An allowed name that resolves to a private address (DNS rebinding) is not allowed by the name,
	// and * does not cover it either.
	for _, ip := range []string{"10.0.0.5", "192.168.1.1", "172.17.0.1", "127.0.0.1", "169.254.169.254", "100.64.1.1", "0.0.0.0", "224.0.0.1"} {
		if got, _ := Evaluate(rules, dest(ip, 80, "internal.example.com"), Deny); got != Deny {
			t.Errorf("%s was allowed by a name rule or *", ip)
		}
	}
	// An explicit address rule can allow one, on its own ports.
	if got, _ := Evaluate(rules, dest("10.20.3.4", 5432), Deny); got != Allow {
		t.Error("an explicit CIDR rule must allow")
	}
	if got, _ := Evaluate(rules, dest("10.20.3.4", 22), Deny); got != Deny {
		t.Error("other ports of a private range stay denied")
	}
	if got, _ := Evaluate(rules, dest("93.184.216.34", 22), Deny); got != Allow {
		t.Error("* allows public addresses")
	}
	for ip, want := range map[string]bool{"8.8.8.8": false, "93.184.216.34": false, "10.1.1.1": true, "172.16.0.1": true, "172.32.0.1": false, "192.168.0.1": true, "100.127.255.255": true, "100.128.0.0": false, "169.254.1.1": true, "240.0.0.1": true, "255.255.255.255": true} {
		if IsSpecial(netip.MustParseAddr(ip)) != want {
			t.Errorf("IsSpecial(%s) = %v", ip, !want)
		}
	}
}

func TestProtocolsAndICMP(t *testing.T) {
	udp := rule(t, "1.1.1.1", "53", Allow, UDP)
	if !udp.Matches(Dest{IP: netip.MustParseAddr("1.1.1.1"), Port: 53, Proto: UDP}) || udp.Matches(dest("1.1.1.1", 53)) {
		t.Error("a udp rule is for udp only")
	}
	def := rule(t, "1.1.1.1", "", Allow)
	if !def.AppliesTo(TCP) || !def.AppliesTo(UDP) || def.AppliesTo(ICMP) {
		t.Error("the default protocols of an address are tcp and udp")
	}
	if name := rule(t, "example.com", "", Allow); !name.AppliesTo(TCP) || name.AppliesTo(UDP) {
		t.Error("a name means tcp only")
	}
	ping := rule(t, "1.1.1.1", "", Allow, ICMP)
	if !ping.Matches(Dest{IP: netip.MustParseAddr("1.1.1.1"), Proto: ICMP}) {
		t.Error("icmp")
	}
	if p, err := ParseProtos([]string{"TCP", "udp,icmp"}); err != nil || len(p) != 3 {
		t.Errorf("%v %v", p, err)
	}
	if _, err := ParseProtos([]string{"sctp"}); err == nil {
		t.Error("sctp")
	}
}

func TestResolveName(t *testing.T) {
	rules := []Rule{
		rule(t, "tracker.example.com", "", Deny),
		rule(t, "partial.example.com", "25", Deny),
		rule(t, "*.example.com", "443", Allow),
		rule(t, "10.0.0.0/8", "", Allow),
	}
	for _, c := range []struct {
		name       string
		restricted bool
		want       bool
	}{
		{"tracker.example.com", false, false}, // an explicit deny on every port refuses the name
		{"www.example.com", true, true},
		{"partial.example.com", false, true}, // a deny on one port does not
		{"partial.example.com", true, true},  // and the allow rule covers it
		{"other.org", false, true},           // open: names nobody mentioned resolve
		{"other.org", true, false},           // restricted: they do not
		{"Tracker.Example.COM.", true, false},
	} {
		if got := ResolveName(rules, c.name, c.restricted); got != c.want {
			t.Errorf("ResolveName(%q, restricted=%v) = %v", c.name, c.restricted, got)
		}
	}
}

func TestFormatRule(t *testing.T) {
	if got := FormatRule(rule(t, "*.github.com", "22, 443", Allow, UDP, TCP)); got != "allow *.github.com ports 22, 443 proto tcp,udp" {
		t.Errorf("%q", got)
	}
	if got := FormatRule(rule(t, "10.0.0.1", "*", Deny)); got != "deny 10.0.0.1" {
		t.Errorf("%q", got)
	}
}

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "network.json")
	s := OpenStore(path)
	if rules, err := s.Rules(); err != nil || len(rules) != 0 {
		t.Fatalf("empty: %v %v", rules, err)
	}
	a := rule(t, "github.com", "22, 443", Allow)
	b := rule(t, "10.0.0.0/8", "5432", Deny, TCP)
	for _, r := range []Rule{a, b, a} { // the duplicate is ignored
		if err := s.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := s.Rules()
	if err != nil || len(rules) != 2 {
		t.Fatalf("%v %v", rules, err)
	}
	if rules[0].Target.String() != "github.com" || rules[0].Ports.String() != "22, 443" || rules[0].Decision != Allow ||
		rules[1].Target.String() != "10.0.0.0/8" || rules[1].Decision != Deny || len(rules[1].Protos) != 1 {
		t.Errorf("round trip: %+v", rules)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the file must be private: %v %v", st, err)
	}
	if err := s.Forget(5); err == nil {
		t.Error("forgetting a rule that is not there is an error")
	}
	if err := s.Forget(0); err != nil {
		t.Fatal(err)
	}
	if rules, _ := s.Rules(); len(rules) != 1 || rules[0].Target.String() != "10.0.0.0/8" {
		t.Errorf("after forget: %+v", rules)
	}
	// A damaged entry is skipped, a damaged file is an error.
	os.WriteFile(path, []byte(`{"rules":[{"to":"::1","decision":"allow"},{"to":"ok.example.com","decision":"allow"}]}`), 0o600)
	if rules, err := s.Rules(); err != nil || len(rules) != 1 {
		t.Errorf("skip the bad entry: %v %v", rules, err)
	}
	os.WriteFile(path, []byte(`{`), 0o600)
	if _, err := s.Rules(); err == nil {
		t.Error("a damaged file must be reported")
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if rules, err := s.Rules(); err != nil || len(rules) != 0 {
		t.Errorf("after clear: %v %v", rules, err)
	}
}
