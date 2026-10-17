// SPDX-License-Identifier: GPL-3.0-only

package examples

import (
	"regexp"
	"strings"
	"testing"

	"github.com/schorsch3000/aicatraz/internal/config"
)

func TestEveryExampleIsAValidConfigWithoutWarnings(t *testing.T) {
	list := List()
	if len(list) < 5 || list[0].Name != "starter" {
		t.Fatalf("examples: %+v", list)
	}
	for _, ex := range list {
		t.Run(ex.Name, func(t *testing.T) {
			files, err := Render(ex.Name, "/home/u/demo")
			if err != nil {
				t.Fatal(err)
			}
			if ex.Description == "" {
				t.Error("every example needs a description")
			}
			cfg, ps := config.Parse([]byte(files[ConfigFile]), config.Options{Home: "/home/u", Dir: "/srv/checkout/proj"})
			if cfg == nil || len(ps) != 0 {
				t.Fatalf("problems: %+v\n%s", ps, files[ConfigFile])
			}
			if len(cfg.Resolved) == 0 || cfg.Resolved[0].Path != "/srv/checkout/proj" {
				t.Errorf("project not resolved relative to the config: %+v", cfg.Resolved)
			}
			for name, content := range files {
				if strings.Contains(content, "{{") {
					t.Errorf("%s: unreplaced placeholder", name)
				}
			}
			if ex.Files[0] != ConfigFile || len(ex.Files) != len(files) {
				t.Errorf("file list %v vs %d rendered", ex.Files, len(files))
			}
		})
	}
}

func TestAgentExamplesShipADockerfile(t *testing.T) {
	for _, n := range []string{"claude", "opencode"} {
		files, _ := Render(n, "/home/u/demo")
		df, ok := files["Dockerfile.az"]
		if !ok || !strings.Contains(df, "FROM debian") || !strings.Contains(df, "nodejs") {
			t.Errorf("%s: Dockerfile.az missing or not glibc-based with node:\n%s", n, df)
		}
		if !strings.Contains(files[ConfigFile], "tool: "+n) {
			t.Errorf("%s: wrong agent in config", n)
		}
	}
}

func TestRenderUnknown(t *testing.T) {
	if _, err := Render("nope", "/x"); err == nil || !strings.Contains(err.Error(), "starter") {
		t.Errorf("%v", err)
	}
}

func TestCheckProjectDir(t *testing.T) {
	for _, ok := range []string{"/home/u/git/p", "/srv/work/p", "/home/u/p/"} {
		if err := CheckProjectDir(ok, "/home/u"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/home/u", "/home", "/"} {
		if err := CheckProjectDir(bad, "/home/u"); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

// The starter config documents every option in comments. Uncommented, those
// examples must be valid and free of linter warnings, so they can't rot.
func TestStarterCommentedOptionsAreValid(t *testing.T) {
	files, err := Render("starter", "/home/u/demo")
	if err != nil {
		t.Fatal(err)
	}
	keys := regexp.MustCompile(`^# (project|image|env|limits|approval|canary|global|network|ports|devices|bar|rules|agent):`)
	// The image has two alternative forms: check each on its own.
	for _, drop := range []func(string) bool{
		func(l string) bool { return strings.HasPrefix(l, "image: debian") },
		func(l string) bool {
			return strings.HasPrefix(l, "image:  ") || strings.HasPrefix(l, "  dockerfile") || strings.HasPrefix(l, "  context")
		},
	} {
		var out []string
		on := false
		for _, l := range strings.Split(files[ConfigFile], "\n") {
			if l == "network: off" { // the active line; the commented mapping stands in for it
				continue
			}
			switch {
			case keys.MatchString(l):
				on = true
			case strings.TrimSpace(l) == "":
				on = false
			}
			if on {
				l = strings.TrimPrefix(strings.TrimPrefix(l, "#"), " ")
				if drop(l) {
					continue
				}
			}
			out = append(out, l)
		}
		text := strings.Join(out, "\n")
		for _, key := range []string{"project:", "env:", "limits:", "approval:", "canary:", "global:", "network:", "ports:", "devices:", "bar:", "rules:", "agent:"} {
			if !strings.Contains("\n"+text, "\n"+key) {
				t.Errorf("option %s is not documented in the starter config", key)
			}
		}
		cfg, ps := config.Parse([]byte(text), config.Options{Home: "/home/u", Dir: "/srv/p"})
		if cfg == nil || len(ps) != 0 {
			t.Errorf("uncommented starter has problems: %+v\n%s", ps, text)
		}
	}
}

func TestProjectNameFromTheDirectory(t *testing.T) {
	for dir, want := range map[string]string{
		"/foo/bar/baz":                   "baz",
		"/foo/bar/baz/":                  "baz",
		"/home/u/My Project":             "My-Project",
		"/x/--weird__":                   "weird",
		"/x/a  b!!c":                     "a-b-c",
		"/x/.hidden":                     "hidden",
		"/x/v1.2_beta-3":                 "v1.2_beta-3",
		"/":                              "my-project",
		"/x/!!!":                         "my-project",
		"/x/ünï":                         "n", // only ASCII letters and digits are kept
		"/x/" + strings.Repeat("a", 100): strings.Repeat("a", 64),
	} {
		if got := ProjectName(dir); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", dir, got, want)
		}
	}
	files, _ := Render("starter", "/foo/bar/baz")
	if !strings.Contains(files[ConfigFile], "#   id: baz\n") || strings.Contains(files[ConfigFile], "{{") {
		t.Errorf("the starter's example id should be the directory's name:\n%s", files[ConfigFile])
	}
}
