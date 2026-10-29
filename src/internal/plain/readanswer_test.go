package plain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/doc"
)

// scannedAnswer is the answer document of run with status.dlp.result result (ok or fail).
func scannedAnswer(run, result string) string {
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: run},
		Sections: []doc.Section{{Title: "1\n", Output: "secret\n"}}, Result: doc.ResultOK, Status: &doc.AnswerStatus{DLP: &doc.DLP{Result: result}}}
	return string(a.Encode())
}

// TestReadAnswer: every state of a run directory; only answer[.FAIL].dlp-ok.yml with status.dlp.result ok is printed,
// and a status line never holds anything of a file. A name that is no answer document is no answer.
func TestReadAnswer(t *testing.T) {
	dir := t.TempDir()
	for f, body := range map[string]string{
		"calm-otter/ask.sh":                        "x",
		"done/deep-elm/answer.dlp-ok.yml":          scannedAnswer("deep-elm", doc.ResultOK),
		"done/keen-owl/answer.FAIL.dlp-ok.yml":     scannedAnswer("keen-owl", doc.ResultOK),
		"done/gold-oak/answer.FAIL.dlp-ok.yml":     scannedAnswer("gold-oak", doc.ResultFail),
		"done/pale-ash/answer.dlp-ok.yml":          "secret: not an answer\n",
		"done/wild-bee/answer.INVALID.yml":         "secret",
		"warm-sun/answer.new.yml":                  "secret",
		"cold-sea/running.yml":                     "secret",
		"lone-yew/answer.dlp-ok.yml":               scannedAnswer("lone-yew", doc.ResultOK),
		"lone-yew/running.yml":                     "secret rerun",
		"done/dark-sky/answer.DLP-FAIL.yml":        "secret",
		"done/dark-sky/answer.dlp-ok.yml":          scannedAnswer("dark-sky", doc.ResultOK),
		"done/quiet-tide/answer.FAIL.DLP-FAIL.yml": "secret",
		"done/noble-pine/answer.md":                "secret",
		"done/answer-old-elm.md":                   "secret",
	} {
		write(t, filepath.Join(dir, f), body)
	}
	for _, c := range []struct {
		run    string
		code   int
		body   string
		failed bool
	}{
		{"deep-elm", 0, scannedAnswer("deep-elm", doc.ResultOK), false},
		{"keen-owl", 0, scannedAnswer("keen-owl", doc.ResultOK), true},
		{"calm-otter", AnswerNone, "", false},
		{"no-such", AnswerNone, "", false},
		{"cold-sea", AnswerRunning, "", false},
		{"lone-yew", AnswerRunning, "", false},
		{"gold-oak", AnswerDLPFail, "", false},
		{"dark-sky", AnswerDLPFail, "", false},
		{"quiet-tide", AnswerDLPFail, "", false},
		{"pale-ash", AnswerInvalid, "", false},
		{"wild-bee", AnswerInvalid, "", false},
		{"warm-sun", AnswerNotScanned, "", false},
		{"noble-pine", AnswerNone, "", false},
		{"old-elm", AnswerNone, "", false},
	} {
		a, err := ReadAnswer(dir, c.run)
		if err != nil {
			t.Errorf("%s: %v", c.run, err)
			continue
		}
		if a.Code != c.code || string(a.Body) != c.body || a.Failed != c.failed {
			t.Errorf("%s: %+v, want code %d body %q", c.run, a, c.code, c.body)
		}
		if a.Code != 0 && (a.Status == "" || strings.Contains(a.Status, "secret") || !strings.HasPrefix(a.Status, c.run+": ") || !strings.Contains(a.Status, " next: ")) {
			t.Errorf("%s: status %q", c.run, a.Status)
		}
	}
	if _, err := ReadAnswer(dir, "../etc"); err == nil {
		t.Error("a bad RUN_NAME was accepted")
	}
}

// TestReadAnswerSymlink: an answer behind a symlink is never read, and a symlinked run directory isn't a run.
func TestReadAnswerSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "answer.dlp-ok.yml")
	write(t, outside, scannedAnswer("calm-otter", doc.ResultOK))
	write(t, filepath.Join(dir, "calm-otter", "ask.sh"), "x")
	if err := os.Symlink(outside, filepath.Join(dir, "calm-otter", "answer.dlp-ok.yml")); err != nil {
		t.Fatal(err)
	}
	if a, err := ReadAnswer(dir, "calm-otter"); err == nil && a.Body != nil {
		t.Errorf("read through a symlink: %q", a.Body)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "keen-owl")); err != nil {
		t.Fatal(err)
	}
	if a, err := ReadAnswer(dir, "keen-owl"); err != nil || a.Code != AnswerNone {
		t.Errorf("a symlinked run directory: %+v %v", a, err)
	}
}
