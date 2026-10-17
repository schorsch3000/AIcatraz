// SPDX-License-Identifier: GPL-3.0-only

package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The tool cache holds the agents' executables on the host, installed once and
// mounted read-only into every jail:
//
//	~/.local/share/aicatraz/tools/<tool>/<version>/<binary>
//	~/.local/share/aicatraz/tools/<tool>/current -> <version>
//
// The agents are dynamically linked against glibc, so they run in Debian-based
// images; an Alpine image can't run them.

// ErrNotInstalled means the cache has no (matching) version of the tool.
var ErrNotInstalled = errors.New("not installed")

var (
	versionName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	versionToken = regexp.MustCompile(`\d+(?:\.\d+)+(?:[-+][0-9A-Za-z.]+)?`)
)

const installerImage = "aicatraz/installer:1"

// CacheDir returns the tool cache directory.
func CacheDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "aicatraz", "tools"), nil
}

// Installed is one cached version.
type Installed struct {
	Tool, Version, Dir string
	Current            bool
}

// List returns every cached version, grouped by tool.
func List(cache string) ([]Installed, error) {
	tools, err := os.ReadDir(cache)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Installed
	for _, t := range tools {
		if !t.IsDir() {
			continue
		}
		toolDir := filepath.Join(cache, t.Name())
		cur, _ := os.Readlink(filepath.Join(toolDir, "current"))
		versions, _ := os.ReadDir(toolDir)
		for _, v := range versions {
			if !v.IsDir() || strings.HasPrefix(v.Name(), ".") {
				continue
			}
			out = append(out, Installed{Tool: t.Name(), Version: v.Name(), Dir: filepath.Join(toolDir, v.Name()), Current: v.Name() == cur})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tool != out[j].Tool {
			return out[i].Tool < out[j].Tool
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// Resolve finds the directory holding the tool's executable for a version
// ("" or "latest" means the current one).
func Resolve(cache string, p *Profile, version string) (dir, resolved string, err error) {
	toolDir := filepath.Join(cache, p.Name)
	if version == "" || version == "latest" {
		cur, lerr := os.Readlink(filepath.Join(toolDir, "current"))
		if lerr != nil {
			return "", "", fmt.Errorf("%s: %w", p.Name, ErrNotInstalled)
		}
		version = cur
	}
	if !versionName.MatchString(version) {
		return "", "", fmt.Errorf("invalid version %q", version)
	}
	dir = filepath.Join(toolDir, version)
	if st, serr := os.Stat(filepath.Join(dir, p.Binary)); serr != nil || !st.Mode().IsRegular() {
		return "", "", fmt.Errorf("%s %s: %w", p.Name, version, ErrNotInstalled)
	}
	return dir, version, nil
}

// VersionOf runs `binary --version` and extracts the version number.
func VersionOf(binary string) (string, error) {
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", binary, err)
	}
	return ParseVersion(string(out))
}

// ParseVersion extracts the first version number from `--version` output.
func ParseVersion(out string) (string, error) {
	if v := versionToken.FindString(out); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no version number in %q", strings.TrimSpace(out))
}

// publish moves a freshly installed directory into place and makes it current.
func publish(cache string, p *Profile, tmp, version string) (string, error) {
	if !versionName.MatchString(version) {
		return "", fmt.Errorf("invalid version %q", version)
	}
	toolDir := filepath.Join(cache, p.Name)
	final := filepath.Join(toolDir, version)
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	link := filepath.Join(toolDir, "current")
	tmpLink := link + ".new"
	os.Remove(tmpLink)
	if err := os.Symlink(version, tmpLink); err != nil {
		return "", err
	}
	if err := os.Rename(tmpLink, link); err != nil {
		return "", err
	}
	return final, nil
}

func newTmp(cache string, p *Profile) (string, error) {
	toolDir := filepath.Join(cache, p.Name)
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(toolDir, ".install-")
}

// InstallFromHost copies the executable found on the host's PATH into the
// cache. For people who already have the tool; it needs no network.
func InstallFromHost(cache string, p *Profile) (version string, err error) {
	found, err := exec.LookPath(p.Binary)
	if err != nil {
		return "", fmt.Errorf("%s is not on PATH", p.Binary)
	}
	real, err := filepath.EvalSymlinks(found)
	if err != nil {
		return "", err
	}
	version, err = VersionOf(real)
	if err != nil {
		return "", err
	}
	tmp, err := newTmp(cache, p)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := copyExecutable(real, filepath.Join(tmp, p.Binary)); err != nil {
		return "", err
	}
	if _, err := publish(cache, p, tmp, version); err != nil {
		return "", err
	}
	return version, nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// InstallScript is what runs in the throwaway container: the official
// installer, then the executable is copied out and asked for its version.
const installScript = `set -e
mkdir -p "$HOME"
curl -fsSL "$AZ_URL" | bash -s -- $AZ_ARGS
cp -L "$HOME/$AZ_BIN" "/out/$AZ_NAME"
chmod 755 "/out/$AZ_NAME"
"/out/$AZ_NAME" --version > /out/VERSION
`

const installerDockerfile = `FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates bash && rm -rf /var/lib/apt/lists/*
`

// InstallInContainer runs the official installer in a throwaway Debian
// container, so the downloaded script never touches the host, and caches the
// resulting executable. version "" or "latest" installs the newest.
func InstallInContainer(cache string, p *Profile, version string, uid, gid int, log io.Writer) (string, error) {
	if version != "" && version != "latest" && !versionName.MatchString(version) {
		return "", fmt.Errorf("invalid version %q", version)
	}
	if exec.Command("docker", "image", "inspect", installerImage).Run() != nil {
		build := exec.Command("docker", "build", "-t", installerImage, "-")
		build.Stdin = strings.NewReader(installerDockerfile)
		build.Stdout, build.Stderr = log, log
		if err := build.Run(); err != nil {
			return "", fmt.Errorf("building the installer image: %w", err)
		}
	}
	tmp, err := newTmp(cache, p)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	run := exec.Command("docker", "run", "--rm",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
		"-e", "HOME=/tmp/h",
		"-e", "AZ_URL="+p.Install.URL,
		"-e", "AZ_ARGS="+p.Install.VersionArgs(version),
		"-e", "AZ_BIN="+p.Install.BinaryPath,
		"-e", "AZ_NAME="+p.Binary,
		"--tmpfs", "/tmp:exec,mode=1777,size=2g",
		"-v", tmp+":/out",
		installerImage, "bash", "-c", installScript)
	run.Stdout, run.Stderr = log, log
	if err := run.Run(); err != nil {
		return "", fmt.Errorf("installing %s: %w", p.Name, err)
	}
	vdata, err := os.ReadFile(filepath.Join(tmp, "VERSION"))
	if err != nil {
		return "", err
	}
	v, err := ParseVersion(string(vdata))
	if err != nil {
		return "", err
	}
	os.Remove(filepath.Join(tmp, "VERSION"))
	if _, err := publish(cache, p, tmp, v); err != nil {
		return "", err
	}
	return v, nil
}
