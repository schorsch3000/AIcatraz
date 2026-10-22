// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/egress"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

const networkUsage = "usage: aicatraz network [-c file] list\n" +
	"       aicatraz network [-c file] allow|deny <host|ip|cidr|*> [--ports 22,443,8000-8080|*]\n" +
	"       aicatraz network [-c file] forget <number> | --all"

// Network implements `aicatraz network`: show the network rules of the
// project and the ones remembered for it, and change the remembered ones.
// They apply to sessions that are running, too.
func Network(args []string, stdout, stderr io.Writer) error {
	cfgPath := config.DefaultPath()
	var rest []string
	for len(args) > 0 {
		if (args[0] == "-c" || args[0] == "--config") && len(args) >= 2 {
			cfgPath = args[1]
			args = args[2:]
			continue
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
	if len(rest) == 0 {
		rest = []string{"list"}
	}
	cfg, id, err := loadForID(cfgPath)
	if err != nil {
		return err
	}
	state, err := StateDir(id)
	if err != nil {
		return err
	}
	store := egress.OpenStore(filepath.Join(state, "network.json"))
	st := ui.For(stdout)

	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			return errors.New(networkUsage)
		}
		switch cfg.NetworkMode() {
		case config.NetOff:
			fmt.Fprintln(stdout, "the network is off: the jail has none (no network section, or `network: off`)")
		case config.NetOpen:
			fmt.Fprintln(stdout, "the network is open: `network: open` lets the jail connect anywhere")
		default:
			fmt.Fprintf(stdout, "%s  default: %s, dns: %s\n", st.Bold("config"), cfg.Network.DefaultOrAsk(), orDefault(cfg.Network.DNS, "open"))
			for i, r := range cfg.NetRules {
				fmt.Fprintf(stdout, "  %2d  %s\n", i+1, egress.FormatRule(r))
			}
			if len(cfg.NetRules) == 0 {
				fmt.Fprintln(stdout, st.Dim("      (no rules)"))
			}
		}
		remembered, err := store.Rules()
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s  (after the config's rules; `%s network forget <number>`)\n", st.Bold("remembered"), ui.Prog)
		for i, r := range remembered {
			fmt.Fprintf(stdout, "  %2d  %s\n", i+1, egress.FormatRule(r))
		}
		if len(remembered) == 0 {
			fmt.Fprintln(stdout, st.Dim("      (nothing)"))
		}
		return nil

	case "allow", "deny":
		var target, ports string
		for i := 1; i < len(rest); i++ {
			switch {
			case rest[i] == "--ports" && i+1 < len(rest):
				ports = rest[i+1]
				i++
			case target == "" && len(rest[i]) > 0 && rest[i][0] != '-':
				target = rest[i]
			default:
				return errors.New(networkUsage)
			}
		}
		if target == "" {
			return errors.New(networkUsage)
		}
		t, err := egress.ParseTarget(target)
		if err != nil {
			return err
		}
		rule := egress.Rule{Target: t, Protos: []egress.Proto{egress.TCP}, Decision: egress.Decision(rest[0])}
		if ports != "" {
			if rule.Ports, err = egress.ParsePorts(ports); err != nil {
				return err
			}
		}
		if err := store.Add(rule); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s\n", st.BoldGreen("remembered"), egress.FormatRule(rule))
		return nil

	case "forget":
		if len(rest) != 2 {
			return errors.New(networkUsage)
		}
		if rest[1] == "--all" {
			if err := store.Clear(); err != nil {
				return err
			}
			fmt.Fprintln(stdout, st.BoldGreen("forgot")+" every remembered rule")
			return nil
		}
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			return errors.New(networkUsage)
		}
		remembered, err := store.Rules()
		if err != nil {
			return err
		}
		if n < 1 || n > len(remembered) {
			return fmt.Errorf("no remembered rule number %d (see `%s network list`)", n, ui.Prog)
		}
		if err := store.Forget(n - 1); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s\n", st.BoldGreen("forgot"), egress.FormatRule(remembered[n-1]))
		return nil
	}
	return errors.New(networkUsage)
}
