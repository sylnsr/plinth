package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the TUI itself when PLINTH_TUI_HELPER=1: TestHandover starts the test binary that way in tmux, with a
// fake plinthctl (PLINTH_TUI_SELF) for the Image screen's commands.
func TestMain(m *testing.M) {
	if os.Getenv("PLINTH_TUI_HELPER") == "1" {
		err := Run(Options{Work: os.Getenv("PLINTH_TUI_WORK"), Version: "test", Getenv: os.LookupEnv, In: os.Stdin, Out: os.Stdout,
			Color: true, Self: os.Getenv("PLINTH_TUI_SELF")})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakePlinthctl stands in for plinthctl on the Image screen: image list prints an empty list; image login asks for a
// password the way podman does (echo off while it reads), so Ctrl-C there leaves echo off. Each run writes its parent's
// PID, the TUI's, to $PLINTH_TUI_PIDFILE.
const fakePlinthctl = `#!/bin/bash
echo "$PPID" > "$PLINTH_TUI_PIDFILE"
case "$2" in
list) printf 'repo: x\npin: ""\nprotection: {protected: true, required-approvals: 1}\nimages: []\n' ;;
login) printf 'Password: '; stty -echo; read -r pw; stty echo; echo; echo "logged in" ;;
esac
`

// TestHandover drives the TUI in a real terminal (tmux, on a server of its own) through the Image screen's hand-over:
// Ctrl-C at the command's password prompt leaves the user's terminal as it was, echo on, at the
// "Press Enter" prompt too and after the TUI quits; a SIGTERM while the command runs ends the TUI once the command has,
// without Enter; a header that fills the width keeps its last column. Skipped without tmux.
func TestHandover(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, work := setup(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "plinthctl")
	write(t, fake, fakePlinthctl)
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "pid")
	sock := "plinth-test-" + strconv.Itoa(os.Getpid())
	tmux := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-L", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		return string(out), err
	}
	socket := "" // the server's socket file: kill-server can leave it behind (cosmic-galaxy-19)
	t.Cleanup(func() {
		tmux("kill-server")
		if socket != "" {
			os.Remove(socket)
		}
	})

	// session starts the TUI in a pane of its own; after the TUI the shell writes the terminal's mode to stty.
	session := func(name string) (screen func() string, send func(keys ...string), waitFor func(string), stty string) {
		stty = filepath.Join(dir, name+".stty")
		cmd := fmt.Sprintf("env PLINTH_TUI_HELPER=1 PLINTH_TUI_WORK=%s PLINTH_TUI_SELF=%s PLINTH_TUI_PIDFILE=%s %s; stty -a > %s; echo EXITED; sleep 30",
			work, fake, pidFile, self, stty)
		if out, err := tmux("new-session", "-d", "-s", name, "-x", "100", "-y", "30", cmd); err != nil {
			t.Fatalf("tmux new-session: %v\n%s", err, out)
		}
		if out, err := tmux("display-message", "-p", "-t", name, "#{socket_path}"); err == nil {
			socket = strings.TrimSpace(out)
		}
		screen = func() string {
			out, _ := tmux("capture-pane", "-p", "-t", name)
			return out
		}
		send = func(keys ...string) {
			if out, err := tmux(append([]string{"send-keys", "-t", name}, keys...)...); err != nil {
				t.Fatalf("tmux send-keys %v: %v\n%s", keys, err, out)
			}
		}
		waitFor = func(s string) {
			t.Helper()
			for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
				if strings.Contains(screen(), s) {
					return
				}
			}
			t.Fatalf("no %q on the screen:\n%s", s, screen())
		}
		return screen, send, waitFor, stty
	}
	echoOn := func(stty string) {
		t.Helper()
		b, err := os.ReadFile(stty)
		fields := strings.Fields(string(b))
		if err != nil || !slices.Contains(fields, "echo") || slices.Contains(fields, "-echo") || !slices.Contains(fields, "icanon") {
			t.Errorf("the terminal after the TUI: %v\n%s", err, b)
		}
	}

	// Ctrl-C at the password prompt.
	screen, send, waitFor, stty := session("ctrlc")
	waitFor("not running")
	for _, l := range strings.Split(screen(), "\n") {
		if strings.Contains(l, "RUN") && strings.Contains(l, "NAMESPACE") && !strings.HasSuffix(strings.TrimRight(l, " "), "AGE") {
			t.Errorf("the Runs header lost its last column: %q", l)
		}
	}
	send("I")
	waitFor("Image michael/devops")
	send("l")
	waitFor("Password:")
	send("C-c")
	waitFor("Press Enter to go back.")
	send("x") // echoed: the terminal has its mode back already
	waitFor("go back.x")
	send("Enter")
	waitFor("Image michael/devops")
	send("q") // back to Runs
	send("Q") // quit: only Q quits
	waitFor("EXITED")
	echoOn(stty)

	// SIGTERM while the command runs: the TUI ends once it has, without Enter.
	_, send, waitFor, stty = session("term")
	waitFor("not running")
	send("I")
	waitFor("Image michael/devops")
	send("l")
	waitFor("Password:")
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	send("C-c")
	waitFor("EXITED")
	echoOn(stty)
}
