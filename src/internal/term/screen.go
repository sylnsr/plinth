package term

import (
	"fmt"
	"io"
	"strings"
)

// Screen draws frames on a terminal in plain VT100/xterm sequences: the alternate screen, no cursor, no autowrap. Every
// line is placed with its own cursor position and cut to the width, so no line can run into another; a frame rewrites
// only the lines that changed since the one before.
type Screen struct {
	out   io.Writer
	color bool
	w, h  int
	prev  []string // the rows as last drawn; nil: redraw them all
	clear bool
}

// Sequences the screen writes; nothing else that reaches the terminal starts with ESC.
const (
	enterSeq = "\x1b[?1049h\x1b[?25l\x1b[?7l" // alternate screen, hide the cursor, autowrap off
	leaveSeq = "\x1b[0m\x1b[?7h\x1b[?25h\x1b[?1049l"
	clearSeq = "\x1b[2J"
	eolSeq   = "\x1b[0m\x1b[K" // default style, erase to the end of the line
)

// NewScreen returns a screen on out; color false (NO_COLOR) draws no colours.
func NewScreen(out io.Writer, color bool) *Screen {
	return &Screen{out: out, color: color}
}

// Enter switches to the alternate screen, hides the cursor and turns autowrap off. The next Draw draws every line.
func (s *Screen) Enter() error {
	s.prev, s.clear = nil, true
	_, err := io.WriteString(s.out, enterSeq)
	return err
}

// Leave undoes Enter: the user's screen is back as it was.
func (s *Screen) Leave() error {
	_, err := io.WriteString(s.out, leaveSeq)
	return err
}

// Resize sets the size of the frames to come; the next Draw clears the screen and draws every line.
func (s *Screen) Resize(w, h int) {
	s.w, s.h = w, h
	s.prev, s.clear = nil, true
}

// Size is the size set by Resize.
func (s *Screen) Size() (w, h int) { return s.w, s.h }

// Draw draws a frame: lines[i] on row i, cut to the width; rows past the last line are blank, lines past the last row
// are not drawn.
func (s *Screen) Draw(lines []*Line) error {
	rows := make([]string, s.h)
	full := make([]bool, s.h) // the row takes the whole width: no erase after it
	for i := range rows {
		if i < len(lines) && lines[i] != nil {
			var b strings.Builder
			l := lines[i].Cut(0, s.w)
			l.render(&b, s.color)
			rows[i], full[i] = b.String(), l.Width() >= s.w
		}
	}
	var b strings.Builder
	if s.clear {
		b.WriteString(clearSeq)
	}
	for i, r := range rows {
		if s.prev != nil && i < len(s.prev) && s.prev[i] == r {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H", i+1)
		b.WriteString(r)
		// With autowrap off the cursor stays on the last column after a full row, and an erase there would take that
		// column's character (the > of a cut line, the last letter of a header).
		if !full[i] {
			b.WriteString(eolSeq)
		}
	}
	s.prev, s.clear = rows, false
	if b.Len() == 0 {
		return nil
	}
	_, err := io.WriteString(s.out, b.String())
	return err
}
