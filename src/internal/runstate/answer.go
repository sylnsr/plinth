package runstate

import (
	"fmt"
	"strings"

	"plinth/internal/doc"
)

// DLPLine is the DLP scan's line of the answer document b, from its status.dlp: "DLP scan: ok (model ...)" or
// "DLP scan: FAILED - <why>. Fixed patterns: <n> mask(s)."; "" for none, and for a document that doesn't decode.
func DLPLine(b []byte) string {
	var a doc.Answer
	if doc.Decode(b, &a) != nil || a.Status == nil || a.Status.DLP == nil {
		return ""
	}
	d := a.Status.DLP
	if d.Result == doc.ResultOK {
		return fmt.Sprintf("DLP scan: ok (model %s, %d chunk(s), %d fixed-pattern mask(s), %d line(s) masked)", d.Model, d.Chunks, d.Fixed, d.Masked)
	}
	why := strings.Join(strings.Fields(d.Reason), " ")
	return fmt.Sprintf("DLP scan: FAILED - %s. Fixed patterns: %d mask(s).", why, d.Fixed)
}

// AnswerLines are the first n lines of the answer document b that aren't blank: its sections, "## <title>" and the
// lines of its output, then its failure. A document that doesn't decode gives none.
func AnswerLines(b []byte, n int) []string {
	var a doc.Answer
	if doc.Decode(b, &a) != nil {
		return nil
	}
	var out []string
	add := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) != "" && len(out) < n {
				out = append(out, l)
			}
		}
	}
	for _, s := range a.Sections {
		if s.Title != "" {
			add("## " + s.Title)
		}
		add(s.Output)
	}
	if a.Failure != "" {
		add("FAIL: " + a.Failure)
	}
	return out
}
