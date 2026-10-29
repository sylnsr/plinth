package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"plinth/internal/controller"
)

func TestLogsScrub(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	queued := filepath.Join(work, "requests", "logs-scrub.queued")
	scrub := func(answer string) (int, string, string) {
		stdin = strings.NewReader(answer)
		t.Cleanup(func() { stdin = os.Stdin })
		return run("logs scrub", "--work", work)
	}
	// The max age comes from the global config, so it has to load.
	if code, _, errs := scrub(""); code != exitFail || !strings.Contains(errs, "doesn't load") {
		t.Errorf("without a config: %d %q", code, errs)
	}
	writeConfig(t, work, "https://forgejo.example.org")
	if code, out, _ := scrub(""); code != exitOK || !strings.Contains(out, "No log file") {
		t.Errorf("no logs: %d %q", code, out)
	}

	logs := filepath.Join(work, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"plinth-2020-01-01.log", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(logs, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Anything but yes queues nothing.
	code, out, _ := scrub("y\n")
	if code != exitFail || !strings.Contains(out, "plinth-2020-01-01.log") || strings.Contains(out, "notes.txt") || !strings.Contains(out, "Nothing queued") {
		t.Errorf("answer y: %d %q", code, out)
	}
	if _, err := os.Lstat(queued); err == nil {
		t.Fatal("queued without a yes")
	}
	code, out, errs := scrub("yes\n")
	if code != exitOK || !strings.Contains(out, "Are you sure?") || !strings.Contains(out, "Queued.") || !strings.Contains(errs, "controller is not running") {
		t.Errorf("answer yes: %d %q %q", code, out, errs)
	}
	if _, err := os.Lstat(queued); err != nil {
		t.Errorf("not queued: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(logs, "plinth-2020-01-01.log")); err != nil {
		t.Error("logs scrub deleted a file itself; only the controller deletes")
	}
	if code, out, _ := scrub("yes\n"); code != exitOK || !strings.Contains(out, "Already queued.") {
		t.Errorf("again: %d %q", code, out)
	}
}

// TestControllerService: plinthctl controller as a service. start runs it in the background (this test binary
// as plinthctl, see TestMain) and returns once it serves; status says how it is, stop ends it, restart does both; a
// config that doesn't load keeps start from starting, with why; bare controller lists the subcommands.
func TestControllerService(t *testing.T) {
	if code, _, errs := run("controller"); code != exitUsage || !strings.Contains(errs, "name what to do") ||
		!strings.Contains(errs, "  start ") || !strings.Contains(errs, "  run ") {
		t.Errorf("bare controller: %d %q", code, errs)
	}
	if code, _, errs := run("controller", "begin"); code != exitUsage || !strings.Contains(errs, `"begin" is not a subcommand`) {
		t.Errorf("controller begin: %d %q", code, errs)
	}
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org")
	ln, err := net.Listen("tcp", "127.0.0.1:0") // a free port: never plinth.mcp.port, which a controller of the user's may hold
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	f, err := os.OpenFile(filepath.Join(work, "config.yml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "plinth:\n  mcp:\n    listen: 127.0.0.1\n    port: %d\n", port)
	f.Close()
	t.Cleanup(func() {
		if running, pid := controller.Running(work); running && pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)

	if code, out, _ := run("controller", "status", "--work", work); code != exitNotRunning || !strings.Contains(out, "not running - next: plinthctl controller start") {
		t.Errorf("status before a start: %d %q", code, out)
	}
	if code, out, errs := run("controller", "start", "--work", work); code != exitOK ||
		!strings.Contains(out, "plinthctl controller: started (PID ") || !strings.Contains(out, "the Plinth MCP server listens on "+mcpURL) {
		t.Fatalf("start: %d %q %q", code, out, errs)
	}
	running, pid := controller.Running(work)
	if !running || pid == os.Getpid() {
		t.Fatalf("after start: running %v, PID %d (this process %d)", running, pid, os.Getpid())
	}
	if code, out, _ := run("controller", "start", "--work", work); code != exitOK || !strings.Contains(out, fmt.Sprintf("already running (PID %d)", pid)) {
		t.Errorf("a second start: %d %q", code, out)
	}
	code, out, _ := run("controller", "status", "--work", work)
	for _, want := range []string{fmt.Sprintf("plinthctl controller: running (PID %d) since ", pid), "  MCP server:     " + mcpURL,
		"  work:           " + work, "  namespaces:     none yet", "  Ollama gateway: not used", "its last lines:", "msg=started component=controller"} {
		if code != exitOK || !strings.Contains(out, want) {
			t.Errorf("status: %d, no %q in\n%s", code, want, out)
		}
	}

	code, out, errs := run("controller", "restart", "--work", work)
	_, again := controller.Running(work)
	if code != exitOK || !strings.Contains(out, fmt.Sprintf("plinthctl controller: stopped (PID %d)", pid)) ||
		!strings.Contains(out, "plinthctl controller: started (PID ") || again == pid || again == 0 {
		t.Errorf("restart: %d %q %q (PID %d, before %d)", code, out, errs, again, pid)
	}
	if code, out, _ := run("controller", "stop", "--work", work); code != exitOK || out != fmt.Sprintf("plinthctl controller: stopped (PID %d)\n", again) {
		t.Errorf("stop: %d %q", code, out)
	}
	if running, _ := controller.Running(work); running {
		t.Error("still running after stop")
	}
	if code, out, _ := run("controller", "stop", "--work", work); code != exitOK || out != "plinthctl controller: not running\n" {
		t.Errorf("a second stop: %d %q", code, out)
	}

	// A global config that doesn't load: start says why, and nothing runs.
	if err := os.WriteFile(filepath.Join(work, "config.yml"), []byte("forgejo-url: \"<url>\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run("controller", "start", "--work", work); code != exitFail || !strings.Contains(errs, "plinthctl controller: didn't start (") ||
		!strings.Contains(errs, "forgejo-url") {
		t.Errorf("start with a config that doesn't load: %d %q", code, errs)
	}
	if running, _ := controller.Running(work); running {
		t.Error("running after a failed start")
	}
}
