// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/devices"
)

// dockerOps makes and removes device nodes in the running jail with
// `docker exec --privileged`, running our own binary: the jail has no
// capability to make device nodes, so the launcher does it from outside. The
// nodes are owned by the jail's user.
type dockerOps struct {
	container string
	uid, gid  int
}

func (d dockerOps) exec(ctx context.Context, args ...string) error {
	full := append([]string{"exec", "--privileged", "-u", "0", d.container, ContainerBinary, devices.NodeCommand}, args...)
	out, err := exec.CommandContext(ctx, "docker", full...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func (d dockerOps) Add(ctx context.Context, n devices.Node) error {
	args := []string{"add", n.Path, strconv.Itoa(int(n.Major)), strconv.Itoa(int(n.Minor)), strconv.Itoa(d.uid), strconv.Itoa(d.gid)}
	return d.exec(ctx, append(args, n.Links...)...)
}

func (d dockerOps) Remove(ctx context.Context, n devices.Node) error {
	return d.exec(ctx, append([]string{"remove", n.Path}, n.Links...)...)
}

// startDeviceWatcher keeps the jail's device nodes in step with the hardware
// the config names, until stop is called. Each change goes into the audit
// log; nothing is printed, because the launcher's output shares the terminal
// with the program in the jail.
func startDeviceWatcher(in RunInput, audit *broker.Audit, session string, scan *devices.Scanner) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &devices.Watcher{
		Specs: in.Cfg.DeviceSpecs,
		Scan:  scan.Scan,
		Ops:   dockerOps{container: in.Name, uid: in.UID, gid: in.GID},
		Log: func(e devices.Event) {
			entry := broker.Entry{Session: session, Event: "device", Name: e.Node.Path, Args: e.Node.Links, Decision: e.Kind, Message: e.Node.Device}
			if e.Err != nil {
				entry.Message = e.Err.Error()
			}
			audit.Log(entry)
		},
	}
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}
