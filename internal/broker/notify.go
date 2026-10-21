// SPDX-License-Identifier: GPL-3.0-only

package broker

import (
	"context"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/schorsch3000/aicatraz/internal/proto"
)

// notifyName is the command the jail's notification shim is called.
const notifyName = "notify-send"

const (
	maxSummary    = 100
	maxBody       = 500
	notifyTimeout = 5 * time.Second
	notifyBurst   = 5
	notifyEvery   = 2 * time.Second
)

// notifyEnv names the host variables notify-send needs to find the desktop.
var notifyEnv = []string{"PATH", "HOME", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "DISPLAY", "WAYLAND_DISPLAY"}

// notifyArgs reads the arguments of a notify-send call and keeps only what a
// jail may decide: urgency, summary and body. Every other option is skipped,
// with its value, so hints, icons, actions and timeouts never reach the host.
func notifyArgs(args []string) (urgency, summary, body string) {
	urgency = "normal"
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case a == "-u" || a == "--urgency":
			if i+1 < len(args) {
				i++
				urgency = pickUrgency(args[i], urgency)
			}
		case strings.HasPrefix(a, "--urgency="):
			urgency = pickUrgency(strings.TrimPrefix(a, "--urgency="), urgency)
		case a == "-a" || a == "-i" || a == "-c" || a == "-h" || a == "-t" || a == "-A" || a == "-p" || a == "-r" || a == "-n" ||
			a == "--app-name" || a == "--icon" || a == "--category" || a == "--hint" || a == "--expire-time" || a == "--action" || a == "--replace-id" || a == "--transient":
			if a != "--transient" {
				i++ // its value
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// some other flag, or a flag with its value attached: skipped
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) > 0 {
		summary = clean(pos[0], maxSummary)
	}
	if len(pos) > 1 {
		body = clean(strings.Join(pos[1:], " "), maxBody)
	}
	return urgency, summary, body
}

func pickUrgency(v, old string) string {
	switch v {
	case "low", "normal", "critical":
		return v
	}
	return old
}

// clean drops control characters, escapes markup (a notification daemon may
// render links and images from it) and cuts the text to max characters.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r) || r == utf8.RuneError:
			continue
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
		n++
	}
	return strings.TrimSpace(b.String())
}

// allowNotify is a token bucket: a burst, then one notification every few seconds.
func (b *Broker) allowNotify() bool {
	b.nmu.Lock()
	defer b.nmu.Unlock()
	now := time.Now()
	if b.ntokens == 0 && b.nlast.IsZero() {
		b.ntokens = notifyBurst
	} else {
		b.ntokens += float64(now.Sub(b.nlast)) / float64(notifyEvery)
		if b.ntokens > notifyBurst {
			b.ntokens = notifyBurst
		}
	}
	b.nlast = now
	if b.ntokens < 1 {
		return false
	}
	b.ntokens--
	return true
}

// notifyCommand prepares the host's notify-send for the jail's call, or says
// why there is none.
func (b *Broker) notifyCommand(ctx context.Context, h *proto.Hello) (*exec.Cmd, string) {
	urgency, summary, body := notifyArgs(h.Args)
	if summary == "" {
		return nil, "notify-send: no summary given"
	}
	if !b.allowNotify() {
		return nil, "notify-send: too many notifications, slow down"
	}
	bin, err := resolveBinary(notifyName, pathOf(b.opts.HostEnv), b.writable)
	if err != nil {
		return nil, "notify-send: " + err.Error()
	}
	app := "AIcatraz"
	if b.opts.Session != "" {
		app += " " + b.opts.Session
	}
	args := []string{"-a", app, "-u", urgency, "--", summary}
	if body != "" {
		args = append(args, body)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	for _, name := range notifyEnv {
		for _, kv := range b.opts.HostEnv {
			if strings.HasPrefix(kv, name+"=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
	}
	cmd.Dir = "/"
	return cmd, ""
}
