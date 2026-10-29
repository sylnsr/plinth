package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/logfile"
)

// post sends body to the server and returns the status and the body.
func post(t *testing.T, url, contentType, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, contentType, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func echoServer(t *testing.T, s *Server) string {
	t.Helper()
	s.Tools = append(s.Tools,
		Tool{Name: "echo", Description: "echo the word", Schema: json.RawMessage(`{"type":"object"}`), Call: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Word string `json:"word"`
			}
			if err := DecodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Word == "" {
				return "", errors.New("no word")
			}
			return "<" + a.Word + ">", nil
		}},
		Tool{Name: "boom", Description: "panics", Schema: json.RawMessage(`{"type":"object"}`), Call: func(context.Context, json.RawMessage) (string, error) {
			panic("boom")
		}})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestInstructions: the initialize result carries the server's instructions, and tools/list a read-only tool's
// annotations.readOnlyHint.
func TestInstructions(t *testing.T) {
	url := echoServer(t, &Server{Name: "test", Version: "1", Instructions: "Call guide first.", Tools: []Tool{{Name: "guide", Description: "start here",
		Schema: json.RawMessage(`{"type":"object"}`), ReadOnly: true, Call: func(context.Context, json.RawMessage) (string, error) { return "g", nil }}}})
	if _, body := post(t, url, "application/json", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); body !=
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"},"instructions":"Call guide first."}}` {
		t.Errorf("initialize: %s", body)
	}
	_, body := post(t, url, "application/json", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.HasPrefix(body, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"guide","description":"start here","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},{"name":"echo","description":"echo the word","inputSchema":{"type":"object"}},`) {
		t.Errorf("tools/list: %s", body)
	}
}

// TestProtocol: initialize with version negotiation, notifications, ping, tools/list, tools/call and its errors.
func TestProtocol(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, logfile.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	url := echoServer(t, &Server{Name: "test", Version: "1", Log: logfile.New(work, logfile.AgentMCP)})
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
			200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}}`},
		{"unknown version", `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
			200, `{"jsonrpc":"2.0","id":"a","result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}}`},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202, ``},
		{"ping", `{"jsonrpc":"2.0","id":2,"method":"ping"}`, 200, `{"jsonrpc":"2.0","id":2,"result":{}}`},
		{"tools/list", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
			200, `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"echo","description":"echo the word","inputSchema":{"type":"object"}},{"name":"boom","description":"panics","inputSchema":{"type":"object"}}]}}`},
		{"call", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"word":"hi"}}}`,
			200, `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"<hi>"}],"isError":false}}`},
		{"tool error", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo"}}`,
			200, `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"no word"}],"isError":true}}`},
		{"unknown argument", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"echo","arguments":{"wrod":"hi"}}}`,
			200, `{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"arguments: json: unknown field \"wrod\""}],"isError":true}}`},
		{"panic", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"boom"}}`,
			200, `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"internal error: boom"}],"isError":true}}`},
		{"unknown tool", `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"nope"}}`,
			200, `{"jsonrpc":"2.0","id":8,"error":{"code":-32602,"message":"unknown tool \"nope\""}}`},
		{"unknown method", `{"jsonrpc":"2.0","id":9,"method":"resources/list"}`,
			200, `{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"unknown method \"resources/list\""}}`},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, 400, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"batches are not supported"}}`},
		{"not JSON", `{"jsonrpc":`, 400, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"not a JSON object"}}`},
		{"not 2.0", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, 400, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"not a JSON-RPC 2.0 request"}}`},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, 400, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"id must be a string or a number"}}`},
	}
	for _, c := range cases {
		status, body := post(t, url, "application/json", c.body)
		if status != c.status || body != c.want {
			t.Errorf("%s: %d %s\nwant %d %s", c.name, status, body, c.status, c.want)
		}
	}
	files, _ := filepath.Glob(filepath.Join(work, "logs", "plinth-*.log"))
	var raw []byte
	for _, f := range files {
		b, _ := os.ReadFile(f)
		raw = append(raw, b...)
	}
	var logged []logfile.Entry
	for _, l := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if e, ok := logfile.Parse(l); ok {
			logged = append(logged, e)
		}
	}
	if len(logged) != 4 || logged[0].Get("tool") != "echo" || logged[0].Get("outcome") != "ok" || logged[0].Level != "INFO" ||
		logged[1].Get("outcome") != "error: no word" || logged[1].Level != "WARN" || strings.Contains(string(raw), "hi") {
		t.Errorf("log: %q", raw)
	}
}

// TestHTTP: only a JSON POST is handled; the gate decides first; a request over MaxBody is refused.
func TestHTTP(t *testing.T) {
	url := echoServer(t, &Server{Gate: func(r *http.Request) (int, string) {
		if r.Header.Get("X-No") != "" {
			return http.StatusUnauthorized, "no"
		}
		return 0, ""
	}})
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Errorf("GET: %d %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	if status, _ := post(t, url, "text/plain", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); status != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", status)
	}
	if status, body := post(t, url, "application/json; charset=utf-8", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); status != 200 || !strings.Contains(body, `"result":{}`) {
		t.Errorf("with a charset: %d %s", status, body)
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-No", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("gate: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", MaxBody) + `"}}`
	if status, _ := post(t, url, "application/json", big); status != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", status)
	}
}

// TestMaxInFlight: a request past MaxInFlight gets 503 at once; the slot is free again afterwards.
func TestMaxInFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := &Server{MaxInFlight: 1, Tools: []Tool{{Name: "wait", Schema: json.RawMessage(`{}`), Call: func(context.Context, json.RawMessage) (string, error) {
		close(started)
		<-release
		return "done", nil
	}}}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	done := make(chan string, 1)
	go func() {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}}`))
		if err != nil {
			done <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- string(b)
	}()
	<-started
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" {
		t.Errorf("second request: %d", resp.StatusCode)
	}
	close(release)
	if got := <-done; !strings.Contains(got, `"text":"done"`) {
		t.Errorf("first request: %s", got)
	}
	if status, _ := post(t, srv.URL, "application/json", `{"jsonrpc":"2.0","id":3,"method":"ping"}`); status != 200 {
		t.Errorf("after: %d", status)
	}
}

func TestHelpers(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	for h, want := range map[string]string{"": "", "Bearer k1": "k1", "bearer k2": "k2", "Basic k3": "", "Bearer": ""} {
		r.Header.Set("Authorization", h)
		if got := Bearer(r); got != want {
			t.Errorf("Bearer(%q) = %q", h, got)
		}
	}
	if !SameKey("k", "k") || SameKey("k", "K") || SameKey("", "") || SameKey("k", "") {
		t.Error("SameKey")
	}
	var a struct {
		N int `json:"n"`
	}
	if err := DecodeArgs(nil, &a); err != nil || a.N != 0 {
		t.Errorf("none: %v", err)
	}
	if err := DecodeArgs(json.RawMessage(` {"n":2} `), &a); err != nil || a.N != 2 {
		t.Errorf("n: %v %d", err, a.N)
	}
	if err := DecodeArgs(json.RawMessage(`{"n":2} {}`), &a); err == nil {
		t.Error("two values: no error")
	}
	if s, err := JSONText(map[string]string{"a": "<&>"}); err != nil || s != `{"a":"<&>"}` {
		t.Errorf("JSONText: %q %v", s, err)
	}
}

// TestClient: a tool's text, a tool's error as a ToolError, a JSON-RPC error, and a reply that isn't one.
func TestClient(t *testing.T) {
	c := &Client{HTTP: http.DefaultClient, URL: echoServer(t, &Server{Name: "test", Version: "1"})}
	ctx := context.Background()
	if text, err := c.Call(ctx, "echo", map[string]string{"word": "hi"}); err != nil || text != "<hi>" {
		t.Errorf("echo: %q %v", text, err)
	}
	var te *ToolError
	if _, err := c.Call(ctx, "echo", nil); !errors.As(err, &te) || te.Text != "no word" {
		t.Errorf("a tool's error: %v", err)
	}
	if _, err := c.Call(ctx, "nope", nil); err == nil || errors.As(err, &te) || !strings.HasPrefix(err.Error(), "MCP: ") || !strings.Contains(err.Error(), "(-32") {
		t.Errorf("an unknown tool: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "oops", http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	c.URL = bad.URL
	if _, err := c.Call(ctx, "echo", nil); err == nil || !strings.HasPrefix(err.Error(), "MCP: HTTP 500: oops") {
		t.Errorf("not a reply: %v", err)
	}
}
