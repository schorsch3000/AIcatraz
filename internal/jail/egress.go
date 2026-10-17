// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/schorsch3000/aicatraz/internal/approve"
	"github.com/schorsch3000/aicatraz/internal/broker"
	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/egress"
)

// The network helper image: Debian with nftables, built once on the first use
// of a network section.
const (
	egressImage      = "aicatraz/egress:1"
	egressDockerfile = "FROM debian:stable-slim\nRUN apt-get update && apt-get install -y --no-install-recommends nftables && rm -rf /var/lib/apt/lists/*\n"
)

// EgressName is the name of the sidecar container of a session.
func EgressName(id string, pid int) string { return fmt.Sprintf("aicatraz-egress-%s-%d", id, pid) }

// egressRunArgs is the `docker run` line of the sidecar. It owns the network
// namespace the jail joins: NET_ADMIN to set the firewall, NET_BIND_SERVICE
// for the DNS forwarder on port 53, nothing else; IPv6 is switched off; its
// resolver is itself. The socket and the rules reach it as mounts.
func egressRunArgs(name, id string, pid int, binary, sock, rules string, resolvers []string) []string {
	return []string{"run", "-d", "--init",
		"--name", name,
		"--hostname", hostname(id),
		"--label", "aicatraz.project=" + id,
		"--label", "aicatraz.egress=1",
		"--label", "aicatraz.pid=" + strconv.Itoa(pid),
		"--cap-drop=ALL", "--cap-add=NET_ADMIN", "--cap-add=NET_BIND_SERVICE",
		"--security-opt=no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,size=16m",
		"--pids-limit", "512", "--memory", "256m",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"--dns", "127.0.0.1",
		"--mount", bind(binary, "/aicatraz/aicatraz", true),
		"--mount", bind(sock, "/aicatraz/control.sock", true),
		"--mount", bind(rules, "/aicatraz/egress.nft", true),
		"--entrypoint", "/aicatraz/aicatraz",
		egressImage,
		egress.SidecarCommand, "/aicatraz/control.sock", "/aicatraz/egress.nft", strings.Join(resolvers, ","),
	}
}

// ensureEgressImage builds the helper image if this machine does not have it.
func ensureEgressImage(out io.Writer) error {
	if exec.Command("docker", "image", "inspect", egressImage).Run() == nil {
		return nil
	}
	fmt.Fprintln(out, "aicatraz: building the network helper image "+egressImage+" (once; it needs network access)")
	cmd := exec.Command("docker", "build", "-t", egressImage, "-")
	cmd.Stdin = strings.NewReader(egressDockerfile)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building %s failed: %w", egressImage, err)
	}
	return nil
}

// hostResolvers finds the DNS servers of this machine that a container can
// use: loopback stubs (such as systemd-resolved's) are not reachable from one.
func hostResolvers() ([]string, error) {
	for _, file := range []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"} {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		var found []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "nameserver" {
				if a, err := netip.ParseAddr(fields[1]); err == nil && a.Is4() && !a.IsLoopback() {
					found = append(found, a.String())
				}
			}
		}
		f.Close()
		if len(found) > 0 {
			return found, nil
		}
	}
	return nil, fmt.Errorf("found no DNS server on this machine that the network helper can use (only loopback ones); name them in network.resolvers")
}

// removeStaleEgress removes sidecars whose launcher is gone.
func removeStaleEgress() {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "label=aicatraz.egress=1",
		"--format", `{{.Names}} {{.Label "aicatraz.pid"}}`).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err == nil && pid != os.Getpid() && syscall.Kill(pid, 0) == syscall.ESRCH {
			exec.Command("docker", "rm", "-f", f[0]).Run()
		}
	}
}

// egressRun is a running sidecar.
type egressRun struct {
	name string
	stop func()
}

// startEgress starts the sidecar for a session and waits until its firewall
// is in place. The jail then joins its network. The returned stop removes it.
func startEgress(cfg *config.Config, id, state, runDir string, approver approve.Approver, self string, stderr io.Writer) (*egressRun, error) {
	if err := ensureEgressImage(stderr); err != nil {
		return nil, err
	}
	removeStaleEgress()
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	resolvers := cfg.Network.Resolvers
	if len(resolvers) == 0 {
		var err error
		if resolvers, err = hostResolvers(); err != nil {
			return nil, err
		}
	}

	pid := os.Getpid()
	name := EgressName(id, pid)
	rulesFile := filepath.Join(runDir, "egress.nft")
	if err := os.WriteFile(rulesFile, []byte(egress.Ruleset(cfg.NetRules)), 0o644); err != nil {
		return nil, err
	}
	sock := filepath.Join(runDir, "egress.sock")
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	os.Chmod(sock, 0o666) // root in the sidecar has no DAC override; the private runDir is what protects it

	audit, _ := broker.OpenAudit(filepath.Join(state, "audit.log"))
	policy := newNetPolicy(cfg, egress.OpenStore(filepath.Join(state, "network.json")), approver, audit,
		fmt.Sprintf("%s-%d", id, pid), stderr)
	srv := egress.NewServer(policy)
	go srv.Serve(l)

	stop := func() {
		exec.Command("docker", "rm", "-f", name).Run()
		l.Close()
		audit.Close()
		os.Remove(sock)
		os.Remove(rulesFile)
	}

	if out, err := exec.Command("docker", egressRunArgs(name, id, pid, self, sock, rulesFile, resolvers)...).CombinedOutput(); err != nil {
		stop()
		return nil, fmt.Errorf("starting the network helper: %v: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-srv.Ready():
			return &egressRun{name: name, stop: stop}, nil
		case <-deadline:
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			stop()
			return nil, fmt.Errorf("the network helper did not come up: %s", strings.TrimSpace(string(logs)))
		case <-time.After(300 * time.Millisecond):
			if out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
				logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
				stop()
				return nil, fmt.Errorf("the network helper stopped: %s", strings.TrimSpace(string(logs)))
			}
		}
	}
}
