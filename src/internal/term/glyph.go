package term

import "strings"

// UTF8 reports whether the locale is UTF-8: the first non-empty one of LC_ALL, LC_CTYPE and LANG, as the C library
// takes them, holds utf-8 or utf8 in any case. One check for every glyph Plinth shows.
func UTF8(getenv func(string) string) bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToLower(getenv(k)); v != "" {
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}

// Glyphs are the characters Plinth only shows on screen (the rule for characters): Unicode in a
// UTF-8 locale, plain ASCII otherwise. What goes to files, issues or logs is plain ASCII always, and there is no emoji
// (their width differs between terminals).
type Glyphs struct {
	Ellipsis string // what was left out: … or ...
	Wrap     string // the start of a continuation row: ↪ or >
	Mark     string // a run completion can act on now: ✓ or +
	Rule     string // the divider under the TUI's header: ─ or -
}

// GlyphsFor are the glyphs of a UTF-8 locale, or their ASCII fallbacks.
func GlyphsFor(utf8 bool) Glyphs {
	if utf8 {
		return Glyphs{Ellipsis: "…", Wrap: "↪", Mark: "✓", Rule: "─"}
	}
	return Glyphs{Ellipsis: "...", Wrap: ">", Mark: "+", Rule: "-"}
}
