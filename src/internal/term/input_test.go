package term

import (
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestInput: Input reads what comes; paused, it leaves the bytes to whoever else reads the fd (a child given the
// terminal), and reads again after Resume; the end of the input is io.EOF.
func TestInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	fd := int(r.Fd()) // blocking from here on, as a terminal is
	in := NewInput(fd)
	got := make(chan string, 4)
	go func() {
		buf := make([]byte, 16)
		for {
			n, err := in.Read(buf)
			if err != nil {
				got <- "error: " + err.Error()
				return
			}
			got <- string(buf[:n])
		}
	}()
	next := func() string {
		select {
		case s := <-got:
			return s
		case <-time.After(5 * time.Second):
			return "timeout"
		}
	}
	w.Write([]byte("a"))
	if s := next(); s != "a" {
		t.Errorf("before Pause: %q", s)
	}
	in.Pause()
	w.Write([]byte("child"))
	time.Sleep(3 * pollWait)
	buf := make([]byte, 16)
	if n, err := syscall.Read(fd, buf); err != nil || string(buf[:n]) != "child" {
		t.Errorf("the child's read: %q, %v", buf[:n], err)
	}
	select {
	case s := <-got:
		t.Errorf("Input read %q while paused", s)
	default:
	}
	in.Resume()
	w.Write([]byte("b"))
	if s := next(); s != "b" {
		t.Errorf("after Resume: %q", s)
	}
	w.Close()
	if s := next(); s != "error: "+io.EOF.Error() {
		t.Errorf("at the end: %q", s)
	}
}
