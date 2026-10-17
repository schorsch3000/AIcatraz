// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/egress"
)

// netPolicy is the host's side of the egress control: it decides, for every
// connection and every DNS question the sidecar reports, using the rules of the
// config, then the ones the human remembered, then the ones chosen for this
// session. The first rule that matches wins; a connection nothing matches gets
// the default, which usually means asking.
type netPolicy struct {
	base       []egress.Rule
	def        egress.Decision
	restricted bool
	timeout    time.Duration
	store      *egress.Store
	approver   approve.NetApprover // nil: nobody can be asked
	audit      *broker.Audit
	session    string
	stderr     io.Writer

	mu          sync.Mutex
	runtime     []egress.Rule // chosen for this session
	cached      []egress.Rule // from the store
	cachedMod   time.Time
	cachedSize  int64
	warnedAsked bool

	promptMu sync.Mutex // one question at a time; the next one sees the answer to this one
}

func newNetPolicy(cfg *config.Config, store *egress.Store, approver approve.Approver, audit *broker.Audit, session string, stderr io.Writer) *netPolicy {
	p := &netPolicy{
		base:       cfg.NetRules,
		def:        cfg.Network.DefaultOrAsk(),
		restricted: cfg.Network.DNS == "restricted",
		timeout:    cfg.Approval.TimeoutOrDefault(),
		store:      store,
		audit:      audit,
		session:    session,
		stderr:     stderr,
	}
	if na, ok := approver.(approve.NetApprover); ok {
		p.approver = na
	}
	return p
}

// rules returns every rule in effect, in order.
func (p *netPolicy) rules() []egress.Rule {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, err := os.Stat(p.store.Path()); err == nil {
		if !st.ModTime().Equal(p.cachedMod) || st.Size() != p.cachedSize {
			if rules, err := p.store.Rules(); err == nil {
				p.cached, p.cachedMod, p.cachedSize = rules, st.ModTime(), st.Size()
			}
		}
	} else {
		p.cached, p.cachedMod, p.cachedSize = nil, time.Time{}, 0
	}
	all := append([]egress.Rule(nil), p.base...)
	all = append(all, p.cached...)
	return append(all, p.runtime...)
}

func (p *netPolicy) log(d egress.Dest, decision, how string) {
	name := dest(d)
	p.audit.Log(broker.Entry{Session: p.session, Event: "net", Name: name, Args: d.Names, Decision: decision, Message: how})
}

func dest(d egress.Dest) string { return d.IP.String() + ":" + strconv.Itoa(d.Port) }

// DecideTCP implements egress.Handler.
func (p *netPolicy) DecideTCP(ctx context.Context, d egress.Dest) bool {
	switch dec, _ := egress.Evaluate(p.rules(), d, p.def); dec {
	case egress.Allow:
		p.log(d, "allow", "rule")
		return true
	case egress.Deny:
		p.log(d, "deny", "rule")
		return false
	}
	return p.ask(ctx, d)
}

func (p *netPolicy) ask(ctx context.Context, d egress.Dest) bool {
	p.promptMu.Lock()
	defer p.promptMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	// An answer to an earlier question may have decided this one.
	switch dec, _ := egress.Evaluate(p.rules(), d, p.def); dec {
	case egress.Allow:
		p.log(d, "allow", "rule")
		return true
	case egress.Deny:
		p.log(d, "deny", "rule")
		return false
	}

	if p.approver == nil {
		p.mu.Lock()
		first := !p.warnedAsked
		p.warnedAsked = true
		p.mu.Unlock()
		if first {
			fmt.Fprintf(p.stderr, "aicatraz: network: %s is not covered by a rule and nobody can be asked (start with `aicatraz up`, or pass --approval-tty); refused\n", dest(d))
		}
		p.log(d, "deny", "nobody to ask")
		return false
	}

	req := approve.Request{Kind: approve.KindNetwork, Net: &approve.NetInfo{IP: d.IP.String(), Port: d.Port, Names: d.Names}}
	if egress.IsSpecial(d.IP) {
		req.Warnings = append(req.Warnings, "this is an address on the local or a private network, or a special one: only a rule naming the address can allow it")
	}
	reply, err := p.approver.AskNet(ctx, req, p.timeout)
	if err != nil {
		p.log(d, "deny", "no answer: "+err.Error())
		return false
	}
	p.log(d, map[bool]string{true: "allow", false: "deny"}[reply.Allows()], "asked: "+reply.Verb)

	switch reply.Verb {
	case approve.VerbSession, approve.VerbRemember, approve.VerbBlock:
		rule, err := ruleFromReply(reply)
		if err != nil {
			return false
		}
		if reply.Verb == approve.VerbRemember {
			if err := p.store.Add(rule); err != nil {
				fmt.Fprintf(p.stderr, "aicatraz: network: cannot remember the rule: %v\n", err)
			}
		}
		p.mu.Lock()
		p.runtime = append(p.runtime, rule)
		p.mu.Unlock()
	}
	return reply.Allows()
}

// ruleFromReply turns an answer into the TCP rule it asks for.
func ruleFromReply(r approve.Reply) (egress.Rule, error) {
	t, err := egress.ParseTarget(r.Target)
	if err != nil {
		return egress.Rule{}, err
	}
	ports, err := egress.ParsePorts(r.Ports)
	if err != nil {
		return egress.Rule{}, err
	}
	dec := egress.Allow
	if r.Verb == approve.VerbBlock {
		dec = egress.Deny
	}
	return egress.Rule{Target: t, Ports: ports, Protos: []egress.Proto{egress.TCP}, Decision: dec}, nil
}

// DecideDNS implements egress.Handler.
func (p *netPolicy) DecideDNS(name string) bool {
	return egress.ResolveName(p.rules(), name, p.restricted)
}
