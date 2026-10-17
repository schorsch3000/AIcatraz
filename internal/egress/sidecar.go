// SPDX-License-Identifier: GPL-3.0-only

package egress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// SidecarCommand is the hidden subcommand that runs the sidecar.
const SidecarCommand = "__egress"

// RunSidecar is the sidecar: `__egress <control socket> <ruleset file> <resolvers, comma separated>`.
// It loads the firewall rules, starts the DNS forwarder on 127.0.0.1:53 and
// the proxy, tells the host it is ready, and runs until the host hangs up.
func RunSidecar(args []string, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "usage: "+SidecarCommand+" control-socket ruleset-file resolvers")
		return 2
	}
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }

	ctl, err := Dial(args[0])
	if err != nil {
		logf("control: %v", err)
		return 1
	}
	defer ctl.Close()

	if out, err := exec.Command("nft", "-f", args[1]).CombinedOutput(); err != nil {
		logf("nft: %v: %s", err, strings.TrimSpace(string(out)))
		return 1
	}

	var upstreams []string
	for _, r := range strings.Split(args[2], ",") {
		if r = strings.TrimSpace(r); r != "" {
			if !strings.Contains(r, ":") {
				r += ":53"
			}
			upstreams = append(upstreams, r)
		}
	}
	cache := NewNameCache()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fwd := &Forwarder{Upstreams: upstreams, Mark: Mark, Cache: cache, Allow: ctl.DecideDNS, Log: logf}
	udp, err := net.ListenPacket("udp", "127.0.0.1:53")
	if err != nil {
		logf("dns: %v", err)
		return 1
	}
	defer udp.Close()
	tcp, err := net.Listen("tcp", "127.0.0.1:53")
	if err != nil {
		logf("dns: %v", err)
		return 1
	}
	defer tcp.Close()
	go fwd.ServeUDP(ctx, udp)
	go fwd.ServeTCP(ctx, tcp)

	pl, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ProxyPort))
	if err != nil {
		logf("proxy: %v", err)
		return 1
	}
	defer pl.Close()
	proxy := &Proxy{Cache: cache, Decide: ctl.DecideTCP, Mark: Mark, Log: logf,
		Self: netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", ProxyPort))}
	go proxy.Serve(ctx, pl)

	if err := ctl.Hello(); err != nil {
		logf("control: %v", err)
		return 1
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-ctl.Done(): // the host is gone: nothing may leave without it
	case <-sigs:
	}
	return 0
}
