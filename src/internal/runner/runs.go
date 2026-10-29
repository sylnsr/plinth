package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"plinth/internal/agents"
	"plinth/internal/asklint"
	"plinth/internal/auth"
	"plinth/internal/decide"
	"plinth/internal/events"
	"plinth/internal/names"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// Submissions is the directory below <ns-dir> where ask.submit leaves <agent>/<id>[-<issue>].sh.
const Submissions = "submissions"

// Rejected is the directory below <ns-dir> where a refused submission is kept for analysis: <agent>/<id>/ with its
// ask.sh and reason.md.
const Rejected = "rejected"

// maxSubmission bounds a submitted file; ask.size refuses anything over 500 lines anyway.
const maxSubmission = 1 << 20

// Caller is the caller name of a namespace's runner's own steps (the automatic reviews, exec.start and exec.end,
// the DLP scan) in the event log. Older lines say "worker".
const Caller = "runner"

var (
	submissionRe = regexp.MustCompile(`^([0-9a-f]{16})(?:-([0-9]{1,9}))?\.sh$`)
	newRunRe     = regexp.MustCompile(`^\.new\.([a-z]+-[a-z]+(?:-[0-9]+)?)\.([0-9a-f]{16})$`)
	runNameLine  = regexp.MustCompile(`(?m)^RUN_NAME="[^"]*"`)
	agentLine    = regexp.MustCompile(`(?m)^AGENT_NAME="[^"]*"`)
)

// pollRuns is one round of a namespace loop: finish a run left half-created by a crash, take in new submissions, then
// run the automatic reviews of the runs that wait for them.
func (l *Runner) pollRuns(root *os.Root) error {
	registered, err := agents.List(l.Work, l.ns)
	if err != nil {
		return err
	}
	if err := l.recoverNew(root); err != nil {
		return err
	}
	base := filepath.Join(l.ns.Owner, l.ns.Repo, Submissions)
	agentDirs, err := subdirs(root, base)
	if err != nil {
		return err
	}
	for _, agent := range agentDirs {
		if !names.ValidAgent(agent) {
			l.nsLog().Once("submissions "+l.ns.String()+"/"+agent, "not an agent name, left alone", "path", filepath.Join(Submissions, agent))
			continue
		}
		enabled, eerr := agents.Enabled(l.Work, l.ns, agent, l.cfg.Enabled)
		if eerr != nil {
			if err := l.perRun("agent "+agent, eerr); err != nil {
				return err
			}
			continue
		}
		entries, err := safefs.ReadDir(root, filepath.Join(base, agent))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue // a submission still being written (safefs temporary file)
			}
			m := submissionRe.FindStringSubmatch(e.Name())
			if m == nil || !e.Type().IsRegular() {
				l.nsLog().With("agent", agent).Once("submission "+l.ns.String()+"/"+agent+"/"+e.Name(), "not a submission, left alone", "file", e.Name())
				continue
			}
			subErr := l.submit(root, agent, m[1], m[2], !slices.Contains(registered, agent), !enabled)
			if err := l.perRun("submission "+agent+"/"+e.Name(), subErr); err != nil {
				return err
			}
		}
	}
	return l.autoReviews(root)
}

// submit takes in one submission: lint, decide, then a new run or a rejection, each recorded in the event log first.
func (l *Runner) submit(root *os.Root, agent, id, issue string, unknown, notEnabled bool) error {
	sub := filepath.Join(l.ns.Owner, l.ns.Repo, Submissions, agent, id+suffix(issue)+".sh")
	script, rerr := safefs.ReadFile(root, sub, maxSubmission)
	var lintErr error
	if rerr != nil {
		lintErr = rerr
	} else if f := asklint.Check(script, asklint.Options{Plinth: true, Work: l.Work, ForgejoURL: l.Global.ForgejoURL}); len(f) > 0 {
		lintErr = errors.New(strings.Join(f, "; "))
	}
	mode := runstate.ModeOf(script)
	d := decide.Decide(decide.Request{Caller: auth.Submission(agent), Unknown: unknown, NotEnabled: notEnabled, Namespace: l.ns, Action: decide.AskSubmit, Agent: agent,
		Network: l.cfg.Network, Privileged: l.cfg.Privileged,
		Submission: decide.Submission{Script: script, LintErr: lintErr, Network: mode.Network, Privileged: mode.Privileged}})
	if _, err := l.event(agent, decide.AskSubmit, agent+"/"+id, string(d.Outcome), d.Rule, d.Reason); err != nil {
		return err
	}
	if d.Outcome != decide.Allow {
		return l.reject(root, agent, id, sub, d)
	}

	taken, err := runstate.Names(l.Work, l.ns)
	if err != nil {
		return err
	}
	stem, err := names.Draw(taken, rand.IntN)
	if err != nil {
		return err
	}
	run := stem + suffix(issue)
	filled := runNameLine.ReplaceAll(script, []byte(`RUN_NAME="`+run+`"`))
	filled = agentLine.ReplaceAll(filled, []byte(`AGENT_NAME="`+l.ns.Repo+"/"+agent+`"`))
	sum := sha256.Sum256(filled)

	// The run is written under .new.<RUN>.<id> and renamed into place, so it appears complete or not at all; the
	// submission is removed in between (recoverNew finishes the job after a crash).
	agentRuns := filepath.Join(l.ns.Owner, l.ns.Repo, runstate.Dir, agent)
	if _, err := safefs.EnsureDir(root, agentRuns); err != nil {
		return err
	}
	tmp := filepath.Join(agentRuns, ".new."+run+"."+id)
	if err := root.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	if err := safefs.WriteNew(root, tmp, runstate.AskFile, filled, 0o700); err != nil {
		return err
	}
	if err := safefs.WriteNew(root, tmp, runstate.AskSum, []byte(hex.EncodeToString(sum[:])+"  "+runstate.AskFile+"\n"), 0o600); err != nil {
		return err
	}
	if err := safefs.Remove(root, sub); err != nil {
		return err
	}
	if err := safefs.Rename(root, tmp, filepath.Join(agentRuns, run)); err != nil {
		return err
	}
	if _, err := l.event(agent, "ask.stored", agent+"/"+run, "ok", d.Rule, "submission "+id+", sha256 "+hex.EncodeToString(sum[:])); err != nil {
		return err
	}
	l.tookIn(agent, id, Submitted{Run: run, Rule: d.Rule})
	l.runLog(agent, run).Info("submitted", "submission", id)
	return nil
}

// reject keeps a refused submission for analysis: rejected/<agent>/<id>/ask.sh and reason.md with the rule and reason.
func (l *Runner) reject(root *os.Root, agent, id, sub string, d decide.Decision) error {
	dir := filepath.Join(l.ns.Owner, l.ns.Repo, Rejected, agent)
	if _, err := safefs.EnsureDir(root, dir); err != nil {
		return err
	}
	dir = filepath.Join(dir, id)
	if _, err := safefs.EnsureDir(root, dir); err != nil {
		return err
	}
	reason := fmt.Sprintf("# Rejected submission %s\n\nRule: %s\n\nReason: %s\n\nPolicy: %s\n", id, d.Rule, d.Reason, d.Policy)
	if err := safefs.WriteNew(root, dir, "reason.md", []byte(reason), 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := safefs.Rename(root, sub, filepath.Join(dir, runstate.AskFile)); err != nil {
		return err
	}
	l.tookIn(agent, id, Submitted{Rule: d.Rule, Reason: d.Reason})
	l.nsLog().With("agent", agent).Info("submission rejected", "submission", id, "rule", d.Rule, "reason", d.Reason)
	return nil
}

// recoverNew finishes runs a crash left as .new.<RUN>.<id>: complete ones (ask.sh and a matching ask.sh.sha256) are
// put in place after their submission is removed; incomplete ones are removed, so their submission is taken in again.
func (l *Runner) recoverNew(root *os.Root) error {
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
		for _, n := range dirs {
			m := newRunRe.FindStringSubmatch(n)
			if m == nil {
				continue
			}
			tmp := filepath.Join(base, agent, n)
			script, err := safefs.ReadFile(root, filepath.Join(tmp, runstate.AskFile), maxSubmission)
			sumLine, serr := safefs.ReadFile(root, filepath.Join(tmp, runstate.AskSum), 1024)
			sum := sha256.Sum256(script)
			if err != nil || serr != nil || !strings.HasPrefix(string(sumLine), hex.EncodeToString(sum[:])+" ") {
				l.nsLog().Warn("removing an incomplete run left by a crash; its submission is taken in again", "path", tmp)
				if err := root.RemoveAll(tmp); err != nil {
					return err
				}
				continue
			}
			issue := ""
			if parts := strings.Split(m[1], "-"); len(parts) == 3 {
				issue = parts[2]
			}
			sub := filepath.Join(l.ns.Owner, l.ns.Repo, Submissions, agent, m[2]+suffix(issue)+".sh")
			if err := safefs.Remove(root, sub); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := safefs.Rename(root, tmp, filepath.Join(base, agent, m[1])); err != nil {
				return err
			}
			if _, err := l.event(agent, "ask.stored", agent+"/"+m[1], "ok", "allow", "submission "+m[2]+", completed after a crash"); err != nil {
				return err
			}
			l.tookIn(agent, m[2], Submitted{Run: m[1], Rule: "allow"})
			l.runLog(agent, m[1]).Info("completed a run left half-stored by a crash")
		}
	}
	return nil
}

// errEventLog marks an error of the event log: it fails the namespace loop, since nothing may act unrecorded.
var errEventLog = errors.New("event log")

// event appends one line to the namespace's event log, with the config in force.
func (l *Runner) event(caller, action, target, outcome, rule, reason string) (events.Receipt, error) {
	r, err := l.events.Append(events.Event{Caller: caller, Action: action, Target: target, Outcome: outcome, Rule: rule,
		Reason: reason, Config: events.ConfigHash(l.Global.SHA256, l.cfg.SHA256)})
	if err != nil {
		return r, fmt.Errorf("%w: %w", errEventLog, err)
	}
	return r, nil
}

func suffix(issue string) string {
	if issue == "" {
		return ""
	}
	return "-" + issue
}

// subdirs lists the real directories in dir below root (none when dir is missing).
func subdirs(root *os.Root, dir string) ([]string, error) {
	entries, err := safefs.ReadDir(root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// perRun takes the error of one run's or one submission's step (what names it). An error of the event log fails the
// loop, since nothing may act unrecorded; any other is logged once and the loop goes on with the others, so one bad run
// (a hard-linked answer, two answer files) never stops the namespace. The next poll tries it again (Scan 3).
func (l *Runner) perRun(what string, err error) error {
	key := "skip " + l.ns.String() + "/" + what
	switch {
	case err == nil:
		l.Log.Clear(key)
	case errors.Is(err, errEventLog):
		return err
	default:
		l.nsLog().Once(key, "skipped, the next poll tries again", "step", what, "err", err)
	}
	return nil
}
