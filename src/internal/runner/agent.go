package runner

import (
	"context"
	"errors"
	"time"

	"plinth/internal/config"
	"plinth/internal/events"
	"plinth/internal/runstate"
)

// What the Plinth MCP server (package agentmcp) needs of a namespace's runner, from its own goroutines while the
// runner's loop runs: the namespace's config, its event log, a poll at once and the outcome of a submission.

// maxSubmitted bounds the outcomes a runner keeps for ask_submit to collect; the oldest goes first.
const maxSubmitted = 256

// ErrDetached: the runner is detached (its namespace's config changed, or the controller stops); its event log is the
// next runner's now, so nothing is recorded through this one.
var ErrDetached = errors.New("the namespace is being reloaded or the controller stops; call again in a moment")

// Submitted is what the runner made of a submission: the run it stored, or the rule and reason that refused it.
type Submitted struct {
	Run    string // the RUN_NAME; "" when refused
	Rule   string // the deciding rule: allow, or the one that refused it
	Reason string // why it was refused
}

// Config is the namespace's config, as the runner loaded it.
func (l *Runner) Config() *config.Namespace { return l.cfg }

// Live is the namespace's runstate.Live: a run is running while the runner has its exec.
func (l *Runner) Live() runstate.Live { return l.live }

// Record appends an event to the namespace's event log, the one the runner's loop writes, so the chain has one writer.
// It fails while the loop hasn't opened the log, and once the runner is detached (ErrDetached).
func (l *Runner) Record(caller, action, target, outcome, rule, reason string) (events.Receipt, error) {
	l.recMu.Lock()
	defer l.recMu.Unlock()
	if l.detached {
		return events.Receipt{}, ErrDetached
	}
	if l.events == nil {
		return events.Receipt{}, errors.New("the namespace's event log isn't open yet; call again in a moment")
	}
	return l.event(caller, action, target, outcome, rule, reason)
}

// Detached ends Record: the controller calls it once the runner's loop and Finish have returned, before another runner
// of the namespace opens its event log. A Record in progress finishes first.
func (l *Runner) Detached() {
	l.recMu.Lock()
	defer l.recMu.Unlock()
	l.detached = true
}

// Kick makes the runner's loop poll at once (a submission waits for it), or soon after the poll in progress.
func (l *Runner) Kick() {
	select {
	case l.kick <- struct{}{}:
	default: // a poll is due already
	}
}

// Submitted waits until the runner has taken in the agent's submission id, or ctx ends (ok false). An outcome is given
// once; the runner keeps the last maxSubmitted of them.
func (l *Runner) Submitted(ctx context.Context, agent, id string) (s Submitted, ok bool) {
	key := agent + "/" + id
	for {
		l.subMu.Lock()
		s, ok = l.submitted[key]
		if ok {
			delete(l.submitted, key)
		}
		wake := l.subWake
		l.subMu.Unlock()
		if ok {
			return s, true
		}
		select {
		case <-ctx.Done():
			return Submitted{}, false
		case <-wake:
		case <-time.After(time.Second): // the outcome of a submission taken in by an earlier runner never comes here
		}
	}
}

// tookIn records the outcome of the agent's submission id for Submitted and wakes those waiting.
func (l *Runner) tookIn(agent, id string, s Submitted) {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	if l.submitted == nil {
		l.submitted = map[string]Submitted{}
	}
	key := agent + "/" + id
	if _, ok := l.submitted[key]; !ok {
		l.subOrder = append(l.subOrder, key)
	}
	l.submitted[key] = s
	for len(l.subOrder) > maxSubmitted {
		delete(l.submitted, l.subOrder[0])
		l.subOrder = l.subOrder[1:]
	}
	close(l.subWake)
	l.subWake = make(chan struct{})
}
