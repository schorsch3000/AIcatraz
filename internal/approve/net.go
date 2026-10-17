package approve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/schorsch3000/aicatraz/internal/egress"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// Verbs of a network Reply.
const (
	VerbOnce     = "once"     // let this connection through, nothing remembered
	VerbSession  = "session"  // allow the target on the ports until the session ends
	VerbRemember = "remember" // allow it, and remember it for the project
	VerbBlock    = "block"    // refuse the target on the ports until the session ends
	VerbDeny     = "deny"     // refuse this connection
)

// Reply is the human's answer to a network request: what to do, and for the
// rules (session, remember and block), what they cover.
type Reply struct {
	Verb   string `json:"verb"`
	Ports  string `json:"ports,omitempty"`  // a port set: "22, 443, 8000-8080" or "*"
	Target string `json:"target,omitempty"` // a name, an address or a CIDR range
}

// Encode is the text a Reply travels as between processes.
func (r Reply) Encode() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// ParseReply reads what Encode wrote. Anything that is not a valid reply is a
// deny, never an allow.
func ParseReply(s string) Reply {
	var r Reply
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &r) != nil {
		return Reply{Verb: VerbDeny}
	}
	switch r.Verb {
	case VerbOnce, VerbDeny:
		return Reply{Verb: r.Verb}
	case VerbSession, VerbRemember, VerbBlock:
		if _, err := egress.ParsePorts(r.Ports); err != nil {
			return Reply{Verb: VerbDeny}
		}
		if _, err := egress.ParseTarget(r.Target); err != nil {
			return Reply{Verb: VerbDeny}
		}
		return r
	}
	return Reply{Verb: VerbDeny}
}

// Allows reports whether the reply lets the connection through.
func (r Reply) Allows() bool {
	return r.Verb == VerbOnce || r.Verb == VerbSession || r.Verb == VerbRemember
}

// netTargets are what a rule made from the answer can be about: each name the
// jail resolved to the address, then the address itself.
func netTargets(n *NetInfo) []string {
	var t []string
	for _, name := range n.Names {
		if _, err := egress.ParseTarget(name); err == nil {
			t = append(t, strings.ToLower(name))
		}
	}
	return append(t, n.IP)
}

// PromptNet shows a network request on out, with the answers read from keys.
// The ports start as the port of the connection, and the target as its name
// (or its address if the jail did not look it up); e edits the ports, t picks
// another target. Esc, Ctrl-C and Enter deny; an invalid port list is never
// accepted.
func PromptNet(ctx context.Context, keys KeySource, out io.Writer, req Request, timeout time.Duration, raw bool) (Reply, error) {
	if req.Net == nil {
		return Reply{Verb: VerbDeny}, errors.New("not a network request")
	}
	nl := "\n"
	if raw {
		nl = "\r\n"
	}
	st := ui.For(out)
	text := RenderStyled(req, st)
	if raw {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	write := func(s string) { io.WriteString(out, s) }
	write(text)

	targets := netTargets(req.Net)
	ti := 0
	ports := SinglePortText(req.Net.Port)

	show := func() {
		opt := func(key, s string) { write("  " + st.BoldGreen("["+key+"]") + " " + s + nl) }
		write(nl)
		opt("y", "allow this connection once")
		opt("s", fmt.Sprintf("allow %s on %s for the session", st.Bold(Quote(targets[ti])), st.Bold(ports)))
		opt("p", fmt.Sprintf("allow %s on %s and remember it", st.Bold(Quote(targets[ti])), st.Bold(ports)))
		opt("x", fmt.Sprintf("block %s on %s for the session", st.Bold(Quote(targets[ti])), st.Bold(ports)))
		line := "  " + st.BoldGreen("[e]") + " edit the ports"
		if len(targets) > 1 {
			line += "   " + st.BoldGreen("[t]") + " other target"
		}
		write(line + nl)
		write("  " + st.BoldRed("[n] deny") + st.Dim("   (default: Enter)") + nl)
	}
	show()

	deadline := time.Now().Add(timeout)
	finish := func(r Reply, err error) (Reply, error) {
		switch {
		case err == ErrTimeout:
			write(nl + "  " + st.BoldYellow("-> timed out: denied") + nl)
		case err != nil:
			write(nl + "  " + st.Yellow("-> cancelled") + nl)
		case r.Verb == VerbDeny:
			write(nl + "  " + st.BoldRed("-> denied") + nl)
		case r.Verb == VerbBlock:
			write(nl + "  " + st.BoldRed("-> blocked") + nl)
		default:
			write(nl + "  " + st.BoldGreen("-> allowed") + nl)
		}
		return r, err
	}
	rule := func(verb string) Reply { return Reply{Verb: verb, Ports: ports, Target: targets[ti]} }

	for {
		key, err := keys(ctx, deadline)
		switch {
		case err == io.EOF:
			return finish(Reply{Verb: VerbDeny}, nil)
		case err != nil:
			return finish(Reply{Verb: VerbDeny}, err)
		}
		switch key {
		case 'y', 'Y':
			return finish(Reply{Verb: VerbOnce}, nil)
		case 's', 'S':
			return finish(rule(VerbSession), nil)
		case 'p', 'P':
			return finish(rule(VerbRemember), nil)
		case 'x', 'X':
			return finish(rule(VerbBlock), nil)
		case 'n', 'N', '\r', '\n', 27, 3, 4:
			return finish(Reply{Verb: VerbDeny}, nil)
		case 't', 'T':
			if len(targets) > 1 {
				ti = (ti + 1) % len(targets)
				show()
			}
		case 'e', 'E':
			edited, err := editPorts(ctx, keys, out, deadline, ports, nl, st)
			switch {
			case err == io.EOF:
				return finish(Reply{Verb: VerbDeny}, nil)
			case err != nil:
				return finish(Reply{Verb: VerbDeny}, err)
			}
			ports = edited
			show()
		}
	}
}

// SinglePortText is the text of a port set holding one port.
func SinglePortText(port int) string { return egress.SinglePort(port).String() }

// editPorts reads a port list, echoing it on the prompt's line. Enter accepts
// a valid list, Esc and Ctrl-C keep the old one.
func editPorts(ctx context.Context, keys KeySource, out io.Writer, deadline time.Time, current, nl string, st ui.Style) (string, error) {
	buf := []byte(current)
	write := func(s string) { io.WriteString(out, s) }
	write(nl + "  ports> " + string(buf))
	for {
		key, err := keys(ctx, deadline)
		if err != nil {
			return current, err
		}
		switch {
		case key == '\r' || key == '\n':
			set, err := egress.ParsePorts(string(buf))
			if err != nil {
				write(nl + "  " + st.Yellow(err.Error()) + nl + "  ports> " + string(buf))
				continue
			}
			write(nl)
			return set.String(), nil
		case key == 27 || key == 3:
			write(nl)
			return current, nil
		case key == 127 || key == 8:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				write("\b \b")
			}
		case key >= '0' && key <= '9' || key == ',' || key == '-' || key == '*' || key == ' ':
			if len(buf) < 120 {
				buf = append(buf, key)
				write(string(key))
			}
		}
	}
}
