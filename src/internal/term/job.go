package term

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// JobControl reports whether a Ctrl-Z can stop the process and a shell continue it: its process group isn't orphaned.
// The kernel drops SIGTSTP to an orphaned group (POSIX), as when a shell without job control started the process
// (sh -c, ssh host cmd, script -c); a TUI that waited for SIGCONT there would wait for ever. It walks up the parents
// inside the process group to the first one outside it: in the same session, that is the shell running the group as a
// job.
func JobControl() bool {
	ppid, pgrp, sid, err := procStat(os.Getpid())
	for range 64 { // a bound in case /proc changes under the walk
		if err != nil || ppid <= 0 {
			return false
		}
		var pp, pg, ps int
		if pp, pg, ps, err = procStat(ppid); err != nil {
			return false
		}
		if pg != pgrp {
			return ps == sid
		}
		ppid = pp
	}
	return false
}

// procStat is a process's parent, process group and session, from /proc/<pid>/stat.
func procStat(pid int) (ppid, pgrp, sid int, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0, err
	}
	return parseStat(string(b))
}

// parseStat parses a /proc/<pid>/stat line: "pid (comm) state ppid pgrp session ...". comm may hold spaces and
// parentheses, so the fields are counted from its last ")".
func parseStat(s string) (ppid, pgrp, sid int, err error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, 0, errors.New("stat: no command name")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 4 {
		return 0, 0, 0, fmt.Errorf("stat: %d fields", len(f))
	}
	var n [3]int
	for j := range n {
		if n[j], err = strconv.Atoi(f[j+1]); err != nil {
			return 0, 0, 0, fmt.Errorf("stat: %w", err)
		}
	}
	return n[0], n[1], n[2], nil
}
