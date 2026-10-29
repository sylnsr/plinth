package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/ollama/ollamatest"
	"plinth/internal/ollamamcp"
)

// stopGateway stops work's Ollama gateway when the test ends, after the controller (cleanups run last first, so call
// it before start).
func stopGateway(t *testing.T, work string) {
	t.Cleanup(func() { ollamamcp.Stop(work) })
}

// TestOllamaGateway: with ollama.host set the controller starts the Ollama gateway at its start, starts it again when
// it is gone, and leaves it running when it stops; the next start finds it running.
func TestOllamaGateway(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman")
	}
	defer func(d time.Duration) { gatewayCheck = d }(gatewayCheck)
	gatewayCheck = 200 * time.Millisecond
	s := ollamatest.New(t)
	work := setup(t)
	stopGateway(t, work)
	cfg := globalCfg + "ollama:\n  host: " + s.URL + "\n  models:\n    review: rev\n    dlp: dlp\n"
	if err := os.WriteFile(filepath.Join(work, config.FileName), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := ollamamcp.SocketPath(work)
	stderr, stop := start(t, Options{Work: work})
	eventually(t, "the gateway started", logLines(work, 1, `msg="the Ollama gateway started"`, "socket="+sock))
	c := ollamamcp.NewClient(work, true)
	if v, err := c.Version(context.Background()); err != nil || v != "0.0.0-test" {
		t.Fatalf("Version: %q %v", v, err)
	}
	if err := ollamamcp.Stop(work); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the gateway started again", logLines(work, 2, `msg="the Ollama gateway started"`, "socket="+sock))
	if v, err := c.Version(context.Background()); err != nil || v != "0.0.0-test" {
		t.Errorf("Version after the restart: %q %v", v, err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if st, err := ollamamcp.State(work); err != nil || !st.Running {
		t.Errorf("after the controller's stop: %+v %v", st, err)
	}
	if strings.Contains(stderr.String(), "Ollama gateway") {
		t.Errorf("stderr: %q", stderr.String())
	}
	start(t, Options{Work: work})
	eventually(t, "the gateway found running", logLines(work, 1, `msg="the Ollama gateway running"`, "socket="+sock))
}
