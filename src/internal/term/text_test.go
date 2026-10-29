package term

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func untrusted(s string) *Line { return (&Line{}).Untrusted(s, Style{}) }

func TestUntrusted(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain text", "plain text"},
		{"esc \x1b[2J here", `esc \x1b[2J here`},
		{"bell\x07 nul\x00 cr\r del\x7f", `bell\x07 nul\x00 cr\x0d del\x7f`},
		{"osc52 \x1b]52;c;ZXZpbA==\x07", `osc52 \x1b]52;c;ZXZpbA==\x07`},
		{"c1 \u009b31m csi", `c1 \u009b31m csi`},
		{"bad \xff\xfe utf8 \xc3", `bad \xff\xfe utf8 \xc3`},
		{"rlo \u202eevil\u202c", `rlo \u202eevil\u202c`},
		{"zw\u200bsp \u200d \u2066iso\u2069 bom\ufeff", `zw\u200bsp \u200d \u2066iso\u2069 bom\ufeff`},
		{"tag \U000e0041 pua \ue000", `tag \U000e0041 pua \ue000`},
		{"nbsp\u00a0x", `nbsp\u00a0x`},
		{"\u0301lead mark", `\u0301lead mark`},
		{"e\u0301 kept", "e\u0301 kept"},
		{"\u65e5\u672c wide ok", "\u65e5\u672c wide ok"},
		{"a\tb", "a       b"},
		{"1234567\tx", "1234567 x"},
	} {
		if got := untrusted(c.in).String(); got != c.want {
			t.Errorf("Untrusted(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEscapesStandOut(t *testing.T) {
	l := untrusted("a\x1bb")
	var rev []bool
	for _, c := range l.cells {
		rev = append(rev, c.s.Reverse)
	}
	// a, then \x1b in reverse video, then b.
	want := []bool{false, true, true, true, true, false}
	if len(rev) != len(want) {
		t.Fatalf("cells: %q", l.String())
	}
	for i := range want {
		if rev[i] != want[i] {
			t.Errorf("cell %d (%q) reverse = %v", i, l.cells[i].r, rev[i])
		}
	}
	// On a reversed line (the selected row) an escape is in normal video.
	l = (&Line{}).Untrusted("\x1b", Style{Reverse: true})
	if l.cells[0].s.Reverse {
		t.Error("an escape on a reversed line is reversed too")
	}
}

func TestText(t *testing.T) {
	l := (&Line{}).Text("own \x1b text\t|", Style{Bold: true})
	if got := l.String(); got != "own ? text      |" {
		t.Errorf("Text = %q", got)
	}
}

func TestWidth(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"abc", 3}, {"日本", 4}, {"é", 1}, {"🙂", 2}, {"\x1b", 4}, {"한국어", 6}, {"ｆｕｌｌ", 8},
	} {
		if got := untrusted(c.in).Width(); got != c.want {
			t.Errorf("Width(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCut(t *testing.T) {
	for _, c := range []struct {
		in     string
		off, w int
		want   string
	}{
		{"short", 0, 10, "short"},
		{"exactly10!", 0, 10, "exactly10!"},
		{"eleven chars", 0, 10, "eleven ch>"},
		{"日本語です", 0, 5, "日本>"},
		{"日本語です", 0, 6, "日本 >"},
		{"日本語です", 0, 10, "日本語です"},
		{"0123456789abcdef", 4, 6, "45678>"},
		{"0123456789abcdef", 10, 6, "abcdef"},
		{"0123456789abcdef", 20, 6, ""},
		{"日本語", 1, 5, " 本語"},
		{"a\u00e9b", 2, 5, "b"},
		{"ae\u0301b", 2, 5, "b"},
		{"anything", 0, 1, ">"},
		{"anything", 0, 0, ""},
	} {
		l := untrusted(c.in).Cut(c.off, c.w)
		if got := l.String(); got != c.want {
			t.Errorf("Cut(%q, %d, %d) = %q, want %q", c.in, c.off, c.w, got, c.want)
		}
		if l.Width() > c.w {
			t.Errorf("Cut(%q, %d, %d) is %d wide", c.in, c.off, c.w, l.Width())
		}
	}
}

func TestPadAppend(t *testing.T) {
	cellText := untrusted("calm-otter-with-a-long-name").Cut(0, 12)
	l := (&Line{}).Append(cellText).PadTo(13, Style{}).Text("main", Style{})
	if got := l.String(); got != "calm-otter-> main" {
		t.Errorf("row = %q", got)
	}
}

// allowed are the sequences the screen itself writes: a frame holds no other.
var allowed = regexp.MustCompile(`^\x1b(\[[0-9]+;1H|\[0(;[0-9]+)*m|\[K|\[2J|\[\?(1049|25|7)[hl])`)

// ownSeq matches every sequence the screen writes.
var ownSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// checkFrame fails unless out holds only printable text and the screen's own sequences.
func checkFrame(t *testing.T, out string) {
	t.Helper()
	for i := 0; i < len(out); {
		r, n := utf8.DecodeRuneInString(out[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			t.Fatalf("invalid UTF-8 at %d: %q", i, out[max(0, i-20):min(len(out), i+20)])
		case r == 0x1b:
			m := allowed.FindString(out[i:])
			if m == "" {
				t.Fatalf("a sequence the screen doesn't write at %d: %q", i, out[i:min(len(out), i+20)])
			}
			n = len(m)
		case r < 0x20 || (r >= 0x7f && r <= 0x9f) || hidden(r):
			t.Fatalf("control or hidden rune %U at %d: %q", r, i, out[max(0, i-20):min(len(out), i+20)])
		}
		i += n
	}
}

// TestHostileFrame: an answer full of escape sequences renders none of them (plinth-tui.README.md, Untrusted text).
func TestHostileFrame(t *testing.T) {
	answer := strings.Join([]string{
		"\x1b]0;forged title\x07 \x1b]52;c;cm0gLXJmIH4=\x07 clipboard",
		"\x1b[2J\x1b[H\x1b[1;1H Plinth dev  forged header line",
		"\x1b[8mhidden\x1b[0m \x1b[?1049l leave alt screen \x1bc reset",
		"\u009b31m c1 csi \u009d0;x\u009c \x9b\x9d raw c1 bytes",
		"\u202eevil reversed\u202c \u2066isolate\u2069 \u200b\u200d\ufeff",
		"\x1bP1$r q\x1b\\ dcs \x1b_apc\x1b\\ \x1b^pm\x1b\\ \x7f del \r cr \b bs",
		"\u65e5\u672c\u8a9e wide \t tab e\u0301 \u0301 lone mark \U000e0041 tag",
	}, "\n")
	var lines []*Line
	for _, s := range strings.Split(answer, "\n") {
		lines = append(lines, (&Line{}).Text("| ", Style{FG: Cyan}).Untrusted(s, Style{Bold: true}))
	}
	for _, color := range []bool{true, false} {
		var out strings.Builder
		s := NewScreen(&out, color)
		s.Resize(80, 24)
		if err := s.Enter(); err != nil {
			t.Fatal(err)
		}
		if err := s.Draw(lines); err != nil {
			t.Fatal(err)
		}
		if err := s.Leave(); err != nil {
			t.Fatal(err)
		}
		checkFrame(t, out.String())
		// What shows, the screen's own sequences left out: the escapes, as text.
		shown := ownSeq.ReplaceAllString(out.String(), "")
		if !strings.Contains(shown, `\x1b]52;c;cm0gLXJmIH4=\x07 clipboard`) || !strings.Contains(shown, `\u202eevil reversed\u202c`) {
			t.Error("an escape is not shown as text")
		}
	}
}
