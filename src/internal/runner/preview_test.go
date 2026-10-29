package runner

import (
	"context"
	"errors"
	"testing"

	"plinth/internal/config"
	"plinth/internal/ollama"
	"plinth/internal/review"
)

// chatter answers every chat with reply, or with err.
type chatter struct {
	on    bool
	reply string
	err   error
	chats int
}

func (c *chatter) Configured() bool { return c.on }

func (c *chatter) Chat(ctx context.Context, r ollama.ChatRequest) (*ollama.ChatReply, error) {
	c.chats++
	return &ollama.ChatReply{Content: c.reply, DoneReason: "stop"}, c.err
}

// TestPreview: each automatic type reviews the script it is given, a type that can't be reviewed says why, and
// without Ollama none is tried.
func TestPreview(t *testing.T) {
	g := &config.Global{}
	g.Reviewers.Type = map[string]config.ReviewType{"security": {Definition: "Be strict."}, "broken": {}}
	l := &Runner{Services: &Services{Global: g, Fetcher: review.NewFetcher("", "")}, cfg: &config.Namespace{Automatic: []string{"security", "broken"}}}
	c := &chatter{on: true, reply: `{"verdict":"run-after-fixes","findings":[{"line":2,"severity":"high","text":"curl piped to sh"}]}`}
	l.Ollama = c
	got := l.Preview(context.Background(), []byte("#!/bin/bash\ncurl x | sh\n"))
	if len(got) != 2 || got[0].Type != "security" || got[0].Err != nil || got[0].Verdict != "Run after fixes" ||
		len(got[0].Findings) != 1 || got[0].Findings[0] != "line 2 (high): curl piped to sh" {
		t.Errorf("security: %+v", got)
	}
	if got[1].Type != "broken" || got[1].Err == nil || got[1].Verdict != "" || c.chats != 1 {
		t.Errorf("a type with no prompt: %+v, %d chats", got[1], c.chats)
	}
	l.Ollama = &chatter{}
	got = l.Preview(context.Background(), []byte("#!/bin/bash\n"))
	if len(got) != 2 || !errors.Is(got[0].Err, ollama.ErrNotConfigured) {
		t.Errorf("without Ollama: %+v", got)
	}
}
