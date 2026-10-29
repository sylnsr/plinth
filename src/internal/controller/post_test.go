package controller

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"plinth/internal/config"
	"plinth/internal/decide"
	"plinth/internal/events"
	"plinth/internal/ollama/ollamatest"
	"plinth/internal/request"
	"plinth/internal/runner"
)

// posted is one POST the stand-in for Forgejo took: an attachment's body is its file.
type posted struct{ path, name, body string }

// fakeForge is a stand-in for Forgejo's issue API over https: it takes issues, comments and their attachments with
// Plinth's token, and records them.
type fakeForge struct {
	URL, CA string
	mu      sync.Mutex
	posts   []posted
}

func newFakeForge(t *testing.T, token string) *fakeForge {
	t.Helper()
	f := &fakeForge{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "token "+token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		p := posted{path: r.URL.Path, name: r.URL.Query().Get("name")}
		if file, _, err := r.FormFile("attachment"); err == nil {
			b, _ := io.ReadAll(file)
			p.body = string(b)
		} else {
			b, _ := io.ReadAll(r.Body)
			p.body = string(b)
		}
		f.mu.Lock()
		f.posts = append(f.posts, p)
		n := len(f.posts)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		switch {
		case strings.HasSuffix(r.URL.Path, "/assets"):
			fmt.Fprintf(w, `{"id":%d}`, n)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprintf(w, `{"id":%d,"html_url":"%s%s#issuecomment-%d"}`, 500+n, f.URL, strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/comments"), "/api/v1/repos"), 500+n)
		default:
			fmt.Fprintf(w, `{"id":%d,"number":7,"html_url":"%s%s/7"}`, 900+n, f.URL, strings.TrimPrefix(r.URL.Path, "/api/v1/repos"))
		}
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	f.CA = filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, f.CA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	return f
}

func (f *fakeForge) all() []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts)
}

// unzip is a zip's files, name -> content.
func unzip(t *testing.T, b string) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader([]byte(b)), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		c, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(c)
	}
	return out
}

// TestSaveAndReport: Save zips the run into a Run artifacts comment on the run's own issue in plinth.repo, which it
// opens first (issue.md points to it, so a second Save adds a comment to the same issue); Report adds a comment with the
// deciding events and attaches the run's files, all of it masked first, and writes report.md, so there is one report per
// run; both refuse what decide refuses, and Report posts nothing without Ollama.
func TestSaveAndReport(t *testing.T) {
	forge := newFakeForge(t, "plinth-token")
	cfg := strings.Replace(globalCfg, "https://127.0.0.1:9", forge.URL, 1)
	cfg = strings.Replace(cfg, "plinth:\n", "plinth:\n  repo: michael/runs\n", 1) + "forgejo-token: plinth-token\n"
	mkRuns := func(work string) {
		nsDir := filepath.Join(work, "michael", "devops")
		script := filledTemplate(t)
		sum := sha256.Sum256(script)
		mk := func(run string, files map[string]string) {
			dir := filepath.Join(nsDir, "runs", "main", run)
			writeFile(t, filepath.Join(dir, "ask.sh"), script, 0o700)
			writeFile(t, filepath.Join(dir, "ask.sh.sha256"), []byte(hex.EncodeToString(sum[:])+"  ask.sh\n"), 0o600)
			for n, body := range files {
				writeFile(t, filepath.Join(dir, n), []byte(body), 0o600)
			}
			writeFile(t, filepath.Join(nsDir, request.Dir, "main", run, "exec."+request.Finished), nil, 0o600)
		}
		mk("calm-otter-12", map[string]string{"review.security.md": "## Verdict\nOK to run\n", "answer.dlp-ok.yml": string(answerDoc("calm-otter-12", "disks ok\n"))})
		mk("quiet-wren", map[string]string{"review.security.FAILED.md": "# Review security: FAILED\n\nthe script prints hunter2\n",
			"answer.DLP-FAIL.yml": string(answerDoc("quiet-wren", "db password=hunter2\n"))})
		evlog, err := events.Open(work, filepath.Join("michael", "devops"), 10000)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range []events.Event{
			{Caller: runner.Caller, Action: "review.security", Target: "main/quiet-wren", Outcome: "failed", Rule: "review.failed", Reason: "verdict Do not run; it echoes hunter2"},
			{Caller: runner.Caller, Action: "dlp.scan", Target: "main/quiet-wren", Outcome: "failed", Rule: "dlp.failed", Reason: "the model's answer is not the expected JSON"},
		} {
			if _, err := evlog.Append(e); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Without Ollama Report posts nothing: it can't mask.
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(cfg), 0o600)
	enabled(t, work, "main")
	mkRuns(work)
	_, stop := start(t, Options{Work: work, ForgejoCAFile: forge.CA})
	queue(t, work, "quiet-wren", decide.Report)
	if f := settled(t, work, "quiet-wren"); !strings.Contains(f[decide.Report], "not available without Ollama") || len(forge.all()) != 0 {
		t.Errorf("report without Ollama: %v, posted %v", f, forge.all())
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// Report masks through the Ollama gateway, a podman container: without podman (the build's container) only Save is
	// checked here.
	_, podmanErr := exec.LookPath("podman")
	work = setup(t)
	stopGateway(t, work)
	fake := ollamatest.New(t)
	dlp := ollamatest.DLP(func(text string) string {
		if strings.Contains(text, "hunter2") {
			return "password"
		}
		return ""
	})
	fake.SetReply(func(ctx context.Context, c ollamatest.Chat) (int, string) {
		if !isDLP(c) {
			return http.StatusServiceUnavailable, "down"
		}
		return dlp(ctx, c)
	})
	writeFile(t, filepath.Join(work, config.FileName), []byte(cfg+"ollama:\n  host: "+fake.URL+"\n  models:\n    review: rev\n    dlp: dlp\n  retry-seconds: 0\n"), 0o600)
	enabled(t, work, "main")
	mkRuns(work)
	start(t, Options{Work: work, ForgejoCAFile: forge.CA})
	queue(t, work, "calm-otter-12", decide.Save)
	queue(t, work, "calm-otter-12", decide.Report) // no failed scan
	queue(t, work, "quiet-wren", decide.Save)      // its DLP scan failed and is not overridden
	if podmanErr == nil {
		queue(t, work, "quiet-wren", decide.Report)
	}
	if f := settled(t, work, "calm-otter-12"); len(f) != 1 || !strings.Contains(f[decide.Report], "has no failed scan") {
		t.Errorf("calm-otter-12: %v\n%s", f, logText(work))
	}
	queue(t, work, "calm-otter-12", decide.Save) // again: the same issue, a second comment (issue.md points to it)
	if f := settled(t, work, "calm-otter-12"); len(f) != 1 {
		t.Errorf("calm-otter-12, saved again: %v\n%s", f, logText(work))
	}
	if f := settled(t, work, "quiet-wren"); len(f) != 1 || !strings.Contains(f[decide.Save], "dlp.failed") {
		t.Errorf("quiet-wren: %v\n%s", f, logText(work))
	}

	posts := forge.all()
	const repoAPI = "/api/v1/repos/michael/runs"
	where := func(keep func(p posted) bool) []posted {
		var out []posted
		for _, p := range posts {
			if keep(p) {
				out = append(out, p)
			}
		}
		return out
	}
	titled := func(title string) []posted {
		return where(func(p posted) bool {
			return p.path == repoAPI+"/issues" && strings.Contains(p.body, `"title":"`+title+`"`)
		})
	}
	commented := func(text string) []posted {
		return where(func(p posted) bool { return p.path == repoAPI+"/issues/7/comments" && strings.Contains(p.body, text) })
	}
	zips := func(name string) []posted {
		return where(func(p posted) bool {
			return strings.HasPrefix(p.path, repoAPI+"/issues/comments/") && strings.HasSuffix(p.path, "/assets") && p.name == name
		})
	}
	// Save: the run's own issue in plinth.repo is opened first (not the namespace's issue 12 its name ends in), then the
	// comment, then its zip with every file of the run; a second Save adds a comment to the same issue.
	if is := titled("michael/devops calm-otter: check the disks"); len(is) != 1 || !strings.Contains(is[0].body, "Run: calm-otter-12") || !strings.Contains(is[0].body, "Agent: main") {
		t.Errorf("Save's issue: %+v", is)
	}
	if c := where(func(p posted) bool { return strings.HasPrefix(p.path, "/api/v1/repos/michael/devops/") }); len(c) != 0 {
		t.Errorf("posted in the namespace's repo: %+v", c)
	}
	if c := commented("### Run artifacts"); len(c) != 2 {
		t.Errorf("Save's comments: %+v", c)
	}
	zipped := zips("calm-otter-12.zip")
	if len(zipped) != 2 {
		t.Fatalf("Save's attachments: %+v", zipped)
	}
	inZip := unzip(t, zipped[0].body)
	for _, want := range []string{"calm-otter-12/answer.dlp-ok.yml", "calm-otter-12/ask.sh", "calm-otter-12/ask.sh.sha256", "calm-otter-12/review.security.md"} {
		if _, ok := inZip[want]; !ok {
			t.Errorf("no %s in the zip: %v", want, slices.Sorted(maps.Keys(inZip)))
		}
	}
	pointer := filepath.Join(work, "michael", "devops", "runs", "main", "calm-otter-12", "issue.md")
	if b, _ := os.ReadFile(pointer); !strings.Contains(string(b), "Issue: "+forge.URL+"/michael/runs/issues/7\n") {
		t.Errorf("issue.md: %s", b)
	}

	if podmanErr != nil {
		t.Log("no podman: Report isn't checked")
		return
	}
	// Report: the run's issue is opened (its title and text masked, no failed checks in the title), one comment holds the
	// deciding events, one zip is attached to it, each file in it masked.
	if is := titled("michael/devops quiet-wren: check the disks"); len(is) != 1 || !strings.Contains(is[0].body, "Run: quiet-wren") {
		t.Fatalf("Report's issue: %+v", is)
	}
	comment := commented("### Report of failed scans: review security, DLP scan")
	if len(comment) != 1 {
		t.Fatalf("Report's comments: %+v", comment)
	}
	for _, want := range []string{"Rule: review.failed", "Rule: dlp.failed", "the model's answer is not the expected JSON", "quiet-wren.report.zip",
		"- ask.sh", "- review.security.FAILED.md", "- answer.DLP-FAIL.yml"} {
		if !strings.Contains(comment[0].body, want) {
			t.Errorf("no %q in the report's comment: %s", want, comment[0].body)
		}
	}
	attached := zips("quiet-wren.report.zip")
	if len(attached) != 1 {
		t.Fatalf("Report's attachments: %+v", attached)
	}
	inReport := unzip(t, attached[0].body)
	if got := slices.Sorted(maps.Keys(inReport)); !slices.Equal(got, []string{"quiet-wren/answer.DLP-FAIL.yml", "quiet-wren/ask.sh", "quiet-wren/review.security.FAILED.md"}) {
		t.Errorf("in the report's zip: %v", got)
	}
	for name, body := range inReport {
		if strings.Contains(body, "hunter2") {
			t.Errorf("%s: posted unmasked: %s", name, body)
		}
	}
	for _, p := range posts {
		if strings.Contains(p.body, "hunter2") {
			t.Errorf("%s %s: posted unmasked: %s", p.path, p.name, p.body)
		}
	}
	report := filepath.Join(work, "michael", "devops", "runs", "main", "quiet-wren", "report.md")
	if b, _ := os.ReadFile(report); !strings.Contains(string(b), "Issue: "+forge.URL+"/michael/runs/issues/7") {
		t.Errorf("report.md: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(report), "issue.md")); !strings.Contains(string(b), "Issue: "+forge.URL+"/michael/runs/issues/7\n") {
		t.Errorf("issue.md of the report: %s", b)
	}
	if !load(t, work, "quiet-wren").Facts.Reported { // decide refuses a second report (decide's test)
		t.Error("quiet-wren is not reported")
	}
}

// TestAutosaveRuns: with plinth.autosave.runs the loop queues Save by itself right after Done and carries it out
// like the user's: the run's issue is opened, the comment posted, and the event log says Plinth queued it.
func TestAutosaveRuns(t *testing.T) {
	forge := newFakeForge(t, "plinth-token")
	cfg := strings.Replace(globalCfg, "https://127.0.0.1:9", forge.URL, 1)
	cfg = strings.Replace(cfg, "plinth:\n", "plinth:\n  repo: michael/runs\n  autosave:\n    runs: true\n", 1) + "forgejo-token: plinth-token\n"
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(cfg), 0o600)
	enabled(t, work, "main")
	nsDir := filepath.Join(work, "michael", "devops")
	script := filledTemplate(t)
	sum := sha256.Sum256(script)
	dir := filepath.Join(nsDir, "runs", "main", "calm-otter")
	writeFile(t, filepath.Join(dir, "ask.sh"), script, 0o700)
	writeFile(t, filepath.Join(dir, "ask.sh.sha256"), []byte(hex.EncodeToString(sum[:])+"  ask.sh\n"), 0o600)
	writeFile(t, filepath.Join(dir, "review.security.md"), []byte("## Verdict\nOK to run\n"), 0o600)
	writeFile(t, filepath.Join(dir, "answer.dlp-ok.yml"), []byte(string(answerDoc("calm-otter", "disks ok\n"))), 0o600)
	writeFile(t, filepath.Join(nsDir, request.Dir, "main", "calm-otter", "exec."+request.Finished), nil, 0o600)
	start(t, Options{Work: work, ForgejoCAFile: forge.CA})
	queue(t, work, "calm-otter", decide.Done)
	eventually(t, "the run saved by itself", func() bool { return finished(work, "calm-otter", decide.Save) })
	if f := settled(t, work, "calm-otter"); len(f) != 0 {
		t.Errorf("failed requests: %v\n%s", f, logText(work))
	}
	var comments int
	for _, p := range forge.all() {
		if p.path == "/api/v1/repos/michael/runs/issues/7/comments" && strings.Contains(p.body, "### Run artifacts") {
			comments++
		}
	}
	if comments != 1 {
		t.Errorf("Save's comments: %d in %+v", comments, forge.all())
	}
	if !strings.Contains(strings.Join(eventList(t, work), "\n"), "autosave.save") {
		t.Errorf("no autosave.save in the events:\n%s", strings.Join(eventList(t, work), "\n"))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "issue.md")); !strings.Contains(string(b), "Issue: "+forge.URL+"/michael/runs/issues/7\n") {
		t.Errorf("issue.md: %s", b)
	}
}
