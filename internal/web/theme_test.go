package web

import (
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"pademelon/internal/model"
)

// TestThemesSyncWithPage checks the three places theme names live against
// each other: the Go allowlist in theme.go, the THEMES map in the page's
// head script, the picker <option>s, and the CSS palette blocks.
func TestThemesSyncWithPage(t *testing.T) {
	page := string(indexHTML)

	js := regexp.MustCompile(`"([a-z0-9-]+)":\s*\[`)
	opt := regexp.MustCompile(`value="([a-z0-9-]+)"`)

	jsThemes := map[string]bool{}
	for _, m := range js.FindAllStringSubmatch(page, -1) {
		if !ValidTheme(m[1]) {
			continue
		}
		jsThemes[m[1]] = true
	}
	if len(jsThemes) != len(themes) {
		t.Errorf("THEMES map in page has %d themes, allowlist has %d", len(jsThemes), len(themes))
	}

	optThemes := map[string]bool{}
	for _, m := range opt.FindAllStringSubmatch(page, -1) {
		if ValidTheme(m[1]) {
			optThemes[m[1]] = true
		}
	}
	for _, name := range themes {
		if !optThemes[name] {
			t.Errorf("theme %q has no picker <option>", name)
		}
		if !jsThemes[name] {
			t.Errorf("theme %q missing from THEMES map in head script", name)
		}
		if !strings.Contains(page, ":root[data-theme="+name+"]") {
			t.Errorf("theme %q has no CSS palette block", name)
		}
	}
}

// TestThemeInjection checks that handleIndex swaps the placeholder on <html>
// for the configured theme, and that an empty theme falls back to the default.
func TestThemeInjection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache := model.NewCache()

	get := func(theme string) string {
		srv := New(Config{Cache: cache, Log: log, Theme: theme})
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		return rec.Body.String()
	}

	if body := get("dracula"); !strings.Contains(body, `data-theme="dracula"`) {
		t.Error("configured theme dracula not injected into the page")
	}
	if body := get(""); !strings.Contains(body, `data-theme="`+DefaultTheme+`"`) {
		t.Errorf("empty theme did not fall back to default %q", DefaultTheme)
	}
}

// requiredVars are the custom properties every palette block must define.
var requiredVars = []string{
	"bg", "bg-pattern", "card", "card2", "border-dot", "border-soft", "shadow",
	"text", "muted", "accent", "accent-soft",
	"ok-bg", "ok-tx", "off-bg", "off-tx", "warn-bg", "warn-tx", "bad-bg", "bad-tx",
	"track", "fill", "fill-warn", "fill-bad",
}

// contrastPairs are foreground/background var pairs that must reach WCAG AA
// (4.5:1) so no theme ships unreadable text.
var contrastPairs = [][2]string{
	{"text", "card"},
	{"text", "bg"},
	{"muted", "card"},
	{"muted", "card2"},
	{"ok-tx", "ok-bg"},
	{"off-tx", "off-bg"},
	{"warn-tx", "warn-bg"},
	{"bad-tx", "bad-bg"},
}

// TestThemePaletteBlocksAreComplete parses every palette block out of
// index.html and checks it defines the full variable set.
func TestThemePaletteBlocksAreComplete(t *testing.T) {
	page := string(indexHTML)
	block := regexp.MustCompile(`(?s):root\[data-theme=([a-z0-9-]+)\](\[data-mode="dark"\])?\s*\{(.*?)\n\}`)
	varLine := regexp.MustCompile(`--([a-z0-9-]+):\s*([^;\n]+)`)

	found := map[string]map[string]bool{}
	for _, m := range block.FindAllStringSubmatch(page, -1) {
		name := m[1]
		vars := map[string]bool{}
		for _, v := range varLine.FindAllStringSubmatch(m[3], -1) {
			vars[v[1]] = true
		}
		key := name
		if m[2] != "" {
			key = name + "/dark"
		}
		found[key] = vars
		for _, want := range requiredVars {
			if !vars[want] {
				t.Errorf("palette block %s is missing --%s", key, want)
			}
		}
	}

	for _, name := range themes {
		if found[name] == nil {
			t.Errorf("theme %q has no base palette block", name)
		}
	}
	// Two-variant themes must also ship a dark block; single-variant themes
	// must not (the base block is the only variant).
	twoVariant := map[string]bool{"sweetpea": true, "classic": true, "charm": true, "noctis": true}
	for _, name := range themes {
		if twoVariant[name] && found[name+"/dark"] == nil {
			t.Errorf("theme %q needs a [data-mode=dark] palette block", name)
		}
		if !twoVariant[name] && found[name+"/dark"] != nil {
			t.Errorf("theme %q should be single-variant but has a dark block", name)
		}
	}
}

// TestThemeContrastAA fails the build when any theme ships a text/background
// pair below WCAG AA (4.5:1).
func TestThemeContrastAA(t *testing.T) {
	page := string(indexHTML)
	block := regexp.MustCompile(`(?s):root\[data-theme=([a-z0-9-]+)\](\[data-mode="dark"\])?\s*\{(.*?)\n\}`)
	varLine := regexp.MustCompile(`--([a-z0-9-]+):\s*([^;\n]+)`)
	hex := regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

	for _, m := range block.FindAllStringSubmatch(page, -1) {
		name := m[1]
		if m[2] != "" {
			name += "/dark"
		}
		vars := map[string]string{}
		for _, v := range varLine.FindAllStringSubmatch(m[3], -1) {
			vars[v[1]] = strings.TrimSpace(v[2])
		}
		for _, pair := range contrastPairs {
			fg, bg := vars[pair[0]], vars[pair[1]]
			if !hex.MatchString(fg) || !hex.MatchString(bg) {
				t.Errorf("theme %s: pair %s/%s is not a plain hex colour (%q/%q) — check it manually", name, pair[0], pair[1], fg, bg)
				continue
			}
			if ratio := contrastRatio(fg, bg); ratio < 4.5 {
				t.Errorf("theme %s: %s (%s) on %s (%s) is %.2f:1, need 4.5:1", name, pair[0], fg, pair[1], bg, ratio)
			}
		}
	}
}

// contrastRatio computes the WCAG 2.x contrast ratio between two #rrggbb
// colours.
func contrastRatio(a, b string) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func relLuminance(s string) float64 {
	h := strings.TrimPrefix(s, "#")
	var c [3]float64
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseUint(h[i*2:i*2+2], 16, 64)
		if err != nil {
			return 0
		}
		f := float64(v) / 255
		if f <= 0.04045 {
			f /= 12.92
		} else {
			f = math.Pow((f+0.055)/1.055, 2.4)
		}
		c[i] = f
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}
