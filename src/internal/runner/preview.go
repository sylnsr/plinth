package runner

import (
	"context"
	"time"

	"plinth/internal/logfile"
	"plinth/internal/ollama"
	"plinth/internal/review"
	"plinth/internal/runstate"
)

// PreviewReview is one review type's opinion of a script that no run holds (ask_preview).
type PreviewReview struct {
	Type     string
	Verdict  string   // "OK to run", "Run after fixes" or "Do not run"; "" when no review was made
	Findings []string // as review.Result.Findings
	Err      error    // why no review was made: no Ollama, the prompt can't be read, the model failed
}

// Preview reviews script by each automatic review type, as the review of a stored ask.sh would, and keeps nothing: no
// run, no file, no event (the caller records one). One preview runs at a time, so previews queue behind each other
// and not in front of the reviews of stored runs, which have the Ollama semaphore too.
func (l *Runner) Preview(ctx context.Context, script []byte) []PreviewReview {
	l.previewMu.Lock()
	defer l.previewMu.Unlock()
	var out []PreviewReview
	lg := l.nsLog().Component(logfile.Reviews)
	for _, t := range l.cfg.Automatic {
		p := PreviewReview{Type: t}
		start := time.Now()
		lg.Info("preview review starts", "type", t)
		if !l.Ollama.Configured() {
			p.Err = ollama.ErrNotConfigured
			out = append(out, p)
			continue
		}
		prompt, err := l.Fetcher.Load(ctx, t, l.Global.Reviewers.Type[t])
		if err == nil {
			var res *review.Result
			in := review.Input{Type: t, Kind: review.Script, Name: runstate.AskFile, Body: script}
			if res, err = review.Run(ctx, l.Ollama, l.Global.Ollama.Models.Review, prompt, in, time.Now); err == nil {
				p.Verdict, p.Findings = res.Verdict, res.Findings
			}
		}
		p.Err = err
		if err != nil {
			lg.Warn("preview review", "type", t, "took", time.Since(start).Round(time.Second).String(), "err", err)
		} else {
			lg.Info("preview review", "type", t, "verdict", p.Verdict, "took", time.Since(start).Round(time.Second).String())
		}
		out = append(out, p)
	}
	return out
}
