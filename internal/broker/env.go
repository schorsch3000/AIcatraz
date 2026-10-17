// SPDX-License-Identifier: GPL-3.0-only

package broker

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/schorsch3000/aicatraz/internal/config"
)

var termValue = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// baseEnv names the variables a host command always gets from the broker's
// own environment (never from the container).
func isBaseEnv(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "LANG", "LANGUAGE", "TZ":
		return true
	}
	return strings.HasPrefix(name, "LC_")
}

// BuildEnv builds the environment of a host command from scratch:
//
//  1. a small base set from the broker's own environment (host) — PATH is
//     the broker's, never the container's;
//  2. TERM from the request, if it looks sane;
//  3. the rule's env_pass names, with the values the container sent;
//  4. minus global.env_strip, and minus the hardcoded never-pass set.
//
// A variable that isn't named anywhere is not passed.
func BuildEnv(host, requested []string, rule *config.Rule, envStrip []string) (env []string, passed []string) {
	out := map[string]string{}

	for _, kv := range host {
		k, v, ok := strings.Cut(kv, "=")
		if ok && isBaseEnv(k) {
			out[k] = v
		}
	}

	reqMap := map[string]string{}
	for _, kv := range requested {
		if k, v, ok := strings.Cut(kv, "="); ok {
			reqMap[k] = v
		}
	}
	if t, ok := reqMap["TERM"]; ok && termValue.MatchString(t) {
		out["TERM"] = t
	}

	if rule != nil {
		for _, name := range rule.EnvPass {
			if config.IsNeverPass(name) {
				continue
			}
			if v, ok := reqMap[name]; ok {
				out[name] = v
				passed = append(passed, name)
			}
		}
	}

	for k := range out {
		if matchesAny(envStrip, k) || (config.IsNeverPass(k) && k != "PATH") {
			delete(out, k)
			passed = remove(passed, k)
		}
	}

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+out[k])
	}
	sort.Strings(passed)
	return env, passed
}

func matchesAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
