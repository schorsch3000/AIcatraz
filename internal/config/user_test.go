// SPDX-License-Identifier: GPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func userSetup(t *testing.T, user, project string) (home, cfgPath string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	home = t.TempDir()
	if user != "" {
		p := UserConfigPath(home)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(user), 0o644)
	}
	proj := t.TempDir()
	cfgPath = filepath.Join(proj, "aicatraz.yaml")
	os.WriteFile(cfgPath, []byte(project), 0o644)
	return home, cfgPath
}

func TestUserConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := UserConfigPath("/home/u"); got != "/home/u/.config/aicatraz/config.yaml" {
		t.Error(got)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := UserConfigPath("/home/u"); got != "/xdg/aicatraz/config.yaml" {
		t.Error(got)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if got := UserConfigPath("/home/u"); got != "/home/u/.config/aicatraz/config.yaml" {
		t.Error(got)
	}
}

func TestUserConfigFillsWhatTheProjectLeavesOut(t *testing.T) {
	home, path := userSetup(t, `
approval: {timeout: 30s, idle: 8s}
bar: {theme: nord, symbol: "!", symbol_width: 1}
limits: {memory: 4g}
env: {static: {EDITOR: vim}}
network:
  default: ask
canary: {action: warn}
`, `
volumes: [".:rw"]
approval: {timeout: 3m}
bar: {fg: white}
`)
	cfg, ps, err := Load(path, Options{Home: home})
	if err != nil || HasErrors(ps) {
		t.Fatalf("%v %+v", err, ps)
	}
	a := cfg.Approval
	if a.TimeoutOrDefault() != 3*time.Minute || a.IdleOrDefault() != 8*time.Second || a.GuardOrDefault() != DefaultApprovalGuard {
		t.Errorf("approval: timeout %v idle %v guard %v", a.TimeoutOrDefault(), a.IdleOrDefault(), a.GuardOrDefault())
	}
	if cfg.Bar.Theme != "nord" || cfg.Bar.FG != "white" || cfg.Bar.SymbolOrDefault() != "!" || cfg.Bar.SymbolWidthOrDefault() != 1 {
		t.Errorf("bar: %+v", cfg.Bar)
	}
	if cfg.Limits.Memory != "4g" || cfg.Env.Static["EDITOR"] != "vim" || cfg.Canary.ActionOrDefault() != "warn" {
		t.Errorf("limits/env/canary: %+v %+v %+v", cfg.Limits, cfg.Env, cfg.Canary)
	}
	if cfg.NetworkMode() != NetFiltered {
		t.Errorf("network mode %v", cfg.NetworkMode())
	}
}

func TestProjectNetworkSectionReplacesTheUsers(t *testing.T) {
	home, path := userSetup(t, "network: {default: ask}\n", "volumes: [\".:rw\"]\nnetwork: off\n")
	cfg, ps, err := Load(path, Options{Home: home})
	if err != nil || HasErrors(ps) {
		t.Fatalf("%v %+v", err, ps)
	}
	if cfg.NetworkMode() != NetOff {
		t.Errorf("an explicit `network: off` must win, got %v", cfg.NetworkMode())
	}
}

func TestUserConfigRefusesWhatBelongsToTheProject(t *testing.T) {
	for _, key := range []string{"volumes: [\"/:rw\"]", "sudo: true", "image: debian:13", "ports: [80]", "project: {id: x}", "rules: []"} {
		home, path := userSetup(t, key+"\n", "volumes: [\".:rw\"]\n")
		_, _, err := Load(path, Options{Home: home})
		if err == nil || !strings.Contains(err.Error(), "belongs in the project") {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestUserConfigProblemsNameTheUserFile(t *testing.T) {
	home, path := userSetup(t, "approval: {idle: soon}\n", "volumes: [\".:rw\"]\n")
	_, _, err := Load(path, Options{Home: home})
	if err == nil || !strings.Contains(err.Error(), "config.yaml") || !strings.Contains(err.Error(), "idle") {
		t.Errorf("%v", err)
	}
	home, path = userSetup(t, "approvall: {}\n", "volumes: [\".:rw\"]\n")
	if _, _, err = Load(path, Options{Home: home}); err == nil {
		t.Error("an unknown key in the user config must be an error")
	}
}

func TestNoUserConfigIsFine(t *testing.T) {
	home, path := userSetup(t, "", "volumes: [\".:rw\"]\n")
	cfg, ps, err := Load(path, Options{Home: home})
	if err != nil || HasErrors(ps) || cfg.Approval.IdleOrDefault() != DefaultApprovalIdle || cfg.NetworkMode() != NetOff {
		t.Errorf("%v %+v", err, ps)
	}
}

func TestApprovalIdleAndGuardAreChecked(t *testing.T) {
	for text, bad := range map[string]bool{
		"approval: {idle: 0}":      false,
		"approval: {idle: 5s}":     false,
		"approval: {idle: 11m}":    true,
		"approval: {idle: -1s}":    true,
		"approval: {guard: 500ms}": false,
		"approval: {guard: 10ms}":  true,
		"approval: {guard: 1m}":    true,
		"approval: {guard: x}":     true,
	} {
		_, ps := Parse([]byte("volumes: [\".:rw\"]\n"+text+"\n"), Options{Home: "/h", Dir: "/d"})
		if HasErrors(ps) != bad {
			t.Errorf("%s: problems %+v", text, ps)
		}
	}
}

func TestBarSymbol(t *testing.T) {
	var b BarStyle
	if b.SymbolOrDefault() != "⚠️" || b.SymbolWidthOrDefault() != 2 {
		t.Errorf("defaults: %q %d", b.SymbolOrDefault(), b.SymbolWidthOrDefault())
	}
	for text, bad := range map[string]bool{
		"bar: {symbol: \"🔔\"}":                  false,
		"bar: {symbol: \"\"}":                   false,
		"bar: {symbol: \"⚠\", symbol_width: 1}": false,
		"bar: {symbol_width: 5}":                true,
		"bar: {symbol_width: -1}":               true,
		"bar: {symbol: \"123456789\"}":          true,
		"bar: {symbol: \"a\\tb\"}":              true,
	} {
		_, ps := Parse([]byte("volumes: [\".:rw\"]\n"+text+"\n"), Options{Home: "/h", Dir: "/d"})
		if HasErrors(ps) != bad {
			t.Errorf("%s: problems %+v", text, ps)
		}
	}
	cfg, _ := Parse([]byte("bar: {symbol: \"\"}\n"), Options{Home: "/h", Dir: "/d"})
	if cfg.Bar.SymbolOrDefault() != "" || cfg.Bar.SymbolWidthOrDefault() != 0 {
		t.Error("an empty symbol means no symbol")
	}
}
