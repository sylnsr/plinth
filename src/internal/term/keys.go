package term

import (
	"io"
	"time"
	"unicode"
	"unicode/utf8"
)

// KeyCode says which key was pressed. A character is KeyRune with the rune; Ctrl plus a letter is KeyCtrl with the
// letter ('a'-'z'), other than the letters that are keys of their own (Tab, Enter, Backspace).
type KeyCode int

const (
	KeyRune KeyCode = iota
	KeyCtrl
	KeyEnter
	KeyTab
	KeyBackspace
	KeyEsc
	KeyUp
	KeyDown
	KeyRight
	KeyLeft
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
	KeyInsert
	KeyDelete
	KeyUnknown // a byte or sequence that is no key here: ignored
)

// Key is one key press.
type Key struct {
	Code KeyCode
	Rune rune
}

var keyNames = map[KeyCode]string{
	KeyEnter: "Enter", KeyTab: "Tab", KeyBackspace: "Backspace", KeyEsc: "Esc", KeyUp: "Up", KeyDown: "Down",
	KeyRight: "Right", KeyLeft: "Left", KeyHome: "Home", KeyEnd: "End", KeyPgUp: "PgUp", KeyPgDn: "PgDn",
	KeyInsert: "Insert", KeyDelete: "Delete", KeyUnknown: "Unknown",
}

func (k Key) String() string {
	switch k.Code {
	case KeyRune:
		return string(k.Rune)
	case KeyCtrl:
		return "Ctrl-" + string(unicode.ToUpper(k.Rune))
	}
	return keyNames[k.Code]
}

// EscWait is how long a lone ESC waits for the rest of a sequence before it counts as Esc (plinth-tui.README.md, Keys).
const EscWait = 50 * time.Millisecond

// maxSeq bounds a CSI sequence; a longer one is no key.
const maxSeq = 32

// Decoder turns the bytes a terminal sends into keys. Feed returns the keys complete so far and keeps an incomplete
// sequence (an ESC that may start one, part of a UTF-8 character); Pending reports one. Flush, called when no byte
// followed within EscWait, ends it: a lone ESC is Esc, the rest is decoded as typed.
type Decoder struct {
	buf []byte
}

// Feed decodes p after what is pending.
func (d *Decoder) Feed(p []byte) []Key {
	d.buf = append(d.buf, p...)
	return d.decode(false)
}

// Pending reports whether an incomplete sequence waits for more bytes.
func (d *Decoder) Pending() bool { return len(d.buf) > 0 }

// Flush ends what is pending.
func (d *Decoder) Flush() []Key { return d.decode(true) }

func (d *Decoder) decode(final bool) []Key {
	var keys []Key
	for len(d.buf) > 0 {
		k, n := decodeKey(d.buf, final)
		if n == 0 {
			break
		}
		if k.Code != KeyUnknown {
			keys = append(keys, k)
		}
		d.buf = d.buf[n:]
	}
	if len(d.buf) == 0 {
		d.buf = nil
	}
	return keys
}

// decodeKey decodes the key at the start of b and the bytes it took; 0 bytes when b holds only the start of one and
// more may follow (not final).
func decodeKey(b []byte, final bool) (Key, int) {
	c := b[0]
	switch {
	case c == 0x1b:
		return decodeEsc(b, final)
	case c == '\r' || c == '\n':
		return Key{Code: KeyEnter}, 1
	case c == '\t':
		return Key{Code: KeyTab}, 1
	case c == 0x7f || c == 0x08:
		return Key{Code: KeyBackspace}, 1
	case c >= 0x01 && c <= 0x1a:
		return Key{Code: KeyCtrl, Rune: rune('a' + c - 1)}, 1
	case c < 0x20:
		return Key{Code: KeyUnknown}, 1
	case c < utf8.RuneSelf:
		return Key{Code: KeyRune, Rune: rune(c)}, 1
	}
	if !utf8.FullRune(b) {
		if final {
			return Key{Code: KeyUnknown}, len(b)
		}
		return Key{}, 0
	}
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError || !unicode.IsPrint(r) || hidden(r) {
		return Key{Code: KeyUnknown}, n
	}
	return Key{Code: KeyRune, Rune: r}, n
}

// decodeEsc decodes what starts with ESC: a CSI sequence (ESC [), an SS3 one (ESC O, what arrow keys send in the
// application cursor mode), or Esc. Any other byte after ESC is a key of its own (Alt plus a key reads as Esc, then the
// key).
func decodeEsc(b []byte, final bool) (Key, int) {
	if len(b) == 1 {
		if final {
			return Key{Code: KeyEsc}, 1
		}
		return Key{}, 0
	}
	switch b[1] {
	case '[':
		return decodeCSI(b, final)
	case 'O':
		if len(b) == 2 {
			if final {
				return Key{Code: KeyEsc}, 1
			}
			return Key{}, 0
		}
		return Key{Code: cursorKey(b[2])}, 3
	}
	return Key{Code: KeyEsc}, 1
}

// decodeCSI decodes ESC [ <parameter and intermediate bytes> <final byte>.
func decodeCSI(b []byte, final bool) (Key, int) {
	i := 2
	for ; i < len(b) && i < maxSeq; i++ {
		c := b[i]
		switch {
		case c >= 0x40 && c <= 0x7e:
			return Key{Code: csiKey(b[2:i], c)}, i + 1
		case c < 0x20 || c > 0x3f: // not a parameter or intermediate byte: no sequence; that byte is a key of its own
			return Key{Code: KeyUnknown}, i
		}
	}
	if i >= maxSeq {
		return Key{Code: KeyUnknown}, i
	}
	if final {
		return Key{Code: KeyEsc}, 1
	}
	return Key{}, 0
}

func cursorKey(c byte) KeyCode {
	switch c {
	case 'A':
		return KeyUp
	case 'B':
		return KeyDown
	case 'C':
		return KeyRight
	case 'D':
		return KeyLeft
	case 'H':
		return KeyHome
	case 'F':
		return KeyEnd
	}
	return KeyUnknown
}

// csiKey maps a CSI sequence to its key. Modifiers (ESC [ 1 ; 5 A, Ctrl-Up) are ignored: the key is the key.
func csiKey(params []byte, final byte) KeyCode {
	if final != '~' {
		return cursorKey(final)
	}
	n := 0
	for _, c := range params {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		if n > 99 {
			return KeyUnknown
		}
	}
	switch n {
	case 1, 7:
		return KeyHome
	case 2:
		return KeyInsert
	case 3:
		return KeyDelete
	case 4, 8:
		return KeyEnd
	case 5:
		return KeyPgUp
	case 6:
		return KeyPgDn
	}
	return KeyUnknown
}

// ReadKeys reads r until a read fails and sends every key on keys; a lone ESC is sent as Esc once wait passed without
// another byte. It closes keys and returns the read error (io.EOF at the end of the input).
func ReadKeys(r io.Reader, keys chan<- Key, wait time.Duration) error {
	chunks := make(chan []byte)
	errc := make(chan error, 1)
	go func() {
		for {
			buf := make([]byte, 256)
			n, err := r.Read(buf)
			if n > 0 {
				chunks <- buf[:n]
			}
			if err != nil {
				errc <- err
				close(chunks)
				return
			}
		}
	}()
	defer close(keys)
	var d Decoder
	var timeout <-chan time.Time
	for {
		select {
		case p, ok := <-chunks:
			if !ok {
				for _, k := range d.Flush() {
					keys <- k
				}
				return <-errc
			}
			for _, k := range d.Feed(p) {
				keys <- k
			}
			timeout = nil
			if d.Pending() {
				timeout = time.After(wait)
			}
		case <-timeout:
			for _, k := range d.Flush() {
				keys <- k
			}
			timeout = nil
		}
	}
}
