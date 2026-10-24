// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/osrelease"
)

// SudoCaps are the capabilities a jail with `sudo: true` keeps, so that root
// (reached through sudo) can install packages and manage files. The rest of
// the set, including everything that touches mounts, namespaces, devices or
// the network stack, stays dropped.
var SudoCaps = []string{
	"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETUID", "SETGID",
	"SETFCAP", "KILL", "AUDIT_WRITE", "SYS_CHROOT",
}

// SudoRef is the tag of the image that adds sudo to the project's image.
func SudoRef(id string) string { return "aicatraz/" + id + ":sudo" }

// installers maps a distribution ID to the command that installs sudo. The
// twelve distributions `init` writes a Dockerfile.az for are covered; others have to
// bring sudo themselves.
var installers = map[string]string{
	"debian":              "apt-get update && apt-get install -y --no-install-recommends sudo && rm -rf /var/lib/apt/lists/*",
	"ubuntu":              "apt-get update && apt-get install -y --no-install-recommends sudo && rm -rf /var/lib/apt/lists/*",
	"fedora":              "dnf install -y sudo && dnf clean all",
	"rocky":               "dnf install -y sudo && dnf clean all",
	"almalinux":           "dnf install -y sudo && dnf clean all",
	"centos":              "dnf install -y sudo && dnf clean all",
	"rhel":                "(dnf install -y sudo || microdnf install -y sudo || yum install -y sudo) && (dnf clean all || microdnf clean all || yum clean all || true)",
	"amzn":                "(dnf install -y sudo || yum install -y sudo) && (dnf clean all || yum clean all || true)",
	"alpine":              "apk add --no-cache sudo",
	"arch":                "pacman -Sy --noconfirm sudo && rm -rf /var/cache/pacman/pkg/*",
	"opensuse-leap":       "zypper --non-interactive install sudo && zypper clean --all",
	"opensuse-tumbleweed": "zypper --non-interactive install sudo && zypper clean --all",
	// IDs that only show up as ID_LIKE.
	"suse": "zypper --non-interactive install sudo && zypper clean --all",
}

// sudoInstaller finds the install command for a distribution, going by its
// ID and then by the IDs it is like.
func sudoInstaller(rel map[string]string) (string, bool) {
	for _, id := range osrelease.Like(rel) {
		if cmd, ok := installers[id]; ok {
			return cmd, true
		}
	}
	return "", false
}

// SudoDockerfile is the Dockerfile of the sudo layer. install is the command
// that installs sudo, or "" if the image has it already. The sudoers entry
// names the host user by number, because the user's name only exists at run
// time (the passwd file is generated), and goes into a drop-in file that sudo
// accepts as long as root owns it and nobody else can write it.
func SudoDockerfile(base string, uid int, install string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\nUSER root\n", base)
	if install != "" {
		fmt.Fprintf(&b, "RUN command -v sudo >/dev/null 2>&1 || { %s; }\n", install)
	}
	fmt.Fprintf(&b, "RUN mkdir -p /etc/sudoers.d && "+
		"printf '#%%d ALL=(ALL:ALL) NOPASSWD: ALL\\n' %d > /etc/sudoers.d/aicatraz && "+
		"chmod 0440 /etc/sudoers.d/aicatraz && "+
		"{ grep -qs '^[#@]includedir /etc/sudoers.d' /etc/sudoers || echo '#includedir /etc/sudoers.d' >> /etc/sudoers; }\n", uid)
	// The jail's passwd file is generated and there is no shadow entry for the
	// user, so PAM's account check would refuse them ("account validation
	// failure"). Who may use sudo is decided by sudoers alone.
	b.WriteString("RUN for f in sudo sudo-i; do [ -d /etc/pam.d ] && " +
		"printf 'auth sufficient pam_permit.so\\naccount sufficient pam_permit.so\\nsession sufficient pam_permit.so\\n' > /etc/pam.d/$f; done; true\n")
	return b.String()
}

// probeSudo asks the image whether it has sudo and which distribution it is.
func probeSudo(docker, image string) (hasSudo bool, rel map[string]string, err error) {
	script := `if command -v sudo >/dev/null 2>&1; then echo AICATRAZ_HAS_SUDO; fi; cat /etc/os-release 2>/dev/null || cat /usr/lib/os-release 2>/dev/null; true`
	out, err := exec.Command(docker, "run", "--rm", "--user", "0", "--entrypoint", "sh", image, "-c", script).Output()
	if err != nil {
		return false, nil, fmt.Errorf("cannot inspect image %s (it needs a shell for `sudo: true`): %w", image, err)
	}
	text := string(out)
	hasSudo = strings.Contains(text, "AICATRAZ_HAS_SUDO")
	return hasSudo, osrelease.Parse(strings.NewReader(text)), nil
}

// BuildSudoImage builds tag from base: sudo installed if it is missing, and
// the host user allowed to use it without a password.
func BuildSudoImage(base, tag string, uid int, noCache bool, out io.Writer) error {
	hasSudo, rel, err := probeSudo("docker", base)
	if err != nil {
		return err
	}
	install := ""
	if !hasSudo {
		var ok bool
		if install, ok = sudoInstaller(rel); !ok {
			name := rel["PRETTY_NAME"]
			if name == "" {
				name = "this distribution"
			}
			return fmt.Errorf("`sudo: true` needs sudo in the image, and aicatraz can't install it on %s; "+
				"install sudo in your Dockerfile.az (aicatraz adds the sudoers entry itself)", name)
		}
	}
	args := []string{"build", "-t", tag, "-"}
	if noCache {
		args = []string{"build", "--no-cache", "-t", tag, "-"}
	}
	cmd := exec.Command("docker", args...)
	cmd.Stdin = strings.NewReader(SudoDockerfile(base, uid, install))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building the sudo image failed: %w", err)
	}
	return nil
}
