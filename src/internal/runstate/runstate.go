// Package runstate reads a run's state from the names of the files in its directory, <ns-dir>/runs/<agent>/<RUN>/, and
// nothing else: there is no database and no state file. It is the shared view model of the controller (the facts decide
// needs), the get and describe commands, the TUI and the web UI. Spec: plinthctl.README.md, "Runs".
package runstate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"plinth/internal/decide"
	"plinth/internal/names"
	"plinth/internal/safefs"
)

// Dir is the runs directory below <ns-dir>.
const Dir = "runs"

// AskFile is the script; AskSum its sha256, written with it on submission.
const (
	AskFile = "ask.sh"
	AskSum  = "ask.sh.sha256"
	// Running is the answer while the script runs: the controller writes it empty right before the start, and the
	// AAA library's EXIT trap renames it to answer[.FAIL].new.yml. Left over = no answer.
	Running = "running.yml"

	// AcceptNetwork is the controller's record that the user accepted a network run (accept.network).
	AcceptNetwork = "accept.network.md"

	// IssueFile is the controller's pointer to the run's issue in plinth.repo: the line "Issue: <page>". Save and
	// Report find the run's issue by it.
	IssueFile = "issue.md"
)

var (
	reviewRe       = regexp.MustCompile(`^review\.([a-z][a-z0-9-]{0,31})(\.FAILED)?\.md$`)
	answerReviewRe = regexp.MustCompile(`^review\.answer\.([a-z][a-z0-9-]{0,31})(\.FAILED)?\.md$`)
	// answerRe is a run's answer, a document, in any state.
	answerRe = regexp.MustCompile(`^answer(\.FAIL)?\.(new|dlp-ok|DLP-FAIL|INVALID|released)\.yml$`)
	// interactiveRe, networkRe and privilegedRe are a script's mode lines when they say 1 (Mode).
	interactiveRe = regexp.MustCompile(`^INTERACTIVE=1(\s|$)`)
	networkRe     = regexp.MustCompile(`^NETWORK=1(\s|$)`)
	privilegedRe  = regexp.MustCompile(`^PRIVILEGED=1(\s|$)`)
)

// File is one entry of a run directory.
type File struct {
	Name string
	Size int64
	Mod  time.Time
}

// Run is one run as its file names describe it.
type Run struct {
	NS    names.Namespace
	Agent string
	Name  string
	Files []File
	Facts decide.Run
	// Gate: the answer is answer.FAIL.*: the script stopped at a gate.
	Gate bool
	// Reviews: per review type, "ok", "failed" or "" (pending); answer reviews under "answer.<type>".
	Reviews map[string]string
	// Newest is the newest modification time in the directory (the list's order).
	Newest time.Time
	// Broken: List could not read the run (e.g. two answer files, a hard-linked state file); only NS, Agent and Name are
	// set. It is shown as broken and skipped, so one bad run never stops the others (Scan 3).
	Broken error
}

// Live reports whether the controller has a child running the run, from its own record; nil means none does (a left-over
// running.yml = no answer).
type Live func(agent, run string) bool

// Load reads the run <ns-dir>/runs/<agent>/<run> below work. automatic are the namespace's automatic review types: a
// type without a review file is pending. A run that doesn't exist has Facts.Exists false and no error.
func Load(work string, ns names.Namespace, agent, run string, automatic []string, live Live) (*Run, error) {
	if !names.ValidAgent(agent) || !names.ValidRun(run) {
		return nil, fmt.Errorf("%s/%s: not an agent and a run name", agent, run)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return load(root, ns, agent, run, automatic, live)
}

func load(root *os.Root, ns names.Namespace, agent, run string, automatic []string, live Live) (*Run, error) {
	r := &Run{NS: ns, Agent: agent, Name: run, Reviews: map[string]string{}}
	dir := RunDir(ns, agent, run)
	fi, err := safefs.Lstat(root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if errors.Is(err, safefs.ErrSymlink) {
		r.Facts.Symlink = true
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
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
	r.Facts.Exists = true
	answers := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // gone meanwhile
		}
		n := e.Name()
		r.Files = append(r.Files, File{Name: n, Size: info.Size(), Mod: info.ModTime()})
		if info.ModTime().After(r.Newest) {
			r.Newest = info.ModTime()
		}
		if e.Type()&fs.ModeSymlink != 0 {
			r.Facts.Symlink = true
			continue
		}
		switch m := reviewRe.FindStringSubmatch(n); {
		case m != nil && m[1] != "answer":
			if m[2] != "" {
				r.Reviews[m[1]] = "failed"
			} else if r.Reviews[m[1]] != "failed" {
				r.Reviews[m[1]] = "ok"
				if verdict(root, filepath.Join(dir, n)) == "Do not run" {
					r.Reviews[m[1]] = "failed"
				}
			}
			continue
		}
		if m := answerReviewRe.FindStringSubmatch(n); m != nil {
			if m[2] != "" {
				r.Reviews["answer."+m[1]] = "failed"
			} else {
				r.Reviews["answer."+m[1]] = "ok"
			}
			continue
		}
		if m := answerRe.FindStringSubmatch(n); m != nil {
			answers++
			r.Gate = m[1] != ""
			r.Facts.Answer = m[2]
			continue
		}
		switch n {
		case "override.review.md":
			r.Facts.ReviewOverridden = true
		case "override.dlp.md":
			r.Facts.DLPOverridden = true
		case AcceptNetwork:
			r.Facts.NetworkAccepted = true
		case "report.md":
			r.Facts.Reported = true
		}
	}
	if answers > 1 {
		return nil, fmt.Errorf("%s: %d answer files; a run has at most one", dir, answers)
	}
	slices.SortFunc(r.Files, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	for _, t := range automatic {
		if r.Reviews[t] == "" {
			r.Facts.ReviewsPending = append(r.Facts.ReviewsPending, t)
		}
	}
	for t, v := range r.Reviews {
		if v == "failed" && !strings.HasPrefix(t, "answer.") {
			r.Facts.ReviewsFailed = append(r.Facts.ReviewsFailed, t)
		}
	}
	slices.Sort(r.Facts.ReviewsFailed)
	// Running is the controller's record (its child, or the exec request), not running.yml: a script can remove that.
	alive := live != nil && live(agent, run)
	r.Facts.Running = alive
	r.Facts.Ran = r.Facts.Answer != "" || (r.Has(Running) && !alive)
	return r, nil
}

// Has reports whether the run directory holds name.
func (r *Run) Has(name string) bool {
	return slices.ContainsFunc(r.Files, func(f File) bool { return f.Name == name })
}

// RunDir is the run's directory below <work>.
func RunDir(ns names.Namespace, agent, run string) string {
	return filepath.Join(ns.Owner, ns.Repo, Dir, agent, run)
}

// AskStatus is the run's status in get ask: broken, reviewing, review-failed, approved, overridden, running, no-answer
// or ran.
func (r *Run) AskStatus() string {
	f := r.Facts
	switch {
	case r.Broken != nil:
		return "broken"
	case f.Running:
		return "running"
	case r.Has(Running) && f.Answer == "":
		return "no-answer" // not done: only an answer says that; it may run again
	case f.Ran:
		return "ran"
	case len(f.ReviewsFailed) > 0 && f.ReviewOverridden:
		return "overridden"
	case len(f.ReviewsFailed) > 0:
		return "review-failed"
	case len(f.ReviewsPending) > 0:
		return "reviewing"
	}
	return "approved"
}

// Approved reports whether the script may be run: every automatic review passed, or a failed one was overridden, and no
// review is pending.
func (r *Run) Approved() bool {
	f := r.Facts
	return f.Exists && len(f.ReviewsPending) == 0 && (len(f.ReviewsFailed) == 0 || f.ReviewOverridden)
}

// AnswerStatus is the run's status in get ans: scanning, dlp-ok, dlp-failed, overridden, invalid or released, with
// " (gate)" for a .FAIL answer; "" without an answer.
func (r *Run) AnswerStatus() string {
	var s string
	switch r.Facts.Answer {
	case "":
		return ""
	case "new":
		s = "scanning"
	case "dlp-ok":
		s = "dlp-ok"
	case "DLP-FAIL":
		s = "dlp-failed"
		if r.Facts.DLPOverridden {
			s = "overridden"
		}
	case "INVALID":
		s = "invalid" // not a valid document: a bug, never data
	case "released":
		s = "released"
	}
	if r.Gate {
		s += " (gate)"
	}
	return s
}

// AnswerFile is the name of the run's answer file, "" without one.
func (r *Run) AnswerFile() string {
	for _, f := range r.Files {
		if answerRe.MatchString(f.Name) {
			return f.Name
		}
	}
	return ""
}

// SaveFiles are the files Save zips: every file of the run directory but running.yml.
func (r *Run) SaveFiles() []string {
	var out []string
	for _, f := range r.Files {
		if f.Name != Running {
			out = append(out, f.Name)
		}
	}
	return out
}

// FailedChecks are the run's failed scans, as Report names them: "review <type>" for each failed review of the script,
// then "DLP scan" for a DLP-FAIL answer.
func (r *Run) FailedChecks() []string {
	var out []string
	for _, t := range r.Facts.ReviewsFailed {
		out = append(out, "review "+t)
	}
	if r.Facts.Answer == "DLP-FAIL" {
		out = append(out, "DLP scan")
	}
	return out
}

// ReportFiles are the files Report attaches, each masked first: ask.sh, the review files and, for a failed DLP scan,
// the answer.
func (r *Run) ReportFiles() []string {
	var out []string
	for _, f := range r.Files {
		if f.Name == AskFile || reviewRe.MatchString(f.Name) || answerReviewRe.MatchString(f.Name) {
			out = append(out, f.Name)
		}
	}
	if r.Facts.Answer == "DLP-FAIL" {
		out = append(out, r.AnswerFile())
	}
	return out
}

// List returns every run of the namespace (of agent only, unless agent is ""), not released first, then newest first.
// Directories that aren't runs (a .scrub.* or .new.* in progress, a bad name) are skipped. A run that can't be read is
// listed with Broken set, never an error for the whole list.
func List(work string, ns names.Namespace, agent string, automatic []string, live Live) ([]*Run, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	base := filepath.Join(ns.Owner, ns.Repo, Dir)
	agents, err := dirs(root, base)
	if err != nil {
		return nil, err
	}
	var out []*Run
	for _, a := range agents {
		if !names.ValidAgent(a) || (agent != "" && a != agent) {
			continue
		}
		runs, err := dirs(root, filepath.Join(base, a))
		if err != nil {
			return nil, err
		}
		for _, n := range runs {
			if !names.ValidRun(n) {
				continue
			}
			r, err := load(root, ns, a, n, automatic, live)
			if err != nil {
				r = &Run{NS: ns, Agent: a, Name: n, Broken: err}
			}
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *Run) int {
		ar, br := a.Facts.Answer == "released", b.Facts.Answer == "released"
		if ar != br {
			if ar {
				return 1
			}
			return -1
		}
		return b.Newest.Compare(a.Newest)
	})
	return out, nil
}

// Names lists every run name in the namespace, any agent, including runs being created (.new.<RUN>) or scrubbed
// (.scrub.<RUN>), so a new name never collides with one.
func Names(work string, ns names.Namespace) (map[string]bool, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	base := filepath.Join(ns.Owner, ns.Repo, Dir)
	agents, err := dirs(root, base)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, a := range agents {
		runs, err := dirs(root, filepath.Join(base, a))
		if err != nil {
			return nil, err
		}
		for _, n := range runs {
			if rest, ok := strings.CutPrefix(n, ".new."); ok {
				n, _, _ = strings.Cut(rest, ".") // .new.<RUN>.<id>
			}
			n = strings.TrimPrefix(n, ".scrub.")
			if s := names.Stem(n); s != "" {
				taken[s] = true
			}
		}
	}
	return taken, nil
}

// dirs lists the real directories in dir below root; a missing dir has none.
func dirs(root *os.Root, dir string) ([]string, error) {
	fi, err := safefs.Lstat(root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsDir() {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

// Mode is how a script says it runs, in its header: INTERACTIVE, NETWORK and PRIVILEGED, each 0 or 1 (ask lint
// allows exactly one line of each). A script without a NETWORK or PRIVILEGED line (submitted by an older version) is normal.
type Mode struct {
	Interactive bool // it asks the user something: only in the user's own terminal
	Network     bool // network mode: a network of its own, once the user accepted it (accept.network)
	Privileged  bool // privileged mode: only in the user's own terminal, never as the controller's child
}

// Name is the mode's name: normal, network or privileged.
func (m Mode) Name() string {
	switch {
	case m.Privileged:
		return "privileged"
	case m.Network:
		return "network"
	}
	return "normal"
}

// ModeOf reads the mode from a script's lines.
func ModeOf(script []byte) Mode {
	var m Mode
	for _, l := range strings.Split(string(script), "\n") {
		switch {
		case interactiveRe.MatchString(l):
			m.Interactive = true
		case networkRe.MatchString(l):
			m.Network = true
		case privilegedRe.MatchString(l):
			m.Privileged = true
		}
	}
	return m
}

// Header returns the leading comment lines of the run's ask.sh (after #!/bin/bash) and its mode.
func Header(work string, ns names.Namespace, agent, run string) ([]string, Mode, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, Mode{}, err
	}
	defer root.Close()
	b, err := safefs.ReadFile(root, filepath.Join(RunDir(ns, agent, run), AskFile), 1<<20)
	if err != nil {
		return nil, Mode{}, err
	}
	var header []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for i := 0; sc.Scan(); i++ {
		l := sc.Text()
		if i > 0 && strings.HasPrefix(l, "# ") && len(header) == i-1 {
			header = append(header, strings.TrimPrefix(l, "# "))
		}
	}
	return header, ModeOf(b), nil
}

// Script reads the run's ask.sh, the one place besides Header where runstate reads content: its mode, and whether it
// still matches the ask.sh.sha256 stored with it (changed: it doesn't, or the sum is missing). exec and accept.network
// need both facts.
func Script(work string, ns names.Namespace, agent, run string) (mode Mode, changed bool, err error) {
	b, _, matches, err := readScript(work, ns, agent, run)
	if err != nil {
		return Mode{}, false, err
	}
	return ModeOf(b), !matches, nil
}

// CheckScript is exec's last check, right before the script starts: ask.sh must still match its ask.sh.sha256, since
// it could have changed after decide checked it. It returns the bytes it checked, which are what runs (a copy of them
// is mounted in the run's container), and their sha256, for the exec.start event (Scan 3).
func CheckScript(work string, ns names.Namespace, agent, run string) ([]byte, string, error) {
	b, sum, matches, err := readScript(work, ns, agent, run)
	if err != nil {
		return nil, "", err
	}
	if !matches {
		return nil, "", fmt.Errorf("%s no longer matches the %s stored with it", AskFile, AskSum)
	}
	return b, sum, nil
}

// readScript reads the run's ask.sh and reports its sha256 and whether it matches ask.sh.sha256 (a missing or
// unreadable sum doesn't).
func readScript(work string, ns names.Namespace, agent, run string) (b []byte, sum string, matches bool, err error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, "", false, err
	}
	defer root.Close()
	dir := RunDir(ns, agent, run)
	if b, err = safefs.ReadFile(root, filepath.Join(dir, AskFile), 1<<20); err != nil {
		return nil, "", false, err
	}
	h := sha256.Sum256(b)
	sum = hex.EncodeToString(h[:])
	stored, serr := safefs.ReadFile(root, filepath.Join(dir, AskSum), 1024)
	return b, sum, serr == nil && string(stored) == sum+"  "+AskFile+"\n", nil
}

// verdict is a review's verdict: the first non-empty line after "## Verdict", or "".
func verdict(root *os.Root, name string) string {
	b, err := safefs.ReadFile(root, name, 1<<20)
	if err != nil {
		return ""
	}
	_, after, ok := bytes.Cut(b, []byte("\n## Verdict\n"))
	if !ok {
		return ""
	}
	for _, l := range strings.Split(string(after), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// BeforeExec reports whether name is one a run directory can hold before its script starts, all written by the controller:
// ask.sh, its sum, a review of the script, override.review.md, accept.network.md, report.md and issue.md.
func BeforeExec(name string) bool {
	switch name {
	case AskFile, AskSum, "override.review.md", AcceptNetwork, "report.md", IssueFile:
		return true
	}
	m := reviewRe.FindStringSubmatch(name)
	return m != nil && m[1] != "answer"
}

// StateFile reports whether name is one of the names that make up a run's state besides ask.sh and its sum: a review,
// an override, accept.network.md, report.md, issue.md, running.yml or an answer.
func StateFile(name string) bool {
	switch name {
	case Running, "override.review.md", "override.dlp.md", AcceptNetwork, "report.md", IssueFile:
		return true
	}
	return reviewRe.MatchString(name) || answerReviewRe.MatchString(name) || answerRe.MatchString(name)
}
