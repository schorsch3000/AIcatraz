// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"slices"
	"strings"
	"testing"

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
