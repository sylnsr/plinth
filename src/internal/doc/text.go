package doc

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// AppendText appends b to dst as the lines of a literal block (|2) whose content starts at column indent: each line
// of b, cut at \n, indented by indent spaces and ended by \n, with every byte YAML can't hold in a literal block
// escaped as \xNN (control characters but tab, DEL, the C1 controls, the line breaks NEL, LS and PS, a byte order
// mark, U+FFFE and U+FFFF, and invalid UTF-8). The block's header (key: |2) is the caller's; with it, the lines decode
// to b's text, escapes left as written, ending in one \n (none for no lines). Bash never escapes anything itself: the
// AAA library sends every text through here (TextMain).
func AppendText(dst, b []byte, indent int) []byte {
	pad := strings.Repeat(" ", indent)
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		dst = append(dst, pad...)
		dst = appendEscaped(dst, line)
		dst = append(dst, '\n')
	}
	return dst
}

// appendEscaped appends s with every rune YAML doesn't allow in a literal block escaped, byte by byte.
func appendEscaped(dst, s []byte) []byte {
	for len(s) > 0 {
		r, n := utf8.DecodeRune(s)
		if allowed(r, n) {
			dst = append(dst, s[:n]...)
		} else {
			for _, c := range s[:n] {
				dst = fmt.Appendf(dst, `\x%02x`, c)
			}
		}
		s = s[n:]
	}
	return dst
}

// allowed reports whether rune r, n bytes long, may stand in a literal block as it is.
func allowed(r rune, n int) bool {
	switch {
	case r == utf8.RuneError && n <= 1: // invalid UTF-8
		return false
	case r == '\t':
		return true
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f: // C0 (CR included: a line break in YAML), DEL, C1 (NEL included)
		return false
	case r == 0x2028, r == 0x2029, r == 0xfeff, r == 0xfffe, r == 0xffff:
		return false
	}
	return true
}

// WriteText reads r, at most MaxSize bytes, and writes it to w through AppendText. Input past MaxSize is not read;
// a line saying so ends the text.
func WriteText(w io.Writer, r io.Reader, indent int) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return err
	}
	cut := len(b) > MaxSize
	if cut {
		b = b[:MaxSize]
	}
	out := AppendText(nil, b, indent)
	if cut {
		out = AppendText(out, fmt.Appendf(nil, "[cut: more than %d bytes]", MaxSize), indent)
	}
	_, err = w.Write(out)
	return err
}
