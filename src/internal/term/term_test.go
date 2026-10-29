package term

import (
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPTY opens a pseudo-terminal: the master (the "keyboard and screen" side) and the terminal the TUI would get. It
// skips the test where there is none (a build container without /dev/ptmx).
func openPTY(t *testing.T) (master, tty *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	unlock := int32(0)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("unlockpt: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("ptsname: %v", e)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open the terminal: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

func TestTTY(t *testing.T) {
	m, s := openPTY(t)
	if !IsTerminal(int(s.Fd())) {
		t.Fatal("the pseudo-terminal is no terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(int(f.Fd())) {
		t.Error("a file is a terminal")
	}
	if _, err := NewTTY(int(f.Fd())); err == nil {
		t.Error("NewTTY on a file passed")
	}

	tty, err := NewTTY(int(s.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := getTermios(int(s.Fd()))
	if before.Lflag&syscall.ICANON == 0 {
		t.Skip("the pseudo-terminal doesn't start in canonical mode")
	}
	if err := tty.MakeRaw(); err != nil {
		t.Fatal(err)
	}
	raw, _ := getTermios(int(s.Fd()))
	if raw.Lflag&(syscall.ICANON|syscall.ECHO|syscall.ISIG) != 0 || raw.Iflag&(syscall.ICRNL|syscall.IXON) != 0 || raw.Oflag&syscall.OPOST != 0 {
		t.Errorf("not raw: lflag %#x iflag %#x oflag %#x", raw.Lflag, raw.Iflag, raw.Oflag)
	}
	// In raw mode a key arrives at once, without Enter, and Ctrl-C is a byte, not a signal.
	if _, err := m.Write([]byte("a\x03")); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		var b []byte
		buf := make([]byte, 8)
		for len(b) < 2 {
			n, err := s.Read(buf)
			b = append(b, buf[:n]...)
			if err != nil {
				break
			}
		}
		got <- string(b)
	}()
	select {
	case g := <-got:
		if g != "a\x03" {
			t.Errorf("read %q in raw mode, want \"a\\x03\"", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing read in raw mode within 5 s") // the cleanup closes the master: the read ends
	}
	if err := tty.Restore(); err != nil {
		t.Fatal(err)
	}
	after, _ := getTermios(int(s.Fd()))
	if *after != *before {
		t.Errorf("Restore didn't put the mode back: %+v, want %+v", *after, *before)
	}
	if err := tty.Restore(); err != nil {
		t.Errorf("a second Restore: %v", err)
	}

	// The size the window has.
	ws := winsize{Row: 31, Col: 97}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		t.Fatal(e)
	}
	if w, h, err := tty.Size(); err != nil || w != 97 || h != 31 {
		t.Errorf("Size = %d x %d, %v; want 97 x 31", w, h, err)
	}
}
