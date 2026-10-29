package term

import (
	"strings"
	"testing"
)

// TestWriter: the CLI's sanitizer escapes as Untrusted does, keeps tabs and newlines, and gives the same output when a
// rune is split across writes; Flush escapes what was left incomplete.
func TestWriter(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain text\n", "plain text\n"},
		{"esc \x1b[2J here", `esc \x1b[2J here`},
		{"bell\x07 nul\x00 cr\r del\x7f", `bell\x07 nul\x00 cr\x0d del\x7f`},
		{"osc52 \x1b]52;c;ZXZpbA==\x07", `osc52 \x1b]52;c;ZXZpbA==\x07`},
		{"c1 \u009b31m csi", `c1 \u009b31m csi`},
		{"bad \xff\xfe utf8 \xc3x", `bad \xff\xfe utf8 \xc3x`},
		{"rlo \u202eevil\u202c", `rlo \u202eevil\u202c`},
		{"zw\u200bsp bom\ufeff nbsp\u00a0x", `zw\u200bsp bom\ufeff nbsp\u00a0x`},
		{"\u0301lead mark\n\u0301line start", "\\u0301lead mark\n\\u0301line start"},
		{"e\u0301 kept, \u65e5\u672c ok", "e\u0301 kept, \u65e5\u672c ok"},
		{"a\tb\nc", "a\tb\nc"},
	} {
		for _, split := range []bool{false, true} {
			var b strings.Builder
			w := NewWriter(&b)
			if split {
				for i := 0; i < len(c.in); i++ {
					w.Write([]byte{c.in[i]})
				}
			} else if n, err := w.Write([]byte(c.in)); n != len(c.in) || err != nil {
				t.Errorf("Write(%q) = %d, %v", c.in, n, err)
			}
			w.Flush()
			if b.String() != c.want {
				t.Errorf("Writer(%q), split %v = %q, want %q", c.in, split, b.String(), c.want)
			}
		}
	}
	var b strings.Builder
	w := NewWriter(&b)
	w.Write([]byte("cut \xe6\x97"))
	if b.String() != "cut " {
		t.Errorf("an incomplete rune was written before Flush: %q", b.String())
	}
	w.Flush()
	if b.String() != `cut \xe6\x97` {
		t.Errorf("after Flush: %q", b.String())
	}
	for r, want := range map[rune]bool{'a': false, '\t': false, ' ': false, '\u65e5': false, '\x1b': true, '\r': true, '\u202e': true, '\u200b': true, '\u009b': true} {
		if Unprintable(r) != want {
			t.Errorf("Unprintable(%U) = %v", r, !want)
		}
	}
}
