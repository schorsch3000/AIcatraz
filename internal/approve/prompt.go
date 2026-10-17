// SPDX-License-Identifier: GPL-3.0-only

package approve

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/ui"
)

const pollSlice = 100 * time.Millisecond

// Prompt shows req on out and reads the answer from in (a single key; no
// Enter needed on a terminal). Enter, Esc, Ctrl-C, Ctrl-D and any other
// unlisted key during the wait never approve: only y or s do. It returns
// ErrTimeout after timeout and ctx.Err() if ctx ends first.
//
// If in is a terminal it is put in raw mode and anything typed before the
// prompt appeared is discarded, so a stray keypress can't answer it.
func Prompt(ctx context.Context, in *os.File, out io.Writer, req Request, timeout time.Duration) (Answer, error) {
	raw := term.IsTerminal(int(in.Fd()))
	if raw {
		old, err := term.MakeRaw(int(in.Fd()))
		if err == nil {
			defer term.Restore(int(in.Fd()), old)
		} else {
			raw = false
		}
		// Discard type-ahead.
		_ = unix.IoctlSetInt(int(in.Fd()), unix.TCFLSH, unix.TCIFLUSH)
	}
	return PromptKeys(ctx, fdKeys(in), out, req, timeout, raw)
}

// PromptNetFile is PromptNet on a terminal file: it is put in raw mode, and
// anything typed before the prompt appeared is discarded.
func PromptNetFile(ctx context.Context, in *os.File, out io.Writer, req Request, timeout time.Duration) (Reply, error) {
	raw := term.IsTerminal(int(in.Fd()))
	if raw {
		old, err := term.MakeRaw(int(in.Fd()))
		if err == nil {
			defer term.Restore(int(in.Fd()), old)
		} else {
			raw = false
		}
		_ = unix.IoctlSetInt(int(in.Fd()), unix.TCFLSH, unix.TCIFLUSH)
	}
	return PromptNet(ctx, fdKeys(in), out, req, timeout, raw)
}

// KeySource returns the next key of the person answering. It returns
// io.EOF when nobody is there any more, ErrTimeout once deadline has passed
// and ctx.Err() if ctx ends first.
type KeySource func(ctx context.Context, deadline time.Time) (byte, error)

// PromptKeys is Prompt for a caller that owns the input: the keys come from
// keys, the prompt is drawn on out. raw says out is a raw terminal, which
// does not translate newlines. Enter, Esc, Ctrl-C, Ctrl-D and any other
// unlisted key never approve: only y, s and p do.
func PromptKeys(ctx context.Context, keys KeySource, out io.Writer, req Request, timeout time.Duration, raw bool) (Answer, error) {
	text := RenderStyled(req, ui.For(out))
	if raw {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if _, err := io.WriteString(out, text); err != nil {
		return Deny, err
	}

	deadline := time.Now().Add(timeout)
	answer, err := readAnswer(ctx, keys, deadline, req)
	nl := "\n"
	if raw {
		nl = "\r\n"
	}
	st := ui.For(out)
	switch {
	case err == ErrTimeout:
		fmt.Fprintf(out, "%s  %s%s", nl, st.BoldYellow("-> timed out: denied"), nl)
	case err != nil:
		fmt.Fprintf(out, "%s  %s%s", nl, st.Yellow("-> cancelled"), nl)
	case answer == Deny:
		fmt.Fprintf(out, "%s  %s%s", nl, st.BoldRed("-> "+answerText(answer)), nl)
	default:
		fmt.Fprintf(out, "%s  %s%s", nl, st.BoldGreen("-> "+answerText(answer)), nl)
	}
	return answer, err
}

func answerText(a Answer) string {
	switch a {
	case AllowOnce:
		return "running once"
	case AllowSession:
		return "running; this exact call is allowed for the session"
	case AllowPin:
		return "approved and remembered"
	}
	return "denied"
}

// fdKeys reads keys from a file descriptor, polling so the deadline and the
// context are honoured.
func fdKeys(in *os.File) KeySource {
	fd := int(in.Fd())
	buf := make([]byte, 1)
	return func(ctx context.Context, deadline time.Time) (byte, error) {
		for {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			left := time.Until(deadline)
			if left <= 0 {
				return 0, ErrTimeout
			}
			wait := min(left, pollSlice)
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(fds, int(wait.Milliseconds())+1)
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				return 0, err
			}
			if n == 0 {
				continue
			}
			if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
				return 0, io.ErrUnexpectedEOF
			}
			m, err := in.Read(buf)
			if m == 0 || err != nil {
				return 0, io.EOF // hangup: nobody is there to answer
			}
			return buf[0], nil
		}
	}
}

func readAnswer(ctx context.Context, keys KeySource, deadline time.Time, req Request) (Answer, error) {
	for {
		key, err := keys(ctx, deadline)
		switch {
		case err == io.EOF:
			return Deny, nil
		case err != nil:
			return Deny, err
		}
		switch key {
		case 'y', 'Y':
			return AllowOnce, nil
		case 's', 'S':
			if req.Kind != KindBuild {
				return AllowSession, nil
			}
		case 'p', 'P':
			if req.CanPin {
				return AllowPin, nil
			}
		case 'n', 'N', '\r', '\n', 27, 3, 4:
			return Deny, nil
		}
		// Any other key is ignored; only an explicit y, s or p approves.
	}
}
