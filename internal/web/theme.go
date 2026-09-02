package web

// DefaultTheme is the theme name baked into index.html's <html> tag. The
// server swaps it for the configured default; -theme sets that.
const DefaultTheme = "sweetpea"

// themes is the allowlist of palettes defined in index.html. It must stay in
// sync with the THEMES map in the page's head script and the picker options —
// the tests in theme_test.go enforce all three.
var themes = []string{
	"sweetpea",
	"classic",
	"charm",
	"catppuccin-latte",
	"catppuccin-frappe",
	"catppuccin-macchiato",
	"catppuccin-mocha",
	"dracula",
	"noctis",
}

// ValidTheme reports whether name is a theme embedded in the page.
func ValidTheme(name string) bool {
	for _, t := range themes {
		if t == name {
			return true
		}
	}
	return false
}

// Themes returns the valid theme names.
func Themes() []string {
	out := make([]string, len(themes))
	copy(out, themes)
	return out
}
