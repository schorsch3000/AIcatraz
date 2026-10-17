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
			files, err := Render(ex.Name)
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
		files, _ := Render(n)
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
	if _, err := Render("nope"); err == nil || !strings.Contains(err.Error(), "starter") {
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
