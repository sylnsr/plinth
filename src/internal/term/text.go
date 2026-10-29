package term

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Color is one of the 8 basic ANSI colours, a 24-bit one (RGB), or the terminal's own.
type Color uint32

const (
	Default Color = iota
	Black
	Red
	Green
	Yellow
	Blue
	Magenta
	Cyan
	White
)

// rgbFlag marks a Color made by RGB; its low 24 bits are the colour.
const rgbFlag Color = 1 << 24

// RGB is the 24-bit colour 0xRRGGBB, drawn with SGR 38;2 / 48;2 (truecolor).
func RGB(hex uint32) Color {
	return rgbFlag | Color(hex&0xffffff)
}

// sgrColor is the SGR parameters of c as a foreground (layer 3) or background (layer 4) colour.
func sgrColor(layer int, c Color) string {
	if c&rgbFlag != 0 {
		return fmt.Sprintf(";%d8;2;%d;%d;%d", layer, c>>16&0xff, c>>8&0xff, c&0xff)
	}
	return fmt.Sprintf(";%d%d", layer, c-Black)
}

// Style is how a cell is drawn. Without colour (NO_COLOR) FG is ignored and a BG becomes reverse video, so a bar still
// shows; bold, dim and reverse stay.
type Style struct {
	FG      Color
	BG      Color
	Bold    bool
	Dim     bool
	Reverse bool
}

// cell is one rune on a line and the columns it takes: 0 for a combining mark, 1, or 2 for a wide character. join
// marks the rest of an escape: Wrap never breaks a row before it.
type cell struct {
	r    rune
	w    int
	s    Style
	join bool
}

// Line is one line of the screen, built from the TUI's own text (Text) and from text out of files (Untrusted). Nothing
// but Untrusted takes text from a file: it is the sanitizer.
type Line struct {
	cells []cell
	width int
}

// tabWidth is the distance between tab stops.
const tabWidth = 8

// Text appends the TUI's own text. It doesn't go through the sanitizer; as a seatbelt, a rune that isn't printable
// becomes '?' (the TUI's own text has none).
func (l *Line) Text(s string, st Style) *Line {
	for _, r := range s {
		switch {
		case r == '\t':
			l.tab(st)
		case !unicode.IsPrint(r) || hidden(r):
			l.put('?', 1, st)
		default:
			l.put(r, runeWidth(r), st)
		}
	}
	return l
}

// Untrusted appends text that came from a file (a script, an answer, a review, a file name): the sanitizer. Control
// characters (C0 but tab, DEL, C1) become escapes such as \x1b, invalid UTF-8 \xNN, bidi controls, invisible characters
// and every other rune unicode.IsPrint rejects \uNNNN, each escape in reverse video; a tab becomes spaces to the next
// tab stop; a combining mark with no character of the same text before it becomes an escape too, so it can't change
// the TUI's own text. What reaches the terminal is printable text only: it can't move the cursor, set the title, write
// the clipboard, or hide or reorder text (plinth-tui.README.md, Untrusted text).
func (l *Line) Untrusted(s string, st Style) *Line {
	base := false // a character of s that a combining mark may combine with
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			l.escape(fmt.Sprintf(`\x%02x`, s[i]), st)
			base = false
		case r == '\t':
			l.tab(st)
			base = false
		case Escape(r) != "":
			l.escape(Escape(r), st)
			base = false
		default:
			w := runeWidth(r)
			if w == 0 && !base {
				l.escape(uEscape(r), st)
				break
			}
			l.put(r, w, st)
			base = true
		}
		i += n
	}
	return l
}

// Append appends m's cells.
func (l *Line) Append(m *Line) *Line {
	for _, c := range m.cells {
		l.cells = append(l.cells, c)
		l.width += c.w
	}
	return l
}

// PadTo appends spaces until the line is col columns wide.
func (l *Line) PadTo(col int, st Style) *Line {
	for l.width < col {
		l.put(' ', 1, st)
	}
	return l
}

// Width is the number of columns the line takes.
func (l *Line) Width() int { return l.width }

// String is the line's text without its styles (for tests and the log).
func (l *Line) String() string {
	var b strings.Builder
	for _, c := range l.cells {
		b.WriteRune(c.r)
	}
	return b.String()
}

// Cut returns the part of the line that shows from column off on, at most w columns wide. When the rest doesn't fit,
// its last column is '>', in the style of the last cell that shows (a cut bar or selected row keeps its look). A wide
// character that would cross either edge is left out (a space at the left edge).
func (l *Line) Cut(off, w int) *Line {
	out := &Line{}
	if w <= 0 {
		return out
	}
	col, i := 0, 0
	for ; i < len(l.cells) && col < off; i++ {
		col += l.cells[i].w
	}
	if col > off { // a wide character crossed the left edge
		out.put(' ', 1, l.cells[i-1].s)
	}
	for i < len(l.cells) && l.cells[i].w == 0 && off > 0 { // the marks of a character left of the edge
		i++
	}
	fits := out.width+l.width-col <= w
	for ; i < len(l.cells); i++ {
		c := l.cells[i]
		if !fits && out.width+c.w > w-1 {
			break
		}
		out.put(c.r, c.w, c.s)
	}
	if !fits {
		st := Style{}
		if n := len(out.cells); n > 0 {
			st = out.cells[n-1].s
		}
		out.PadTo(w-1, st).put('>', 1, st)
	}
	return out
}

func (l *Line) put(r rune, w int, st Style) {
	l.cells = append(l.cells, cell{r: r, w: w, s: st})
	l.width += w
}

func (l *Line) tab(st Style) {
	for n := tabWidth - l.width%tabWidth; n > 0; n-- {
		l.put(' ', 1, st)
	}
}

// escape appends the visible escape e in reverse video (normal video on a reversed line), so it stands out.
func (l *Line) escape(e string, st Style) {
	st.Reverse = !st.Reverse
	for i, r := range e {
		l.put(r, 1, st)
		l.cells[len(l.cells)-1].join = i > 0
	}
}

// render writes the line with SGR sequences for its styles; a line starts and ends in the default style.
func (l *Line) render(b *strings.Builder, color bool) {
	cur := Style{}
	for _, c := range l.cells {
		st := c.s
		if !color {
			if st.BG != Default {
				st.Reverse = !st.Reverse
			}
			st.FG, st.BG = Default, Default
		}
		if st != cur {
			b.WriteString(sgr(st))
			cur = st
		}
		b.WriteRune(c.r)
	}
	if cur != (Style{}) {
		b.WriteString(sgr(Style{}))
	}
}

// sgr is the sequence that sets st, starting from the default style.
func sgr(st Style) string {
	s := "\x1b[0"
	if st.Bold {
		s += ";1"
	}
	if st.Dim {
		s += ";2"
	}
	if st.Reverse {
		s += ";7"
	}
	if st.FG != Default {
		s += sgrColor(3, st.FG)
	}
	if st.BG != Default {
		s += sgrColor(4, st.BG)
	}
	return s + "m"
}

// hidden reports the runes the sanitizer always escapes although some are printable to unicode: C1 controls, bidi
// controls and invisible characters (Trojan Source).
func hidden(r rune) bool {
	switch {
	case r >= 0x80 && r <= 0x9f,
		r >= 0x200b && r <= 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x2060 && r <= 0x2069,
		r == 0xfeff:
		return true
	}
	return false
}

// Escape is the visible escape the sanitizer puts for r, "" for a rune it lets through: \xNN for a control character
// (C0 with tab, DEL), \uNNNN for a C1 control, a bidi control, an invisible character or any other rune
// unicode.IsPrint rejects. The caller treats tab and newline itself.
func Escape(r rune) string {
	switch {
	case r < 0x20 || r == 0x7f:
		return fmt.Sprintf(`\x%02x`, r)
	case hidden(r) || !unicode.IsPrint(r):
		return uEscape(r)
	}
	return ""
}

func uEscape(r rune) string {
	if r > 0xffff {
		return fmt.Sprintf(`\U%08x`, r)
	}
	return fmt.Sprintf(`\u%04x`, r)
}

// runeWidth is the number of columns r takes: 0 for a combining mark, 2 for an East Asian wide or fullwidth character
// or an emoji, else 1. A terminal that disagrees only misplaces the rest of that line: the screen positions every line
// itself and turns autowrap off, so no line can run into another.
func runeWidth(r rune) int {
	if unicode.In(r, unicode.Mn, unicode.Me) {
		return 0
	}
	i := sort.Search(len(wide), func(i int) bool { return wide[i][1] >= r })
	if i < len(wide) && wide[i][0] <= r {
		return 2
	}
	return 1
}

// wide holds the ranges of wide characters, sorted (from Unicode's EastAsianWidth W and F, and the emoji blocks).
var wide = [][2]rune{
	{0x1100, 0x115f}, {0x231a, 0x231b}, {0x2329, 0x232a}, {0x23e9, 0x23ec}, {0x23f0, 0x23f0}, {0x23f3, 0x23f3},
	{0x25fd, 0x25fe}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267f, 0x267f}, {0x2693, 0x2693}, {0x26a1, 0x26a1},
	{0x26aa, 0x26ab}, {0x26bd, 0x26be}, {0x26c4, 0x26c5}, {0x26ce, 0x26ce}, {0x26d4, 0x26d4}, {0x26ea, 0x26ea},
	{0x26f2, 0x26f3}, {0x26f5, 0x26f5}, {0x26fa, 0x26fa}, {0x26fd, 0x26fd}, {0x2705, 0x2705}, {0x270a, 0x270b},
	{0x2728, 0x2728}, {0x274c, 0x274c}, {0x274e, 0x274e}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27b0, 0x27b0}, {0x27bf, 0x27bf}, {0x2b1b, 0x2b1c}, {0x2b50, 0x2b50}, {0x2b55, 0x2b55}, {0x2e80, 0x303e},
	{0x3041, 0x33ff}, {0x3400, 0x4dbf}, {0x4e00, 0x9fff}, {0xa000, 0xa4cf}, {0xa960, 0xa97f}, {0xac00, 0xd7a3},
	{0xf900, 0xfaff}, {0xfe10, 0xfe19}, {0xfe30, 0xfe6f}, {0xff00, 0xff60}, {0xffe0, 0xffe6}, {0x16fe0, 0x16fe4},
	{0x17000, 0x18cff}, {0x1b000, 0x1b2ff}, {0x1f004, 0x1f004}, {0x1f0cf, 0x1f0cf}, {0x1f18e, 0x1f18e},
	{0x1f191, 0x1f19a}, {0x1f200, 0x1f251}, {0x1f300, 0x1f64f}, {0x1f680, 0x1f6ff}, {0x1f7e0, 0x1f7eb},
	{0x1f90c, 0x1f9ff}, {0x1fa70, 0x1faff}, {0x20000, 0x2fffd}, {0x30000, 0x3fffd},
}

// Wrap breaks the line into rows: the first at most first columns wide, the others at most rest. It is word
// wrap: a row breaks after the last space that fits, or at the column when a word is longer than a row. A wide
// character, a combining mark with its base and an escape of the sanitizer never split; a space a row breaks at stays
// at its end, one that would start the next row is left out. An empty line is one empty row.
func (l *Line) Wrap(first, rest int) []*Line {
	var rows []*Line
	var row []cell
	width, limit, brk := 0, max(1, first), -1 // brk: the row's cells up to brk end in a space
	for i := 0; i < len(l.cells); {
		j := i + 1 // the unit: a cell with the marks, or the rest of its escape, after it
		for j < len(l.cells) && (l.cells[j].w == 0 || l.cells[j].join) {
			j++
		}
		w := lineOf(l.cells[i:j]).width
		if c := l.cells[i]; width+w > limit && width > 0 && c.r == ' ' && !c.join {
			// a space that doesn't fit is the break itself
			rows = append(rows, lineOf(row))
			row, width, limit, brk = nil, 0, max(1, rest), -1
			i = j
			continue
		}
		if width+w > limit && width > 0 {
			cut := len(row)
			if brk > 0 && lineOf(row[brk:]).width+w <= max(1, rest) {
				cut = brk
			}
			rows = append(rows, lineOf(row[:cut]))
			row = append([]cell(nil), row[cut:]...)
			width, limit, brk = lineOf(row).width, max(1, rest), -1
		}
		row = append(row, l.cells[i:j]...)
		width += w
		if c := l.cells[i]; c.r == ' ' && !c.join {
			brk = len(row)
		}
		i = j
	}
	return append(rows, lineOf(row))
}

// lineOf is a line of the cells cs.
func lineOf(cs []cell) *Line {
	l := &Line{}
	for _, c := range cs {
		l.cells = append(l.cells, c)
		l.width += c.w
	}
	return l
}
