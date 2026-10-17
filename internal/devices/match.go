// SPDX-License-Identifier: GPL-3.0-only

// Package devices gives the jail access to hardware that comes and goes: USB
// serial adapters, Arduino, ESP and Pico boards. The host is watched for the
// devices a config names, and their device nodes are created in the running
// jail, and removed again, as they appear and disappear.
package devices

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Spec names devices to hand to the jail. Exactly one of Serial, USB and Path
// is set.
type Spec struct {
	Serial    string // glob over the names in /dev/serial/by-id
	USB       string // vendor:product, lower case hex, ? and * allowed
	USBSerial string // glob over the USB serial number, with USB
	Path      string // a device node on the host, a glob under /dev
}

// String says what a spec is for, in the config's words.
func (s Spec) String() string {
	switch {
	case s.Serial != "":
		return "serial " + s.Serial
	case s.USB != "" && s.USBSerial != "":
		return "usb " + s.USB + " serial " + s.USBSerial
	case s.USB != "":
		return "usb " + s.USB
	}
	return "path " + s.Path
}

// Node is a device node of a device that matches.
type Node struct {
	Path   string   // the node on the host, which is also its path in the jail: /dev/ttyACM0
	Major  uint32   //
	Minor  uint32   //
	Links  []string // stable names that lead to it: /dev/serial/by-id/usb-Arduino_...
	Device string   // the USB device it belongs to, for messages
}

// Same reports whether two nodes are the same node with the same links.
func (n Node) Same(o Node) bool {
	if n.Path != o.Path || n.Major != o.Major || n.Minor != o.Minor || len(n.Links) != len(o.Links) {
		return false
	}
	for i := range n.Links {
		if n.Links[i] != o.Links[i] {
			return false
		}
	}
	return true
}

// Scanner finds the nodes that match a list of specs.
type Scanner struct {
	DevRoot string // /dev
	SysRoot string // /sys
	// Stat reports the device number of a node and whether it is a character
	// device; tests replace it, since they can't create device nodes.
	Stat func(path string) (rdev uint64, char bool, err error)
}

// NewScanner scans the real host.
func NewScanner() *Scanner {
	return &Scanner{DevRoot: "/dev", SysRoot: "/sys", Stat: statDevice}
}

func statDevice(p string) (uint64, bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil { // follows symlinks
		return 0, false, err
	}
	return uint64(st.Rdev), st.Mode&syscall.S_IFMT == syscall.S_IFCHR, nil
}

func (sc *Scanner) node(p string, links []string, dev string) (Node, bool) {
	rdev, char, err := sc.Stat(p)
	if err != nil || !char {
		return Node{}, false
	}
	sort.Strings(links)
	return Node{Path: p, Major: unix.Major(rdev), Minor: unix.Minor(rdev), Links: links, Device: dev}, true
}

// hostPath maps /dev/... to the scanner's root.
func (sc *Scanner) hostPath(p string) string {
	return filepath.Join(sc.DevRoot, strings.TrimPrefix(p, "/dev/"))
}

// jailPath maps a host path under DevRoot back to /dev/...
func (sc *Scanner) jailPath(p string) string {
	rel, err := filepath.Rel(sc.DevRoot, p)
	if err != nil {
		return p
	}
	return "/dev/" + filepath.ToSlash(rel)
}

// byID lists the links in /dev/serial/by-id with the node each leads to.
func (sc *Scanner) byID() map[string]string {
	dir := filepath.Join(sc.DevRoot, "serial", "by-id")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		target, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name()))
		if err == nil {
			out[e.Name()] = target
		}
	}
	return out
}

// linksTo returns the by-id links that lead to the node at target.
func (sc *Scanner) linksTo(byID map[string]string, target string) []string {
	var links []string
	for name, t := range byID {
		if t == target {
			links = append(links, sc.jailPath(filepath.Join(sc.DevRoot, "serial", "by-id", name)))
		}
	}
	sort.Strings(links)
	return links
}

type usbDevice struct {
	dir, vid, pid, serial, manufacturer, product string
	bus, dev                                     int
}

// usbDevices lists the USB devices (not their interfaces) from sysfs.
func (sc *Scanner) usbDevices() []usbDevice {
	base := filepath.Join(sc.SysRoot, "bus", "usb", "devices")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []usbDevice
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		get := func(name string) string {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(b))
		}
		vid, pid := get("idVendor"), get("idProduct")
		if vid == "" || pid == "" {
			continue // an interface
		}
		// In sysfs the entries of bus/usb/devices are links into the device tree,
		// and the walk below must start at the real directory.
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			real = dir
		}
		d := usbDevice{dir: real, vid: strings.ToLower(vid), pid: strings.ToLower(pid), serial: get("serial"),
			manufacturer: get("manufacturer"), product: get("product")}
		d.bus, _ = strconv.Atoi(get("busnum"))
		d.dev, _ = strconv.Atoi(get("devnum"))
		out = append(out, d)
	}
	return out
}

var ttyName = func(n string) bool {
	return (strings.HasPrefix(n, "ttyACM") || strings.HasPrefix(n, "ttyUSB")) && strings.Trim(n[6:], "0123456789") == "" && len(n) > 6
}

// ttyNodes finds the serial ports below a USB device in sysfs.
func ttyNodes(dir string) []string {
	var names []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && ttyName(d.Name()) {
			names = append(names, d.Name())
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(names)
	return names
}

func globOK(pattern, s string) bool {
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

// Scan returns the nodes of the devices that match any spec, once each,
// sorted by path.
func (sc *Scanner) Scan(specs []Spec) []Node {
	found := map[string]Node{}
	add := func(n Node) {
		if have, dup := found[n.Path]; dup { // keep the links of both
			links := map[string]bool{}
			for _, l := range append(append([]string(nil), have.Links...), n.Links...) {
				links[l] = true
			}
			n.Links = nil
			for l := range links {
				n.Links = append(n.Links, l)
			}
			sort.Strings(n.Links)
			if n.Device == "" {
				n.Device = have.Device
			}
		}
		found[n.Path] = n
	}

	byID := sc.byID()
	var usb []usbDevice
	for _, s := range specs {
		switch {
		case s.Serial != "":
			for name, target := range byID {
				if !globOK(s.Serial, name) {
					continue
				}
				if n, ok := sc.node(target, sc.linksTo(byID, target), name); ok {
					n.Path = sc.jailPath(n.Path)
					add(n)
				}
			}

		case s.USB != "":
			if usb == nil {
				usb = sc.usbDevices()
			}
			vp := strings.SplitN(s.USB, ":", 2)
			for _, d := range usb {
				if len(vp) != 2 || !globOK(vp[0], d.vid) || !globOK(vp[1], d.pid) {
					continue
				}
				if s.USBSerial != "" && !globOK(s.USBSerial, d.serial) {
					continue
				}
				label := d.vid + ":" + d.pid
				if d.product != "" {
					label += " " + d.product
				}
				// The raw device, for tools that talk USB themselves.
				if d.bus > 0 && d.dev > 0 {
					raw := filepath.Join(sc.DevRoot, "bus", "usb", fmt.Sprintf("%03d", d.bus), fmt.Sprintf("%03d", d.dev))
					if n, ok := sc.node(raw, nil, label); ok {
						n.Path = sc.jailPath(n.Path)
						add(n)
					}
				}
				// Its serial ports.
				for _, name := range ttyNodes(d.dir) {
					node := filepath.Join(sc.DevRoot, name)
					if n, ok := sc.node(node, sc.linksTo(byID, node), label); ok {
						n.Path = sc.jailPath(n.Path)
						add(n)
					}
				}
			}

		case s.Path != "":
			matches, _ := filepath.Glob(sc.hostPath(s.Path))
			for _, m := range matches {
				if n, ok := sc.node(m, sc.linksTo(byID, m), s.Path); ok {
					n.Path = sc.jailPath(m)
					add(n)
				}
			}
		}
	}

	out := make([]Node, 0, len(found))
	for _, n := range found {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Majors of the device classes a spec can name. Their numbers are fixed in
// Linux, so the jail can be given access to them before any device is there.
const (
	MajorACM    = 166 // /dev/ttyACM*, Arduino and the like
	MajorUSBTTY = 188 // /dev/ttyUSB*, CH340, CP210x, FTDI
	MajorUSBRaw = 189 // /dev/bus/usb/*
)

// CgroupRules are the device-cgroup rules the jail starts with. A rule can't
// be added to a running container, so they are the classes a spec can name
// (serial and usb), and the exact device numbers of the nodes that match a
// path now. The jail can't create nodes (it has no MKNOD, even with sudo), so
// a rule alone gives it nothing: only the nodes the launcher creates exist.
func CgroupRules(specs []Spec, present []Node) []string {
	var rules []string
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			rules = append(rules, r)
		}
	}
	for _, s := range specs {
		if s.Serial != "" || s.USB != "" {
			add(fmt.Sprintf("c %d:* rwm", MajorACM))
			add(fmt.Sprintf("c %d:* rwm", MajorUSBTTY))
			add(fmt.Sprintf("c %d:* rwm", MajorUSBRaw))
		}
	}
	for _, n := range present {
		add(fmt.Sprintf("c %d:%d rwm", n.Major, n.Minor))
	}
	sort.Strings(rules)
	return rules
}

// Info describes a USB device of the host, for `aicatraz devices`.
type Info struct {
	VID, PID, Manufacturer, Product, Serial string
	Nodes                                   []Node
}

// List returns the USB devices of the host with their nodes, root hubs left
// out.
func (sc *Scanner) List() []Info {
	byID := sc.byID()
	var out []Info
	for _, d := range sc.usbDevices() {
		if d.vid == "1d6b" { // the kernel's root hubs
			continue
		}
		info := Info{VID: d.vid, PID: d.pid, Manufacturer: d.manufacturer, Product: d.product, Serial: d.serial}
		label := d.vid + ":" + d.pid
		if d.bus > 0 && d.dev > 0 {
			raw := filepath.Join(sc.DevRoot, "bus", "usb", fmt.Sprintf("%03d", d.bus), fmt.Sprintf("%03d", d.dev))
			if n, ok := sc.node(raw, nil, label); ok {
				n.Path = sc.jailPath(n.Path)
				info.Nodes = append(info.Nodes, n)
			}
		}
		for _, name := range ttyNodes(d.dir) {
			node := filepath.Join(sc.DevRoot, name)
			if n, ok := sc.node(node, sc.linksTo(byID, node), label); ok {
				n.Path = sc.jailPath(n.Path)
				info.Nodes = append(info.Nodes, n)
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VID != out[j].VID {
			return out[i].VID < out[j].VID
		}
		return out[i].PID < out[j].PID
	})
	return out
}
