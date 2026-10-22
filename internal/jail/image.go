// SPDX-License-Identifier: GPL-3.0-only

package jail

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/schorsch3000/aicatraz/internal/config"
)

const (
	defaultDockerfile = "Dockerfile.az"
	defaultBaseImage  = "debian:stable-slim"
)

// Build describes a `docker build`: absolute paths.
type Build struct {
	Dockerfile string
	Context    string
}

// Args returns the arguments for `docker build`.
func (b Build) Args(tag string) []string {
	return []string{"build", "-f", b.Dockerfile, "-t", tag, b.Context}
}

// Image is the image a container starts from, and how to get it.
type Image struct {
	Ref   string // what `docker run` receives
	Build *Build // non-nil if the image has to be built first
}

// ResolveImage decides which image to use: an explicit reference; an explicit
// or default (.az/Dockerfile.az, or Dockerfile.az in the project directory) build; or a built-in Debian.
func ResolveImage(cfg *config.Config, cfgDir, id string) (Image, error) {
	spec := cfg.Image
	if spec.Ref != "" {
		return Image{Ref: spec.Ref}, nil
	}

	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = filepath.Join(config.AzDir, defaultDockerfile)
		if _, err := os.Stat(filepath.Join(cfgDir, dockerfile)); err != nil {
			dockerfile = defaultDockerfile // projects from before .az
			if _, err := os.Stat(filepath.Join(cfgDir, dockerfile)); err != nil && spec.Context == "" {
				return Image{Ref: defaultBaseImage}, nil
			}
		}
	}
	b := &Build{
		Dockerfile: absFrom(cfgDir, dockerfile),
		Context:    absFrom(cfgDir, orDefault(spec.Context, ".")),
	}
	if st, err := os.Stat(b.Dockerfile); err != nil || st.IsDir() {
		return Image{}, fmt.Errorf("Dockerfile %s not found", b.Dockerfile)
	}
	if st, err := os.Stat(b.Context); err != nil || !st.IsDir() {
		return Image{}, fmt.Errorf("build context %s is not a directory", b.Context)
	}
	return Image{Ref: "aicatraz/" + id + ":latest", Build: b}, nil
}

func absFrom(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
