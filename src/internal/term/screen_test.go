package term

import (
	"strings"
	"testing"
)

func lines(texts ...string) []*Line {
	var ls []*Line
	for _, s := range texts {
		ls = append(ls, (&Line{}).Text(s, Style{}))
	}
	return ls
}

func TestScreen(t *testing.T) {
	var out strings.Builder
	s := NewScreen(&out, true)
	s.Resize(20, 4)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != enterSeq {
		t.Errorf("Enter wrote %q", got)
	}

	// The first frame clears the screen and draws every row, the blank ones too.
	out.Reset()
	if err := s.Draw(lines("one", "two")); err != nil {
		t.Fatal(err)
	}
	want := clearSeq + "\x1b[1;1Hone" + eolSeq + "\x1b[2;1Htwo" + eolSeq + "\x1b[3;1H" + eolSeq + "\x1b[4;1H" + eolSeq
	if got := out.String(); got != want {
		t.Errorf("first frame:\n%q\nwant\n%q", got, want)
	}

	// The next one only the rows that changed; an unchanged frame writes nothing.
	out.Reset()
	if err := s.Draw(lines("one", "TWO", "", "four")); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "\x1b[2;1HTWO"+eolSeq+"\x1b[4;1Hfour"+eolSeq; got != want {
		t.Errorf("second frame: %q, want %q", got, want)
	}
	out.Reset()
	if err := s.Draw(lines("one", "TWO", "", "four")); err != nil || out.Len() != 0 {
		t.Errorf("an unchanged frame wrote %q, %v", out.String(), err)
	}

	// Lines are cut to the width; lines past the last row are not drawn; styles are reset at the end of each row. A row
	// that takes the whole width gets no erase after it: with autowrap off it would erase the last column.
	out.Reset()
	l := (&Line{}).Text("bold", Style{Bold: true}).Text(" red", Style{FG: Red}).Text(strings.Repeat("x", 30), Style{})
	if err := s.Draw([]*Line{l, nil, nil, nil, (&Line{}).Text("five", Style{})}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "\x1b[1;1H\x1b[0;1mbold\x1b[0;31m red\x1b[0mxxxxxxxxxxx>\x1b[2;1H") {
		t.Errorf("styled, cut row: %q", got)
	}
	if strings.Contains(got, "five") || strings.Contains(got, "\x1b[5;1H") {
		t.Errorf("a row past the last was drawn: %q", got)
	}

	// After a resize everything is drawn again.
	out.Reset()
	s.Resize(10, 2)
	if err := s.Draw(lines("one", "TWO")); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), clearSeq+"\x1b[1;1Hone"+eolSeq+"\x1b[2;1HTWO"+eolSeq; got != want {
		t.Errorf("after a resize: %q, want %q", got, want)
	}

	out.Reset()
	if err := s.Leave(); err != nil || out.String() != leaveSeq {
		t.Errorf("Leave wrote %q, %v", out.String(), err)
	}
}

func TestNoColor(t *testing.T) {
	var out strings.Builder
	s := NewScreen(&out, false)
	s.Resize(20, 1)
	l := (&Line{}).Text("a", Style{FG: Red}).Text("b", Style{FG: Green, Bold: true})
	if err := s.Draw([]*Line{l}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, ";3") || !strings.Contains(got, "a\x1b[0;1mb") {
		t.Errorf("without colour: %q", got)
	}
	// A bar (a background colour) is reverse video without colour, and black on its colour with it: a basic one, or
	// a 24-bit one.
	bar := (&Line{}).Text("keys", Style{FG: Black, BG: Magenta})
	var b strings.Builder
	bar.render(&b, false)
	if got := b.String(); got != "\x1b[0;7mkeys\x1b[0m" {
		t.Errorf("a bar without colour: %q", got)
	}
	b.Reset()
	bar.render(&b, true)
	if got := b.String(); got != "\x1b[0;30;45mkeys\x1b[0m" {
		t.Errorf("a bar: %q", got)
	}
	b.Reset()
	(&Line{}).Text("keys", Style{FG: Black, BG: RGB(0xa863f8), Bold: true}).render(&b, true)
	if got := b.String(); got != "\x1b[0;1;30;48;2;168;99;248mkeys\x1b[0m" {
		t.Errorf("a 24-bit bar: %q", got)
	}
	// The TUI's bar: black as RGB too, which no terminal brightens for bold as it does palette black (Konsole).
	b.Reset()
	(&Line{}).Text("keys", Style{FG: RGB(0), BG: RGB(0xa863f8), Bold: true}).render(&b, true)
	if got := b.String(); got != "\x1b[0;1;38;2;0;0;0;48;2;168;99;248mkeys\x1b[0m" {
		t.Errorf("an RGB-black bar: %q", got)
	}
}
