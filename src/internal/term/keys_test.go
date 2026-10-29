package term

import (
	"io"
	"strings"
	"testing"
	"time"
)

func names(keys []Key) string {
	var s []string
	for _, k := range keys {
		s = append(s, k.String())
	}
	return strings.Join(s, " ")
}

func TestDecode(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"abc", "a b c"},
		{"\x1b[A\x1b[B\x1b[C\x1b[D", "Up Down Right Left"},
		{"\x1bOA\x1bOD\x1bOH\x1bOF", "Up Left Home End"},
		{"\x1b[H\x1b[F\x1b[1~\x1b[4~\x1b[7~\x1b[8~", "Home End Home End Home End"},
		{"\x1b[5~\x1b[6~\x1b[2~\x1b[3~", "PgUp PgDn Insert Delete"},
		{"\x1b[1;5A\x1b[5;2~", "Up PgUp"},
		{"\r\n\t\x7f\x08", "Enter Enter Tab Backspace Backspace"},
		{"\x03\x04\x1a\x01", "Ctrl-C Ctrl-D Ctrl-Z Ctrl-A"},
		{"\x00\x1c\x1f", ""},
		{"\x1b[99~\x1b[Z\x1b[?1;2c", ""},
		{"\x1b[12\x07x", "Ctrl-G x"},
		{"h\u00e9 \u65e5", "h \u00e9   \u65e5"},
		{"\xff\xc3(", "("},
		{"\u009b\u202e!", "!"},
		{"\x1bx", "Esc x"},
		{"\x1b\x1b[A", "Esc Up"},
	} {
		var d Decoder
		keys := append(d.Feed([]byte(c.in)), d.Flush()...)
		if got := names(keys); got != c.want {
			t.Errorf("decode %q = %q, want %q", c.in, got, c.want)
		}
		if d.Pending() {
			t.Errorf("decode %q: still pending after Flush", c.in)
		}
	}
}

// TestDecodeSplit: a sequence or UTF-8 character split across reads is one key; a lone ESC waits.
func TestDecodeSplit(t *testing.T) {
	var d Decoder
	for _, c := range []struct {
		in      string
		want    string
		pending bool
	}{
		{"\x1b", "", true},
		{"[", "", true},
		{"5", "", true},
		{"~j", "PgUp j", false},
		{"\xe6\x97", "", true},
		{"\xa5", "日", false},
		{"\x1bO", "", true},
		{"B", "Down", false},
	} {
		if got := names(d.Feed([]byte(c.in))); got != c.want || d.Pending() != c.pending {
			t.Errorf("Feed(%q) = %q, pending %v; want %q, %v", c.in, got, d.Pending(), c.want, c.pending)
		}
	}
	// Flush ends what waits: a lone ESC is Esc; the rest of a sequence that never ended is decoded as typed.
	for _, c := range []struct{ in, want string }{
		{"\x1b", "Esc"}, {"\x1b[", "Esc ["}, {"\x1b[1;", "Esc [ 1 ;"}, {"\x1bO", "Esc O"}, {"\xe6\x97", ""},
	} {
		var d Decoder
		d.Feed([]byte(c.in))
		if got := names(d.Flush()); got != c.want {
			t.Errorf("Flush after %q = %q, want %q", c.in, got, c.want)
		}
	}
	// A CSI sequence that never ends is no key, and doesn't swallow what follows.
	var e Decoder
	if got := names(e.Feed([]byte("\x1b[" + strings.Repeat("1", 40) + "q"))); got != "1 1 1 1 1 1 1 1 1 1 q" {
		t.Errorf("an overlong sequence: %q", got)
	}
}

// TestReadKeys: a lone ESC becomes Esc after the wait; a sequence arriving in pieces within it is one key.
func TestReadKeys(t *testing.T) {
	r, w := io.Pipe()
	keys := make(chan Key, 16)
	done := make(chan error, 1)
	go func() { done <- ReadKeys(r, keys, 500*time.Millisecond) }()
	next := func() string {
		select {
		case k := <-keys:
			return k.String()
		case <-time.After(5 * time.Second):
			return "timeout"
		}
	}
	write := func(s string) {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	write("\x1b")
	if got := next(); got != "Esc" {
		t.Errorf("a lone ESC: %q", got)
	}
	write("a\x1b[")
	write("B")
	if got := next() + " " + next(); got != "a Down" {
		t.Errorf("a split sequence: %q", got)
	}
	w.Close()
	if err := <-done; err != io.EOF {
		t.Errorf("ReadKeys returned %v, want EOF", err)
	}
	if _, open := <-keys; open {
		t.Error("keys is still open")
	}
}
