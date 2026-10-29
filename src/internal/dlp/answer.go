package dlp

import (
	"context"

	"plinth/internal/doc"
	"plinth/internal/ollama"
)

// ScanAnswer is the DLP scan of an answer document: its texts masked in place (ScanTexts; the fixed masks also
// after an error) and status.dlp set, ok, or fail with the reason.
func ScanAnswer(ctx context.Context, c ollama.Chatter, model string, a *doc.Answer) (Result, error) {
	ptrs := a.Texts()
	texts := make([][]byte, len(ptrs))
	for i, p := range ptrs {
		texts[i] = []byte(*p)
	}
	out, res, err := ScanTexts(ctx, c, model, texts)
	for i, p := range ptrs {
		*p = string(out[i])
	}
	d := &doc.DLP{Result: doc.ResultOK, Model: model, Chunks: res.Chunks, Fixed: res.Fixed, Masked: res.Lines}
	if err != nil {
		d.Result, d.Reason = doc.ResultFail, err.Error()
	}
	a.Status = &doc.AnswerStatus{DLP: d}
	return res, err
}
