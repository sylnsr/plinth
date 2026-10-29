// Package term is the terminal under plinthctl tui: raw mode and the window size (ioctl through syscall, no terminfo),
// whether Ctrl-Z can suspend (JobControl), the key decoder, the screen and the sanitizer every byte from a file goes
// through before it reaches the screen (Line.Untrusted). Standard library only. Spec: plinth-tui.README.md, Runtime,
// Untrusted text and Keys.
package term

import (
	"fmt"
	"syscall"
	"unsafe"
)

// TTY is a terminal device.
type TTY struct {
	fd    int
	saved syscall.Termios
	raw   bool
}

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd int) bool {
	_, err := getTermios(fd)
	return err == nil
}

// NewTTY returns the terminal on fd; an error if fd is none.
func NewTTY(fd int) (*TTY, error) {
	t, err := getTermios(fd)
	if err != nil {
		return nil, fmt.Errorf("not a terminal: %w", err)
	}
	return &TTY{fd: fd, saved: *t}, nil
}

// MakeRaw puts the terminal in raw mode (as cfmakeraw): no echo, no line editing, no signals from keys (Ctrl-C and
// Ctrl-Z arrive as keys, so the TUI restores the terminal before it stops), no output processing; a read returns each
// byte as it comes. Restore puts back the mode from before, read again at each MakeRaw (the shell may change it while
// the TUI is stopped).
func (t *TTY) MakeRaw() error {
	cur, err := getTermios(t.fd)
	if err != nil {
		return err
	}
	if !t.raw {
		t.saved = *cur
	}
	raw := *cur
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR |
		syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := setTermios(t.fd, &raw); err != nil {
		return err
	}
	t.raw = true
	return nil
}

// Restore puts back the mode from before MakeRaw; nothing when the terminal isn't raw.
func (t *TTY) Restore() error {
	if !t.raw {
		return nil
	}
	if err := setTermios(t.fd, &t.saved); err != nil {
		return err
	}
	t.raw = false
	return nil
}

// Reset puts back the mode saved at the last MakeRaw, whatever the terminal is in now: after another program had the
// terminal, which may have left it changed (podman login killed at its password prompt leaves echo off).
func (t *TTY) Reset() error {
	if err := setTermios(t.fd, &t.saved); err != nil {
		return err
	}
	t.raw = false
	return nil
}

// Size is the window size in columns and rows.
func (t *TTY) Size() (w, h int, err error) {
	var ws winsize
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0, 0, e
	}
	return int(ws.Col), int(ws.Row), nil
}

// winsize is the kernel's struct winsize.
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

func getTermios(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	return &t, nil
}

func setTermios(fd int, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}
