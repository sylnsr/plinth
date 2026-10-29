package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/doc"
	"plinth/internal/ollama"
	"plinth/internal/ollama/ollamatest"
	"plinth/internal/ollamamcp"
)

// TestMain is the Ollama gateway when its container runs the test binary (TestGatewayCommands), through Run.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ollamamcp.MainArg {
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	// controller start runs its own binary as plinthctl controller run.
	if len(os.Args) > 2 && os.Args[1] == "controller" && os.Args[2] == "run" {
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// serveGateway serves the Ollama gateway for the Ollama at host on work's socket, in the test's process, until the
// test ends.
func serveGateway(t *testing.T, work, host string) {
	t.Helper()
	dir := filepath.Join(work, filepath.FromSlash(ollamamcp.SocketDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := ollamamcp.Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	c := ollama.New(config.Ollama{Host: host, TimeoutSeconds: 5, MaxParallel: 1})
	go func() { done <- ollamamcp.Serve(ctx, ln, ollamamcp.New(ollamamcp.Options{Client: c})) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestOllamaCommands: ollama status and dlp-scan read ollama: from <work>/config.yml alone and ask Ollama through the
// gateway; without it they fail and say how to start it.
func TestOllamaCommands(t *testing.T) {
	work := t.TempDir()
	if code, out, _ := run("ollama", "status", "--work", work); code != exitFail || out != "Ollama: not configured (ollama.host is not set)\n" {
		t.Errorf("not configured: %d %q", code, out)
	}
	s := ollamatest.New(t)
	s.SetModels([]string{"rev", "dlp"}, []string{"dlp"})
	cfg := "ollama:\n  host: " + s.URL + "\n  models:\n    review: rev\n    dlp: dlp\nplinth:\n  repo: \"<owner>/<repo>\"\n"
	if err := os.WriteFile(filepath.Join(work, "config.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "calm-otter", "answer.yml")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: "calm-otter"},
		Sections: []doc.Section{{Output: "password=hunter2\n"}}, Result: doc.ResultOK}
	body := string(a.Encode())
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	notRunning := "the Ollama gateway isn't running (plinthctl ollama-mcp start)"
	if code, out, _ := run("ollama", "status", "--work", work); code != exitFail || out != "Ollama "+s.URL+": unreachable: "+notRunning+"\n" {
		t.Errorf("no gateway: %d %q", code, out)
	}
	if code, _, errs := run("dlp-scan", "--work", work, file); code != exitFail || !strings.Contains(errs, "FAILED (the file is untouched)") || !strings.Contains(errs, notRunning) {
		t.Errorf("dlp-scan without the gateway: %d %q", code, errs)
	}
	if b, _ := os.ReadFile(file); string(b) != body {
		t.Errorf("the file was touched: %q", b)
	}

	serveGateway(t, work, s.URL)
	code, out, errs := run("ollama", "status", "--work", work)
	if code != exitOK || !strings.Contains(out, ": reachable, version 0.0.0-test\n") || !strings.Contains(out, "review model rev: present\n") ||
		!strings.Contains(out, "dlp model dlp: present, loaded\n") {
		t.Errorf("status: %d %q %q", code, out, errs)
	}
	s.SetModels([]string{"rev"}, nil)
	if code, out, _ := run("ollama", "status", "--work", work); code != exitFail || !strings.Contains(out, "dlp model dlp: missing") {
		t.Errorf("a missing model: %d %q", code, out)
	}

	code, out, errs = run("dlp-scan", "--work", work, file)
	if code != exitOK || !strings.Contains(out, "1 fixed-pattern mask(s), 0 line(s) masked, model dlp") {
		t.Fatalf("dlp-scan: %d %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "aaa-raw", "calm-otter", "answer.yml.orig")); string(b) != body {
		t.Errorf("original: %q", b)
	}
	if code, out, _ := run("dlp-scan", "--work", work, file); code != exitOK || !strings.Contains(out, "already scanned") {
		t.Errorf("again: %d %q", code, out)
	}
	if code, _, errs := run("dlp-scan", "--work", t.TempDir(), file+".x"); code != exitFail || !strings.Contains(errs, "FAILED (the file is untouched)") {
		t.Errorf("no file: %d %q", code, errs)
	}
}

// TestReviewCommand: review reads reviewers: and ollama: from <work>/config.yml and asks through the gateway; exit 2
// for a file in done/; without the gateway it fails closed.
func TestReviewCommand(t *testing.T) {
	work := t.TempDir()
	s := ollamatest.New(t)
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		return 200, `{"verdict":"ok-to-run","findings":[]}`
	})
	cfg := "ollama:\n  host: " + s.URL + "\n  models:\n    review: rev\n    dlp: dlp\nreviewers:\n  type:\n    sec:\n      definition: Find danger.\n"
	if err := os.WriteFile(filepath.Join(work, "config.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "calm-otter")
	if err := os.MkdirAll(filepath.Join(dir, "done", "keen-owl"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ask.sh", "done/keen-owl/ask.sh"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("echo hi\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, errs := run("review", "--work", work, "sec", filepath.Join(dir, "ask.sh")); code != exitFail || !strings.Contains(errs, "the Ollama gateway isn't running") {
		t.Errorf("review without the gateway: %d %q", code, errs)
	}
	serveGateway(t, work, s.URL)
	if code, out, errs := run("review", "--work", work, "sec", filepath.Join(dir, "ask.sh")); code != exitOK || !strings.Contains(out, "review.sec.md") {
		t.Errorf("review: %d %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "review.sec.md")); !strings.Contains(string(b), "## Verdict\nOK to run\n") {
		t.Errorf("review.sec.md:\n%s", b)
	}
	if code, _, errs := run("review", "--work", work, "sec", filepath.Join(dir, "done", "keen-owl", "ask.sh")); code != exitUsage || !strings.Contains(errs, "in done/") {
		t.Errorf("done/: %d %q", code, errs)
	}
	if code, _, errs := run("review", "--work", work, "sec"); code != exitUsage {
		t.Errorf("one argument: %d %q", code, errs)
	}
}

// TestGatewayCommands: ollama-mcp start needs ollama.host; with podman it starts the gateway (the test binary, through
// Run), status says it runs and shows its lines, ollama status asks through it, a second start leaves it, stop ends it.
func TestGatewayCommands(t *testing.T) {
	if code, _, errs := run("ollama-mcp", "start", "--work", t.TempDir()); code != exitFail || !strings.Contains(errs, "not available without Ollama (ollama.host is not set)") {
		t.Errorf("not configured: %d %q", code, errs)
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman")
	}
	if code, out, _ := run("ollama-mcp", "status", "--work", t.TempDir()); code != exitFail || out != "the Ollama gateway: not running (plinthctl ollama-mcp start)\n" {
		t.Errorf("status of none: %d %q", code, out)
	}
	s := ollamatest.New(t)
	s.SetModels([]string{"rev", "dlp"}, nil)
	work := t.TempDir()
	t.Cleanup(func() { ollamamcp.Stop(work) })
	cfg := "ollama:\n  host: " + s.URL + "\n  models:\n    review: rev\n    dlp: dlp\n"
	if err := os.WriteFile(filepath.Join(work, "config.yml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := ollamamcp.SocketPath(work)
	if code, out, errs := run("ollama-mcp", "start", "--work", work); code != exitOK || out != "ollama-mcp: started, for Ollama "+s.URL+" on "+sock+"\n" {
		t.Fatalf("start: %d %q %q", code, out, errs)
	}
	if code, out, errs := run("ollama", "status", "--work", work); code != exitOK || !strings.Contains(out, ": reachable, version 0.0.0-test\n") {
		t.Errorf("ollama status: %d %q %q", code, out, errs)
	}
	if code, out, _ := run("ollama-mcp", "start", "--work", work); code != exitOK || !strings.HasPrefix(out, "ollama-mcp: running, ") {
		t.Errorf("start again: %d %q", code, out)
	}
	var out string
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end) && !strings.Contains(out, " ollama_version: "); time.Sleep(100 * time.Millisecond) {
		_, out, _ = run("ollama-mcp", "status", "--work", work)
	}
	if !strings.Contains(out, "the Ollama gateway: running since ") || !strings.Contains(out, "its last lines:\n") ||
		strings.Contains(out, "not the one") || strings.Contains(out, "0.0.0-test") {
		t.Errorf("status: %q", out)
	}
	if code, out, _ := run("ollama-mcp", "stop", "--work", work); code != exitOK || out != "ollama-mcp: stopped\n" {
		t.Errorf("stop: %d %q", code, out)
	}
	if code, out, _ := run("ollama-mcp", "status", "--work", work); code != exitFail || !strings.Contains(out, "not running") {
		t.Errorf("status after stop: %d %q", code, out)
	}
}
