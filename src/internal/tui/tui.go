// Package tui is plinthctl tui, Plinth's terminal UI: the web UI's screens and actions in the user's own terminal on the
// host, like k9s for kubectl. It is a process of its own. It reads the files the commands read, through the same
// packages, and writes nothing but request files and its lines in the general log; every action asks decide first, as
// the controller will when it carries the request out. The Image screen runs plinthctl's image commands, which do their own
// writing.
//
// A Model holds the state. Key updates it and calls the Store for its I/O (loading, queueing); View draws it as lines,
// which reach the terminal through package term, every string from a file through term's sanitizer. So the screens
// are tested without a terminal. Run is the loop around them: raw mode, the alternate screen, keys, signals, refresh.
// Spec: plinth-tui.README.md.
package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"plinth/internal/config"
	"plinth/internal/logfile"
	"plinth/internal/term"
)

// Options is what plinthctl tui passes to Run.
type Options struct {
	Work      string
	Namespace string // -n: start filtered to it
	Version   string
	Getenv    config.Getenv
	In, Out   *os.File
	Color     bool   // false with NO_COLOR
	Self      string // the plinthctl the Image screen runs; "" for this one (tests set a fake)
}

// ErrNoTerminal: stdin or stdout is not a terminal.
var ErrNoTerminal = errors.New("needs a terminal")

// PanicError is a panic Run recovered: the terminal is restored first, then the caller reports it (stderr, exit 1).
type PanicError struct {
	Value any
	Stack []byte
}

func (p *PanicError) Error() string { return fmt.Sprintf("panic: %v\n%s", p.Value, p.Stack) }

// Run runs the TUI until the user quits (q, Ctrl-C) or a SIGTERM, SIGHUP or SIGINT comes, and always restores the
// terminal: on a panic too. Ctrl-Z restores it and stops the process; SIGCONT enters raw mode and the alternate
// screen again and redraws. Without job control, Ctrl-Z only says so: nothing would continue the process. The Image
// screen's commands get the terminal while they run (handover).
func Run(o Options) (err error) {
	if !term.IsTerminal(int(o.In.Fd())) || !term.IsTerminal(int(o.Out.Fd())) {
		return ErrNoTerminal
	}
	store := &Store{Work: o.Work, Getenv: o.Getenv, Now: time.Now}
	m, err := New(store, o.Version, o.Namespace)
	if err != nil {
		return err
	}
	tty, err := term.NewTTY(int(o.In.Fd()))
	if err != nil {
		return err
	}
	scr := term.NewScreen(o.Out, o.Color)
	enter := func() error {
		if err := tty.MakeRaw(); err != nil {
			return err
		}
		return scr.Enter()
	}
	leave := func() {
		scr.Leave()
		tty.Restore()
	}
	if err := enter(); err != nil {
		tty.Restore()
		return err
	}
	defer func() {
		p := recover()
		leave()
		if p != nil {
			err = &PanicError{Value: p, Stack: debug.Stack()}
			logfile.New(o.Work, logfile.TUI).Error("panic", "value", fmt.Sprint(p))
		}
	}()

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGCONT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(sigs)
	keys := make(chan term.Key, 64)
	in := term.NewInput(int(o.In.Fd())) // paused while a command has the terminal
	go term.ReadKeys(in, keys, term.EscWait)
	resize := func() {
		if w, h, err := tty.Size(); err == nil {
			scr.Resize(w, h)
		}
	}
	resize()

	// The Image screen's commands get the terminal: the input pauses (first, so no read already under way takes the
	// command's first byte), the screen leaves, the command runs in cooked mode as if typed, and the user reads its output
	// and presses Enter. A signal meanwhile is the command's (Ctrl-C, Ctrl-\), except SIGTERM and SIGHUP, which end the
	// TUI as soon as the command has. Afterwards the terminal gets back the mode from before the command, whatever the
	// command left (podman login killed at its password prompt leaves echo off).
	var quitSig bool
	var enterErr error
	handover := func(cmd *exec.Cmd) (int, error) {
		in.Pause()
		leave()
		signal.Notify(sigs, syscall.SIGQUIT)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = o.In, o.Out, os.Stderr
		err := cmd.Run()
		tty.Reset()
		code, status := -1, ""
		if cmd.ProcessState != nil {
			code, status = cmd.ProcessState.ExitCode(), cmd.ProcessState.String()
		} else if err != nil {
			status = err.Error()
		}
		fmt.Fprintf(o.Out, "\n[plinthctl tui] %s. Press Enter to go back.", status)
		quitSig = waitEnter(int(o.In.Fd()), sigs)
		signal.Reset(syscall.SIGQUIT)
		for drained := false; !drained; {
			select {
			case s := <-sigs:
				quitSig = quitSig || s == syscall.SIGTERM || s == syscall.SIGHUP
			default:
				drained = true
			}
		}
		enterErr = enter()
		resize()
		in.Resume()
		return code, err
	}
	self, serr := o.Self, error(nil)
	if self == "" {
		self, serr = os.Executable()
	}
	if serr == nil {
		store.Images = &hostImages{work: o.Work, self: self, handover: handover}
	}
	tick := time.NewTicker(time.Duration(m.RefreshSeconds()) * time.Second)
	defer tick.Stop()
	stopped := false // after Ctrl-Z until SIGCONT: nothing is drawn
	for {
		if !stopped {
			w, h := scr.Size()
			if err := scr.Draw(m.View(w, h)); err != nil {
				return err
			}
		}
		select {
		case k, ok := <-keys:
			if !ok {
				return nil // stdin is gone
			}
			if !stopped {
				m.Key(k)
			}
		case s := <-sigs:
			switch s {
			case syscall.SIGWINCH:
				resize()
			case syscall.SIGCONT:
				if err := enter(); err != nil {
					return err
				}
				resize()
				stopped = false
				m.Reload()
			default:
				return nil
			}
		case <-tick.C:
			if !stopped {
				m.Reload()
			}
		case f := <-m.Async():
			f()
		}
		if enterErr != nil {
			return enterErr
		}
		if m.Quit() || quitSig {
			return nil
		}
		if m.Suspended() {
			if !term.JobControl() {
				m.say(errStyle, "not suspended: no job control (nothing could continue plinthctl)")
				continue
			}
			leave()
			stopped = true
			syscall.Kill(0, syscall.SIGTSTP) // the process group stops here, as for Ctrl-Z in a shell; SIGCONT goes on
		}
	}
}

// waitEnter reads the terminal, in cooked mode, until a line ends: the user has read a command's output. A SIGTERM or
// SIGHUP, during the command or now, ends the wait at once (quit); any other signal is dropped (Ctrl-C there: only Enter
// goes back).
func waitEnter(fd int, sigs <-chan os.Signal) (quit bool) {
	b := make([]byte, 1)
	for {
		select {
		case s := <-sigs:
			if s == syscall.SIGTERM || s == syscall.SIGHUP {
				return true
			}
			continue
		default:
		}
		ready, err := term.Readable(fd, 100*time.Millisecond)
		if err != nil {
			return false
		}
		if !ready {
			continue
		}
		n, err := syscall.Read(fd, b)
		if err == syscall.EINTR || err == syscall.EAGAIN {
			continue
		}
		if err != nil || n == 0 || b[0] == '\n' || b[0] == '\r' {
			return false
		}
	}
}
