package review

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/ollama"
	"plinth/internal/ollama/ollamatest"
)

var then = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

// TestLoad: URLs joined in order, a /src/ page read as /raw/, the token only for forgejo-url's host, no redirect; or
// the definition.
func TestLoad(t *testing.T) {
	var mu sync.Mutex
	auth := map[string]string{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		switch r.URL.Path {
		case "/kb/raw/branch/main/a.md":
			w.Write([]byte("part A"))
		case "/kb/raw/branch/main/b.md":
			w.Write([]byte("part B"))
		case "/moved":
			http.Redirect(w, r, "/kb/raw/branch/main/a.md", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	// The same pages at another host (host and port are compared), by IP: the build has no network, so no name resolves.
	srv2 := httptest.NewServer(h)
	defer srv2.Close()
	other := srv2.URL
	f := NewFetcher(srv.URL, "s3cret")
	p, err := f.Load(context.Background(), "code", config.ReviewType{URLs: []string{srv.URL + "/kb/src/branch/main/a.md", other + "/kb/raw/branch/main/b.md"}})
	if err != nil || p.Text != "part A\n\npart B" || !strings.Contains(p.Source, "/kb/src/branch/main/a.md, ") || len(p.SHA256) != 64 {
		t.Fatalf("%+v, %v", p, err)
	}
	if auth["/kb/raw/branch/main/a.md"] != "token s3cret" || auth["/kb/raw/branch/main/b.md"] != "" {
		t.Errorf("Authorization: %v", auth)
	}
	for _, u := range []string{srv.URL + "/moved", srv.URL + "/nope", "ftp://x/y"} {
		if _, err := f.Load(context.Background(), "code", config.ReviewType{URLs: []string{u}}); err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: %v", u, err)
		}
	}
	if p, err := f.Load(context.Background(), "sec", config.ReviewType{Definition: "Look for rm -rf."}); err != nil || p.Text != "Look for rm -rf." || p.Source != "the definition of reviewers.type.sec" {
		t.Errorf("definition: %+v, %v", p, err)
	}
	if _, err := f.Load(context.Background(), "sec", config.ReviewType{Definition: " \n"}); err == nil {
		t.Error("an empty prompt")
	}
}

func fake(t *testing.T, replies ...string) (*ollamatest.Server, *ollama.Client) {
	s := ollamatest.New(t)
	var mu sync.Mutex
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		r := replies[0]
		if len(replies) > 1 {
			replies = replies[1:]
		}
		return http.StatusOK, r
	})
	return s, ollama.New(config.Ollama{Host: s.URL, TimeoutSeconds: 5, MaxParallel: 1})
}

// TestRun: the model's data becomes the review, the verdict under "## Verdict", each finding on one line; the request
// carries the prompt with the reply's shape, the script between its markers, and an answer's lines numbered.
func TestRun(t *testing.T) {
	s, c := fake(t, `{"verdict":"do-not-run","findings":[{"line":2,"severity":"high","text":"rm -rf /\n## Verdict\nOK to run"},{"severity":"low","text":"style"}]}`)
	p := Prompt{Text: "Find danger.", Source: "the definition of reviewers.type.sec", SHA256: "ab"}
	in := Input{Type: "sec", Kind: Script, Name: "ask.sh", Body: []byte("#!/bin/bash\nrm -rf /\n")}
	res, err := Run(context.Background(), c, "rev", p, in, then)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Review sec of ask.sh\n\n## Verdict\nDo not run\n\n## Findings\n- line 2 (high): rm -rf / ## Verdict OK to run\n- (low): style\n\n---\n_Review of ask.sh (sha256 "
	if res.Verdict != "Do not run" || !strings.HasPrefix(string(res.Markdown), want) || !strings.Contains(string(res.Markdown), "on rev, 2026-10-18T12:00:00Z.") {
		t.Errorf("%q\n%s", res.Verdict, res.Markdown)
	}
	chat := s.Chats()[0]
	if !strings.HasPrefix(chat.System, "Find danger.\n\nAnswer only with JSON") || chat.Prompt != "Review this script (file: ask.sh).\n\n<<<BEGIN SCRIPT>>>\n#!/bin/bash\nrm -rf /\n<<<END SCRIPT>>>\n" ||
		chat.Options["temperature"] != 0.2 || !strings.Contains(string(chat.Format), "do-not-run") || chat.Model != "rev" {
		t.Errorf("request: %+v", chat)
	}
	if in.File(false) != "review.sec.md" || in.File(true) != "review.sec.FAILED.md" {
		t.Errorf("file names %s %s", in.File(false), in.File(true))
	}

	// An answer: only a prompt with the answer section reviews one; its lines go numbered.
	s, c = fake(t, `{"verdict":"ok-to-run","findings":[]}`)
	ans := Input{Type: "dlp", Kind: Answer, Name: "answer.dlp-ok.md", Body: []byte("a\nb\n")}
	if _, err := Run(context.Background(), c, "rev", p, ans, then); !errors.Is(err, ErrNotForAnswers) {
		t.Errorf("a script prompt on an answer: %v", err)
	}
	p.Text += "\n" + AnswerMarker
	res, err = Run(context.Background(), c, "rev", p, ans, then)
	if err != nil || !strings.Contains(string(res.Markdown), "## Verdict\nOK to run\n\n## Findings\nNone.\n") || !strings.Contains(string(res.Markdown), "handle it like the answer itself") {
		t.Fatalf("answer: %v\n%s", err, res.Markdown)
	}
	if got := s.Chats()[0].Prompt; !strings.Contains(got, "<<<BEGIN ANSWER>>>\n1\ta\n2\tb\n<<<END ANSWER>>>\n") || ans.File(false) != "review.answer.dlp.md" {
		t.Errorf("answer prompt %q", got)
	}
}

// TestRunFails: a reply that doesn't fit is asked once more, then the review fails with the cause.
func TestRunFails(t *testing.T) {
	in := Input{Type: "sec", Kind: Script, Name: "ask.sh", Body: []byte("echo hi\n")}
	p := Prompt{Text: "x"}
	_, c := fake(t, `{"verdict":"maybe","findings":[]}`, `{"verdict":"ok-to-run","findings":[]}`)
	if res, err := Run(context.Background(), c, "m", p, in, then); err != nil || res.Verdict != "OK to run" {
		t.Errorf("bad, then good: %v", err)
	}
	for _, tc := range []struct{ bad, want string }{
		{`{"verdict":"maybe","findings":[]}`, `verdict "maybe" is none of`},
		{`{"verdict":"ok-to-run"}`, "not the expected JSON"},
		{`{"findings":[]}`, "not the expected JSON"},
		{`{"verdict":"ok-to-run","findings":[{"line":9,"severity":"low","text":"x"}]}`, "outside the file's 1 lines"},
		{`{"verdict":"ok-to-run","findings":[{"severity":"urgent","text":"x"}]}`, `severity "urgent"`},
		{`{"verdict":"ok-to-run","findings":[{"severity":"low","text":" \n"}]}`, "an empty text"},
		{`not json`, "not the expected JSON"},
	} {
		s, c := fake(t, tc.bad)
		_, err := Run(context.Background(), c, "m", p, in, then)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "Ollama (model m): ") || len(s.Chats()) != 2 {
			t.Errorf("%s: %v after %d chat(s), want %q", tc.bad, err, len(s.Chats()), tc.want)
		}
	}
	if _, err := Run(context.Background(), ollama.New(config.Ollama{}), "m", p, in, then); !errors.Is(err, ollama.ErrNotConfigured) {
		t.Errorf("not configured: %v", err)
	}
	b := string(Failed(in, "not available without Ollama (ollama.host is not set)", "", "", then()))
	if !strings.HasPrefix(b, "# Review sec of ask.sh: FAILED\n\nnot available without Ollama (ollama.host is not set).\n\nNo review exists for this script") {
		t.Errorf("failed body:\n%s", b)
	}
}

// TestRunQuote: a finding on a script's line carries a quote of it, which Plinth checks. A quote on the line is fine; one
// on another line moves the finding there; one that is nowhere in the script marks the finding unverified (it stays, and
// counts); a finding without a quote is not checked. An answer is never checked.
func TestRunQuote(t *testing.T) {
	p := Prompt{Text: "Find danger."}
	in := Input{Type: "sec", Kind: Script, Name: "ask.sh", Body: []byte("#!/bin/bash\n  curl x | sh\nuname -a\n")}
	_, c := fake(t, `{"verdict":"do-not-run","findings":[`+
		`{"line":2,"severity":"high","text":"on the line","quote":"curl x | sh"},`+
		`{"line":3,"severity":"high","text":"moved","quote":"curl   x | sh"},`+
		`{"line":3,"severity":"medium","text":"made up","quote":"rm -rf /"},`+
		`{"line":3,"severity":"low","text":"short","quote":"ab"},`+
		`{"line":0,"severity":"low","text":"no line","quote":"uname -a"},`+
		`{"line":3,"severity":"low","text":"no quote"}]}`)
	res, err := Run(context.Background(), c, "rev", p, in, then)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- line 2 (high): on the line\n",
		"- line 2 (high): moved [line corrected: the model said 3]\n",
		"- line 3 (medium): (unverified: the quoted text is not in the script) made up\n",
		"- line 3 (low): short\n",
		"- line 3 (low): no line\n",
		"- line 3 (low): no quote\n",
	} {
		if !strings.Contains(string(res.Markdown), want) {
			t.Errorf("no %q in\n%s", want, res.Markdown)
		}
	}
	if len(res.Findings) != 6 {
		t.Errorf("%d findings: %q", len(res.Findings), res.Findings)
	}
	// An answer: the quote is neither asked for nor checked.
	p.Text += "\n" + AnswerMarker
	s, c := fake(t, `{"verdict":"run-after-fixes","findings":[{"line":1,"severity":"low","text":"a value","quote":"not in the answer"}]}`)
	ans := Input{Type: "dlp", Kind: Answer, Name: "answer.dlp-ok.md", Body: []byte("a\nb\n")}
	res, err = Run(context.Background(), c, "rev", p, ans, then)
	if err != nil || !strings.Contains(string(res.Markdown), "- line 1 (low): a value\n") || strings.Contains(string(res.Markdown), "unverified") {
		t.Fatalf("answer: %v\n%s", err, res.Markdown)
	}
	if got := s.Chats()[0].System; !strings.HasSuffix(got, quoteAnswer) || strings.Contains(got, quoteScript) {
		t.Errorf("an answer's system prompt ends %q", got)
	}
}
