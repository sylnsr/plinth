package controller

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"plinth/internal/decide"
	"plinth/internal/names"
	"plinth/internal/request"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// Running reports whether a controller holds the lock in work, and the PID it wrote there (0 when it can't be read). It
// tests the lock with a shared flock, which it holds for a moment only when the lock is free; a missing or unreadable
// lock file means not running. The commands and the TUI ask this.
func Running(work string) (bool, int) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return false, 0
	}
	defer root.Close()
	f, err := safefs.OpenFile(root, filepath.FromSlash(LockFile), os.O_RDONLY)
	if err != nil {
		return false, 0
	}
	defer f.Close() // closing releases a lock taken below
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, 0
	}
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return true, pid
}

// Live is the runstate.Live of the commands and the TUI. Without a controller nothing runs, so a running.yml is left over
// (killed). With one, a run is running while its exec request is .running: from right before its script starts until
// the controller has finished the exec (then .finished), at its start too for one an earlier controller left.
func Live(work string, ns names.Namespace) runstate.Live {
	if running, _ := Running(work); !running {
		return nil
	}
	return func(agent, run string) bool {
		_, err := os.Lstat(filepath.Join(work, request.RunDir(ns, agent, run), decide.Exec+"."+request.Running))
		return err == nil
	}
}
