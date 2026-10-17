// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"slices"
	"strings"
	"testing"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/osrelease"
)

func TestRunArgsWithSudo(t *testing.T) {
	plain, err := RunArgs(testInput(t, "volumes:\n  - ~/p:rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plain, "--read-only") || !slices.Contains(plain, "--security-opt=no-new-privileges") {
		t.Error("without sudo the root filesystem is read-only and no-new-privileges is on")
	}
	for _, a := range plain {
		if strings.HasPrefix(a, "--cap-add") {
			t.Errorf("no capabilities without sudo, got %s", a)
		}
	}

	args, err := RunArgs(testInput(t, "sudo: true\nvolumes:\n  - ~/p:rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "--read-only") || slices.Contains(args, "--security-opt=no-new-privileges") {
		t.Error("sudo needs a writable root filesystem and setuid to work")
	}
	if !slices.Contains(args, "--cap-drop=ALL") {
		t.Error("everything is still dropped first")
	}
	for _, c := range SudoCaps {
		if !slices.Contains(args, "--cap-add="+c) {
			t.Errorf("missing capability %s", c)
		}
	}
	for _, bad := range []string{"SYS_ADMIN", "NET_ADMIN", "NET_RAW", "SYS_PTRACE", "MKNOD", "DAC_READ_SEARCH"} {
		if slices.Contains(SudoCaps, bad) {
			t.Errorf("%s must not be granted", bad)
		}
	}
}

func TestSudoDockerfile(t *testing.T) {
	df := SudoDockerfile("debian:13", 1000, "apt-get install -y sudo")
	for _, want := range []string{
		"FROM debian:13\nUSER root\n",
		"command -v sudo >/dev/null 2>&1 || { apt-get install -y sudo; }",
		"printf '#%d ALL=(ALL:ALL) NOPASSWD: ALL\\n' 1000 > /etc/sudoers.d/aicatraz",
		"chmod 0440 /etc/sudoers.d/aicatraz",
		"includedir /etc/sudoers.d",
		"pam_permit.so",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, df)
		}
	}
	if strings.Contains(SudoDockerfile("x", 1, ""), "command -v sudo") {
		t.Error("no install step when the image has sudo")
	}
}

func TestSudoInstaller(t *testing.T) {
	for rel, want := range map[string]string{
		"ID=debian": "apt-get",
		"ID=ubuntu": "apt-get",
		"ID=linuxmint\nID_LIKE=\"ubuntu debian\"": "apt-get",
		"ID=fedora":                          "dnf",
		"ID=rocky":                           "dnf",
		"ID=almalinux":                       "dnf",
		"ID=centos":                          "dnf",
		"ID=rhel":                            "microdnf",
		"ID=amzn":                            "yum",
		"ID=alpine":                          "apk add",
		"ID=arch":                            "pacman",
		"ID=opensuse-leap":                   "zypper",
		"ID=opensuse-tumbleweed":             "zypper",
		"ID=sles\nID_LIKE=\"suse opensuse\"": "zypper",
	} {
		cmd, ok := sudoInstaller(osrelease.Parse(strings.NewReader(rel)))
		if !ok || !strings.Contains(cmd, want) {
			t.Errorf("%q: got %q (ok=%v), want %q", rel, cmd, ok, want)
		}
	}
	if _, ok := sudoInstaller(osrelease.Parse(strings.NewReader("ID=plan9"))); ok {
		t.Error("unknown distribution must not get an installer")
	}
	if _, ok := sudoInstaller(nil); ok {
		t.Error("no os-release must not get an installer")
	}
}

func TestRunArgsNetworkModes(t *testing.T) {
	network := func(yaml string) []string {
		args, err := RunArgs(testInput(t, yaml))
		if err != nil {
			t.Fatal(err)
		}
		return args
	}
	// No section, or `off`: the jail has no network at all.
	for _, yaml := range []string{"volumes:\n  - ~/p:rw\n", "network: off\nvolumes:\n  - ~/p:rw\n"} {
		args := network(yaml)
		if !has(args, "--network", "none") || !slices.Contains(args, "--hostname") {
			t.Errorf("%q: the network must be none: %v", yaml, args)
		}
	}
	// open: Docker's normal network, left alone.
	if args := network("network: open\nvolumes:\n  - ~/p:rw\n"); slices.Contains(args, "--network") || !slices.Contains(args, "--hostname") {
		t.Errorf("open must not touch the network: %v", args)
	}
	// filtered: the sidecar's network, and no hostname of its own.
	in := testInput(t, "network: {}\nvolumes:\n  - ~/p:rw\n")
	in.NetworkContainer = "sidecar-1"
	args, err := RunArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	if !has(args, "--network", "container:sidecar-1") || slices.Contains(args, "--hostname") || has(args, "--network", "none") {
		t.Errorf("filtered joins the sidecar: %v", args)
	}
}

func TestRunArgsPublishedPorts(t *testing.T) {
	// With Docker's normal network the jail publishes its own ports.
	args, err := RunArgs(testInput(t, "network: open\nports: [3000, \"0.0.0.0:8080:80\"]\nvolumes:\n  - ~/p:rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !has(args, "-p", "127.0.0.1:3000:3000/tcp") || !has(args, "-p", "0.0.0.0:8080:80/tcp") {
		t.Errorf("ports must be published: %v", args)
	}
	// With rules the sidecar owns the network and publishes them; the jail
	// can't (Docker refuses -p together with a shared network).
	in := testInput(t, "network: {}\nports: [3000]\nvolumes:\n  - ~/p:rw\n")
	in.NetworkContainer = "sidecar-1"
	args, err = RunArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "-p") {
		t.Errorf("the jail must not publish ports when it shares the sidecar's network: %v", args)
	}
	side := egressRunArgs("sidecar-1", "proj", 1, "/bin/aicatraz", "/run/s.sock", "/run/r.nft", []string{"9.9.9.9"}, in.Cfg.PortMaps)
	if !has(side, "-p", "127.0.0.1:3000:3000/tcp") {
		t.Errorf("the sidecar publishes the ports: %v", side)
	}
	if slices.Index(side, "-p") > slices.Index(side, "--entrypoint") {
		t.Errorf("options must come before the image: %v", side)
	}
}

func TestRunArgsDeviceRules(t *testing.T) {
	in := testInput(t, "devices: [{serial: 'usb-*'}]\nvolumes:\n  - ~/p:rw\n")
	in.DeviceRules = []string{"c 166:* rwm", "c 189:* rwm"}
	args, err := RunArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	if !has(args, "--device-cgroup-rule", "c 166:* rwm") || !has(args, "--device-cgroup-rule", "c 189:* rwm") {
		t.Errorf("rules missing: %v", args)
	}
	if slices.Contains(args, "--device") || slices.Contains(args, "--privileged") {
		t.Errorf("the jail gets rules, never a device or privileges: %v", args)
	}
	for _, c := range []string{"MKNOD", "SYS_ADMIN"} {
		if slices.Contains(args, "--cap-add="+c) {
			t.Errorf("%s must not be added", c)
		}
	}
	if args, _ := RunArgs(testInput(t, "volumes:\n  - ~/p:rw\n")); slices.Contains(args, "--device-cgroup-rule") {
		t.Error("no devices, no rules")
	}
}

func TestBarColorsPrecedence(t *testing.T) {
	cfg := func(yaml string) *config.Config {
		c, ps := config.Parse([]byte(yaml), config.Options{Home: "/home/u", Dir: "/p"})
		if config.HasErrors(ps) {
			t.Fatalf("%+v", ps)
		}
		return c
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	want := func(o Options, c *config.Config, e map[string]string, fg, bg string) {
		t.Helper()
		got, err := barColors(o, c, env(e))
		if err != nil || got.FG != fg || got.BG != bg {
			t.Errorf("got %+v %v, want fg %q bg %q", got, err, fg, bg)
		}
	}
	none := cfg("volumes: []\n")
	project := cfg("bar: {theme: brick}\n")
	partial := cfg("bar: {bg: '#102030'}\n")
	envs := map[string]string{BarThemeEnv: "plum", BarFGEnv: "yellow"}

	want(Options{}, none, nil, "38;5;231", "48;5;22")                 // the default: forest
	want(Options{}, none, envs, "33", "48;5;53")                      // the user's environment
	want(Options{}, project, envs, "33", "48;5;88")                   // the project's theme beats the user's theme; the user's text color is still used
	want(Options{BarTheme: "amber"}, project, envs, "33", "48;5;214") // a flag beats the project
	want(Options{BarFG: "red", BarBG: "blue"}, project, envs, "31", "44")
	want(Options{}, partial, map[string]string{BarThemeEnv: "plum"}, "38;5;231", "48;2;16;32;48") // one color from the project, the rest from the theme
	if _, err := barColors(Options{BarTheme: "nope"}, none, env(nil)); err == nil {
		t.Error("an unknown theme is an error")
	}
}
