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

	text := Render(req)
	if raw {
		text = strings.ReplaceAll(text, "\n", "\r\n") // raw mode doesn't translate newlines
	}
	if _, err := io.WriteString(out, text); err != nil {
		return Deny, err
	}

	deadline := time.Now().Add(timeout)
	answer, err := readAnswer(ctx, in, deadline, req)
	nl := "\n"
	if raw {
		nl = "\r\n"
	}
	switch {
	case err == ErrTimeout:
		fmt.Fprintf(out, "%s  -> timed out: denied%s", nl, nl)
	case err != nil:
		fmt.Fprintf(out, "%s  -> cancelled%s", nl, nl)
	default:
		fmt.Fprintf(out, "%s  -> %s%s", nl, answerText(answer), nl)
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

func readAnswer(ctx context.Context, in *os.File, deadline time.Time, req Request) (Answer, error) {
	fd := int(in.Fd())
	buf := make([]byte, 1)
	for {
		if err := ctx.Err(); err != nil {
			return Deny, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return Deny, ErrTimeout
		}
		wait := min(left, pollSlice)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(wait.Milliseconds())+1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return Deny, err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return Deny, io.ErrUnexpectedEOF
		}
		m, err := in.Read(buf)
		if m == 0 || err != nil {
			// EOF or hangup: nobody is there to answer.
			return Deny, nil
		}
		switch buf[0] {
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
		// Any other key is ignored; only an explicit y or s approves.
	}
}
