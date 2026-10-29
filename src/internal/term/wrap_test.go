package term

import (
	"slices"
	"strings"
	"testing"
)

// TestLineWrap: word wrap at the last space that fits, at the column without one; a wide character, a combining mark
// with its base and an escape never split; the first row may be wider than the rest.
func TestLineWrap(t *testing.T) {
	for _, c := range []struct {
		in          string
		first, rest int
		want        []string
	}{
		{"aaa bbb ccc", 7, 7, []string{"aaa bbb", "ccc"}},
		{"aaa bbb ccc", 6, 6, []string{"aaa ", "bbb ", "ccc"}},
		{"abcdefghij", 4, 4, []string{"abcd", "efgh", "ij"}},
		{"\u65e5\u672c\u8a9e", 5, 5, []string{"\u65e5\u672c", "\u8a9e"}},
		{"ab\x1bcd", 5, 5, []string{"ab", `\x1bc`, "d"}},
		{"e\u0301e\u0301e\u0301", 2, 2, []string{"e\u0301e\u0301", "e\u0301"}},
		{"aaaa bbbb cccc", 9, 6, []string{"aaaa bbbb", "cccc"}},
		{"", 5, 5, []string{""}},
		{"short", 80, 78, []string{"short"}},
	} {
		var got []string
		for _, r := range (&Line{}).Untrusted(c.in, Style{}).Wrap(c.first, c.rest) {
			got = append(got, r.String())
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("Wrap(%q, %d, %d) = %q, want %q", c.in, c.first, c.rest, got, c.want)
		}
	}
}

// TestWriterWrap: the CLI's sanitizer wraps to its width, at a space within a write and at the column in a word wider
// than a row, the continuation row starting with the mark and a space; an escape never splits.
func TestWriterWrap(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"one two three four five six seven\n", "one two three four \n> five six seven\n"},
		{"abcdefghijklmnopqrstuvwxyz0123456789\n", "abcdefghijklmnopqrst\n> uvwxyz0123456789\n"},
		{"abcdefghijklmnopqr\x1b[x\n", "abcdefghijklmnopqr\n> \\x1b[x\n"},
		{"short\nlines\n", "short\nlines\n"},
	} {
		var b strings.Builder
		w := NewWrapWriter(&b, 20, ">")
		w.Write([]byte(c.in))
		w.Flush()
		if b.String() != c.want {
			t.Errorf("wrapped %q = %q, want %q", c.in, b.String(), c.want)
		}
	}
	// A prompt shows at once: nothing is held back past a write.
	var b strings.Builder
	w := NewWrapWriter(&b, 20, ">")
	w.Write([]byte("Type yes:"))
	if b.String() != "Type yes:" {
		t.Errorf("a prompt was held back: %q", b.String())
	}
}

// TestGlyphs: the locale is the first non-empty one of LC_ALL, LC_CTYPE and LANG; UTF-8 shows Unicode, anything else
// its ASCII fallback.
func TestGlyphs(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"LANG": "en_US.UTF-8"}, true},
		{map[string]string{"LC_CTYPE": "de_DE.utf8", "LANG": "C"}, true},
		{map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, false},
		{map[string]string{}, false},
	} {
		if got := UTF8(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("UTF8(%v) = %v", c.env, got)
		}
	}
	if g := GlyphsFor(false); g.Ellipsis != "..." || g.Wrap != ">" || g.Mark != "+" || g.Rule != "-" {
		t.Errorf("ASCII glyphs: %+v", g)
	}
	if g := GlyphsFor(true); g.Ellipsis != "…" || g.Wrap != "↪" || g.Mark != "✓" || g.Rule != "─" {
		t.Errorf("UTF-8 glyphs: %+v", g)
	}
}
