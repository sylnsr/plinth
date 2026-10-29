package plain

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/config"
	"plinth/internal/doc"
	"plinth/internal/ollama"
	"plinth/internal/ollama/ollamatest"
)

// TestDLPScan: dlp-scan scans only an answer document in a run directory; a failed scan, or one that can't ask
// the model, leaves the file as it was.
func TestDLPScan(t *testing.T) {
	s := ollamatest.New(t)
	s.SetReply(func(context.Context, ollamatest.Chat) (int, string) { return http.StatusNotFound, "model not found" })
	c := ollama.New(config.Ollama{Host: s.URL, TimeoutSeconds: 5, MaxParallel: 1})
	raw := filepath.Join(t.TempDir(), "raw")
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: "brave-fox-12"},
		Sections: []doc.Section{{Output: "token=abc\n"}}, Result: doc.ResultOK}
	file := filepath.Join(t.TempDir(), "brave-fox-12", "answer.yml")
	write(t, file, string(a.Encode()))
	if _, err := DLPScan(context.Background(), c, "m", raw, file); err == nil || !strings.Contains(err.Error(), "HTTP 404: model not found") {
		t.Errorf("failed scan: %v", err)
	}
	if _, err := DLPScan(context.Background(), ollama.New(config.Ollama{}), "m", raw, file); !errors.Is(err, ollama.ErrNotConfigured) {
		t.Errorf("not configured: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != string(a.Encode()) || exists(filepath.Join(raw, "brave-fox-12")) {
		t.Errorf("after a failed scan: %q", b)
	}
	md := filepath.Join(filepath.Dir(file), "answer.md")
	write(t, md, "token=abc\n")
	flat := filepath.Join(t.TempDir(), "answer-brave-fox-12.yml")
	write(t, flat, string(a.Encode()))
	for f, want := range map[string]string{md: "not an answer document (.yml)", flat: "not in a run directory"} {
		if _, err := DLPScan(context.Background(), c, "m", raw, f); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

// TestDLPScanDocument: an answer document is scanned as one: its texts masked, status.dlp set, the original kept; a
// scanned one is left alone, an unfinished or invalid one refused, the file untouched.
func TestDLPScanDocument(t *testing.T) {
	s := ollamatest.New(t)
	s.SetReply(ollamatest.DLP(func(text string) string {
		if strings.Contains(text, "Zq8vX2pLm9") {
			return "encryption_key"
		}
		return ""
	}))
	c := ollama.New(config.Ollama{Host: s.URL, TimeoutSeconds: 5, MaxParallel: 1})
	raw := filepath.Join(t.TempDir(), "raw")
	dir := filepath.Join(t.TempDir(), "calm-otter")
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: "calm-otter"},
		Sections: []doc.Section{{Title: "1. keys\n", Output: "luks: Zq8vX2pLm9TtR4wE\npassword=hunter2\n"}}, Failure: "token=abc\n", Result: doc.ResultFail}
	file := filepath.Join(dir, "answer.FAIL.yml")
	write(t, file, string(a.Encode()))
	res, err := DLPScan(context.Background(), c, "m", raw, file)
	if err != nil || res.Lines != 1 || res.Fixed != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	b, _ := os.ReadFile(file)
	var got doc.Answer
	if err := doc.Decode(b, &got); err != nil || got.Check(false) != nil || got.Sections[0].Output != "[line 1 masked by dlp-scan: encryption_key]\npassword=<masked>\n" ||
		got.Failure != "token=<masked>\n" || got.Status.DLP.Result != doc.ResultOK || got.Status.DLP.Masked != 1 {
		t.Errorf("%+v %v\n%s", got, err, b)
	}
	if o, _ := os.ReadFile(filepath.Join(raw, "calm-otter", "answer.FAIL.yml.orig")); string(o) != string(a.Encode()) {
		t.Errorf("original: %q", o)
	}
	if _, err := DLPScan(context.Background(), c, "m", raw, file); !errors.Is(err, ErrScanned) {
		t.Errorf("again: %v", err)
	}
	for name, body := range map[string]string{"running.yml": string(a.Encode()), "answer.new.yml": string(a.Encode()), "answer.x.yml": "kind: Answer\n"} {
		f := filepath.Join(dir, name)
		write(t, f, body)
		if _, err := DLPScan(context.Background(), c, "m", raw, f); err == nil {
			t.Errorf("%s: scanned", name)
		}
		if b, _ := os.ReadFile(f); string(b) != body {
			t.Errorf("%s: changed", name)
		}
	}
}
