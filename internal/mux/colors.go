// SPDX-License-Identifier: GPL-3.0-only

package mux

import (
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Colors are the SGR parameters of the bar's text and background colors, for
// example "38;5;255" and "48;5;18".
type Colors struct {
	FG, BG  string
	Rainbow bool // the background is a gradient across the bar; BG is only the fallback
}

// ParseColor reads a color and returns its SGR parameters, for the text
// (background false) or the background:
//
//	a name       black red green yellow blue magenta cyan white, gray, bright-red ...
//	a number     0 to 255, from the 256-color palette
//	#rgb #rrggbb true color
//	default      the terminal's own color
//
// Names and the numbers 0 to 15 are the terminal's theme colors, so they look
// different from terminal to terminal; 16 to 255 and #rrggbb are fixed.
func ParseColor(spec string, background bool) (string, error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	base, ext, def := 30, 38, "39"
	if background {
		base, ext, def = 40, 48, "49"
	}
	names := []string{"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"}
	switch {
	case s == "default":
		return def, nil
	case s == "gray" || s == "grey":
		return strconv.Itoa(base + 60), nil
	case strings.HasPrefix(s, "#"):
		h := s[1:]
		if len(h) == 3 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		if len(h) != 6 {
			break
		}
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil {
			break
		}
		return fmt.Sprintf("%d;2;%d;%d;%d", ext, v>>16, v>>8&0xff, v&0xff), nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n > 255 {
			return "", fmt.Errorf("color %q: numbers go from 0 to 255", spec)
		}
		return fmt.Sprintf("%d;5;%d", ext, n), nil
	}
	bright := false
	name := s
	for _, p := range []string{"bright-", "bright", "light-", "light"} {
		if strings.HasPrefix(name, p) {
			bright, name = true, strings.TrimPrefix(name, p)
			break
		}
	}
	for i, n := range names {
		if n == name {
			if bright {
				return strconv.Itoa(base + 60 + i), nil
			}
			return strconv.Itoa(base + i), nil
		}
	}
	return "", fmt.Errorf("color %q: use a name (red, bright-blue, gray ...), a number from 0 to 255, #rrggbb or default", spec)
}

// Theme is a named pair of colors.
type Theme struct {
	Name, FG, BG, Note string
}

// Themes are the ready-made color pairs. All but the last few are in the
// 256-color palette (16 to 255), which terminal themes don't change.
var Themes = []Theme{
	{"navy", "231", "18", "white on dark blue"},
	{"deep-navy", "231", "17", "white on a darker blue"},
	{"teal", "231", "23", "white on dark teal"},
	{"forest", "231", "22", "white on dark green (default)"},
	{"plum", "231", "53", "white on dark purple"},
	{"brick", "231", "88", "white on dark red"},
	{"charcoal", "252", "236", "light gray on dark gray, quiet"},
	{"amber", "16", "214", "black on amber, hard to miss"},
	{"gold", "16", "220", "black on yellow"},
	{"mint", "16", "121", "black on light green"},
	{"lime", "16", "118", "black on lime green, loud"},
	{"coral", "16", "210", "black on coral"},
	{"rose", "16", "218", "black on light pink"},
	{"ocean", "231", "24", "white on deep sea blue"},
	{"sky", "17", "153", "dark blue on light blue"},
	{"lavender", "16", "183", "black on light purple"},
	{"light", "16", "252", "black on light gray, for light terminals"},
	{"nord", "#eceff4", "#3b4252", "true color: Nord"},
	{"solarized", "#fdf6e3", "#073642", "true color: Solarized dark"},
	{"github-dark", "#c9d1d9", "#161b22", "true color: GitHub dark"},
	{"dracula", "#f8f8f2", "#44475a", "true color: Dracula"},
	{"gruvbox", "#ebdbb2", "#3c3836", "true color: Gruvbox"},
	{RainbowTheme, "16", "#ffe066", "true color: black on a rainbow gradient (contrast of its yellow)"},
}

// RainbowTheme is the theme whose background runs through the colors of the
// rainbow instead of being one color. Setting a background turns that off.
const RainbowTheme = "rainbow"

// DefaultTheme is the theme used when none is chosen.
const DefaultTheme = "forest"

// DefaultColors are the colors of DefaultTheme: white on dark green, 8:1
// contrast, colors from the fixed part of the 256-color palette so that a
// terminal theme can't change them.
var DefaultColors = mustColors(DefaultTheme, "", "")

func mustColors(theme, fg, bg string) Colors {
	c, err := ResolveColors(theme, fg, bg)
	if err != nil {
		panic(err)
	}
	return c
}

// RandomTheme is the name that picks one of the themes at random each time
// the bar is set up.
const RandomTheme = "random"

// randomIndex picks a number below n; tests replace it.
var randomIndex = rand.IntN

// ResolveColors combines a theme name (empty: the default; "random": one of
// the others, picked now) with an optional text color and background color,
// which override the theme's.
func ResolveColors(theme, fg, bg string) (Colors, error) {
	c, _, err := ResolveTheme(theme, fg, bg)
	return c, err
}

// ResolveTheme is ResolveColors that also says which theme it used: for
// "random", the one that was drawn.
func ResolveTheme(theme, fg, bg string) (Colors, string, error) {
	name := strings.ToLower(strings.TrimSpace(theme))
	if name == "" {
		name = DefaultTheme
	}
	if name == RandomTheme {
		name = Themes[randomIndex(len(Themes))].Name
	}
	var t Theme
	found := false
	for _, c := range Themes {
		if c.Name == name {
			t, found = c, true
		}
	}
	if !found {
		names := []string{RandomTheme}
		for _, c := range Themes {
			names = append(names, c.Name)
		}
		return Colors{}, "", fmt.Errorf("bar theme %q: use one of %s (`aicatraz bar-colors` shows them)", theme, strings.Join(names, ", "))
	}
	if fg == "" {
		fg = t.FG
	}
	bgSet := bg
	if bg == "" {
		bg = t.BG
	}
	f, err := ParseColor(fg, false)
	if err != nil {
		return Colors{}, "", err
	}
	b, err := ParseColor(bg, true)
	if err != nil {
		return Colors{}, "", err
	}
	return Colors{FG: f, BG: b, Rainbow: name == RainbowTheme && bgSet == ""}, name, nil
}

// palette256 returns the RGB of a color of the xterm 256-color palette.
func palette256(n int) (r, g, b float64) {
	switch {
	case n < 16:
		base := [16][3]float64{{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0}, {0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
			{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255}}
		return base[n][0], base[n][1], base[n][2]
	case n < 232:
		n -= 16
		lv := [6]float64{0, 95, 135, 175, 215, 255}
		return lv[n/36], lv[n/6%6], lv[n%6]
	}
	v := float64(8 + (n-232)*10)
	return v, v, v
}

// rgbOf finds the RGB of a color spec that has fixed colors (a number or
// #rrggbb); ok is false for names and default, whose colors are the theme's.
func rgbOf(spec string) (r, g, b float64, ok bool) {
	s := strings.TrimSpace(spec)
	if strings.HasPrefix(s, "#") {
		h := s[1:]
		if len(h) == 3 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil || len(h) != 6 {
			return 0, 0, 0, false
		}
		return float64(v >> 16), float64(v >> 8 & 0xff), float64(v & 0xff), true
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 255 {
		r, g, b = palette256(n)
		return r, g, b, true
	}
	return 0, 0, 0, false
}

func luminance(r, g, b float64) float64 {
	f := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*f(r) + 0.7152*f(g) + 0.0722*f(b)
}

// Contrast is the WCAG contrast ratio of two color specs, or 0 if either has
// no fixed color.
func Contrast(fg, bg string) float64 {
	fr, fgn, fb, ok1 := rgbOf(fg)
	br, bgn, bb, ok2 := rgbOf(bg)
	if !ok1 || !ok2 {
		return 0
	}
	l1, l2 := luminance(fr, fgn, fb), luminance(br, bgn, bb)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// Preview writes every theme as the bar would look, for choosing one.
func Preview(w io.Writer, width int, styled bool) {
	width = min(max(width, 40), 100)
	now := time.Now()
	for _, t := range Themes {
		c, _ := ResolveColors(t.Name, "", "")
		info := BarInfo{Project: "myproject", Label: "claude", Key: "Ctrl-Y", Started: now.Add(-72 * time.Minute), Colors: c}
		bar := info.Render(width, now)
		if !styled {
			bar = visibleText(bar)
		}
		ratio := "     "
		if r := Contrast(t.FG, t.BG); r > 0 {
			ratio = fmt.Sprintf("%4.1f:", r)
		}
		fmt.Fprintf(w, "%s\n  %-12s fg %-8s bg %-8s %s1  %s\n\n", bar, t.Name, t.FG, t.BG, ratio, t.Note)
	}
	fmt.Fprint(w, `Pick one with --bar-theme <name> ("random" takes a different one every time), or set the colors yourself with --bar-fg and
--bar-bg. A color is a name (red, bright-blue, gray), a number from 0 to 255,
#rrggbb, or default (the terminal's own). Names and 0 to 15 follow your terminal's
theme; 16 to 255 and #rrggbb are the same everywhere. To keep a choice, put it in
your shell config:

  export AICATRAZ_BAR_THEME=charcoal
  export AICATRAZ_BAR_FG=white        # optional, overrides the theme's text color
  export AICATRAZ_BAR_BG='#1f2a44'    # optional, overrides the theme's background
`)
}

// visibleText removes the escape sequences of a rendered bar.
func visibleText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
