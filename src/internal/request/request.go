// Package request is the user's requests about a run: one file each, <ns-dir>/requests/<agent>/<RUN>/<action>.<state>,
// whose name carries the whole request (nothing ever acts on a file's content). The commands queue them, and the TUI
// and the web UI will; the namespace loop carries them out. Both ask decide with the facts Decide gathers, so a command
// refuses at once what the loop would refuse. Spec: plinthctl.README.md, "Requests".
package request

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"plinth/internal/asklint"
	"plinth/internal/auth"
	"plinth/internal/config"
	"plinth/internal/decide"
	"plinth/internal/names"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// Dir is the requests directory below <ns-dir> (and the process-level one below <work>).
const Dir = "requests"

// The states of a request, its file name's last part. The loop takes a queued request as running, an exec as starting
// until right before its script starts; a carried-out one is finished, a refused or failed one FAILED. Only Scrub
// deletes a run's requests, with its request directory.
const (
	Queued   = "queued"
	Starting = "starting"
	Running  = "running"
	Finished = "finished"
	Failed   = "FAILED"
)

// order is the order in which the loop carries out one run's requests: an override before the action it releases,
// scrub last.
var order = []string{decide.OverrideReview, decide.OverrideDLP, decide.AcceptNetwork, decide.Review, decide.Exec, decide.Done, decide.Save, decide.Report, decide.Scrub}

var reviewRe = regexp.MustCompile(`^review\.[a-z][a-z0-9-]{0,31}$`)

var (
	// ErrQueued: the same request is queued already.
	ErrQueued = errors.New("already queued")
	// ErrInProgress: the loop is carrying out the same request.
	ErrInProgress = errors.New("in progress")
)

// Valid reports whether action is the action of a run request: one of order, a review as review.<type>.
func Valid(action string) bool {
	return reviewRe.MatchString(action) || (action != decide.Review && slices.Contains(order, action))
}

// Rank is action's place in the loop's order.
func Rank(action string) int {
	if reviewRe.MatchString(action) {
		action = decide.Review
	}
	return slices.Index(order, action)
}

// Parse splits a request file's name into its action and state; ok is false for any other name. Only an exec is ever
// starting.
func Parse(name string) (action, state string, ok bool) {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return "", "", false
	}
	action, state = name[:i], name[i+1:]
	if !slices.Contains([]string{Queued, Starting, Running, Finished, Failed}, state) || !Valid(action) ||
		(state == Starting && action != decide.Exec) {
		return "", "", false
	}
	return action, state, true
}

// RunDir is the run's request directory below <work>.
func RunDir(ns names.Namespace, agent, run string) string {
	return filepath.Join(ns.Owner, ns.Repo, Dir, agent, run)
}

// Queue writes <action>.queued for the run, creating its directories. The same request queued or in progress (starting
// or running) already gives ErrQueued or ErrInProgress. A finished or FAILED one doesn't count: decide says whether the
// action can happen again.
func Queue(work string, ns names.Namespace, agent, run, action string) error {
	if !names.ValidAgent(agent) || !names.ValidRun(run) || !Valid(action) {
		return fmt.Errorf("%s/%s %s: not an agent, a run and an action", agent, run, action)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := RunDir(ns, agent, run)
	for _, d := range []string{filepath.Dir(filepath.Dir(dir)), filepath.Dir(dir), dir} { // requests/, <agent>/, <RUN>/
		if _, err := safefs.EnsureDir(root, d); err != nil {
			return err
		}
	}
	for _, s := range []string{Starting, Running} {
		if _, err := safefs.Lstat(root, filepath.Join(dir, action+"."+s)); err == nil {
			return ErrInProgress
		}
	}
	err = safefs.WriteNew(root, dir, action+"."+Queued, nil, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return ErrQueued
	}
	return err
}

// File is one request of a run.
type File struct {
	Action string
	State  string
	Reason string // of a FAILED one: its content on one line
}

// List returns the run's requests in the loop's order; none when it has no request directory.
func List(work string, ns names.Namespace, agent, run string) ([]File, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir := RunDir(ns, agent, run)
	if _, err := safefs.Lstat(root, dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	var out []File
	for _, e := range entries {
		action, state, ok := Parse(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		f := File{Action: action, State: state}
		if state == Failed {
			b, _ := safefs.ReadFile(root, filepath.Join(dir, e.Name()), 4096)
			f.Reason = strings.Join(strings.Fields(strings.ReplaceAll(string(b), "\n", "; ")), " ")
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b File) int { return Rank(a.Action) - Rank(b.Action) })
	return out, nil
}

// Decide gathers what the gatekeeper needs about the user's action on a run (the run's state from its file names,
// whether its exec request says the script started and, for exec and accept.network, the script's mode and whether
// ask.sh still matches its sum, and what the namespace allows) and asks it. It returns the run as loaded.
func Decide(work string, ns names.Namespace, g *config.Global, cfg *config.Namespace, agent, run, action string, live runstate.Live) (decide.Decision, *runstate.Run, error) {
	return decideRun(work, ns, g, cfg, agent, run, action, live, false, true)
}

// DecideTerminal is Decide for an action from the user's terminal, where a privileged run runs (plinthctl exec over the
// controller's socket).
func DecideTerminal(work string, ns names.Namespace, g *config.Global, cfg *config.Namespace, agent, run, action string, live runstate.Live) (decide.Decision, *runstate.Run, error) {
	return decideRun(work, ns, g, cfg, agent, run, action, live, true, true)
}

// Hint is Decide (DecideTerminal when terminal) for a hint, shell completion's mark: the same facts, but ask lint
// --plinth doesn't run and Lint stays "", so an exec it allows may still be refused for a script that fails lint.
func Hint(work string, ns names.Namespace, g *config.Global, cfg *config.Namespace, agent, run, action string, live runstate.Live, terminal bool) (decide.Decision, error) {
	d, _, err := decideRun(work, ns, g, cfg, agent, run, action, live, terminal, false)
	return d, err
}

func decideRun(work string, ns names.Namespace, g *config.Global, cfg *config.Namespace, agent, run, action string, live runstate.Live, terminal, lint bool) (decide.Decision, *runstate.Run, error) {
	r, err := runstate.Load(work, ns, agent, run, cfg.Automatic, live)
	if err != nil {
		return decide.Decision{}, nil, err
	}
	facts := r.Facts
	if facts.Exists && !facts.Symlink {
		if facts.Started, facts.Unfinished, err = started(work, ns, agent, run); err != nil {
			return decide.Decision{}, nil, err
		}
	}
	if (action == decide.Exec || action == decide.AcceptNetwork) && facts.Exists && !facts.Symlink {
		mode, changed, err := runstate.Script(work, ns, agent, run)
		if err != nil {
			return decide.Decision{}, nil, err
		}
		facts.Interactive, facts.Network, facts.Privileged, facts.ScriptChanged = mode.Interactive, mode.Network, mode.Privileged, changed
		if action == decide.Exec && !changed && lint {
			script, _, err := runstate.CheckScript(work, ns, agent, run)
			if err != nil {
				return decide.Decision{}, nil, err
			}
			facts.Lint = strings.Join(asklint.Check(script, asklint.Options{Plinth: true, Work: work, ForgejoURL: g.ForgejoURL}), "; ")
		}
	}
	req := decide.Request{Caller: auth.HostUser(), Namespace: ns, Action: action, Agent: agent, RunName: run, Run: facts,
		ReviewTypes: slices.Sorted(maps.Keys(g.Reviewers.Type)), Repo: g.Plinth.Repo,
		Network: cfg.Network, Privileged: cfg.Privileged, Terminal: terminal}
	if t, ok := strings.CutPrefix(action, decide.Review+"."); ok {
		req.Action, req.ReviewType = decide.Review, t
	}
	return decide.Decide(req), r, nil
}

// Started is started for the Plinth MCP server, which asks decide for an agent: whether the run's exec request
// says its script started, and whether that exec is still running.
func Started(work string, ns names.Namespace, agent, run string) (bool, bool, error) {
	return started(work, ns, agent, run)
}

// started reports whether the run's exec request is running or finished: the controller started its script; and whether
// it is running: that exec isn't finished yet. The name is out of the script's reach, unlike running.yml or an answer in
// the run directory.
func started(work string, ns names.Namespace, agent, run string) (started, unfinished bool, err error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return false, false, err
	}
	defer root.Close()
	for _, s := range []string{Running, Finished} {
		_, err := safefs.Lstat(root, filepath.Join(RunDir(ns, agent, run), decide.Exec+"."+s))
		if err == nil {
			started = true
			unfinished = unfinished || s == Running
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, false, err
		}
	}
	return started, unfinished, nil
}
