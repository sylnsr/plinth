package term

import (
	"os"
	"syscall"
	"testing"
)

func TestParseStat(t *testing.T) {
	ppid, pgrp, sid, err := parseStat("4242 (a) b (c) R 17 4242 9 34816 4242 4194304 107 0\n")
	if err != nil || ppid != 17 || pgrp != 4242 || sid != 9 {
		t.Errorf("hostile comm: %d %d %d %v", ppid, pgrp, sid, err)
	}
	for _, bad := range []string{"", "4242 a R 17 4242 9", "4242 (a) R 17", "4242 (a) R x 4242 9"} {
		if _, _, _, err := parseStat(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestProcStat(t *testing.T) {
	ppid, pgrp, _, err := procStat(os.Getpid())
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	if ppid != os.Getppid() || pgrp != syscall.Getpgrp() {
		t.Errorf("parent %d, group %d; want %d, %d", ppid, pgrp, os.Getppid(), syscall.Getpgrp())
	}
	JobControl() // either answer: it depends on how the test was started
}
