// Package shim is the in-container side of the command proxy. It runs in
// place of a proxied command, forwards the call to the host broker, and
// behaves like the command: same output, same exit code.
package shim

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/schorsch3000/aicatraz/internal/proto"
)

// Exit codes for failures of the proxy itself, chosen to be distinguishable
// from the proxied command's own codes where possible.
const (
	ExitDenied      = 126 // the policy said no
	ExitNotFound    = 127
	ExitUnavailable = 69 // cannot reach the broker (EX_UNAVAILABLE)
	ExitLost        = 70 // lost the broker mid-command (EX_SOFTWARE)
)

// SocketPath is where the broker's socket is mounted inside the container.
var SocketPath = "/aicatraz/broker.sock"

// BinDir is the directory holding the shim symlinks; it is skipped when
// looking for the container's own binary.
var BinDir = "/aicatraz/bin"

// Dial timing: the connection is retried with exponential backoff, since the
// broker may still be starting or restarting. DialTimeout bounds the whole
// attempt.
var (
	dialFirst   = 50 * time.Millisecond
	dialMax     = time.Second
	DialTimeout = 8 * time.Second
)

// IO holds the streams the shim uses.
type IO struct {
	Stdin          *os.File
	Stdout, Stderr io.Writer
}

// Run executes one proxied command and returns its exit code.
func Run(name string, args []string, std IO) int {
	conn, err := dial()
	if err != nil {
		fmt.Fprintf(std.Stderr, "aicatraz: cannot reach the broker at %s: %v\n", SocketPath, err)
		return ExitUnavailable
	}
	defer conn.Close()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(std.Stderr, "aicatraz: %v\n", err)
		return ExitNotFound
	}
	stdinFd := int(std.Stdin.Fd())
	tty := term.IsTerminal(stdinFd) && isTerminalWriter(std.Stdout)
	hello := proto.Hello{Version: proto.Version, Name: name, Args: args, Cwd: cwd, Env: os.Environ(), TTY: tty}
	if tty {
		if w, h, err := term.GetSize(stdinFd); err == nil {
			hello.Cols, hello.Rows = uint16(w), uint16(h)
		}
	}
	if err := conn.WriteJSON(proto.THello, hello); err != nil {
		fmt.Fprintf(std.Stderr, "aicatraz: %v\n", err)
		return ExitUnavailable
	}

	var dec proto.Decision
	if err := conn.ReadJSON(proto.TDecision, &dec); err != nil {
		fmt.Fprintf(std.Stderr, "aicatraz: no answer from the broker: %v\n", err)
		return ExitLost
	}
	switch dec.Action {
	case proto.ActionDeny:
		fmt.Fprintf(std.Stderr, "aicatraz: %s: %s\n", name, dec.Message)
		return ExitDenied
	case proto.ActionLocal:
		return execLocal(name, args, std.Stderr)
	case proto.ActionRun:
		return stream(conn, std, tty)
	}
	fmt.Fprintf(std.Stderr, "aicatraz: unknown decision %q\n", dec.Action)
	return ExitLost
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func dial() (*proto.Conn, error) {
	delay := dialFirst
	deadline := time.Now().Add(DialTimeout)
	for {
		c, err := net.Dial("unix", SocketPath)
		if err == nil {
			return proto.New(c), nil
		}
		if time.Now().Add(delay).After(deadline) {
			return nil, err
		}
		time.Sleep(delay)
		if delay *= 2; delay > dialMax {
			delay = dialMax
		}
	}
}

// stream forwards stdin, signals and window size to the broker and prints
// what comes back until the command exits.
func stream(conn *proto.Conn, std IO, tty bool) int {
	if tty {
		if old, err := term.MakeRaw(int(std.Stdin.Fd())); err == nil {
			defer term.Restore(int(std.Stdin.Fd()), old)
		}
	}

	stop := make(chan struct{})
	defer close(stop)
	go conn.KeepAlive(stop)

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH)
	defer signal.Stop(sigs)
	go func() {
		for {
			select {
			case <-stop:
				return
			case s := <-sigs:
				if s == syscall.SIGWINCH {
					if w, h, err := term.GetSize(int(std.Stdin.Fd())); err == nil {
						conn.Write(proto.TResize, proto.ResizePayload(uint16(h), uint16(w)))
					}
				} else if n, ok := s.(syscall.Signal); ok {
					conn.Write(proto.TSignal, []byte{byte(n)})
				}
			}
		}
	}()

	go func() {
		buf := make([]byte, proto.ChunkSize)
		for {
			n, err := std.Stdin.Read(buf)
			if n > 0 {
				if conn.Write(proto.TStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				conn.Write(proto.TStdinEOF, nil)
				return
			}
		}
	}()

	for {
		t, p, err := conn.Read()
		if err != nil {
			if tty {
				fmt.Fprint(std.Stderr, "\r") // the terminal is still raw
			}
			fmt.Fprintf(std.Stderr, "aicatraz: lost the connection to the broker: %v\n", err)
			return ExitLost
		}
		switch t {
		case proto.TStdout:
			std.Stdout.Write(p)
		case proto.TStderr:
			std.Stderr.Write(p)
		case proto.TExit:
			var e proto.Exit
			if err := json.Unmarshal(p, &e); err != nil {
				return ExitLost
			}
			return e.Code
		}
	}
}

// execLocal replaces the shim with the container's own binary, looked up on
// PATH while skipping the shim directory.
func execLocal(name string, args []string, stderr io.Writer) int {
	real, err := lookPathSkipping(name, os.Getenv("PATH"), BinDir)
	if err != nil {
		fmt.Fprintf(stderr, "aicatraz: %s: command not found in the container\n", name)
		return ExitNotFound
	}
	err = syscall.Exec(real, append([]string{name}, args...), os.Environ())
	fmt.Fprintf(stderr, "aicatraz: %s: %v\n", name, err)
	return ExitNotFound
}

func lookPathSkipping(name, pathEnv, skipDir string) (string, error) {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" || filepath.Clean(dir) == filepath.Clean(skipDir) {
			continue
		}
		cand := filepath.Join(dir, name)
		if st, err := os.Stat(cand); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", exec.ErrNotFound
}
