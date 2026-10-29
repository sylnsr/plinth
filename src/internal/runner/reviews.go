package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"plinth/internal/decide"
	"plinth/internal/dlp"
	"plinth/internal/logfile"
	"plinth/internal/ollama"
	"plinth/internal/request"
	"plinth/internal/review"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// reviewJob is one Ollama review the loop started (autoReviews, reviewRequest): the prompt and the model call run in
// the background, behind the process's Ollama semaphore; once done is closed, applyReviews writes the result.
type reviewJob struct {
	agent, run string
	in         review.Input
	scriptSum  string // ask.sh's sha256 when the job started: the run the review belongs to
	request    bool   // from the request review.<type>: it replaces the type's review, then the request is finished
	done       chan struct{}
	prompt     review.Prompt
	res        *review.Result
	err        error
}

func reviewKey(agent, run string, in review.Input) string {
	return agent + "/" + run + "/" + in.File(false)
}

// startReview starts a review of in for the run; err, if not nil, is the review's result at once (it fails).
func (l *Runner) startReview(agent, run, scriptSum string, in review.Input, request bool, err error) {
	j := &reviewJob{agent: agent, run: run, in: in, scriptSum: scriptSum, request: request, done: make(chan struct{}), err: err}
	l.reviews[reviewKey(agent, run, in)] = j
	if err == nil && !l.Ollama.Configured() {
		j.err = ollama.ErrNotConfigured // no prompt to fetch for a review that can't run
	}
	if j.err != nil {
		close(j.done)
		return
	}
	ctx, model, t := l.ctx, l.Global.Ollama.Models.Review, l.Global.Reviewers.Type[in.Type]
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer close(j.done)
		if j.prompt, j.err = l.Fetcher.Load(ctx, in.Type, t); j.err != nil {
			return
		}
		j.res, j.err = review.Run(ctx, l.Ollama, model, j.prompt, in, time.Now)
	}()
}

// autoReviews applies the reviews that are done, then starts the automatic reviews (reviewers.automatic) every run
// waits for: of its ask.sh until it runs, and of its answer once the DLP scan passed (dlp-ok or released), for the types
// whose prompt reviews answers. A type whose prompt doesn't is remembered as such while the loop runs.
func (l *Runner) autoReviews(root *os.Root) error {
	if err := l.applyReviews(root); err != nil {
		return err
	}
	runs, err := runstate.List(l.Work, l.ns, "", l.cfg.Automatic, l.live)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Broken != nil {
			l.runLog(r.Agent, r.Name).Once("broken "+l.ns.String()+"/"+r.Agent+"/"+r.Name, "run is broken, skipped", "err", r.Broken)
			continue
		}
		if r.Facts.Running {
			continue
		}
		var script []byte
		var sum string
		var serr error
		read := func() {
			if script == nil && serr == nil {
				script, sum, serr = runstate.CheckScript(l.Work, l.ns, r.Agent, r.Name)
			}
		}
		if !r.Facts.Ran {
			for _, t := range r.Facts.ReviewsPending {
				in := review.Input{Type: t, Kind: review.Script, Name: runstate.AskFile}
				if l.reviews[reviewKey(r.Agent, r.Name, in)] != nil {
					continue
				}
				read()
				in.Body = script
				l.startReview(r.Agent, r.Name, sum, in, false, serr)
			}
		}
		if a := r.Facts.Answer; (a != "dlp-ok" && a != "released") || !l.Ollama.Configured() {
			continue // an answer review is advice: without Ollama none is written
		}
		for _, t := range l.cfg.Automatic {
			in := review.Input{Type: t, Kind: review.Answer, Name: r.AnswerFile()}
			if l.answerless[t] || r.Reviews["answer."+t] != "" || l.reviews[reviewKey(r.Agent, r.Name, in)] != nil {
				continue
			}
			read()
			body, err := safefs.ReadFile(root, filepath.Join(runstate.RunDir(l.ns, r.Agent, r.Name), in.Name), dlp.MaxSize)
			in.Body = body
			l.startReview(r.Agent, r.Name, sum, in, false, errors.Join(serr, err))
		}
	}
	return nil
}

// reviewRequest carries out the request review.<type>: a review of the run's ask.sh as it was stored, which replaces
// the type's review once it is done; the request stays .running until then (applyReviews finishes it).
func (l *Runner) reviewRequest(r *runstate.Run, typ string) {
	script, sum, err := runstate.CheckScript(l.Work, l.ns, r.Agent, r.Name)
	l.startReview(r.Agent, r.Name, sum, review.Input{Type: typ, Kind: review.Script, Name: runstate.AskFile, Body: script}, true, err)
}

// applyReviews writes the result of every review that is done, in a fixed order: review[.answer].<type>.md, or
// .FAILED.md with the reason, and the event review[.answer].<type> (a Do not run verdict fails, as in runstate). An
// automatic review never replaces a file that is there; a requested one replaces the type's review, passed or failed.
// A review whose run is gone, or is another run of that name now (ask.sh's sum), is dropped; one of an ask.sh that
// couldn't be read is written as failed. One cut short by the
// controller's stop is never applied.
func (l *Runner) applyReviews(root *os.Root) error {
	var keys []string
	for k, j := range l.reviews {
		select {
		case <-j.done:
			keys = append(keys, k)
		default:
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		j := l.reviews[k]
		if j.err != nil && l.ctx.Err() != nil {
			continue
		}
		err := l.applyReview(root, j)
		if err == nil {
			delete(l.reviews, k) // an error keeps it, for the next poll
		}
		if err := l.perRun("review "+k, err); err != nil {
			return err
		}
	}
	return nil
}

func (l *Runner) applyReview(root *os.Root, j *reviewJob) error {
	key, dir := j.agent+"/"+j.run, runstate.RunDir(l.ns, j.agent, j.run)
	reqDir, action := request.RunDir(l.ns, j.agent, j.run), decide.Review+"."+j.in.Type
	if errors.Is(j.err, review.ErrNotForAnswers) {
		l.answerless[j.in.Type] = true
		l.nsLog().Component(logfile.Reviews).Info("review type reviews no answer", "type", j.in.Type, "reason", j.err)
		return nil
	}
	if _, sum, err := runstate.CheckScript(l.Work, l.ns, j.agent, j.run); j.scriptSum != "" && (err != nil || sum != j.scriptSum) {
		if j.request {
			if err := l.failRequest(root, reqDir, action, request.Running, "Error: the run changed or is gone while it was reviewed\n"); err != nil {
				return err
			}
		}
		return nil
	}
	failed, body := j.err != nil, []byte(nil)
	outcome, rule, reason := "ok", "allow", ""
	if failed {
		body = review.Failed(j.in, j.err.Error(), j.prompt.Source, l.Global.Ollama.Models.Review, time.Now())
		outcome, rule, reason = "failed", "review.failed", j.err.Error()
	} else {
		body = j.res.Markdown
		h := sha256.Sum256(j.in.Body)
		reason = fmt.Sprintf("verdict %s; %s sha256 %s; model %s, prompt sha256 %s", j.res.Verdict, j.in.Name, hex.EncodeToString(h[:]),
			l.Global.Ollama.Models.Review, j.prompt.SHA256)
		if j.res.Verdict == "Do not run" && j.in.Kind == review.Script {
			outcome, rule = "failed", "review.failed"
		}
	}
	name := j.in.File(failed)
	if j.request {
		if err := safefs.Replace(root, dir, name, body, 0o600); errors.Is(err, fs.ErrNotExist) {
			return nil // scrubbed meanwhile
		} else if err != nil {
			return err
		}
		if err := safefs.Remove(root, filepath.Join(dir, j.in.File(!failed))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else if err := safefs.WriteNew(root, dir, name, body, 0o600); errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	event := decide.Review + "." + j.in.Type
	if j.in.Kind == review.Answer {
		event = decide.Review + ".answer." + j.in.Type
	}
	if _, err := l.event(Caller, event, key, outcome, rule, reason); err != nil {
		return err
	}
	lg, fields := l.runLog(j.agent, j.run).Component(logfile.Reviews), []any{"event", event, "outcome", outcome, "reason", reason}
	if outcome == "failed" {
		lg.Warn("review", fields...)
	} else {
		lg.Info("review", fields...)
	}
	if outcome == "failed" && j.in.Kind == review.Script { // plinth.autosave.failures
		if err := l.autosave(root, j.agent, j.run, decide.Report); err != nil {
			return err
		}
	}
	if j.request {
		return l.requestDone(root, reqDir, action)
	}
	return nil
}
