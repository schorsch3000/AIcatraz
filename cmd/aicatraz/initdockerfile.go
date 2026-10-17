// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/osrelease"
	"github.com/schorsch3000/aicatraz/internal/ui"
)

// osReleasePath is where the running distribution describes itself.
var osReleasePath = "/etc/os-release"

const dockerfileName = "Dockerfile.az"

// runInitDockerfile writes a one-line Dockerfile.az whose base image matches
// the distribution this machine runs.
func runInitDockerfile(args []string, stdout, stderr io.Writer) int {
	force := false
	for _, a := range args {
		if a != "--force" {
			fmt.Fprint(stderr, styledUsage(ui.For(stderr)))
			return 2
		}
		force = true
	}
	st := ui.For(stdout)

	f, err := os.Open(osReleasePath)
	if err != nil {
		ui.Error(stderr, "cannot detect the distribution (%v); write a `FROM <image>` line into %s yourself", err, dockerfileName)
		return 1
	}
	defer f.Close()
	image, note, ok := baseImage(osrelease.Parse(f))
	if !ok {
		ui.Error(stderr, "no matching base image for this distribution; write a `FROM <image>` line into %s yourself", dockerfileName)
		return 1
	}

	if _, err := os.Lstat(dockerfileName); err == nil && !force {
		ui.Error(stderr, "%s already exists; nothing written (use --force to overwrite)", dockerfileName)
		return 1
	}
	if err := os.WriteFile(dockerfileName, []byte("FROM "+image+"\n"), 0o644); err != nil {
		ui.Error(stderr, "%v", err)
		return 2
	}
	fmt.Fprintf(stdout, "%s %s  (FROM %s)\n", st.BoldGreen("wrote"), dockerfileName, image)
	if note != "" {
		ui.Note(stderr, "%s", note)
	}
	return 0
}

// major returns the part of a version before the first dot.
func major(v string) string { return strings.SplitN(v, ".", 2)[0] }

// majorMinor returns "3.20" for "3.20.3".
func majorMinor(v string) string {
	p := strings.SplitN(v, ".", 3)
	if len(p) < 2 {
		return v
	}
	return p[0] + "." + p[1]
}

// baseImage maps an os-release description to a Docker image for the twelve
// most common distributions. note is a hint worth showing; ok is false for
// anything else.
func baseImage(rel map[string]string) (image, note string, ok bool) {
	id, ver := rel["ID"], rel["VERSION_ID"]
	withVer := func(name, v string) string {
		if v == "" {
			return name + ":latest"
		}
		return name + ":" + v
	}
	switch id {
	case "debian":
		if ver == "" { // testing or unstable carry no version
			return "debian:testing", "", true
		}
		return "debian:" + major(ver), "", true
	case "ubuntu":
		return withVer("ubuntu", ver), "", true
	case "fedora":
		return withVer("fedora", ver), "", true
	case "alpine":
		return withVer("alpine", majorMinor(ver)),
			"Alpine uses musl: the jail works, but the claude and opencode agents need a glibc image", true
	case "rocky":
		return withVer("rockylinux", major(ver)), "", true
	case "almalinux":
		return withVer("almalinux", major(ver)), "", true
	case "centos":
		return "quay.io/centos/centos:stream" + major(ver), "", true
	case "rhel":
		return "registry.access.redhat.com/ubi" + major(ver) + "/ubi", "", true
	case "arch":
		return "archlinux:latest", "", true
	case "opensuse-leap":
		return withVer("opensuse/leap", ver), "", true
	case "opensuse-tumbleweed":
		return "opensuse/tumbleweed:latest", "", true
	case "amzn":
		return withVer("amazonlinux", ver), "", true
	}
	// Derivatives: use the release they are built on.
	like := " " + rel["ID_LIKE"] + " "
	switch {
	case rel["UBUNTU_CODENAME"] != "":
		return "ubuntu:" + rel["UBUNTU_CODENAME"], fmt.Sprintf("%s is based on Ubuntu %s", rel["NAME"], rel["UBUNTU_CODENAME"]), true
	case strings.Contains(like, " debian "):
		return "debian:stable", fmt.Sprintf("%s is based on Debian; using the current stable release", rel["NAME"]), true
	}
	return "", "", false
}
