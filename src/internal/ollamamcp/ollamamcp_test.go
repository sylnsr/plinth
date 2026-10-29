package ollamamcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/ollama"
	"plinth/internal/ollama/ollamatest"
)

// TestMain is the gateway itself when the container runs the test binary (TestGateway).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == MainArg {
		os.Exit(Main(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func client(host string) *ollama.Client {
	return ollama.New(config.Ollama{Host: host, TimeoutSeconds: 30, MaxParallel: 1})
}

type textItem struct {
	Text string `json:"text"`
}

type toolName struct {
	Name string `json:"name"`
}

// rpcBody is what the tests read of a result: initialize's, tools/list's or tools/call's.
type rpcBody struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Tools           []toolName `json:"tools"`
	Content         []textItem `json:"content"`
	IsError         bool       `json:"isError"`
}

type rpcError struct {
	Message string `json:"message"`
}

// rpcResult is a reply: its result, or the JSON-RPC error.
type rpcResult struct {
	Result rpcBody   `json:"result"`
	Error  *rpcError `json:"error"`
}

// rpc posts one JSON-RPC message (no id: a notification) and returns the status and the decoded reply.
func rpc(t *testing.T, url, body string) (int, rpcResult) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out rpcResult
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	return resp.StatusCode, out
}

// callTool is tools/call of name with args (a JSON object): the result's text and isError.
func callTool(t *testing.T, url, name, args string) (string, bool) {
	t.Helper()
	status, r := rpc(t, url, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	if status != http.StatusOK || r.Error != nil || len(r.Result.Content) != 1 {
		t.Fatalf("%s: %d %+v", name, status, r)
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

// TestTools: initialize, notifications/initialized, tools/list, and every tool against the fake Ollama, ollama_chat's
// format too.
func TestTools(t *testing.T) {
	s := ollamatest.New(t)
	s.SetModels([]string{"rev", "dlp"}, []string{"rev"})
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		return 200, "looks <fine> & safe"
	})
	srv := httptest.NewServer(New(Options{Client: client(s.URL), Version: "v1"}))
	t.Cleanup(srv.Close)
	url := srv.URL

	if status, r := rpc(t, url, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"plinthctl","version":"1"}}}`); status != 200 || r.Result.ProtocolVersion != "2025-06-18" {
		t.Errorf("initialize: %d %+v", status, r)
	}
	if status, _ := rpc(t, url, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`); status != http.StatusAccepted {
		t.Errorf("notification: %d", status)
	}
	_, r := rpc(t, url, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var names []string
	for _, tl := range r.Result.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, " ") != "ollama_chat ollama_list_models ollama_ps ollama_show ollama_version" {
		t.Errorf("tools: %v", names)
	}

	text, isErr := callTool(t, url, "ollama_chat", `{"model":"rev","system":"Be brief.","prompt":"Review this.","options":{"temperature":0.2,"num_ctx":32768}}`)
	var chat map[string]any
	if err := json.Unmarshal([]byte(text), &chat); err != nil || isErr {
		t.Fatalf("chat: %v %s", err, text)
	}
	if chat["model"] != "rev" || chat["reply"] != "looks <fine> & safe" || chat["done_reason"] != "stop" || chat["eval_count"] != 7.0 ||
		chat["total_duration_ms"] != 1.0 || len(chat) != 5 {
		t.Errorf("chat: %s", text)
	}
	if !strings.Contains(text, "<fine> &") {
		t.Errorf("chat: HTML-escaped: %s", text)
	}
	if cs := s.Chats(); len(cs) != 1 || cs[0].System != "Be brief." || cs[0].Prompt != "Review this." || cs[0].Options["num_ctx"] != 32768.0 || cs[0].Format != nil {
		t.Errorf("Ollama got: %+v", cs)
	}

	for i, c := range []struct{ format, want string }{
		{`"json"`, `"json"`},
		{`{"type":"object","properties":{"verdict":{"type":"string"}}}`, `{"type":"object","properties":{"verdict":{"type":"string"}}}`},
		{`null`, ``},
	} {
		if text, isErr := callTool(t, url, "ollama_chat", `{"model":"rev","prompt":"x","format":`+c.format+`}`); isErr {
			t.Errorf("format %s: %s", c.format, text)
		}
		if cs := s.Chats(); len(cs) != i+2 || string(cs[i+1].Format) != c.want {
			t.Errorf("format %s: Ollama got %s", c.format, cs[len(cs)-1].Format)
		}
	}

	for _, c := range []struct{ tool, args, want string }{
		{"ollama_list_models", `{}`, `"name":"dlp"`},
		{"ollama_ps", `{}`, `{"models":[{"name":"rev","size":1}]}`},
		{"ollama_version", `null`, `{"version":"0.0.0-test"}`},
		{"ollama_show", `{"model":"rev"}`, `"modelfile":"FROM rev"`},
	} {
		if text, isErr := callTool(t, url, c.tool, c.args); isErr || !strings.Contains(text, c.want) {
			t.Errorf("%s: %v %s", c.tool, isErr, text)
		}
	}

	for _, c := range []struct{ tool, args, want string }{
		{"ollama_show", `{"model":"nope"}`, "Ollama: HTTP 404: model 'nope' not found"},
		{"ollama_show", `{}`, "arguments: model is required"},
		{"ollama_chat", `{"model":"rev"}`, "arguments: model and prompt are required"},
		{"ollama_chat", `{"model":"rev","prompt":"x","stream":true}`, `arguments: json: unknown field "stream"`},
		{"ollama_chat", `{"model":"rev","prompt":"x","format":"yaml"}`, `arguments: format is "json" or a JSON schema (an object)`},
		{"ollama_chat", `{"model":"rev","prompt":"x","format":3}`, `arguments: format is "json" or a JSON schema (an object)`},
		{"ollama_ps", `{"all":true}`, `arguments: json: unknown field "all"`},
	} {
		if text, isErr := callTool(t, url, c.tool, c.args); !isErr || text != c.want {
			t.Errorf("%s %s: %v %q", c.tool, c.args, isErr, text)
		}
	}

	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		return 500, "model runner crashed"
	})
	if text, isErr := callTool(t, url, "ollama_chat", `{"model":"rev","prompt":"x"}`); !isErr || text != "Ollama: HTTP 500: model runner crashed" {
		t.Errorf("Ollama error: %v %q", isErr, text)
	}
	resp, err := http.Post(url+"/other", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("another path: %d", resp.StatusCode)
	}
}

// TestNotConfigured: without ollama.host every tool says so; the client says so without asking; Start starts nothing.
func TestNotConfigured(t *testing.T) {
	srv := httptest.NewServer(New(Options{Client: client("")}))
	t.Cleanup(srv.Close)
	if text, isErr := callTool(t, srv.URL, "ollama_version", `{}`); !isErr || text != "not available without Ollama (ollama.host is not set)" {
		t.Errorf("version: %v %q", isErr, text)
	}
	if text, isErr := callTool(t, srv.URL, "ollama_chat", `{"model":"m","prompt":"p"}`); !isErr || text != "not available without Ollama (ollama.host is not set)" {
		t.Errorf("chat: %v %q", isErr, text)
	}
	c := NewClient(t.TempDir(), false)
	if _, err := c.Chat(context.Background(), ollama.ChatRequest{Model: "m", Prompt: "p"}); c.Configured() || !errors.Is(err, ollama.ErrNotConfigured) {
		t.Errorf("client: %v", err)
	}
	if _, err := (Gateway{Work: t.TempDir()}).Start(); !errors.Is(err, ollama.ErrNotConfigured) {
		t.Errorf("Start: %v", err)
	}
}

// serveSocket serves the gateway for the Ollama at host on work's socket until the test ends.
func serveSocket(t *testing.T, work, host string) (stop func()) {
	t.Helper()
	dir := filepath.Join(work, filepath.FromSlash(SocketDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, New(Options{Client: client(host)})) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Serve did not end within 10 s")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// TestSocket: the client asks the gateway on the socket (mode 0600; a left-over one is replaced): Chat with its
// format, Version, Tags, PS, and Ollama's errors as they are. When the server stops, a call waiting for Ollama ends at
// once, and so does Serve.
func TestSocket(t *testing.T) {
	s := ollamatest.New(t)
	s.SetModels([]string{"rev", "dlp"}, []string{"rev"})
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		return 200, `{"verdict":"ok"}`
	})
	work := t.TempDir()
	dir := filepath.Join(work, filepath.FromSlash(SocketDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SocketName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := serveSocket(t, work, s.URL)
	if fi, err := os.Stat(SocketPath(work)); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", fi.Mode(), err)
	}
	c := NewClient(work, true)
	ctx := context.Background()
	rep, err := c.Chat(ctx, ollama.ChatRequest{Model: "rev", System: "sys", Prompt: "p", Format: json.RawMessage(`{"type":"object"}`),
		Options: map[string]any{"seed": 1}})
	if err != nil || rep.Model != "rev" || rep.Content != `{"verdict":"ok"}` || rep.DoneReason != "stop" || rep.EvalCount != 7 || rep.TotalDuration != time.Millisecond {
		t.Fatalf("Chat: %+v %v", rep, err)
	}
	if cs := s.Chats(); len(cs) != 1 || cs[0].System != "sys" || string(cs[0].Format) != `{"type":"object"}` || cs[0].Options["seed"] != 1.0 {
		t.Errorf("Ollama got: %+v", cs)
	}
	if v, err := c.Version(ctx); err != nil || v != "0.0.0-test" {
		t.Errorf("Version: %q %v", v, err)
	}
	if ms, err := c.Tags(ctx); err != nil || len(ms) != 2 || ms[1].Name != "dlp" {
		t.Errorf("Tags: %+v %v", ms, err)
	}
	if ms, err := c.PS(ctx); err != nil || len(ms) != 1 || ms[0].Name != "rev" {
		t.Errorf("PS: %+v %v", ms, err)
	}
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		return 500, "model runner crashed"
	})
	if _, err := c.Chat(ctx, ollama.ChatRequest{Model: "rev", Prompt: "p"}); err == nil || err.Error() != "Ollama: HTTP 500: model runner crashed" {
		t.Errorf("Ollama error: %v", err)
	}

	waiting := make(chan struct{})
	var once sync.Once
	s.SetReply(func(ctx context.Context, _ ollamatest.Chat) (int, string) {
		once.Do(func() { close(waiting) })
		<-ctx.Done()
		return 500, "gone"
	})
	called := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, ollama.ChatRequest{Model: "rev", Prompt: "p"})
		called <- err
	}()
	<-waiting
	start := time.Now()
	stop()
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Serve ended after %s: it waited for the call", d)
	}
	select {
	case err := <-called:
		if err == nil {
			t.Error("the call that the stop ended succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Error("the call did not end")
	}
}

// TestNotRunning: nobody at the socket (none, or a left-over one) is "the Ollama gateway isn't running"; a cancelled
// call is its context's error.
func TestNotRunning(t *testing.T) {
	work := t.TempDir()
	c := NewClient(work, true)
	ctx := context.Background()
	if _, err := c.Version(ctx); !errors.Is(err, errNotRunning) || err.Error() != "the Ollama gateway isn't running (plinthctl ollama-mcp start)" {
		t.Errorf("no socket: %v", err)
	}
	dir := filepath.Join(work, filepath.FromSlash(SocketDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := c.Chat(ctx, ollama.ChatRequest{Model: "m", Prompt: "p"}); !errors.Is(err, errNotRunning) {
		t.Errorf("left-over socket: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Version(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// TestStatic: the gateway's binary must be static; a dynamic one is refused with how to build it.
func TestStatic(t *testing.T) {
	bin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		t.Fatal(err)
	}
	_, err = staticSum(bin)
	switch {
	case err == nil:
		t.Skipf("%s is static", bin)
	case !strings.Contains(err.Error(), "is not static (build it with CGO_ENABLED=0)"):
		t.Errorf("%s: %v", bin, err)
	}
	if _, err := staticSum(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing binary: no error")
	}
}

// TestGateway: the gateway's container (the test binary as plinthctl, TestMain) in an empty root: Start starts it and
// waits for its socket, the client asks Ollama through it (by name: the empty root resolves localhost), a second Start
// leaves it, another config replaces it, State and Logs say so, Stop ends it, and then the client says it isn't
// running.
func TestGateway(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman")
	}
	bin, err := Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staticSum(bin); err != nil {
		t.Fatalf("%v (run the tests with CGO_ENABLED=0)", err)
	}
	s := ollamatest.New(t)
	s.SetModels([]string{"rev"}, nil)
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		return 200, "fine"
	})
	host := strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
	work := t.TempDir()
	t.Cleanup(func() { Stop(work) })
	g := Gateway{Work: work, Bin: bin, Ollama: config.Ollama{Host: host, TimeoutSeconds: 30, RetrySeconds: 1, MaxParallel: 1}, Version: "test"}
	if did, err := g.Start(); err != nil || did != "started" {
		t.Fatalf("Start: %q %v", did, err)
	}
	c := NewClient(work, true)
	ctx := context.Background()
	if rep, err := c.Chat(ctx, ollama.ChatRequest{Model: "rev", Prompt: "p"}); err != nil || rep.Content != "fine" {
		t.Fatalf("Chat through the container: %+v %v", rep, err)
	}
	if did, err := g.Start(); err != nil || did != "running" {
		t.Errorf("Start again: %q %v", did, err)
	}
	g.Ollama.MaxParallel = 2
	if did, err := g.Start(); err != nil || did != "replaced" {
		t.Errorf("Start of another config: %q %v", did, err)
	}
	if v, err := c.Version(ctx); err != nil || v != "0.0.0-test" {
		t.Errorf("Version after the replacement: %q %v", v, err)
	}
	st, err := State(work)
	spec, _ := g.Spec()
	if err != nil || !st.Exists || !st.Running || st.Spec != spec || st.Started.IsZero() {
		t.Errorf("State: %+v %v", st, err)
	}
	if out, err := Logs(work, 10); err != nil || !strings.Contains(out, "max_parallel=2") || !strings.Contains(out, "ollama_version") {
		t.Errorf("Logs: %q %v", out, err)
	}
	if err := Stop(work); err != nil {
		t.Fatal(err)
	}
	if st, err := State(work); err != nil || st.Exists {
		t.Errorf("State after Stop: %+v %v", st, err)
	}
	if _, err := c.Version(ctx); !errors.Is(err, errNotRunning) {
		t.Errorf("after Stop: %v", err)
	}
	if err := Stop(work); err != nil {
		t.Errorf("Stop of none: %v", err)
	}
}
