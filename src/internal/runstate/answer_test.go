package runstate

import (
	"slices"
	"testing"

	"plinth/internal/doc"
)

func TestDLPLine(t *testing.T) {
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: "keen-tapir"},
		Sections: []doc.Section{{Title: "1. disks\n", Output: "sda ok\n\nsdb ok\n"}}, Failure: "sdc gone\n", Result: doc.ResultFail,
		Status: &doc.AnswerStatus{DLP: &doc.DLP{Result: doc.ResultOK, Model: "m", Chunks: 1, Fixed: 2, Masked: 3}}}
	if got := DLPLine(a.Encode()); got != "DLP scan: ok (model m, 1 chunk(s), 2 fixed-pattern mask(s), 3 line(s) masked)" {
		t.Errorf("ok: %q", got)
	}
	a.Status.DLP = &doc.DLP{Result: doc.ResultFail, Fixed: 1, Reason: "not available without Ollama\n"}
	if got := DLPLine(a.Encode()); got != "DLP scan: FAILED - not available without Ollama. Fixed patterns: 1 mask(s)." {
		t.Errorf("failed: %q", got)
	}
	if got := AnswerLines(a.Encode(), 5); !slices.Equal(got, []string{"## 1. disks", "sda ok", "sdb ok", "FAIL: sdc gone"}) {
		t.Errorf("AnswerLines = %q", got)
	}
	if got := AnswerLines(a.Encode(), 2); len(got) != 2 {
		t.Errorf("AnswerLines of 2 = %q", got)
	}
	a.Status = nil
	if got := DLPLine(a.Encode()); got != "" {
		t.Errorf("no status: %q", got)
	}
	if got, lines := DLPLine([]byte("not: [a document")), AnswerLines([]byte("x: ["), 5); got != "" || lines != nil {
		t.Errorf("not a document: %q, %q", got, lines)
	}
}
