// Package runner is one namespace's part of the controller: its submissions, requests, decisions, events,
// reviews, DLP scans and runs. The controller (package controller) attaches a runner to every namespace it serves,
// supervises its loop and gives it what the whole process shares (Services). Spec: plinthctl.README.md, "Process".
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"plinth/internal/config"
	"plinth/internal/events"
	"plinth/internal/logfile"
	"plinth/internal/names"
	"plinth/internal/ollama"
	"plinth/internal/review"
	"plinth/internal/safefs"
)

// Heartbeat is the file below <ns-dir> whose modification time a runner's loop sets at every poll; it holds the PID.
const Heartbeat = "state/heartbeat"

// Notifier writes to the controller's stderr what the user has to act on, once per distinct message for a key, and records
// it in the general log as a notice. fields are alternating keys and values: err, effect and next say why, what it
// costs and what to do; ns, agent, run and the like say where.
type Notifier interface {
	Once(key, msg string, fields ...any)
	Clear(key string)
}

// Services are what the controller gives its runners, one for all of them: its <work> and global config, its log and
// stderr, what its check at start found, and what the whole process shares.
type Services struct {
	Work     string          // absolute
	Global   *config.Global  // config.yml, as the controller loaded it at its start
	Log      *logfile.Log    // the general log (component controller; the runner's lines take Runner, Reviews, DLP)
	Out      Notifier        // stderr
	Stopping context.Context // the controller's: cancelled when it stops

	// What the controller's check at start found: Home is the user's $HOME, where a run's empty $HOME is a tmpfs;
	// Podman says why runs can't start on this host (no script runs then), Pasta why network runs can't (no pasta,
	// package passt; normal runs still do).
	Home          string
	Podman, Pasta error

	Ollama  ollama.Chatter  // the Ollama gateway's client: the gateway's semaphore is ollama.max-parallel
	Fetcher *review.Fetcher // the review prompts' URLs, with forgejo-token for forgejo-url's host

	// ForgejoCAFile is a CA bundle for forgejo-url's certificate, for the drift check; "" for the system's (tests set
	// one).
	ForgejoCAFile string

	// Poll, when set, is called by a runner's loop at the end of every poll: a hook for tests. An error or a panic
	// fails the loop, which the controller restarts with backoff.
	Poll func(ctx context.Context, ns names.Namespace, cfg *config.Namespace) error

	// ExecTimeoutUnit is the unit of plinth.runner.exec-timeout-minutes and KillGrace the time between SIGTERM and
	// SIGKILL; zero for a minute and 30 s (tests shorten both).
	ExecTimeoutUnit, KillGrace time.Duration
	// ScanRetry is the wait before an answer's DLP scan that couldn't ask the model starts again; zero for a minute.
	ScanRetry time.Duration

	smokeMu sync.Mutex
	smoked  map[string]error // "<image> <mode>" -> what its smoke container found, nil for nothing wrong
}

// Runner is one attached namespace.
type Runner struct {
	*Services
	ns     names.Namespace
	cfg    *config.Namespace
	events *events.Log
	ctx    context.Context // the runner's: cancelled when it is detached, which stops the DLP scans' model steps too

	mu        sync.Mutex
	execs     map[string]*child // <agent>/<RUN> -> its exec, from the start until the loop has finished it
	wg        sync.WaitGroup    // the execs' watchers, the DLP scans, the reviews and the posts
	lastDrift string            // "<pin tree> <branch tree>" of the last image.drift; "" since no drift
	term      chan TerminalOp   // a run to start in the user's terminal (terminalExec)

	reviews    map[string]*reviewJob // <agent>/<RUN>/<review file> -> its review while it runs; the loop's alone
	answerless map[string]bool       // review types whose prompt reviews no answer (autoReviews)
	posts      map[string]*postJob   // <agent>/<RUN>/<save|report> -> its post while it runs; the loop's alone

	// previewMu: one Preview at a time (preview.go).
	previewMu sync.Mutex

	// For the Plinth MCP server (agent.go): recMu guards events against the loop opening it again, and detached; kick
	// asks the loop for a poll at once; submitted holds the outcomes of submissions (keys <agent>/<id>, oldest first in
	// subOrder) until Submitted collects them, and subWake is closed whenever one comes in.
	recMu     sync.Mutex
	detached  bool
	kick      chan struct{}
	subMu     sync.Mutex
	submitted map[string]Submitted
	subOrder  []string
	subWake   chan struct{}
}

// New is the runner of namespace ns, with its config cfg, until ctx is cancelled.
func New(ctx context.Context, s *Services, ns names.Namespace, cfg *config.Namespace) *Runner {
	return &Runner{Services: s, ns: ns, cfg: cfg, ctx: ctx, execs: map[string]*child{}, term: make(chan TerminalOp, 1),
		reviews: map[string]*reviewJob{}, answerless: map[string]bool{}, posts: map[string]*postJob{}, kick: make(chan struct{}, 1),
		subWake: make(chan struct{})}
}

// Run is one life of the runner's loop, until its context is cancelled (then nil): it writes the heartbeat file, then
// at every poll sets its time, takes in the submissions, carries out the requests and calls the Poll hook. A panic
// becomes its error.
func (l *Runner) Run() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v: %s", r, bytes.ReplaceAll(debug.Stack(), []byte("\n"), []byte(" | ")))
		}
	}()
	root, err := os.OpenRoot(l.Work)
	if err != nil {
		return err
	}
	defer root.Close()
	base := filepath.Join(l.ns.Owner, l.ns.Repo)
	// The event log first: a log that doesn't continue cleanly keeps the namespace from acting (fail closed). Under
	// recMu: the Plinth MCP server records through it too (Record).
	l.recMu.Lock()
	l.events, err = events.Open(l.Work, base, l.Global.Plinth.EventLogMax)
	l.recMu.Unlock()
	if err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	beat := filepath.Join(base, filepath.FromSlash(Heartbeat))
	if err := safefs.Replace(root, filepath.Dir(beat), filepath.Base(beat), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	t := time.NewTicker(time.Duration(l.Global.Plinth.Runner.PollSeconds) * time.Second)
	defer t.Stop()
	for {
		if err := safefs.Touch(root, beat, time.Now()); err != nil {
			return fmt.Errorf("heartbeat: %w", err)
		}
		if err := l.pollRuns(root); err != nil {
			return err
		}
		if err := l.pollRequests(root); err != nil {
			return err
		}
		if l.Poll != nil {
			if err := l.Poll(l.ctx, l.ns, l.cfg); err != nil {
				return err
			}
		}
		select {
		case <-l.ctx.Done():
			return nil
		case op := <-l.term:
			l.terminalExec(root, op)
		case <-l.kick: // a submission waits (Kick)
		case <-t.C:
		}
	}
}

// Finish is the runner's end, once its context is cancelled and Run has returned: it waits for the watchers of its
// execs, its DLP scans and its reviews, then finishes an exec the stop ended (see watch) like any other: the state files
// its script wrote itself moved out, exec.end, the DLP scan. What can't be finished now, leftoverExec finishes at
// the next start.
func (l *Runner) Finish() {
	l.wg.Wait()
	if l.Busy() == 0 || l.events == nil {
		return
	}
	root, err := os.OpenRoot(l.Work)
	if err == nil {
		err = l.finishExecs(root)
		root.Close()
	}
	if err != nil {
		l.nsLog().Warn("finishing the stopped execs failed; the next start finishes them", "err", err)

	}
}

// nsLog is the runner's log: component runner, the namespace as a field.
func (l *Runner) nsLog() *logfile.Log {
	return l.Log.Component(logfile.Runner).With("ns", l.ns.String())
}

// runLog is nsLog with the run's agent and name as fields.
func (l *Runner) runLog(agent, run string) *logfile.Log {
	return l.nsLog().With("agent", agent, "run", run)
}
