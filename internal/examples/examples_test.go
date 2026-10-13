package examples

import (
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
			files, err := Render(ex.Name, "~/proj")
			if err != nil {
				t.Fatal(err)
			}
			if ex.Description == "" {
				t.Error("every example needs a description")
			}
			cfg, ps := config.Parse([]byte(files[ConfigFile]), config.Options{Home: "/home/u"})
			if cfg == nil || len(ps) != 0 {
				t.Fatalf("problems: %+v\n%s", ps, files[ConfigFile])
			}
			if len(cfg.Resolved) == 0 || cfg.Resolved[0].Path != "/home/u/proj" {
				t.Errorf("project path not substituted: %+v", cfg.Resolved)
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
		files, _ := Render(n, "~/p")
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
	if _, err := Render("nope", "~/p"); err == nil || !strings.Contains(err.Error(), "starter") {
		t.Errorf("%v", err)
	}
}

func TestProjectPath(t *testing.T) {
	for dir, want := range map[string]string{
		"/home/u/git/p": "~/git/p",
		"/srv/work/p":   "/srv/work/p",
		"/home/u/p/":    "~/p",
	} {
		got, err := ProjectPath(dir, "/home/u")
		if err != nil || got != want {
			t.Errorf("%s: %q %v", dir, got, err)
		}
	}
	for _, bad := range []string{"/home/u", "/home", "/"} {
		if _, err := ProjectPath(bad, "/home/u"); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}
