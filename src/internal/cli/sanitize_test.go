package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPTY opens a pseudo-terminal: the master and the terminal a user's shell would hand plinthctl. It skips the test
// where there is none (a build container without /dev/ptmx).
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

// TestTerminalSanitized: what a command prints from a file reaches a terminal only escaped, a script by get ask and a
// file name by describe; piped, get ask stays byte-exact.
func TestTerminalSanitized(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org") // automatic: [security]
	script := "#!/bin/bash\n# AAA script: check the disks\x1b]0;pwned\x07\n# READ-ONLY\nRUN_NAME=\"calm-otter\"\necho \u202eevil\u202c\n"
	fixtureRun(t, work, "calm-otter", map[string]string{"ask.sh": script, "review.security.md": "## Verdict\nOK to run\n",
		"notes\x1b[2J.txt": "x"})
	ns := []string{"-n", "michael/devops", "--work", work}

	if code, out, _ := run(append(append([]string{"get", "ask"}, ns...), "calm-otter")...); code != exitOK || out != script {
		t.Errorf("get ask, piped: %d %q, want it byte-exact", code, out)
	}

	m, tty := openPTY(t)
	var mu sync.Mutex
	var seen strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			mu.Lock()
			seen.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	shown := func(want string) string {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			s := seen.String()
			mu.Unlock()
			if strings.Contains(s, want) {
				return s
			}
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no %q on the terminal: %q", want, seen.String())
		return ""
	}
	if code := Run(append(append([]string{"get", "ask"}, ns...), "calm-otter"), tty, tty); code != exitOK {
		t.Fatalf("get ask on a terminal: exit %d", code)
	}
	s := shown(`echo \u202eevil\u202c`)
	if !strings.Contains(s, `check the disks\x1b]0;pwned\x07`) || strings.ContainsAny(s, "\x1b\x07\u202e\u202c") {
		t.Errorf("get ask on a terminal: %q", s)
	}
	if code := Run(append(append([]string{"describe"}, ns...), "calm-otter"), tty, tty); code != exitOK {
		t.Fatalf("describe on a terminal: exit %d", code)
	}
	if s := shown(`notes\x1b[2J.txt`); strings.ContainsAny(s, "\x1b\x07\u202e\u202c") {
		t.Errorf("describe on a terminal: %q", s)
	}
}

// TestTerminalWrapped: on a terminal 40 columns wide, a script's long line wraps at a space, the continuation row
// starting with the mark (> with LC_ALL=C), and no row is wider than the terminal; piped, it stays one line.
func TestTerminalWrapped(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org")
	long := "echo one two three four five six seven eight nine ten eleven twelve"
	script := "#!/bin/bash\n# AAA script: count\n# READ-ONLY\nRUN_NAME=\"calm-otter\"\n" + long + "\n"
	fixtureRun(t, work, "calm-otter", map[string]string{"ask.sh": script, "review.security.md": "## Verdict\nOK to run\n"})
	args := []string{"get", "ask", "-n", "michael/devops", "--work", work, "calm-otter"}
	if code, out, _ := run(args...); code != exitOK || out != script {
		t.Errorf("get ask, piped: %d %q, want it byte-exact", code, out)
	}

	m, tty := openPTY(t)
	ws := struct{ Row, Col, X, Y uint16 }{24, 40, 0, 0}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		t.Skipf("TIOCSWINSZ: %v", e)
	}
	var mu sync.Mutex
	var seen strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			mu.Lock()
			seen.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	if code := Run(args, tty, tty); code != exitOK {
		t.Fatalf("get ask on a terminal: exit %d", code)
	}
	var s string
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		s = seen.String()
		mu.Unlock()
		if strings.Contains(s, "twelve") {
			break
		}
	}
	if !strings.Contains(s, "seven \r\n> eight nine ten eleven twelve") {
		t.Errorf("not wrapped at a space with the mark: %q", s)
	}
	for _, l := range strings.Split(s, "\n") {
		if n := len([]rune(strings.TrimRight(l, "\r"))); n > 40 {
			t.Errorf("a row of %d columns: %q", n, l)
		}
	}
}
