// SPDX-License-Identifier: GPL-3.0-only

package approve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tmux shows the prompt as a popup over the pane the agent runs in. The popup
// is a separate pty owned by the host-side tmux client: the agent's keystrokes
// and output can't reach it, and the container has no access to the tmux
// socket.
type Tmux struct {
	Tmux string // tmux binary
	Pane string // $TMUX_PANE of the pane the agent runs in
	Self string // path of this aicatraz binary, run inside the popup
	Dir  string // private directory for the request/answer files

	mu  sync.Mutex
	seq int
}

// PromptCommand is the hidden subcommand the popup runs.
const PromptCommand = "__approve"

func (t *Tmux) Ask(ctx context.Context, req Request, timeout time.Duration) (Answer, error) {
	text, err := t.popup(ctx, req, timeout)
	if err != nil {
		return Deny, err
	}
	return ParseAnswer(text), nil
}

// AskNet shows a network request in the popup.
func (t *Tmux) AskNet(ctx context.Context, req Request, timeout time.Duration) (Reply, error) {
	text, err := t.popup(ctx, req, timeout)
	if err != nil {
		return Reply{Verb: VerbDeny}, err
	}
	return ParseReply(text), nil
}

// popup runs the prompt in a popup and returns what it wrote as the answer.
func (t *Tmux) popup(ctx context.Context, req Request, timeout time.Duration) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	reqFile := filepath.Join(t.Dir, "ask-"+strconv.Itoa(t.seq)+".json")
	ansFile := filepath.Join(t.Dir, "ask-"+strconv.Itoa(t.seq)+".answer")
	defer os.Remove(reqFile)
	defer os.Remove(ansFile)

	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(reqFile, data, 0o600); err != nil {
		return "", err
	}

	title := " aicatraz: approve a host command "
	if req.Kind == KindNetwork {
		title = " aicatraz: the jail wants to connect "
	}
	inner := shellJoin(t.Self, PromptCommand, reqFile, ansFile, strconv.Itoa(int(timeout.Seconds())))
	cmd := exec.CommandContext(ctx, t.Tmux, "display-popup",
		"-t", t.Pane, "-E", "-w", "90%", "-h", "70%", "-b", "rounded",
		"-T", title, "exec "+inner)
	cmd.WaitDelay = time.Second
	out, runErr := cmd.CombinedOutput()

	if ctx.Err() != nil {
		// The agent gave up or the broker is closing: take the popup away.
		_ = exec.Command(t.Tmux, "display-popup", "-C", "-t", t.Pane).Run()
		return "", ctx.Err()
	}
	ans, readErr := os.ReadFile(ansFile)
	if readErr != nil {
		if runErr != nil {
			return "", fmt.Errorf("%w: tmux: %v: %s", ErrUnavailable, runErr, strings.TrimSpace(string(out)))
		}
		return "deny", nil // popup closed without an answer
	}
	if strings.TrimSpace(string(ans)) == "timeout" {
		return "", ErrTimeout
	}
	return string(ans), nil
}

// RunPrompt is the body of the popup: read the request, ask on the popup's own
// terminal, write the answer. It returns the process exit code.
func RunPrompt(reqFile, ansFile string, timeout time.Duration) int {
	data, err := os.ReadFile(reqFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aicatraz:", err)
		return 2
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintln(os.Stderr, "aicatraz:", err)
		return 2
	}
	var result string
	if req.Kind == KindNetwork {
		reply, err := PromptNetFile(context.Background(), os.Stdin, os.Stdout, req, timeout)
		result = reply.Encode()
		if err == ErrTimeout {
			result = "timeout"
		}
	} else {
		ans, err := Prompt(context.Background(), os.Stdin, os.Stdout, req, timeout)
		result = ans.String()
		if err == ErrTimeout {
			result = "timeout"
		}
	}
	if err := os.WriteFile(ansFile, []byte(result), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "aicatraz:", err)
		return 2
	}
	time.Sleep(400 * time.Millisecond) // let the result line be seen before the popup closes
	return 0
}

// shellJoin quotes words for sh.
func shellJoin(words ...string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
