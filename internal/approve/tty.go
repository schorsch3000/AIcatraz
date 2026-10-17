// SPDX-License-Identifier: GPL-3.0-only

package approve

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// TTY prompts on a terminal device the agent has no access to, for example
// the /dev/pts/N of a second terminal window. Nothing else should read from
// that terminal while it is used: run something idle there, such as
// `sleep infinity`, not an interactive shell.
type TTY struct {
	Path string
	mu   sync.Mutex
}

func (t *TTY) Ask(ctx context.Context, req Request, timeout time.Duration) (Answer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.OpenFile(t.Path, os.O_RDWR|syscallNoCtty, 0)
	if err != nil {
		return Deny, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer f.Close()
	return Prompt(ctx, f, f, req, timeout)
}

// AskNet shows a network request on the terminal.
func (t *TTY) AskNet(ctx context.Context, req Request, timeout time.Duration) (Reply, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.OpenFile(t.Path, os.O_RDWR|syscallNoCtty, 0)
	if err != nil {
		return Reply{Verb: VerbDeny}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer f.Close()
	return PromptNetFile(ctx, f, f, req, timeout)
}
