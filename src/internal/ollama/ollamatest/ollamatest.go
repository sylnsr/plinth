// Package ollamatest is a fake Ollama for tests: /api/version, /api/tags, /api/ps, /api/show and /api/chat, with the
// chat replies a test chooses.
package ollamatest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Chat is one /api/chat request as the fake received it.
type Chat struct {
	Model   string
	System  string
	Prompt  string
	Format  json.RawMessage
	Options map[string]any
}

// Server is the fake.
type Server struct {
	*httptest.Server

	mu     sync.Mutex
	reply  func(ctx context.Context, c Chat) (status int, content string)
	models []string // /api/tags
	loaded []string // /api/ps
	chats  []Chat
}

// SetReply sets how the fake answers a chat: the HTTP status and the message's content (Ollama's error text for a
// status other than 200). ctx ends when the client gives up. Until it is set: DLP(nil), no findings.
func (s *Server) SetReply(f func(ctx context.Context, c Chat) (status int, content string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = f
}

// SetModels sets the models /api/tags and /api/ps list.
func (s *Server) SetModels(models, loaded []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models, s.loaded = models, loaded
}

// New starts the fake; the test's cleanup stops it.
func New(t *testing.T) *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Chats are the chat requests so far.
func (s *Server) Chats() []Chat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Chat(nil), s.chats...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	models, loaded, reply := s.models, s.loaded, s.reply
	s.mu.Unlock()
	list := func(names []string) {
		var ms []map[string]any
		for _, n := range names {
			ms = append(ms, map[string]any{"name": n, "size": 1})
		}
		json.NewEncoder(w).Encode(map[string]any{"models": ms})
	}
	switch r.URL.Path {
	case "/api/version":
		w.Write([]byte(`{"version":"0.0.0-test"}`))
	case "/api/tags":
		list(models)
	case "/api/ps":
		list(loaded)
	case "/api/show":
		var in struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		if !slices.Contains(models, in.Model) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "model '" + in.Model + "' not found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"modelfile": "FROM " + in.Model, "details": map[string]string{"family": "test"}})
	case "/api/chat":
		var in struct {
			Model    string          `json:"model"`
			Stream   *bool           `json:"stream"`
			Format   json.RawMessage `json:"format"`
			Options  map[string]any  `json:"options"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Stream == nil || *in.Stream {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		c := Chat{Model: in.Model, Format: in.Format, Options: in.Options}
		for _, m := range in.Messages {
			switch m.Role {
			case "system":
				c.System = m.Content
			case "user":
				c.Prompt = m.Content
			}
		}
		s.mu.Lock()
		s.chats = append(s.chats, c)
		s.mu.Unlock()
		if reply == nil {
			reply = DLP(nil)
		}
		status, content := reply(r.Context(), c)
		if status != http.StatusOK {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": content})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": c.Model, "message": map[string]string{"role": "assistant", "content": content},
			"done": true, "done_reason": "stop", "eval_count": 7, "total_duration": 1500000})
	default:
		http.NotFound(w, r)
	}
}

// DLP answers as the DLP model: every numbered line ("<n>\t<text>") for which flag returns a kind is a finding.
func DLP(flag func(text string) string) func(context.Context, Chat) (int, string) {
	return func(_ context.Context, c Chat) (int, string) {
		type finding struct {
			Line int    `json:"line"`
			Kind string `json:"kind"`
		}
		out := struct {
			Findings []finding `json:"findings"`
		}{Findings: []finding{}}
		for _, l := range strings.Split(c.Prompt, "\n") {
			num, text, ok := strings.Cut(l, "\t")
			n, err := strconv.Atoi(num)
			if !ok || err != nil || flag == nil {
				continue
			}
			if k := flag(text); k != "" {
				out.Findings = append(out.Findings, finding{n, k})
			}
		}
		b, _ := json.Marshal(out)
		return http.StatusOK, string(b)
	}
}
