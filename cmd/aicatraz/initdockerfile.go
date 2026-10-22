// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
	"github.com/schorsch3000/aicatraz/internal/osrelease"
)

// osReleasePath is where the running distribution describes itself.
var osReleasePath = "/etc/os-release"

const dockerfileName = "Dockerfile.az"

// ignoreFileName is the ignore file docker reads for it (BuildKit).
const ignoreFileName = dockerfileName + ".dockerignore"

// fallbackImage is what the Dockerfile.az is based on when the distribution
// of this machine is unknown; it is also what a project without one gets.
const fallbackImage = "debian:stable-slim"

// generatedDockerfile returns a short Dockerfile.az whose base image matches
// the distribution this machine runs, with a line that upgrades its packages.
// note is a hint worth showing. An unknown distribution gets fallbackImage.
func generatedDockerfile() (content, image, note string, err error) {
	rel := map[string]string{}
	if f, oerr := os.Open(osReleasePath); oerr == nil {
		rel = osrelease.Parse(f)
		f.Close()
	}
	image, note, ok := baseImage(rel)
	if !ok {
		image, note = fallbackImage, "could not match this distribution to a base image; "+dockerfileName+" is based on "+fallbackImage
		rel = map[string]string{"ID": "debian"}
	}
	upgrade, err := upgradeLines(rel)
	if err != nil {
		return "", "", "", err
	}
	return "FROM " + image + "\n" + upgrade, image, note, nil
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

// upgradeLines returns the lines that follow FROM: the user config's
// init.dockerfile.lines if it sets them, else the distribution's upgrade line.
func upgradeLines(rel map[string]string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return upgradeLine(rel), nil
	}
	user, err := config.LoadUser(home)
	if err != nil {
		return "", err
	}
	if user == nil || user.Init.Dockerfile.Lines == nil {
		return upgradeLine(rel), nil
	}
	lines := *user.Init.Dockerfile.Lines
	if lines != "" && !strings.HasSuffix(lines, "\n") {
		lines += "\n"
	}
	return lines, nil
}

// upgradeLine returns the RUN line that upgrades the image's packages, ending
// in a commented-out install command to extend. The package manager is the one
// of the distribution baseImage chose.
func upgradeLine(rel map[string]string) string {
	switch rel["ID"] {
	case "fedora", "rocky", "almalinux", "centos", "rhel":
		return "RUN dnf upgrade -y # && dnf install -y\n"
	case "amzn":
		if rel["VERSION_ID"] == "2" { // Amazon Linux 2 predates dnf
			return "RUN yum update -y # && yum install -y\n"
		}
		return "RUN dnf upgrade -y # && dnf install -y\n"
	case "alpine":
		return "RUN apk update && apk upgrade # && apk add\n"
	case "arch":
		return "RUN pacman -Syu --noconfirm # && pacman -S --noconfirm\n"
	case "opensuse-leap", "opensuse-tumbleweed":
		return "RUN zypper --non-interactive update # && zypper --non-interactive install\n"
	}
	return "RUN apt update && apt upgrade -y # && apt install -y\n"
}
