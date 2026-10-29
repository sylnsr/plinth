package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/ollama/ollamatest"
)

func client(host string, parallel int) *Client {
	return New(config.Ollama{Host: host, TimeoutSeconds: 5, RetrySeconds: 0, MaxParallel: parallel})
}

// TestChat: the request carries the model, both messages, the schema and the options, without streaming; the reply's
// fields come back.
func TestChat(t *testing.T) {
	s := ollamatest.New(t)
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) { return http.StatusOK, `{"findings":[]}` })
	c := client(s.URL+"/", 1)
	r, err := c.Chat(context.Background(), ChatRequest{Model: "m", System: "sys", Prompt: "hi", Format: json.RawMessage(`{"type":"object"}`),
		Options: map[string]any{"temperature": 0}})
	if err != nil || r.Content != `{"findings":[]}` || r.Model != "m" || r.DoneReason != "stop" || r.EvalCount != 7 || r.TotalDuration != 1500*time.Microsecond {
		t.Fatalf("%+v, %v", r, err)
	}
	got := s.Chats()[0]
	if got.Model != "m" || got.System != "sys" || got.Prompt != "hi" || string(got.Format) != `{"type":"object"}` || got.Options["temperature"] != float64(0) {
		t.Errorf("request: %+v", got)
	}
}

// TestRetry: a transient error (HTTP 5xx, an empty reply) is tried once more, other errors are not; the error names
// its cause.
func TestRetry(t *testing.T) {
	type reply struct {
		status  int
		content string
	}
	s := ollamatest.New(t)
	var n atomic.Int32
	var mu sync.Mutex // the handler runs in the server's goroutine
	var replies []reply
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		r := replies[int(n.Add(1))-1]
		return r.status, r.content
	})
	c := client(s.URL, 1)
	for _, tc := range []struct {
		name    string
		replies []reply
		calls   int32
		want    string
	}{
		{"5xx, then ok", []reply{{500, "busy"}, {200, "ok"}}, 2, ""},
		{"empty twice", []reply{{200, " "}, {200, ""}}, 2, "Ollama: empty reply"},
		{"4xx", []reply{{404, "model \"m\" not found\x1b[2J"}}, 1, `Ollama: HTTP 404: model "m" not found [2J`},
	} {
		n.Store(0)
		mu.Lock()
		replies = tc.replies
		mu.Unlock()
		_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Prompt: "p"})
		if (tc.want == "") != (err == nil) || err != nil && err.Error() != tc.want || n.Load() != tc.calls {
			t.Errorf("%s: %v after %d call(s), want %q after %d", tc.name, err, n.Load(), tc.want, tc.calls)
		}
	}
}

// TestUnavailable: not configured and unreachable both say "not available without Ollama".
func TestUnavailable(t *testing.T) {
	_, err := client("", 1).Chat(context.Background(), ChatRequest{Model: "m"})
	if !errors.Is(err, ErrNotConfigured) || err.Error() != "not available without Ollama (ollama.host is not set)" {
		t.Errorf("not configured: %v", err)
	}
	_, err = client("http://127.0.0.1:1", 1).Version(context.Background())
	if !errors.Is(err, ErrUnavailable) || !strings.HasPrefix(err.Error(), "not available without Ollama (http://127.0.0.1:1 unreachable: ") {
		t.Errorf("unreachable: %v", err)
	}
}

// TestTimeoutAndSemaphore: a reply slower than the timeout fails and names the setting; with max-parallel 1, two
// chats never reach Ollama at once; a cancelled context ends a wait for the semaphore.
func TestTimeoutAndSemaphore(t *testing.T) {
	s := ollamatest.New(t)
	var now, most atomic.Int32
	s.SetReply(func(ctx context.Context, _ ollamatest.Chat) (int, string) {
		if v := now.Add(1); v > most.Load() {
			most.Store(v)
		}
		defer now.Add(-1)
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
		}
		return 200, "ok"
	})
	c := client(s.URL, 1)
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Prompt: "p"})
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if most.Load() != 1 {
		t.Errorf("%d chats at once with max-parallel 1", most.Load())
	}
	c.sem <- struct{}{} // taken
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Chat(ctx, ChatRequest{Model: "m", Prompt: "p"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting for the semaphore: %v", err)
	}
	<-c.sem
	c.timeout = 50 * time.Millisecond
	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "ollama.timeout-seconds") {
		t.Errorf("timeout: %v", err)
	}
}

// TestTimeoutInBody: a reply whose headers came in time but whose body stalls past the timeout is a timeout,
// as one before the headers: one call, not retried, and KindOf says Timeout, so the scan ends after ScanAttempts.
func TestTimeoutInBody(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer s.Close()
	c := client(s.URL, 1)
	c.timeout = 200 * time.Millisecond
	_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Prompt: "p"})
	if err == nil || err.Error() != "Ollama: no reply within 200ms (ollama.timeout-seconds)" || KindOf(err) != Timeout || calls.Load() != 1 {
		t.Errorf("%v (kind %d) after %d call(s)", err, KindOf(err), calls.Load())
	}
}

// TestModels: version, tags and ps.
func TestModels(t *testing.T) {
	s := ollamatest.New(t)
	s.SetModels([]string{"a", "b"}, []string{"b"})
	c := client(s.URL, 1)
	v, err := c.Version(context.Background())
	tags, err2 := c.Tags(context.Background())
	ps, err3 := c.PS(context.Background())
	if v != "0.0.0-test" || len(tags) != 2 || len(ps) != 1 || ps[0].Name != "b" || errors.Join(err, err2, err3) != nil {
		t.Errorf("%q %v %v: %v %v %v", v, tags, ps, err, err2, err3)
	}
	raw, err := c.Raw(context.Background(), "/api/ps", nil)
	if err != nil || !strings.Contains(string(raw), `"name":"b"`) {
		t.Errorf("raw: %s, %v", raw, err)
	}
}
