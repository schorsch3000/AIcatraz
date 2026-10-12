package glob

import "testing"

func TestCompile(t *testing.T) {
	for _, tc := range []struct {
		pat, s string
		want   bool
	}{
		{"*.mk", "a.mk", true},
		{"*.mk", "d/a.mk", false}, // * stays in one segment
		{"**/*.mk", "a.mk", true},
		{"**/*.mk", "d/e/a.mk", true},
		{"a/**", "a/b/c", true},
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"[!x]y", "ay", true},
		{"[!x]y", "xy", false},
		{`a\*b`, "a*b", true},
		{"a.b", "aXb", false},
	} {
		re, err := Compile(tc.pat)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(tc.s); got != tc.want {
			t.Errorf("%q ~ %q: got %v", tc.pat, tc.s, got)
		}
	}
	if _, err := Compile("[x"); err == nil {
		t.Error("unterminated class must fail")
	}
}

func TestEscapeMatchesLiterally(t *testing.T) {
	for _, s := range []string{"a*b", "x[1]", `back\slash`, "q?", "plain"} {
		re, err := Compile(Escape(s))
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(s) {
			t.Errorf("%q must match itself", s)
		}
		if s != "plain" && re.MatchString("zzz") {
			t.Errorf("%q escaped must not be a wildcard", s)
		}
	}
}

func TestStaticPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"/p/mk/*.mk": "/p/mk",
		"/p/**/x":    "/p",
		"/p/a/b":     "/p/a/b",
		"/*":         "/",
		"rel/a/*/b":  "rel/a",
		"*.txt":      "",
	} {
		if got := StaticPrefix(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	if HasMeta("plain/path") || !HasMeta("a*") || !HasMeta(`a\b`) {
		t.Error("HasMeta")
	}
}
