package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"plinth/internal/asklint"
	"plinth/internal/config"
	"plinth/internal/controller"
	"plinth/internal/events"
	"plinth/internal/logfile"
	"plinth/internal/names"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
	"plinth/internal/workdir"
)

// maxFile is the most the pager reads of a file. Plinth keeps no run file larger (a larger answer stays in raw/), so
// only a file a script left behind is cut (plinth-tui.README.md, Pager).
const maxFile = 16 << 20

// Store is the TUI's I/O below one <work>: it reads runs, requests, events and logs through the packages the commands
// use, and writes nothing but request files and its lines in the general log. It never reads raw/, running.yml or an
// answer that hasn't been scanned.
type Store struct {
	Work   string
	Getenv config.Getenv
	Now    func() time.Time
	Images Images // the Image screen's commands; nil: no Image screen
}

// snapshot is what the screens show, read at once.
type snapshot struct {
	g          *config.Global
	controller bool
	pid        int
	notices    []logfile.Notice // what needs the user: the notices open in the log
	spaces     []*space
}

// space is one namespace.
type space struct {
	ns    names.Namespace
	cfg   *config.Namespace // nil: its config doesn't load, so the controller doesn't serve it
	err   error             // why it isn't served, or why its runs couldn't be read
	stale bool              // no controller, or no heartbeat within three polls
	runs  []*runInfo
}

// runInfo is a run with what its row and its actions need beyond its file names.
type runInfo struct {
	sp   *space
	r    *runstate.Run
	reqs []request.File
	mode runstate.Mode // what ask.sh's header says (read for a run that hasn't run yet)
	// reqState: where each action's requests stand: "pending" (queued, starting or running), "finished", or "failed" (a FAILED one
	// and no success after it; a FAILED request stays when a retry succeeds). Save and Report read it (plinth.autosave).
	reqState map[string]string
	// waiting: what an unscanned answer waits for, from the event log: the model, or another attempt.
	waiting runner.Waiting
}

func (ri *runInfo) key() string { return ri.sp.ns.String() + " " + ri.r.Agent + "/" + ri.r.Name }

func (s *Store) load() (*snapshot, error) {
	g, err := config.LoadGlobal(s.Work, s.Getenv)
	if err != nil {
		return nil, err
	}
	sn := &snapshot{g: g}
	sn.controller, sn.pid = controller.Running(s.Work)
	sn.notices, _ = logfile.OpenNotices(s.Work, s.Now(), g.Plinth.Notice.Expires)
	nss, err := workdir.Namespaces(s.Work)
	if err != nil {
		return nil, err
	}
	poll := time.Duration(g.Plinth.Runner.PollSeconds) * time.Second
	for _, ns := range nss {
		sp := &space{ns: ns}
		sn.spaces = append(sn.spaces, sp)
		if sp.cfg, _, sp.err = config.LoadNamespace(s.Work, ns, g, s.Getenv); sp.err != nil {
			sp.cfg = nil
			continue
		}
		sp.stale = !sn.controller || s.Now().Sub(s.beat(ns)) > 3*poll
		runs, err := runstate.List(s.Work, ns, "", sp.cfg.Automatic, controller.Live(s.Work, ns))
		if err != nil {
			sp.err = err
			continue
		}
		for _, r := range runs {
			ri := &runInfo{sp: sp, r: r}
			sp.runs = append(sp.runs, ri)
			if r.Broken != nil {
				continue
			}
			reqs, _ := request.List(s.Work, ns, r.Agent, r.Name)
			ri.reqState = reqStates(reqs)
			// A FAILED request stays when a retry succeeds (or is under way): it is shown only while it is the action's latest word.
			ri.reqs = slices.DeleteFunc(reqs, func(q request.File) bool {
				return q.State == request.Finished || q.State == request.Failed && ri.reqState[q.Action] != "failed"
			}) // pending and failed ones only
			// A run without an answer may run (again): Exec needs its mode.
			if r.Facts.Answer == "" && !r.Facts.Running {
				_, ri.mode, _ = runstate.Header(s.Work, ns, r.Agent, r.Name)
			}
			if r.Facts.Answer == "new" {
				ri.waiting, _ = runner.ScanWaiting(s.Work, ns, r.Agent, r.Name)
			}

		}
	}
	return sn, nil
}

// beat is the namespace's last heartbeat; the zero time without one.
func (s *Store) beat(ns names.Namespace) time.Time {
	fi, err := os.Lstat(filepath.Join(s.Work, ns.Owner, ns.Repo, filepath.FromSlash(runner.Heartbeat)))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// dlp is the DLP scan's line of the run's answer document (runstate.DLPLine, from its status.dlp), "" when it can't
// be read.
func (s *Store) dlp(ri *runInfo) string {
	b, cut, err := s.read(ri, ri.r.AnswerFile(), maxFile)
	if err != nil || cut > 0 {
		return ""
	}
	return runstate.DLPLine(b)
}

// detail is what the Run screen shows beyond the row: read when the screen opens and at every refresh.
type detail struct {
	header  []string
	mode    runstate.Mode
	reviews map[string]string // review file -> its verdict, or its first line
	answer  []string          // the first lines of a scanned answer
	dlp     string            // the answer's DLP scan line, from its status.dlp
	err     error
	issue   string
}

func (s *Store) detail(ri *runInfo) detail {
	d := detail{reviews: map[string]string{}}
	r := ri.r
	if d.header, d.mode, d.err = runstate.Header(s.Work, ri.sp.ns, r.Agent, r.Name); d.err != nil {
		return d
	}
	if b, _, err := s.read(ri, runstate.AskFile, 1<<20); err == nil { // the ticket the script says it is for (ISSUE=)
		if ref, ok := asklint.ScriptIssue(b); ok {
			d.issue = ref.URL
		}
	}
	for _, f := range r.Files {
		if strings.HasPrefix(f.Name, "review.") && strings.HasSuffix(f.Name, ".md") && runstate.StateFile(f.Name) {
			b, _, err := s.read(ri, f.Name, 1<<20)
			if err != nil {
				d.reviews[f.Name] = err.Error()
				continue
			}
			d.reviews[f.Name] = summary(b)
		}
	}
	if a := r.Facts.Answer; a == "dlp-ok" || a == "DLP-FAIL" || a == "released" {
		b, _, err := s.read(ri, r.AnswerFile(), maxFile)
		if err != nil {
			d.answer = []string{err.Error()}
			return d
		}
		d.dlp = runstate.DLPLine(b)
		d.answer = runstate.AnswerLines(b, 5)
	}
	return d
}

// summary is a review's verdict (the first line after "## Verdict"), else its first line that isn't empty or a heading.
func summary(b []byte) string {
	if _, after, ok := bytes.Cut(b, []byte("\n## Verdict\n")); ok {
		b = after
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return "(empty)"
}

// errNotScanned refuses running.yml and an answer that hasn't been scanned or is INVALID: the user sees an answer
// only after its scan.
var errNotScanned = errors.New("not scanned yet")

// read reads one of the run's files, at most limit bytes; cut reports a file that is larger.
func (s *Store) read(ri *runInfo, name string, limit int64) (b []byte, cut int64, err error) {
	if name == runstate.Running || strings.HasSuffix(name, ".new.yml") || strings.HasSuffix(name, ".INVALID.yml") {
		return nil, 0, errNotScanned
	}
	return s.readBelow(filepath.Join(runstate.RunDir(ri.sp.ns, ri.r.Agent, ri.r.Name), name), limit)
}

// readBelow reads the file at name below <work> (no symlink, a regular file), at most limit bytes; cut is the file's
// size when it is larger.
func (s *Store) readBelow(name string, limit int64) ([]byte, int64, error) {
	root, err := os.OpenRoot(s.Work)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	f, err := safefs.OpenFile(root, name, os.O_RDONLY)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, 0, err
	}
	var cut int64
	if fi, err := f.Stat(); err == nil && fi.Size() > limit {
		cut = fi.Size()
	}
	return b, cut, nil
}

// events lists the namespace's events (of one run, when run isn't ""), as events show prints them; problem is where
// the chain stops matching (nothing after it is listed).
func (s *Store) events(ns names.Namespace, run string) (lines []string, problem error) {
	err := events.Walk(s.Work, filepath.Join(ns.Owner, ns.Repo), func(e events.Event, _ []byte) error {
		if run != "" && e.Target != run && !strings.HasSuffix(e.Target, "/"+run) {
			return nil
		}
		lines = append(lines, fmt.Sprintf("%d %s %s %s %s %s %s: %s", e.Seq, e.Time, e.Caller, e.Action, e.Target, e.Outcome, e.Rule, e.Reason))
		return nil
	})
	return lines, err
}

// logFile is one file of the general log.
type logFile struct {
	name string
	size int64
	mod  time.Time
	old  bool
}

// logs lists the general log's files, newest first; old ones are those Scrub logs deletes.
func (s *Store) logs(maxAgeDays int) ([]logFile, error) {
	old, err := logfile.Old(s.Work, maxAgeDays, s.Now())
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.Work, logfile.Dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []logFile
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.Type().IsRegular() {
			continue
		}
		out = append(out, logFile{name: e.Name(), size: info.Size(), mod: info.ModTime(), old: slices.Contains(old, e.Name())})
	}
	slices.SortFunc(out, func(a, b logFile) int { return strings.Compare(b.name, a.name) })
	return out, nil
}

// queueLogsScrub writes <work>/requests/logs-scrub.queued, as plinthctl logs scrub does; already is true when it is
// queued already.
func (s *Store) queueLogsScrub() (already bool, err error) {
	if _, err := workdir.EnsureWork(s.Work); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(s.Work)
	if err != nil {
		return false, err
	}
	defer root.Close()
	err = safefs.WriteNew(root, controller.RequestsDir, controller.LogsScrub+"."+request.Queued, nil, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return true, nil
	}
	return false, err
}

// log writes a line to the general log as the command tui, creating <work>/logs/ when it is missing; the error is the
// write's, for the message line.
func (s *Store) log(msg string, fields ...any) error {
	if _, err := workdir.EnsureWork(s.Work); err != nil {
		return err
	}
	l := logfile.New(s.Work, logfile.TUI)
	l.Info(msg, fields...)
	return l.Err()
}

// readLog reads one of the general log's files for the pager.
func (s *Store) readLog(name string) ([]byte, int64, error) {
	return s.readBelow(filepath.Join(logfile.Dir, name), maxFile)
}

// reqStates says where each action's requests stand: pending (queued, starting or running) before finished before
// failed, since a FAILED request stays when a later try of the same action succeeds.
func reqStates(reqs []request.File) map[string]string {
	rank := map[string]int{"": 0, "failed": 1, "finished": 2, "pending": 3}
	out := map[string]string{}
	for _, q := range reqs {
		st := "pending"
		switch q.State {
		case request.Finished:
			st = "finished"
		case request.Failed:
			st = "failed"
		}
		if rank[st] > rank[out[q.Action]] {
			out[q.Action] = st
		}
	}
	return out
}
