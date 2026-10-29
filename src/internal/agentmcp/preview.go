package agentmcp

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"plinth/internal/asklint"
	"plinth/internal/decide"
	"plinth/internal/dlp"
	"plinth/internal/mcp"
	"plinth/internal/runstate"
)

// previewWindow is the span plinth.preview.limit counts an agent's previews in.
const previewWindow = time.Hour

const previewArgs = `{"type":"object","properties":{` +
	`"script":{"type":"string","description":"the whole ask script, from #!/bin/bash on"},` +
	`"id":{"type":"string","description":"the id of a preview that answered pending: gives its result instead of starting one (then give no script)"}` +
	`},"additionalProperties":false}`

// allowPreview counts a preview of agent now and says whether it is within plinth.preview.limit per hour; the
// preview that is over the limit isn't counted, and wait is how long until the oldest one of the hour falls out of it.
func (s *Server) allowPreview(agent string, now time.Time) (ok bool, wait time.Duration) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	if s.previews == nil {
		s.previews = map[string][]time.Time{}
	}
	var recent []time.Time
	for _, t := range s.previews[agent] {
		if now.Sub(t) < previewWindow {
			recent = append(recent, t)
		}
	}
	if ok = len(recent) < s.Global.Plinth.Preview.Limit; ok {
		recent = append(recent, now)
	} else {
		wait = previewWindow - now.Sub(recent[0])
	}
	s.previews[agent] = recent
	return ok, wait
}

// minutes says d in whole minutes, rounded up: "1 minute", "7 minutes".
func minutes(d time.Duration) string {
	if n := int((d + time.Minute - 1) / time.Minute); n > 1 {
		return fmt.Sprintf("%d minutes", n)
	}
	return "1 minute"
}

// previewJob is one preview whose reviews run in the background: the model runs outlast what a reply may take, so
// ask_preview answers "pending <id>" and the agent asks again with the id. It keeps its result for an hour.
type previewJob struct {
	agent string
	made  time.Time
	done  chan struct{} // closed once out is set
	out   string        // the reply
}

// maxRunningPreviews is how many previews of one agent may be unfinished at a time: they queue behind each other for
// the model, so more would only pile up.
const maxRunningPreviews = 3

// previewWait is how long ask_preview waits for the reviews before it answers "pending <id>"; previewMax is the
// longest the reviews of one preview may take.
const (
	previewWait = 30 * time.Second
	previewMax  = 15 * time.Minute
)

// askPreview checks a script as ask_submit and the automatic reviews would, and stores nothing. Its answer is
// advice: the real checks run on the stored script, and the user decides. Every preview is an event, so the user sees
// an agent iterating on one. With id (and no script) it asks for the result of a preview that was still running.
func (s *Server) askPreview(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Script string `json:"script"`
		ID     string `json:"id"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	switch {
	case a.ID != "" && a.Script != "":
		return "", errors.New("arguments: give script to start a preview, or id to ask for the result of one - not both")
	case a.ID != "":
		return s.previewResult(ctx, k, a.ID)
	case a.Script == "":
		return "", errors.New("arguments: script is required (the whole ask script; ask_template gives the one to start from)")
	case len(a.Script) > maxScript:
		return "", fmt.Errorf("the script is %d bytes, at most %d - next: shorten it; guide says what a script must be", len(a.Script), maxScript)
	}
	script := []byte(a.Script)
	mode := runstate.ModeOf(script)
	if _, err := k.decide(decide.AskPreview, "", &decide.Submission{Script: script, Network: mode.Network, Privileged: mode.Privileged}); err != nil {
		return "", err
	}
	if s.Global.Ollama.Host == "" { // a preview with no review would pass a script no model has looked at
		reason := "no review can be made: ollama.host is not set"
		if _, err := k.n.Record(k.c.agent, decide.AskPreview, k.c.agent, string(decide.Deny), "ask.preview.ollama", reason); err != nil {
			return "", err
		}
		return "", fmt.Errorf("refused by rule ask.preview.ollama: %s - next: ask_preview needs Ollama; check the script against the guide, "+
			"then ask_submit it (Plinth's reviews run when Ollama is up) or tell the user", reason)
	}
	if n := s.runningPreviews(k.c.agent); n >= maxRunningPreviews {
		return "", fmt.Errorf("%d of your previews are still running - next: ask for their results (ask_preview with the id) before you "+
			"start another", n)
	}
	limit := s.Global.Plinth.Preview.Limit
	if ok, wait := s.allowPreview(k.c.agent, time.Now()); !ok {
		reason := fmt.Sprintf("at most %d previews per hour (plinth.preview.limit); the next is free in %s", limit, minutes(wait))
		if _, err := k.n.Record(k.c.agent, decide.AskPreview, k.c.agent, string(decide.Deny), "ask.preview.limit", reason); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s - next: don't retry in a loop; fix the script from what the earlier previews showed, then wait or submit "+
			"it with ask_submit (the user reads it and its reviews before anything runs, and a failed round costs more than a preview)", reason)
	}
	var b strings.Builder
	b.WriteString("preview of your script - nothing was stored, nothing runs; the real checks and reviews run on a script you submit, " +
		"and the user decides:\n")
	lint := asklint.Check(script, asklint.Options{Plinth: true, Work: s.Work, ForgejoURL: s.Global.ForgejoURL})
	if len(lint) == 0 {
		b.WriteString("- ask lint: ok\n")
	} else {
		fmt.Fprintf(&b, "- ask lint: %d finding(s), ask_submit refuses the script until they are fixed:\n%s\n", len(lint), strings.Join(lint, "\n"))
	}
	clean, masked := dlp.Fixed(script) // the reviews are given the script with those values masked
	if masked == 0 {
		b.WriteString("- DLP masks: none\n")
	} else {
		fmt.Fprintf(&b, "- DLP masks: %d value(s) look like a secret (a key, a token, a password=-style assignment): never put one in a "+
			"script; read it from the environment or let the tool ask for it\n", masked)
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	job := &previewJob{agent: k.c.agent, made: time.Now(), done: make(chan struct{})}
	s.storePreview(id, job)
	sum := sha256.Sum256(script)
	go func() { // not the request's context: the reviews go on after the reply
		defer close(job.done)
		rctx, cancel := context.WithTimeout(context.Background(), previewMax)
		defer cancel()
		outcomes := []string{}
		for _, r := range k.n.Preview(rctx, clean) {
			switch {
			case r.Err != nil:
				fmt.Fprintf(&b, "- review %s: no review (%v)\n", r.Type, r.Err)
				outcomes = append(outcomes, r.Type+"=none")
			case len(r.Findings) == 0:
				fmt.Fprintf(&b, "- review %s: %s, no findings\n", r.Type, r.Verdict)
				outcomes = append(outcomes, r.Type+"="+r.Verdict)
			default:
				fmt.Fprintf(&b, "- review %s: %s, %d finding(s):\n  - %s\n", r.Type, r.Verdict, len(r.Findings), strings.Join(r.Findings, "\n  - "))
				outcomes = append(outcomes, r.Type+"="+r.Verdict)
			}
		}
		if len(outcomes) == 0 {
			b.WriteString("- reviews: none are automatic in this namespace\n")
		}
		reason := fmt.Sprintf("script sha256 %s; lint %d; dlp masks %d; reviews %s", hex.EncodeToString(sum[:]), len(lint), masked, strings.Join(outcomes, ", "))
		if _, err := k.n.Record(k.c.agent, decide.AskPreview, k.c.agent, string(decide.Allow), "allow", reason); err != nil {
			fmt.Fprintf(&b, "- the preview could not be recorded in the event log (%v); tell the user\n", err)
		}
		b.WriteString("- next: fix what was found and preview again, or ask_submit it. Before you write the next script, remember: " +
			"record in ai/memory/plinth-review-failures.md each pattern a check or review refused and what fixed it (guide: Previewing a script), " +
			"and read that record before writing an ask script")
		job.out = b.String()
	}()
	return s.previewReply(ctx, job, id), nil
}

// previewResult is the reply to ask_preview with an id: the result of the agent's preview, or pending again.
func (s *Server) previewResult(ctx context.Context, k *turn, id string) (string, error) {
	if _, err := k.decide(decide.AskPreview, "", &decide.Submission{}); err != nil {
		return "", err
	}
	s.previewMu.Lock()
	job := s.previewJobs[k.c.agent+"/"+id]
	s.previewMu.Unlock()
	if job == nil {
		return "", fmt.Errorf("no preview %q of yours (a result is kept for an hour) - next: preview the script again", id)
	}
	return s.previewReply(ctx, job, id), nil
}

// previewReply is the job's result once it is done, or "pending <id>" when it isn't within previewWait.
func (s *Server) previewReply(ctx context.Context, job *previewJob, id string) string {
	select {
	case <-job.done:
		return job.out
	case <-time.After(cmp.Or(s.PreviewWait, previewWait)):
	case <-ctx.Done():
	}
	return fmt.Sprintf("pending %s - the reviews are still running (a model run each) - next: call ask_preview with id %s in a few "+
		"seconds; it gives the result when they are done", id, id)
}

// storePreview keeps job under id for an hour and forgets the older ones.
func (s *Server) storePreview(id string, job *previewJob) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	if s.previewJobs == nil {
		s.previewJobs = map[string]*previewJob{}
	}
	for k, j := range s.previewJobs {
		if time.Since(j.made) > previewWindow {
			delete(s.previewJobs, k)
		}
	}
	s.previewJobs[job.agent+"/"+id] = job
}

// runningPreviews is how many of agent's previews are unfinished.
func (s *Server) runningPreviews(agent string) int {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	n := 0
	for _, j := range s.previewJobs {
		select {
		case <-j.done:
		default:
			if j.agent == agent {
				n++
			}
		}
	}
	return n
}

// newID is a preview's id: "pv-" and 8 hex characters from crypto/rand. The prefix keeps a client from reading an id of
// digits (or 12345e6) as a number.
func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "pv-" + hex.EncodeToString(b), nil
}
