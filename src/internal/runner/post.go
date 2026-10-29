package runner

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"plinth/internal/asklint"
	"plinth/internal/decide"
	"plinth/internal/dlp"
	"plinth/internal/events"
	"plinth/internal/forgejo"
	"plinth/internal/names"
	"plinth/internal/request"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// postJob is a Save or a Report the loop started (startSave, startReport): the masking and the posts to Forgejo run in
// the background, so a slow Forgejo or model holds up nothing else; once done is closed, applyPosts finishes the
// request, which is .running until then.
type postJob struct {
	agent, run, action string
	rec                events.Receipt // the event that allowed it
	done               chan struct{}
	url                string   // the comment's or the issue's page
	missing            []string // Report: what isn't attached, and why
	err                error
}

func postKey(agent, run, action string) string { return agent + "/" + run + "/" + action }

// titleMax is the most characters of the script's header in the title of a run's issue.
const titleMax = 100

// issueMu makes "find the run's issue, or open it" one step: Save and Report of one run may run at the same time, and
// a second issue for the run is what the pointer file (issue.md) is there to prevent.
var issueMu sync.Mutex

// runIssue is the run's one issue in plinth.repo: where the comments of Save and Report go.
type runIssue struct {
	repo        names.Namespace
	number, url string
}

// forgejoClient is Plinth's own client for forgejo-url, with forgejo-token: Save and Report never post with an agent's.
func (l *Runner) forgejoClient() (*forgejo.Client, error) {
	if l.Global.ForgejoToken == "" {
		return nil, errors.New("forgejo-token is not set: Plinth posts with its own token")
	}
	return forgejo.New(l.Global.ForgejoURL, l.Global.ForgejoToken, l.ForgejoCAFile)
}

// startPost runs post in the background as the job of the request, then closes the client.
func (l *Runner) startPost(r *runstate.Run, action string, rec events.Receipt, fj *forgejo.Client, post func(j *postJob)) {
	j := &postJob{agent: r.Agent, run: r.Name, action: action, rec: rec, done: make(chan struct{})}
	l.posts[postKey(r.Agent, r.Name, action)] = j
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer close(j.done)
		defer fj.Close()
		post(j)
	}()
}

// titleText is s on one line, printable, cut to n characters, with its fixed-pattern secrets masked: the text of a
// script's header in a title.
func titleText(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "..."
	}
	masked, _ := dlp.Fixed([]byte(s))
	return string(masked)
}

// issueText is the title and the text of a run's issue: "<ns> <stem>: <the script's header>", and the namespace, agent,
// full run name, the script's sha256 and, when its ISSUE= line names an issue on this Forgejo, the origin ticket (context
// only: nothing is posted there). The fixed-pattern masks are applied to all of it.
func (l *Runner) issueText(r *runstate.Run, ask []byte, rec events.Receipt, action string) (title, body string) {
	title = fmt.Sprintf("%s %s", l.ns, names.Stem(r.Name))
	if head := titleText(asklint.Header(ask), titleMax); head != "" {
		title += ": " + head
	}
	sum := sha256.Sum256(ask)
	var b strings.Builder
	fmt.Fprintf(&b, "Plinth's record of one run. Save and Report add what they post as comments.\n\n- Namespace: %s\n- Agent: %s\n- Run: %s\n- Script: ask.sh, sha256 %s\n",
		l.ns, r.Agent, r.Name, hex.EncodeToString(sum[:]))
	if ref, ok := asklint.ScriptIssue(ask); ok && l.Global.ForgejoURL != "" && ref.OnForgejo(l.Global.ForgejoURL) {
		fmt.Fprintf(&b, "- Origin: %s\n", ref.URL)
	}
	fmt.Fprintf(&b, "- Opened: %s by %s, event seq %d\n", time.Now().UTC().Format(time.RFC3339), action, rec.Seq)
	masked, _ := dlp.Fixed([]byte(b.String()))
	return title, string(masked)
}

// parseIssueFile reads the run's issue from issue.md: the line "Issue: <page>".
func parseIssueFile(b []byte) (runIssue, bool) {
	for _, line := range strings.Split(string(b), "\n") {
		if u, ok := strings.CutPrefix(line, "Issue: "); ok {
			ref, ok := asklint.ParseIssueURL(strings.TrimSpace(u))
			return runIssue{repo: ref.Repo, number: ref.Number, url: ref.URL}, ok
		}
	}
	return runIssue{}, false
}

// runIssue returns the run's issue: the one issue.md points to, else a new one in repo (plinth.repo) whose page is
// written to issue.md before anything else is posted. A restart between the two steps would leave a second issue
// possible; the file is written the moment the issue exists. It works on <work> with a root of its own: it runs in the
// post's goroutine, not in the loop.
func (l *Runner) runIssue(ctx context.Context, fj *forgejo.Client, repo names.Namespace, r *runstate.Run, ask []byte, rec events.Receipt, action string) (runIssue, error) {
	issueMu.Lock()
	defer issueMu.Unlock()
	root, err := os.OpenRoot(l.Work)
	if err != nil {
		return runIssue{}, err
	}
	defer root.Close()
	dir := runstate.RunDir(l.ns, r.Agent, r.Name)
	b, err := safefs.ReadFile(root, filepath.Join(dir, runstate.IssueFile), 4<<10)
	switch {
	case err == nil:
		is, ok := parseIssueFile(b)
		if !ok {
			return runIssue{}, fmt.Errorf("%s has no \"Issue: <page>\" line: remove it to open the run's issue again", runstate.IssueFile)
		}
		return is, nil
	case !errors.Is(err, fs.ErrNotExist):
		return runIssue{}, err
	}
	title, body := l.issueText(r, ask, rec, action)
	p, err := fj.CreateIssue(ctx, repo, title, body)
	if err != nil {
		return runIssue{}, err
	}
	is := runIssue{repo: repo, number: strconv.FormatInt(p.Number, 10), url: p.URL}
	note := fmt.Sprintf("# Issue of %s/%s\n\nIssue: %s\n\nOpened at %s by %s. Event: seq %d, sha256 %s (plinthctl events show -n %s %s)\n",
		r.Agent, r.Name, p.URL, time.Now().UTC().Format(time.RFC3339), action, rec.Seq, rec.Hash, l.ns, r.Name)
	if err := safefs.WriteNew(root, dir, runstate.IssueFile, []byte(note), 0o600); err != nil {
		return is, fmt.Errorf("the run's issue %s is open, but %s can't be written (%v): the next Save or Report would open another", p.URL, runstate.IssueFile, err)
	}
	return is, nil
}

// autosave queues action for the run by itself when plinth.autosave says so: Save right after Done
// (autosave.runs), Report when a run's check fails (autosave.failures; once per run, so not for a run reported
// already). The request is carried out at the next poll and decided by the same rules as the user's; the event log says
// it was Plinth's own doing. One already queued or running is left as it is. A failure to queue is logged, never an
// error of the step that led to it.
func (l *Runner) autosave(root *os.Root, agent, run, action string) error {
	key, dir := agent+"/"+run, runstate.RunDir(l.ns, agent, run)
	switch {
	case action == decide.Save && !l.Global.Plinth.Autosave.Runs, action == decide.Report && !l.Global.Plinth.Autosave.Failures:
		return nil
	case action == decide.Report:
		if _, err := safefs.Lstat(root, filepath.Join(dir, "report.md")); err == nil {
			return nil
		}
	}
	if _, err := l.event(Caller, "autosave."+action, key, "queued", "autosave", "plinth.autosave is set for it"); err != nil {
		return err
	}
	switch err := request.Queue(l.Work, l.ns, agent, run, action); {
	case err == nil, errors.Is(err, request.ErrQueued), errors.Is(err, request.ErrInProgress):
	default:
		l.runLog(agent, run).Warn("autosave not queued", "action", action, "err", err)
	}
	return nil
}

// startSave is Save: it zips the run directory (but running.yml) into <RUN>.zip and posts it as a Run artifacts comment
// on the run's issue in plinth.repo, which it opens first when the run has none.
func (l *Runner) startSave(root *os.Root, r *runstate.Run, rec events.Receipt) error {
	repo, err := names.ParseNamespace(l.Global.Plinth.Repo)
	if err != nil {
		return fmt.Errorf("plinth.repo: %w", err)
	}
	dir := runstate.RunDir(l.ns, r.Agent, r.Name)
	ask, err := safefs.ReadFile(root, filepath.Join(dir, runstate.AskFile), dlp.MaxSize)
	if err != nil {
		return err
	}
	var in []zipFile
	files := r.SaveFiles()
	for _, name := range files {
		b, err := safefs.ReadFile(root, filepath.Join(dir, name), dlp.MaxSize)
		if err != nil {
			return err
		}
		f := zipFile{name: name, body: b}
		if i := slices.IndexFunc(r.Files, func(rf runstate.File) bool { return rf.Name == name }); i >= 0 {
			f.mod = r.Files[i].Mod
		}
		in = append(in, f)
	}
	zipped, err := zipOf(r.Name, in)
	if err != nil {
		return err
	}
	fj, err := l.forgejoClient()
	if err != nil {
		return err
	}
	ctx := l.ctx
	body := fmt.Sprintf("### Run artifacts\n\n%s.zip: the %d files of run %s (agent %s) in %s, saved by Plinth (running.yml left out).\n",
		r.Name, len(files), r.Name, r.Agent, l.ns)
	l.startPost(r, decide.Save, rec, fj, func(j *postJob) {
		is, err := l.runIssue(ctx, fj, repo, r, ask, rec, decide.Save)
		if err != nil {
			j.err = err
			return
		}
		c, err := fj.Comment(ctx, is.repo, is.number, body)
		if err != nil {
			j.err = err
			return
		}
		j.url = c.URL
		if err := fj.AttachToComment(ctx, is.repo, c.ID, r.Name+".zip", zipped); err != nil {
			j.err = fmt.Errorf("the comment %s is posted, but without its zip: %w", c.URL, err)
		}
	})
	return nil
}

// startReport is Report: it posts the run's failed scans as a comment on the run's issue in plinth.repo (opened first
// when the run has none), with the deciding events in its text and the run's ask.sh, review files and (for a
// failed DLP scan) answer attached. Everything is masked first (the DLP scan: the fixed patterns, then the model): an
// attachment whose masking fails is left out, and the text says so; without Ollama, or when the text's own masking
// fails, nothing is posted, not even the issue.
func (l *Runner) startReport(root *os.Root, r *runstate.Run, rec events.Receipt) error {
	if !l.Ollama.Configured() {
		return errors.New("not available without Ollama: everything Report posts is masked first (ollama.host); report again once it is set")
	}
	repo, err := names.ParseNamespace(l.Global.Plinth.Repo)
	if err != nil {
		return fmt.Errorf("plinth.repo: %w", err)
	}
	deciding, err := l.decidingEvents(r)
	if err != nil {
		return err
	}
	dir := runstate.RunDir(l.ns, r.Agent, r.Name)
	ask, err := safefs.ReadFile(root, filepath.Join(dir, runstate.AskFile), dlp.MaxSize)
	if err != nil {
		return err
	}
	type file struct {
		name string
		body []byte
		err  error
	}
	var files []file
	for _, name := range r.ReportFiles() {
		b, err := safefs.ReadFile(root, filepath.Join(dir, name), dlp.MaxSize)
		files = append(files, file{name, b, err})
	}
	fj, err := l.forgejoClient()
	if err != nil {
		return err
	}
	checks := r.FailedChecks()
	var text strings.Builder
	fmt.Fprintf(&text, "### Report of failed scans: %s\n\nPlinth's report. Reported: %s, event seq %d\n",
		strings.Join(checks, ", "), time.Now().UTC().Format(time.RFC3339), rec.Seq)
	for _, c := range checks {
		fmt.Fprintf(&text, "\n## %s\n\n", c)
		e, ok := deciding[c]
		if !ok {
			text.WriteString("No event of it in the namespace's event log.\n")
			continue
		}
		fmt.Fprintf(&text, "- Rule: %s\n- Event: seq %d, %s\n- Reason:\n\n%s", e.Rule, e.Seq, e.Time, fence(e.Reason))
	}
	ctx, model := l.ctx, l.Global.Ollama.Models.DLP
	l.startPost(r, decide.Report, rec, fj, func(j *postJob) {
		var attach []zipFile
		for _, f := range files {
			if f.err == nil {
				f.body, _, f.err = dlp.Scan(ctx, l.Ollama, model, f.body)
			}
			if f.err != nil {
				j.missing = append(j.missing, f.name+": its masking failed, left out: "+f.err.Error())
				continue
			}
			attach = append(attach, zipFile{name: f.name, body: f.body})
		}
		zipName := r.Name + ".report.zip"
		body := text.String() + "\n## Attachments\n\n"
		if len(attach) > 0 {
			body += zipName + ", each file in it masked first (the DLP scan):\n\n"
		}
		for _, f := range attach {
			body += "- " + f.name + "\n"
		}
		for _, m := range j.missing {
			body += "- not attached: " + m + "\n"
		}
		var zipped []byte
		if len(attach) > 0 {
			var err error
			if zipped, err = zipOf(r.Name, attach); err != nil {
				j.err = err
				return
			}
		}
		masked, _, err := dlp.Scan(ctx, l.Ollama, model, []byte(body))
		if err != nil {
			j.err = fmt.Errorf("masking the report's text failed, nothing posted: %w", err)
			return
		}
		is, err := l.runIssue(ctx, fj, repo, r, ask, rec, decide.Report)
		if err != nil {
			j.err = err
			return
		}
		c, err := fj.Comment(ctx, is.repo, is.number, string(masked))
		if err != nil {
			j.err = err
			return
		}
		j.url = is.url
		if zipped != nil {
			if err := fj.AttachToComment(ctx, is.repo, c.ID, zipName, zipped); err != nil {
				j.missing = append(j.missing, zipName+": its upload failed: "+err.Error())
			}
		}
	})
	return nil
}

// zipFile is one file of a zip Save or Report attaches; mod is its time (zero: now).
type zipFile struct {
	name string
	body []byte
	mod  time.Time
}

// zipOf zips files into <run>/<name> entries: one attachment, of a type Forgejo allows by default (it has no .sh or
// .yml).
func zipOf(run string, files []zipFile) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		h := &zip.FileHeader{Name: run + "/" + f.name, Method: zip.Deflate, Modified: cmp.Or(f.mod, time.Now())}
		w, err := zw.CreateHeader(h)
		if err == nil {
			_, err = w.Write(f.body)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decidingEvents are the events that decided the run's failed scans (FailedChecks): its last failed review.<type> by
// the runner for each failed review, and its last failed dlp.scan.
func (l *Runner) decidingEvents(r *runstate.Run) (map[string]events.Event, error) {
	key, out := r.Agent+"/"+r.Name, map[string]events.Event{}
	err := events.Walk(l.Work, filepath.Join(l.ns.Owner, l.ns.Repo), func(e events.Event, _ []byte) error {
		if e.Target != key || e.Caller != Caller || e.Outcome != "failed" {
			return nil
		}
		if t, ok := strings.CutPrefix(e.Action, decide.Review+"."); ok && !strings.HasPrefix(t, "answer.") {
			out["review "+t] = e
		} else if e.Action == "dlp.scan" {
			out["DLP scan"] = e
		}
		return nil
	})
	return out, err
}

// fence puts s in a fenced code block, its fence longer than any run of backticks in s, so nothing in it is markdown.
func fence(s string) string {
	n, run := 3, 0
	for _, c := range s {
		if c == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", n)
	return f + "\n" + strings.TrimSuffix(s, "\n") + "\n" + f + "\n"
}

// applyPosts finishes every Save and Report that is done: a Report writes report.md with the issue's link (what
// isn't attached in it too), then the request is .finished; an error fails it. One cut short by the controller's stop
// is never applied: its request stays .running, and the next start carries it out again. One whose run was scrubbed
// meanwhile is dropped.
func (l *Runner) applyPosts(root *os.Root) error {
	var keys []string
	for k, j := range l.posts {
		select {
		case <-j.done:
			keys = append(keys, k)
		default:
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		j := l.posts[k]
		if j.err != nil && l.ctx.Err() != nil {
			continue
		}
		err := l.applyPost(root, j)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			delete(l.posts, k) // another error keeps it, for the next poll
			err = nil
		}
		if err := l.perRun("post "+k, err); err != nil {
			return err
		}
	}
	return nil
}

func (l *Runner) applyPost(root *os.Root, j *postJob) error {
	dir := request.RunDir(l.ns, j.agent, j.run)
	if j.err != nil && j.url == "" {
		l.runLog(j.agent, j.run).Error("request failed", "action", j.action, "err", j.err)
		return l.failRequest(root, dir, j.action, request.Running, "Error: "+j.err.Error()+"\n")
	}
	if j.action == decide.Report {
		body := fmt.Sprintf("# Report of %s/%s\n\nIssue: %s\n\nReported at %s. Event: seq %d, sha256 %s (plinthctl events show -n %s %s)\n",
			j.agent, j.run, j.url, time.Now().UTC().Format(time.RFC3339), j.rec.Seq, j.rec.Hash, l.ns, j.run)
		if len(j.missing) > 0 {
			body += "\nNot attached:\n"
			for _, m := range j.missing {
				body += "- " + m + "\n"
			}
		}
		if err := safefs.WriteNew(root, runstate.RunDir(l.ns, j.agent, j.run), "report.md", []byte(body), 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		l.runLog(j.agent, j.run).Info("reported", "issue", j.url)
		for _, m := range j.missing {
			l.runLog(j.agent, j.run).Warn("report: not attached", "file", m)
		}
		return l.requestDone(root, dir, j.action)
	}
	if j.err != nil { // the comment is posted, its zip isn't: Save may run again
		l.runLog(j.agent, j.run).Error("request failed", "action", j.action, "err", j.err)
		return l.failRequest(root, dir, j.action, request.Running, "Error: "+j.err.Error()+"\n")
	}
	l.runLog(j.agent, j.run).Info("saved", "comment", j.url)
	return l.requestDone(root, dir, j.action)
}

// ReportBox says where Report posts and what, for plinthctl report and the TUI's r.
func ReportBox(repo string, r *runstate.Run) string {
	return fmt.Sprintf("Report posts on the run's issue in %s:\n  \"%s %s: <what the script says>\", opened if the run has none,\n"+
		"  a comment \"Report of failed scans: %s\" with the events that decided it,\nand attaches %s.report.zip:\n  %s\n"+
		"Everything is masked first (the DLP scan); what can't be masked is left out.\n",
		repo, r.NS, names.Stem(r.Name), strings.Join(r.FailedChecks(), ", "), r.Name, strings.Join(r.ReportFiles(), ", "))
}
