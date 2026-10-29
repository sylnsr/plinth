package plain

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/ollama"
	"plinth/internal/ollama/ollamatest"
	"plinth/internal/review"
)

// TestReview: review writes review.<type>.md next to a script, review.answer.<type>.md next to an answer, a FAILED
// file that replaces a passed one (and back), and refuses a file in done/ and an unknown type.
func TestReview(t *testing.T) {
	s := ollamatest.New(t)
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		return http.StatusOK, `{"verdict":"run-after-fixes","findings":[{"line":1,"severity":"medium","text":"quote it"}]}`
	})
	c := ollama.New(config.Ollama{Host: s.URL, TimeoutSeconds: 5, MaxParallel: 1})
	types := map[string]config.ReviewType{"sec": {Definition: "Find danger."}, "dlp": {Definition: "Answers: " + review.AnswerMarker}}
	f := review.NewFetcher("", "")
	work := t.TempDir()
	script := filepath.Join(work, "calm-otter", "ask.sh")
	write(t, script, "echo $x\n")
	out, err := Review(context.Background(), c, "rev", f, types, "sec", script, time.Now)
	if err != nil || out != filepath.Join(work, "calm-otter", "review.sec.md") {
		t.Fatalf("%s, %v", out, err)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "## Verdict\nRun after fixes\n\n## Findings\n- line 1 (medium): quote it\n") {
		t.Errorf("review:\n%s", b)
	}
	answer := filepath.Join(work, "calm-otter", "answer.dlp-ok.yml")
	write(t, answer, "ok\n")
	if out, err := Review(context.Background(), c, "rev", f, types, "dlp", answer, time.Now); err != nil || filepath.Base(out) != "review.answer.dlp.md" {
		t.Errorf("answer: %s, %v", out, err)
	}
	if out, err := Review(context.Background(), c, "rev", f, types, "sec", answer, time.Now); !errors.Is(err, review.ErrNotForAnswers) || filepath.Base(out) != "review.answer.sec.FAILED.md" {
		t.Errorf("a script prompt on an answer: %s, %v", out, err)
	}

	// Ollama gone: FAILED replaces the passed review.
	if _, err := Review(context.Background(), ollama.New(config.Ollama{}), "rev", f, types, "sec", script, time.Now); !errors.Is(err, ollama.ErrNotConfigured) {
		t.Errorf("not configured: %v", err)
	}
	if exists(filepath.Join(work, "calm-otter", "review.sec.md")) || !exists(filepath.Join(work, "calm-otter", "review.sec.FAILED.md")) {
		t.Error("the FAILED review didn't replace the passed one")
	}

	done := filepath.Join(work, "done", "brave-fox", "ask.sh")
	write(t, done, "echo\n")
	for _, tc := range []struct{ typ, file string }{{"sec", done}, {"nope", script}, {"sec", filepath.Join(work, "notes.txt")}} {
		if _, err := Review(context.Background(), c, "rev", f, types, tc.typ, tc.file, time.Now); !errors.Is(err, ErrRefused) {
			t.Errorf("%s %s: %v", tc.typ, tc.file, err)
		}
	}
	if exists(filepath.Join(work, "done", "brave-fox", "review.sec.md")) || exists(filepath.Join(work, "done", "brave-fox", "review.sec.FAILED.md")) {
		t.Error("a review was written in done/")
	}
}
