package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"plinth/internal/auth"
	"plinth/internal/decide"
	"plinth/internal/events"
	"plinth/internal/names"
	"plinth/internal/request"
	"plinth/internal/review"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// pollRequests is the second half of a namespace poll: finish the execs whose child ended and the saves and reports
// that are done, finish a scrub a crash left, then carry out the requests in <ns-dir>/requests/<agent>/<RUN>/, run by
// run.
func (l *Runner) pollRequests(root *os.Root) error {
	if err := l.finishExecs(root); err != nil {
		return err
	}
	if err := l.applyPosts(root); err != nil {
		return err
	}
	if err := l.recoverScrubs(root); err != nil {
		return err
	}
	base := filepath.Join(l.ns.Owner, l.ns.Repo, request.Dir)
	agentDirs, err := subdirs(root, base)
	if err != nil {
		return err
	}
	for _, agent := range agentDirs {
		if agent == "messages" {
			continue // requests/messages/<message-id>/: they come with the messages
		}
		if !names.ValidAgent(agent) {
			l.nsLog().Once("requests "+l.ns.String()+"/"+agent, "not an agent name, left alone", "path", filepath.Join(request.Dir, agent))
			continue
		}
		runs, err := subdirs(root, filepath.Join(base, agent))
		if err != nil {
			return err
		}
		for _, run := range runs {
			if !names.ValidRun(run) {
				l.nsLog().Once("requests "+l.ns.String()+"/"+agent+"/"+run, "not a run name, left alone", "path", filepath.Join(request.Dir, agent, run))
				continue
			}
			if err := l.perRun("requests of "+agent+"/"+run, l.runRequests(root, agent, run)); err != nil {
				return err
			}
		}
	}
	return nil
}

// runRequests carries out one run's queued requests (and those a stopped controller left starting or running), in
// request.Rank order.
func (l *Runner) runRequests(root *os.Root, agent, run string) error {
	dir := request.RunDir(l.ns, agent, run)
	entries, err := safefs.ReadDir(root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // scrubbed meanwhile
	}
	if err != nil {
		return err
	}
	type req struct{ action, state string }
	var reqs []req
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue // a request still being written (safefs temporary file)
		}
		action, state, ok := request.Parse(n)
		if !ok || !e.Type().IsRegular() {
			l.runLog(agent, run).Once("request "+l.ns.String()+"/"+agent+"/"+run+"/"+n, "not a request, left alone", "file", n)
			continue
		}
		if state != request.Failed && state != request.Finished {
			reqs = append(reqs, req{action, state})
		}
	}
	slices.SortFunc(reqs, func(a, b req) int { return request.Rank(a.action) - request.Rank(b.action) })
	for _, q := range reqs {
		if err := l.carryOutRun(root, agent, run, q.action, q.state); err != nil {
			return err
		}
	}
	return nil
}

// carryOutRun carries out one request: .queued is renamed to .running (an exec's to .starting, see startExec), decide is
// asked (and the event log records its answer), then the action runs. The request is renamed to .finished once its
// result is in the run, or to .FAILED with the rule and reason (or the error) as its content; an exec left .running
// never fails: its script started, so an error leaves it for the next poll. Only an event log that can't be written
// fails the loop: nothing may act unrecorded.
func (l *Runner) carryOutRun(root *os.Root, agent, run, action, state string) error {
	key := agent + "/" + run
	dir := request.RunDir(l.ns, agent, run)
	if action == decide.Exec {
		if l.child(key) != nil {
			return nil // its child runs, or ended and is being finished
		}
		if limit := l.Global.Plinth.Runner.MaxParallelExec; state != request.Running && l.Busy() >= limit {
			l.runLog(agent, run).Once("exec wait "+l.ns.String(), "exec waits for a free place (plinth.runner.max-parallel-exec)", "running", l.Busy(), "limit", limit)
			return nil
		}
		l.Log.Clear("exec wait " + l.ns.String())
	}
	if t, ok := strings.CutPrefix(action, decide.Review+"."); ok && l.reviews[reviewKey(agent, run, review.Input{Type: t, Kind: review.Script})] != nil {
		return nil // its review runs
	}
	if l.posts[postKey(agent, run, action)] != nil {
		return nil // its save or report runs
	}
	var err error
	switch {
	case state == request.Queued:
		next := request.Running
		if action == decide.Exec {
			next = request.Starting // .running only once its script starts
		}
		err = safefs.Rename(root, filepath.Join(dir, action+"."+request.Queued), filepath.Join(dir, action+"."+next))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		state = next
	case state == request.Starting:
		l.runLog(agent, run).Info("request left over from an earlier controller, which never started the script: carrying it out again", "action", action)
		err = l.restartExec(root, agent, run)
	case action == decide.Exec && lockHeld(root, TerminalLock(l.ns, agent, run)):
		// A run in the user's terminal that an earlier controller started: its script holds the lock while it runs.
		l.runLog(agent, run).Once("terminal "+l.ns.String()+"/"+key, "request left over from an earlier controller, and still running in the user's terminal: finished once it ends", "action", action)
		return nil
	case action == decide.Exec:
		l.Log.Clear("terminal " + l.ns.String() + "/" + key)
		l.runLog(agent, run).Info("request left over from an earlier controller, which started the script: finishing it", "action", action)
	default:
		l.runLog(agent, run).Info("request left over from an earlier controller: carrying it out again", "action", action)
	}
	if err == nil {
		err = l.act(root, agent, run, action, state)
	}
	if errors.Is(err, errEventLog) {
		return err
	}
	if err != nil && action == decide.Exec && state == request.Running {
		return err // its script started: the request stays .running, the sign of that, and the next poll tries again
	}
	if err != nil && action == decide.Exec && state == request.Starting && l.Stopping.Err() != nil {
		return nil // the controller stops, a drift check cut short: still .starting, so the next start carries it out again
	}
	if err != nil {
		l.runLog(agent, run).Error("request failed", "action", action, "err", err)
		return l.failRequest(root, dir, action, state, "Error: "+err.Error()+"\n")
	}
	return nil
}

// act decides about one request, in state (starting or running), and carries it out; it finishes or fails the request
// itself, except on an error. An exec request left running is finished, never carried out again: its script started.
func (l *Runner) act(root *os.Root, agent, run, action, state string) error {
	key, dir := agent+"/"+run, request.RunDir(l.ns, agent, run)
	typ, isReview := strings.CutPrefix(action, decide.Review+".")
	if action == decide.Exec && state == request.Running {
		return l.leftoverExec(root, agent, run)
	}
	d, r, err := request.Decide(l.Work, l.ns, l.Global, l.cfg, agent, run, action, l.live)
	if err != nil {
		return err
	}
	rec, err := l.event(auth.HostUser().String(), action, key, string(d.Outcome), d.Rule, d.Reason)
	if err != nil {
		return err
	}
	if d.Outcome != decide.Allow {
		l.runLog(agent, run).Info("request refused", "action", action, "outcome", d.Outcome, "rule", d.Rule, "reason", d.Reason)
		return l.failRequest(root, dir, action, state, fmt.Sprintf("Rule: %s\nReason: %s\nPolicy: %s\n", d.Rule, d.Reason, d.Policy))
	}
	switch {
	case isReview:
		l.reviewRequest(r, typ) // the request is .running until the review is written (applyReviews)
		return nil
	case action == decide.Exec:
		return l.startExec(root, r) // the request is .running from right before the start until the exec is finished
	case action == decide.Scrub:
		return l.scrub(root, agent, run) // the request goes with the run's request directory
	case action == decide.Save:
		return l.startSave(root, r, rec) // the request is .running until the comment is posted (applyPosts)
	case action == decide.Report:
		return l.startReport(root, r, rec) // the request is .running until the issue is opened (applyPosts)
	case action == decide.Done:
		err = l.release(root, r)
	case action == decide.OverrideReview, action == decide.OverrideDLP:
		err = l.override(root, r, action, rec)
	case action == decide.AcceptNetwork:
		err = l.acceptNetwork(root, r, rec)
	}
	if err != nil {
		return err
	}
	if err := l.requestDone(root, dir, action); err != nil {
		return err
	}
	if action == decide.Done {
		return l.autosave(root, agent, run, decide.Save) // plinth.autosave.runs: Save right after Done
	}
	return nil
}

// requestDone renames a carried-out request from .running to .finished. It stays, as does a .FAILED an earlier try of
// the same action left: only Scrub deletes a run's requests.
func (l *Runner) requestDone(root *os.Root, dir, action string) error {
	return safefs.Rename(root, filepath.Join(dir, action+"."+request.Running), filepath.Join(dir, action+"."+request.Finished))
}

// failRequest renames <action>.<state> (starting or running) to <action>.FAILED with body (the rule and reason, or the
// error) as its content.
func (l *Runner) failRequest(root *os.Root, dir, action, state, body string) error {
	err := safefs.Replace(root, dir, action+"."+state, []byte(body), 0o600)
	if err == nil {
		err = safefs.Rename(root, filepath.Join(dir, action+"."+state), filepath.Join(dir, action+"."+request.Failed))
	}
	if err != nil {
		l.nsLog().Error("marking a request failed did not work", "path", filepath.Join(dir, action), "err", err)
	}
	return nil
}

// release is Done: the scanned answer (dlp-ok, or DLP-FAIL after override.dlp) becomes answer[.FAIL].released.yml.
func (l *Runner) release(root *os.Root, r *runstate.Run) error {
	dir := runstate.RunDir(l.ns, r.Agent, r.Name)
	name := r.AnswerFile()
	released := "answer.released.yml"
	if r.Gate {
		released = "answer.FAIL.released.yml"
	}
	if err := safefs.Rename(root, filepath.Join(dir, name), filepath.Join(dir, released)); err != nil {
		return err
	}
	l.runLog(r.Agent, r.Name).Info("answer released")
	return nil
}

// override writes override.review.md or override.dlp.md: what failed, when, and the receipt of the event that allowed it.
func (l *Runner) override(root *os.Root, r *runstate.Run, action string, rec events.Receipt) error {
	what := "DLP scan of the answer"
	if action == decide.OverrideReview {
		what = "review(s) " + strings.Join(r.Facts.ReviewsFailed, ", ")
	}
	body := fmt.Sprintf("# Override of %s/%s\n\nThe user overrode the failed %s at %s.\n\nEvent: seq %d, sha256 %s (plinthctl events show -n %s %s)\n",
		r.Agent, r.Name, what, time.Now().UTC().Format(time.RFC3339), rec.Seq, rec.Hash, l.ns, r.Name)
	err := safefs.WriteNew(root, runstate.RunDir(l.ns, r.Agent, r.Name), action+".md", []byte(body), 0o600)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	l.runLog(r.Agent, r.Name).Info("override written", "action", action)
	return nil
}

// acceptNetwork writes accept.network.md: that the user accepted the network run, what it can reach, when, and the
// receipt of the event that allowed it.
func (l *Runner) acceptNetwork(root *os.Root, r *runstate.Run, rec events.Receipt) error {
	reach := "the internet and other machines, but not this machine's own local services; no ssh (nothing of ~/.ssh, no key, no agent)"
	body := fmt.Sprintf("# Network mode for %s/%s\n\nThe user accepted at %s that the run gets a network of its own: %s.\n\nEvent: seq %d, sha256 %s (plinthctl events show -n %s %s)\n",
		r.Agent, r.Name, time.Now().UTC().Format(time.RFC3339), reach, rec.Seq, rec.Hash, l.ns, r.Name)
	err := safefs.WriteNew(root, runstate.RunDir(l.ns, r.Agent, r.Name), runstate.AcceptNetwork, []byte(body), 0o600)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	l.runLog(r.Agent, r.Name).Info("network run accepted", "action", decide.AcceptNetwork)
	return nil
}

// scrub deletes the run fully: one rename takes it out of every list (runs/<agent>/.scrub.<RUN>), then its untouched
// original, its request files and that directory go.
func (l *Runner) scrub(root *os.Root, agent, run string) error {
	runs := filepath.Join(l.ns.Owner, l.ns.Repo, runstate.Dir, agent)
	if err := safefs.Rename(root, filepath.Join(runs, run), filepath.Join(runs, ".scrub."+run)); err != nil {
		return err
	}
	return l.finishScrub(root, agent, run)
}

func (l *Runner) finishScrub(root *os.Root, agent, run string) error {
	base := filepath.Join(l.ns.Owner, l.ns.Repo)
	for _, d := range []string{filepath.Join(base, Raw, agent, run), request.RunDir(l.ns, agent, run), filepath.Join(base, runstate.Dir, agent, ".scrub."+run)} {
		if err := root.RemoveAll(d); err != nil {
			return err
		}
	}
	l.runLog(agent, run).Info("run scrubbed")
	return nil
}

// recoverScrubs finishes every scrub a crash left (runs/<agent>/.scrub.<RUN>).
func (l *Runner) recoverScrubs(root *os.Root) error {
	base := filepath.Join(l.ns.Owner, l.ns.Repo, runstate.Dir)
	agentDirs, err := subdirs(root, base)
	if err != nil {
		return err
	}
	for _, agent := range agentDirs {
		dirs, err := subdirs(root, filepath.Join(base, agent))
		if err != nil {
			return err
		}
		for _, d := range dirs {
			if run, ok := strings.CutPrefix(d, ".scrub."); ok && names.ValidAgent(agent) && names.ValidRun(run) {
				l.runLog(agent, run).Info("finishing the scrub a crash left")
				if err := l.finishScrub(root, agent, run); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
