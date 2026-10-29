package term

import (
	"io"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// pollWait is how long Input waits for a byte before it looks at Pause again: the most a Pause waits.
const pollWait = 50 * time.Millisecond

// Input reads a terminal's fd for ReadKeys, and can stop reading while another process has the terminal: a read
// blocked in the kernel would take that process's first keys. It never blocks in read(2): it waits for a byte with
// select(2), at most pollWait at a time, and reads only what is there. Pause returns once no read is under way, and no
// read starts until Resume.
type Input struct {
	fd     int
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
}

// NewInput reads fd.
func NewInput(fd int) *Input {
	in := &Input{fd: fd}
	in.cond = sync.NewCond(&in.mu)
	return in
}

// Read waits, while the input isn't paused, until a byte comes, and reads what is there; io.EOF at the end.
func (in *Input) Read(p []byte) (int, error) {
	for {
		in.mu.Lock()
		for in.paused {
			in.cond.Wait()
		}
		ready, err := Readable(in.fd, pollWait)
		if err != nil {
			in.mu.Unlock()
			return 0, err
		}
		if !ready {
			in.mu.Unlock()
			continue
		}
		n, err := syscall.Read(in.fd, p)
		in.mu.Unlock()
		switch {
		case err == syscall.EINTR || err == syscall.EAGAIN:
			continue
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		}
		return n, nil
	}
}

// Pause stops reading: it returns once no read is under way.
func (in *Input) Pause() {
	in.mu.Lock()
	in.paused = true
	in.mu.Unlock()
}

// Resume reads again.
func (in *Input) Resume() {
	in.mu.Lock()
	in.paused = false
	in.mu.Unlock()
	in.cond.Broadcast()
}

// Readable waits up to d for fd to have a byte to read (or its end).
func Readable(fd int, d time.Duration) (bool, error) {
	var set syscall.FdSet
	bits := int(unsafe.Sizeof(set.Bits[0])) * 8
	set.Bits[fd/bits] |= 1 << (uint(fd) % uint(bits))
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	n, err := syscall.Select(fd+1, &set, nil, nil, &tv)
	if err == syscall.EINTR {
		return false, nil
	}
	return n > 0, err
}
