package term

import (
	"io"
	"unicode/utf8"
)

// Writer is the sanitizer for a terminal the CLI writes to: everything goes through it, Plinth's own text and
// what came from a file (a script, an answer, a file name) alike, so only printable text, tabs and newlines reach the
// terminal. Every other rune becomes its Escape, invalid UTF-8 \xNN, and a combining mark at the start of a line an
// escape too, as Line.Untrusted does, without the reverse video. Nothing can move the cursor, set the title, write the
// clipboard, or hide or reorder text. A rune split across two writes is held for the next one; Flush writes what is
// left.
//
// With a width (NewWrapWriter) it also wraps: a line wider than the terminal goes on in a row that starts with the
// continuation mark and a space. It breaks at the last space that fits within one write, and at the column otherwise
// (it holds nothing back past a write, so a prompt shows at once); an escape, a wide character and a combining mark
// with its base never split.
type Writer struct {
	w       io.Writer
	pending []byte
	base    bool // the last rune was a printable one on this line: a combining mark may follow

	width int    // the terminal's columns; 0: no wrapping
	mark  string // the continuation mark
	col   int    // the column the next output goes to
	cont  bool   // the row is a continuation: it starts after the mark
	word  []byte // the output since the last space or tab, not written yet
	wordW int    // its columns
}

// NewWriter returns the sanitizer in front of w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// NewWrapWriter returns the sanitizer in front of w that wraps to width columns, continuation rows starting with mark
// (Glyphs.Wrap) and a space.
func NewWrapWriter(w io.Writer, width int, mark string) *Writer {
	return &Writer{w: w, width: width, mark: mark}
}

// Write sanitizes p and writes it on; it reports all of p as written unless w fails.
func (s *Writer) Write(p []byte) (int, error) {
	b := append(s.pending, p...)
	s.pending = nil
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		switch {
		case r == utf8.RuneError && n == 1 && !utf8.FullRune(b[i:]):
			s.pending = append([]byte(nil), b[i:]...) // the rest of the rune comes with the next write
			i = len(b)
			continue
		case r == utf8.RuneError && n == 1:
			out = s.add(out, []byte{'\\', 'x', "0123456789abcdef"[b[i]>>4], "0123456789abcdef"[b[i]&0xf]}, 4)
			s.base = false
		case r == '\n' || r == '\t':
			out = s.space(out, byte(r))
			s.base = false
		case Escape(r) != "":
			out = s.add(out, []byte(Escape(r)), len(Escape(r)))
			s.base = false
		case runeWidth(r) == 0 && !s.base:
			out = s.add(out, []byte(uEscape(r)), len(uEscape(r)))
		case r == ' ':
			out = s.space(out, ' ')
			s.base = true
		default:
			out = s.add(out, b[i:i+n], runeWidth(r))
			s.base = true
		}
		i += n
	}
	out = s.flushWord(out)
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// add is one rune's output b, w columns wide: written at once without a width, held as part of the word with one.
func (s *Writer) add(out, b []byte, w int) []byte {
	if s.width == 0 {
		return append(out, b...)
	}
	s.word = append(s.word, b...)
	s.wordW += w
	return out
}

// space is a space, tab or newline: the word before it is written first.
func (s *Writer) space(out []byte, c byte) []byte {
	if s.width == 0 {
		return append(out, c)
	}
	out = s.flushWord(out)
	switch c {
	case '\n':
		s.col, s.cont = 0, false
	case '\t':
		w := tabWidth - s.col%tabWidth
		if s.col+w > s.width {
			out = s.wrap(out)
			w = tabWidth - s.col%tabWidth
		}
		s.col += w
	default:
		if s.col+1 > s.width {
			return s.wrap(out) // the space ends the row
		}
		s.col++
	}
	return append(out, c)
}

// flushWord writes the word held: in a row of its own when it doesn't fit after the text before it, and broken at the
// column when it is wider than a row. The word's runes were added whole, so a row breaks between them only.
func (s *Writer) flushWord(out []byte) []byte {
	if len(s.word) == 0 {
		return out
	}
	if s.col+s.wordW > s.width && s.col > s.start() {
		out = s.wrap(out)
	}
	if s.col+s.wordW <= s.width {
		out = append(out, s.word...)
		s.col += s.wordW
	} else {
		out = s.breakWord(out)
	}
	s.word, s.wordW = s.word[:0], 0
	return out
}

// breakWord writes a word wider than a row at the column: rune by rune, an escape (\x1b, \u202e) kept whole, a
// combining mark with the rune before it.
func (s *Writer) breakWord(out []byte) []byte {
	for b := s.word; len(b) > 0; {
		n, w := nextUnit(b)
		if s.col+w > s.width && s.col > s.start() {
			out = s.wrap(out)
		}
		out = append(out, b[:n]...)
		s.col += w
		b = b[n:]
	}
	return out
}

// nextUnit is the length and columns of the first unit of sanitized output b: an escape (a backslash, then x and two
// hex digits, u and four, or U and eight), or a rune with the combining marks after it.
func nextUnit(b []byte) (n, w int) {
	if b[0] == '\\' && len(b) > 1 {
		size := map[byte]int{'x': 4, 'u': 6, 'U': 10}[b[1]]
		if size > 0 && size <= len(b) && hexDigits(b[2:size]) {
			return size, size
		}
	}
	r, n := utf8.DecodeRune(b)
	w = runeWidth(r)
	for n < len(b) {
		m, size := utf8.DecodeRune(b[n:])
		if runeWidth(m) != 0 {
			break
		}
		n += size
	}
	return n, w
}

func hexDigits(b []byte) bool {
	for _, c := range b {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// start is the first column text can take in this row: after the mark and its space in a continuation.
func (s *Writer) start() int {
	if s.cont {
		return len([]rune(s.mark)) + 1
	}
	return 0
}

// wrap ends the row and starts a continuation.
func (s *Writer) wrap(out []byte) []byte {
	out = append(append(append(out, '\n'), s.mark...), ' ')
	s.cont = true
	s.col = s.start()
	return out
}

// Flush writes a rune the last write left incomplete, as the escapes of its bytes.
func (s *Writer) Flush() error {
	if len(s.pending) == 0 {
		return nil
	}
	rest := s.pending
	s.pending = nil
	var out []byte
	for _, c := range rest {
		out = s.add(out, []byte{'\\', 'x', "0123456789abcdef"[c>>4], "0123456789abcdef"[c&0xf]}, 4)
	}
	out = s.flushWord(out)
	_, err := s.w.Write(out)
	return err
}

// Unprintable reports whether r is a rune the sanitizer escapes: what ask lint refuses in a script. Tab isn't.
func Unprintable(r rune) bool { return r != '\t' && Escape(r) != "" }
