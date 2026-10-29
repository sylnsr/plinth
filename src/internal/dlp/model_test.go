package dlp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"plinth/internal/config"
	"plinth/internal/ollama"
	"plinth/internal/ollama/ollamatest"
)

func fake(t *testing.T) (*ollamatest.Server, *ollama.Client) {
	s := ollamatest.New(t)
	return s, ollama.New(config.Ollama{Host: s.URL, TimeoutSeconds: 5, MaxParallel: 1})
}

// TestScan: the fixed patterns first (the model never sees what they masked), then the lines the model flags, each
// replaced whole; every other byte stays; the request has the schema, temperature 0 and the system prompt.
func TestScan(t *testing.T) {
	s, c := fake(t)
	s.SetReply(ollamatest.DLP(func(text string) string {
		if strings.Contains(text, "Zq8vX2pLm9") {
			return "encryption_key"
		}
		return ""
	}))
	in := "ok 1.2.3\nluks: Zq8vX2pLm9TtR4wE\npassword=hunter2\n\tindented  spaces \n"
	out, res, err := Scan(context.Background(), c, "dlp-m", []byte(in))
	want := "ok 1.2.3\n[line 2 masked by dlp-scan: encryption_key]\npassword=<masked>\n\tindented  spaces \n"
	if err != nil || string(out) != want {
		t.Fatalf("%q, %v; want %q", out, err, want)
	}
	if res != (Result{Model: "dlp-m", Fixed: 1, Chunks: 1, Lines: 1}) {
		t.Errorf("result %+v", res)
	}

	chat := s.Chats()[0]
	if strings.Contains(chat.Prompt, "hunter2") || !strings.HasPrefix(chat.Prompt, "1\tok 1.2.3\n2\tluks: ") || chat.Model != "dlp-m" ||
		chat.Options["temperature"] != float64(0) || chat.Options["num_ctx"] != float64(numCtx) || !strings.Contains(string(chat.Format), "api_key_or_token") ||
		!strings.HasPrefix(chat.System, "You are a data-loss-prevention scanner") {
		t.Errorf("request: %+v", chat)
	}

	// A last line without \n stays without; nothing flagged leaves the fixed-masked text as it is.
	if out, _, err := Scan(context.Background(), c, "m", []byte("a\nluks Zq8vX2pLm9")); err != nil || string(out) != "a\n[line 2 masked by dlp-scan: encryption_key]" {
		t.Errorf("no final newline: %q, %v", out, err)
	}
}

// TestScanChunks: 120 numbered entries per chunk; a line longer than 400 bytes goes in pieces with its number, cut
// between runes, so a secret past the 400th byte is seen too.
func TestScanChunks(t *testing.T) {
	s, c := fake(t)
	s.SetReply(ollamatest.DLP(func(text string) string {
		if strings.Contains(text, "SECRET") {
			return "other_secret"
		}
		return ""
	}))
	var b strings.Builder
	for i := 1; i <= 250; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString(strings.Repeat("é", 300) + " SECRET\n") // 600 bytes, then the secret: line 251
	out, res, err := Scan(context.Background(), c, "m", []byte(b.String()))
	if err != nil || res.Chunks != 3 || res.Lines != 1 || !strings.HasSuffix(string(out), "line 250\n[line 251 masked by dlp-scan: other_secret]\n") {
		t.Fatalf("%+v, %v\n%s", res, err, out[len(out)-80:])
	}
	chats := s.Chats()
	last := chats[len(chats)-1].Prompt
	pieces := strings.Split(last, "\n")
	if len(pieces) != 10+2 || !strings.HasPrefix(pieces[10], "251\té") || !strings.HasPrefix(pieces[11], "251\té") ||
		!strings.HasSuffix(pieces[11], " SECRET") || len(pieces[10]) != len("251\t")+400 {
		t.Errorf("last chunk: %q", last)
	}
}

// TestScanFails: a reply that doesn't fit is asked once more, then the scan fails with the cause and the text with the
// fixed-pattern masks alone; not configured says so without asking anyone.
func TestScanFails(t *testing.T) {
	s, c := fake(t)
	var n atomic.Int32
	var mu sync.Mutex // the handler runs in the server's goroutine
	replies := []string{`{"findings":[{"line":9,"kind":"password"}]}`, `{"findings":[{"line":1,"kind":"password"}]}`}
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return http.StatusOK, replies[min(int(n.Add(1))-1, len(replies)-1)]
	})
	if out, res, err := Scan(context.Background(), c, "m", []byte("token=abc\nx\n")); err != nil || string(out) != "[line 1 masked by dlp-scan: password]\nx\n" || res.Fixed != 1 {
		t.Errorf("bad, then good: %q %+v %v", out, res, err)
	}
	for _, bad := range []string{`{"findings":[{"line":9,"kind":"password"}]}`, `{"findings":[{"line":1,"kind":"ssn"}]}`,
		`{"findings":null}`, `{}`, `[1]`, `{"findings":[{"line":1}]}`} {
		n.Store(0)
		mu.Lock()
		replies = []string{bad}
		mu.Unlock()
		out, _, err := Scan(context.Background(), c, "m", []byte("token=abc\nx\n"))
		if err == nil || !strings.HasPrefix(err.Error(), "Ollama (model m): the model") || string(out) != "token=<masked>\nx\n" || n.Load() != 2 {
			t.Errorf("%s: %q, %v after %d call(s)", bad, out, err, n.Load())
		}
	}
	out, res, err := Scan(context.Background(), ollama.New(config.Ollama{}), "m", []byte("token=abc\n"))
	if !errors.Is(err, ollama.ErrNotConfigured) || string(out) != "token=<masked>\n" || res.Fixed != 1 {
		t.Errorf("not configured: %q %+v %v", out, res, err)
	}
}

// TestScanTexts: the fixed patterns per text (a key block shrinks to a line within its own text), the model's lines
// numbered across the texts in one chunk, a flagged line masked in its own text by its number there; texts with
// nothing flagged keep their bytes; a reply that doesn't fit twice is a ReplyError, one that can't reach the model
// is not.
func TestScanTexts(t *testing.T) {
	s, c := fake(t)
	s.SetReply(ollamatest.DLP(func(text string) string {
		if strings.Contains(text, "Zq8vX2pLm9") {
			return "encryption_key"
		}
		return ""
	}))
	texts := [][]byte{[]byte("1. keys\n"), []byte("-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\nluks: Zq8vX2pLm9TtR4wE\n"), nil, []byte("tail")}
	out, res, err := ScanTexts(context.Background(), c, "m", texts)
	if err != nil || len(out) != 4 || string(out[0]) != "1. keys\n" || string(out[1]) != "<private key block masked>\n[line 2 masked by dlp-scan: encryption_key]\n" ||
		len(out[2]) != 0 || string(out[3]) != "tail" {
		t.Fatalf("%q, %v", out, err)
	}
	if res != (Result{Model: "m", Fixed: 1, Chunks: 1, Lines: 1}) {
		t.Errorf("result %+v", res)
	}
	if p := s.Chats()[0].Prompt; p != "1\t1. keys\n2\t<private key block masked>\n3\tluks: Zq8vX2pLm9TtR4wE\n4\ttail" {
		t.Errorf("prompt %q", p)
	}

	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) { return http.StatusOK, "not json" })
	var re *ReplyError
	if out, _, err := ScanTexts(context.Background(), c, "m", [][]byte{[]byte("password=x\n")}); !errors.As(err, &re) || string(out[0]) != "password=<masked>\n" {
		t.Errorf("a reply that doesn't fit: %q %v", out, err)
	}
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) { return http.StatusNotFound, "model not found" })
	if _, _, err := ScanTexts(context.Background(), c, "m", [][]byte{[]byte("x\n")}); err == nil || errors.As(err, &re) {
		t.Errorf("an Ollama error: %v", err)
	}
	if _, _, err := ScanTexts(context.Background(), ollama.New(config.Ollama{}), "m", [][]byte{[]byte("x\n")}); !errors.Is(err, ollama.ErrNotConfigured) {
		t.Errorf("not configured: %v", err)
	}
}
