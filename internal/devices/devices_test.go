// SPDX-License-Identifier: GPL-3.0-only

package devices

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// host builds a fake /dev and /sys in a temp directory. Device nodes can't be
// made without root, so plain files stand in for them and the scanner's Stat
// is told their device numbers.
type host struct {
	t    *testing.T
	root string
	rdev map[string]uint64
}

func newHost(t *testing.T) *host {
	t.Helper()
	return &host{t: t, root: t.TempDir(), rdev: map[string]uint64{}}
}

func (h *host) write(rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) node(rel string, major, minor uint32) {
	h.write(rel, "")
	h.rdev[filepath.Join(h.root, rel)] = uint64(unix.Mkdev(major, minor))
}

func (h *host) link(rel, target string) {
	h.t.Helper()
	p := filepath.Join(h.root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.Remove(p)
	if err := os.Symlink(target, p); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) remove(rel string) {
	os.RemoveAll(filepath.Join(h.root, rel))
	delete(h.rdev, filepath.Join(h.root, rel))
}

// usb adds a USB device the way sysfs has it: the real directory in the device
// tree, and a link to it under bus/usb/devices.
func (h *host) usb(name, vid, pid, serial, product string, bus, dev int) {
	real := "sys/devices/pci0000:00/usb1/" + name
	h.link("sys/bus/usb/devices/"+name, filepath.Join(h.root, real))
	base := real + "/"
	h.write(base+"idVendor", vid+"\n")
	h.write(base+"idProduct", pid+"\n")
	h.write(base+"product", product+"\n")
	if serial != "" {
		h.write(base+"serial", serial+"\n")
	}
	h.write(base+"busnum", itoa(bus)+"\n")
	h.write(base+"devnum", itoa(dev)+"\n")
}

func itoa(n int) string { return fmt.Sprintf("%03d", n) }

func (h *host) scanner() *Scanner {
	return &Scanner{
		DevRoot: filepath.Join(h.root, "dev"),
		SysRoot: filepath.Join(h.root, "sys"),
		Stat: func(p string) (uint64, bool, error) {
			r, ok := h.rdev[p]
			if !ok {
				return 0, false, os.ErrNotExist
			}
			return r, true, nil
		},
	}
}

// arduino plugs in an Arduino Uno: a cdc-acm port with a by-id link, and the raw device.
func (h *host) arduino(tty string, bus, dev int) {
	h.usb("1-2", "2341", "0043", "75830333", "Arduino Uno", bus, dev)
	h.write("sys/devices/pci0000:00/usb1/1-2/1-2:1.0/tty/"+tty+"/dev", "166:0\n")
	h.node("dev/"+tty, 166, 0)
	h.link("dev/serial/by-id/usb-Arduino__www.arduino.cc__0043_75830333-if00", "../../"+tty)
	h.node("dev/bus/usb/001/"+itoa(dev), 189, uint32((bus-1)*128+dev-1))
}

func paths(nodes []Node) []string {
	var p []string
	for _, n := range nodes {
		p = append(p, n.Path)
	}
	return p
}

func TestScanFindsDevicesByIdUsbAndPath(t *testing.T) {
	h := newHost(t)
	h.arduino("ttyACM0", 1, 5)
	// A CH340 adapter: a usb-serial port with a by-id link of its own.
	h.usb("1-3", "1a86", "7523", "", "USB Serial", 1, 7)
	h.write("sys/devices/pci0000:00/usb1/1-3/1-3:1.0/ttyUSB0/dev", "188:0\n")
	h.node("dev/ttyUSB0", 188, 0)
	h.link("dev/serial/by-id/usb-1a86_USB_Serial-if00-port0", "../../ttyUSB0")
	h.node("dev/bus/usb/001/007", 189, 6)
	// A Pico in BOOTSEL mode: no serial port, only the raw device.
	h.usb("1-4", "2e8a", "0003", "E0C9125B0D9B", "RP2 Boot", 1, 9)
	h.node("dev/bus/usb/001/009", 189, 8)
	// A device nobody asked for.
	h.usb("1-5", "046d", "c52b", "", "Receiver", 1, 11)
	h.node("dev/bus/usb/001/011", 189, 10)
	h.node("dev/ttyS0", 4, 64)
	sc := h.scanner()

	node := func(nodes []Node, path string) Node {
		for _, n := range nodes {
			if n.Path == path {
				return n
			}
		}
		t.Fatalf("no node %s in %v", path, paths(nodes))
		return Node{}
	}

	got := sc.Scan([]Spec{{Serial: "usb-Arduino_*"}})
	if len(got) != 1 || got[0].Path != "/dev/ttyACM0" || got[0].Major != 166 || got[0].Minor != 0 ||
		len(got[0].Links) != 1 || got[0].Links[0] != "/dev/serial/by-id/usb-Arduino__www.arduino.cc__0043_75830333-if00" {
		t.Errorf("by-id: %+v", got)
	}

	got = sc.Scan([]Spec{{USB: "2341:0043"}})
	if want := []string{"/dev/bus/usb/001/005", "/dev/ttyACM0"}; strings.Join(paths(got), " ") != strings.Join(want, " ") {
		t.Errorf("usb: %v, want %v", paths(got), want)
	}
	if n := node(got, "/dev/ttyACM0"); len(n.Links) != 1 || !strings.Contains(n.Device, "2341:0043") {
		t.Errorf("a usb match carries the by-id link of its port: %+v", n)
	}
	if n := node(got, "/dev/bus/usb/001/005"); n.Major != 189 || n.Minor != 4 {
		t.Errorf("raw node: %+v", n)
	}

	// vendor wildcards, and a serial number.
	if got := sc.Scan([]Spec{{USB: "2e8a:*"}}); len(got) != 1 || got[0].Path != "/dev/bus/usb/001/009" {
		t.Errorf("a Pico has only the raw device: %v", paths(got))
	}
	if got := sc.Scan([]Spec{{USB: "2e8a:0003", USBSerial: "E0C9*"}}); len(got) != 1 {
		t.Errorf("serial number: %v", paths(got))
	}
	if got := sc.Scan([]Spec{{USB: "2e8a:0003", USBSerial: "nope*"}}); len(got) != 0 {
		t.Errorf("wrong serial number: %v", paths(got))
	}
	if got := sc.Scan([]Spec{{USB: "1a86:7523"}}); strings.Join(paths(got), " ") != "/dev/bus/usb/001/007 /dev/ttyUSB0" {
		t.Errorf("usb-serial: %v", paths(got))
	}

	// A path, with a glob; nothing else of /dev comes along.
	if got := sc.Scan([]Spec{{Path: "/dev/ttyUSB*"}}); len(got) != 1 || got[0].Path != "/dev/ttyUSB0" || len(got[0].Links) != 1 {
		t.Errorf("path: %+v", got)
	}

	// Several specs, one node each, whichever finds it.
	got = sc.Scan([]Spec{{Serial: "*"}, {USB: "2341:*"}, {Path: "/dev/ttyACM0"}})
	if strings.Join(paths(got), " ") != "/dev/bus/usb/001/005 /dev/ttyACM0 /dev/ttyUSB0" {
		t.Errorf("combined: %v", paths(got))
	}

	// Unplugged: gone.
	h.remove("dev/ttyACM0")
	h.remove("dev/serial/by-id/usb-Arduino__www.arduino.cc__0043_75830333-if00")
	if got := sc.Scan([]Spec{{Serial: "usb-Arduino_*"}}); len(got) != 0 {
		t.Errorf("an unplugged device is gone: %v", paths(got))
	}
	// Nothing at all, no sysfs, no dev: no nodes, no panic.
	if got := (&Scanner{DevRoot: "/nonexistent", SysRoot: "/nonexistent", Stat: sc.Stat}).Scan([]Spec{{Serial: "*"}, {USB: "*:*"}, {Path: "/dev/*"}}); len(got) != 0 {
		t.Errorf("%v", paths(got))
	}
}

func TestCgroupRules(t *testing.T) {
	rules := CgroupRules([]Spec{{Serial: "usb-*"}}, nil)
	if strings.Join(rules, "|") != "c 166:* rwm|c 188:* rwm|c 189:* rwm" {
		t.Errorf("serial: %v", rules)
	}
	rules = CgroupRules([]Spec{{Path: "/dev/fuse"}}, []Node{{Path: "/dev/fuse", Major: 10, Minor: 229}})
	if strings.Join(rules, "|") != "c 10:229 rwm" {
		t.Errorf("path: %v", rules)
	}
	if rules := CgroupRules([]Spec{{Path: "/dev/video0"}}, nil); len(rules) != 0 {
		t.Errorf("a path that is not there has no rule: %v", rules)
	}
	if rules := CgroupRules(nil, nil); len(rules) != 0 {
		t.Errorf("%v", rules)
	}
}

func TestNodePathsAreChecked(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "/dev/../etc/x", "/dev/", "dev/ttyACM0", "/dev/ttyACM0/", "/dev//x", "/dev/null", "/dev/mem", "/dev/pts/3", "/dev/shm/x", "/dev/ptmx", "/dev/tty"} {
		if validNode(bad) == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	for _, ok := range []string{"/dev/ttyACM0", "/dev/bus/usb/001/005", "/dev/ttyUSB12"} {
		if err := validNode(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"/dev/ttyACM0", "/dev/serial/by-id/", "/dev/serial/by-id/a/b", "/dev/serial/by-id/../x", "/etc/x"} {
		if validLink(bad) == nil {
			t.Errorf("link %q must be refused", bad)
		}
	}
	if validLink("/dev/serial/by-id/usb-Arduino_X-if00") != nil {
		t.Error("a by-id name is fine")
	}
	var errb strings.Builder
	for _, args := range [][]string{nil, {"add"}, {"add", "/dev/x", "1", "2", "3"}, {"add", "/dev/x", "a", "2", "3", "4"}, {"zap", "/dev/x"}, {"remove", "/etc/passwd"}, {"add", "/dev/null", "1", "3", "0", "0"}} {
		if RunNodeCommand(args, &errb) == 0 {
			t.Errorf("%v must fail", args)
		}
	}
}

type fakeOps struct {
	mu      sync.Mutex
	calls   []string
	present map[string]bool
	failAdd map[string]error
}

func (f *fakeOps) Add(ctx context.Context, n Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failAdd[n.Path]; err != nil {
		f.calls = append(f.calls, "add-failed "+n.Path)
		return err
	}
	f.calls = append(f.calls, "add "+n.Path)
	f.present[n.Path] = true
	return nil
}

func (f *fakeOps) Remove(ctx context.Context, n Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "remove "+n.Path)
	delete(f.present, n.Path)
	return nil
}

func TestWatcherFollowsTheDevice(t *testing.T) {
	h := newHost(t)
	sc := h.scanner()
	ops := &fakeOps{present: map[string]bool{}, failAdd: map[string]error{}}
	var events []string
	w := &Watcher{Specs: []Spec{{Serial: "usb-Arduino_*"}}, Scan: sc.Scan, Ops: ops,
		Log: func(e Event) { events = append(events, e.Kind+" "+e.Node.Path) }}
	ctx := context.Background()
	calls := func() string { return strings.Join(ops.calls, ", ") }

	w.Tick(ctx)
	if calls() != "" {
		t.Fatalf("nothing is plugged in: %s", calls())
	}

	h.arduino("ttyACM0", 1, 5) // plugged in
	w.Tick(ctx)
	w.Tick(ctx)
	if calls() != "add /dev/ttyACM0" {
		t.Errorf("one add, however often it is looked at: %s", calls())
	}

	// Unplugged.
	h.remove("dev/ttyACM0")
	h.remove("dev/serial/by-id/usb-Arduino__www.arduino.cc__0043_75830333-if00")
	w.Tick(ctx)
	if calls() != "add /dev/ttyACM0, remove /dev/ttyACM0" || len(ops.present) != 0 {
		t.Errorf("unplugged: %s %v", calls(), ops.present)
	}

	// Plugged in again, and the kernel numbered it ttyACM1 this time.
	h.arduino("ttyACM1", 1, 6)
	w.Tick(ctx)
	if !strings.HasSuffix(calls(), "add /dev/ttyACM1") || !ops.present["/dev/ttyACM1"] || ops.present["/dev/ttyACM0"] {
		t.Errorf("replugged under another name: %s %v", calls(), ops.present)
	}

	// Reset in place: same name, new device number. The node is replaced.
	h.node("dev/ttyACM1", 166, 7)
	w.Tick(ctx)
	if !strings.HasSuffix(calls(), "remove /dev/ttyACM1, add /dev/ttyACM1") {
		t.Errorf("a changed node is replaced: %s", calls())
	}
	if want := "attached /dev/ttyACM0|detached /dev/ttyACM0|attached /dev/ttyACM1|detached /dev/ttyACM1|attached /dev/ttyACM1"; strings.Join(events, "|") != want {
		t.Errorf("events: %v", events)
	}
}

func TestWatcherRetriesWhatFailsAndSaysItOnce(t *testing.T) {
	h := newHost(t)
	h.arduino("ttyACM0", 1, 5)
	ops := &fakeOps{present: map[string]bool{}, failAdd: map[string]error{"/dev/ttyACM0": errors.New("container is not running")}}
	var errs []string
	w := &Watcher{Specs: []Spec{{USB: "2341:*"}}, Scan: h.scanner().Scan, Ops: ops, Log: func(e Event) {
		if e.Kind == "error" {
			errs = append(errs, e.Err.Error())
		}
	}}
	for i := 0; i < 5; i++ {
		w.Tick(context.Background())
	}
	if len(errs) != 1 {
		t.Errorf("the same failure is reported once, not %d times", len(errs))
	}
	ops.mu.Lock()
	delete(ops.failAdd, "/dev/ttyACM0") // the container is up now
	ops.mu.Unlock()
	w.Tick(context.Background())
	if !ops.present["/dev/ttyACM0"] {
		t.Error("a failed add must be retried")
	}
}

func TestWatcherNeverTouchesReservedPaths(t *testing.T) {
	h := newHost(t)
	h.node("dev/null", 1, 3)
	h.node("dev/mem", 1, 1)
	ops := &fakeOps{present: map[string]bool{}, failAdd: map[string]error{}}
	w := &Watcher{Specs: []Spec{{Path: "/dev/null"}, {Path: "/dev/mem"}}, Scan: h.scanner().Scan, Ops: ops}
	w.Tick(context.Background())
	if len(ops.calls) != 0 {
		t.Errorf("reserved nodes must be left alone: %v", ops.calls)
	}
}
