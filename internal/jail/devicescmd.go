// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"fmt"
	"io"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/devices"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// Devices implements `aicatraz devices [-c file]`: the USB devices of this
// machine, with the names to write in a `devices` entry, and which of them the
// config hands to the jail.
func Devices(args []string, stdout, stderr io.Writer) error {
	cfgPath := "aicatraz.yaml"
	for len(args) > 0 {
		if (args[0] == "-c" || args[0] == "--config") && len(args) >= 2 {
			cfgPath = args[1]
			args = args[2:]
			continue
		}
		return fmt.Errorf("usage: aicatraz devices [-c file]")
	}
	var specs []devices.Spec
	if cfg, problems, err := config.LoadFile(cfgPath, config.Options{}); err == nil && !config.HasErrors(problems) {
		specs = cfg.DeviceSpecs
	}

	sc := devices.NewScanner()
	given := map[string]bool{}
	for _, n := range sc.Scan(specs) {
		given[n.Path] = true
	}
	st := ui.For(stdout)
	list := sc.List()
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no USB devices found")
		return nil
	}
	for _, d := range list {
		name := strings.TrimSpace(d.Manufacturer + " " + d.Product)
		if name == "" {
			name = "(unnamed)"
		}
		serial := ""
		if d.Serial != "" {
			serial = "   serial " + d.Serial
		}
		fmt.Fprintf(stdout, "%s  %s%s\n", st.BoldCyan(d.VID+":"+d.PID), name, st.Dim(serial))
		for _, n := range d.Nodes {
			mark := ""
			if given[n.Path] {
				mark = "   " + st.BoldGreen("given to the jail")
			}
			fmt.Fprintf(stdout, "    %s%s\n", n.Path, mark)
			for _, l := range n.Links {
				fmt.Fprintf(stdout, "      %s\n", st.Dim(l))
			}
		}
		if len(d.Nodes) == 0 {
			fmt.Fprintln(stdout, "    "+st.Dim("no device nodes"))
		}
	}
	if len(specs) == 0 {
		fmt.Fprintln(stdout, st.Dim("\nname one in the config:  devices: [{usb: \"vendor:product\"}]  or  [{serial: \"usb-...\"}]"))
	}
	return nil
}
