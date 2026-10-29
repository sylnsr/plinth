package agentmcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"plinth/internal/agents"
	"plinth/internal/asklint"
	"plinth/internal/config"
	"plinth/internal/doc"
	"plinth/internal/events"
	"plinth/internal/mcp"
	"plinth/internal/names"
	"plinth/internal/runner"
	"plinth/internal/runstate"
)

var ns = names.Namespace{Owner: "michael", Repo: "devops"}

// fixtureOllama is the ollama.host the fixture's server has: "" for none (ask_preview then refuses).
var fixtureOllama = "http://ollama.example:11434"

// fixtureLimit is plinth.preview.limit for the fixture's server; 0 keeps the default.
var fixtureLimit = 0

const globalCfg = `forgejo-url: https://forgejo.example.org
reviewers:
  type:
    security:
      urls: [https://forgejo.example.org/ai/policies/raw/branch/main/security.md]
    dlp:
      definition: "Find secrets."
  automatic: [security]
`

// fakeNS is a namespace's runner as the server sees it: it records events, counts kicks and takes a submission in as
// take says (nil: never).
type fakeNS struct {
	cfg *config.Namespace

	mu     sync.Mutex
	events []string
	kicks  int
	take   func(id string) (runner.Submitted, bool)

	preview []runner.PreviewReview // what Preview gives
	seen    [][]byte               // the scripts Preview was given
	gate    chan struct{}          // when set, Preview waits until it is closed
}

func (f *fakeNS) Config() *config.Namespace { return f.cfg }

func (f *fakeNS) Live() runstate.Live { return nil }

func (f *fakeNS) Record(caller, action, target, outcome, rule, reason string) (events.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, caller+" "+action+" "+target+" "+outcome+" "+rule)
	return events.Receipt{}, nil
}

func (f *fakeNS) Kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks++
}

func (f *fakeNS) Submitted(ctx context.Context, agent, id string) (runner.Submitted, bool) {
	f.mu.Lock()
	take := f.take
	f.mu.Unlock()
	if take != nil {
		if s, ok := take(id); ok {
			return s, true
		}
	}
	<-ctx.Done()
	return runner.Submitted{}, false
}

func (f *fakeNS) Preview(ctx context.Context, script []byte) []runner.PreviewReview {
	f.mu.Lock()
	f.seen = append(f.seen, script)
	gate, out := f.gate, f.preview
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	return out
}

func (f *fakeNS) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// fixture is a work directory with namespace michael/devops, agent main registered and enabled from /r, agent ci
// registered but not enabled, and a server on it; it returns main's and ci's clients.
func fixture(t *testing.T) (work string, f *fakeNS, mainc, cic *mcp.Client) {
	t.Helper()
	work = filepath.Join(t.TempDir(), ".plinth")
	write(t, filepath.Join(work, config.FileName), globalCfg)
	mainTok, _, err := agents.Register(work, ns, "main", "/r")
	if err != nil {
		t.Fatal(err)
	}
	ciTok, _, err := agents.Register(work, ns, "ci", "/c")
	if err != nil {
		t.Fatal(err)
	}
	g, err := config.LoadGlobal(work, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	g.Ollama.Host = fixtureOllama
	if fixtureLimit > 0 {
		g.Plinth.Preview.Limit = fixtureLimit
	}
	f = &fakeNS{cfg: &config.Namespace{Automatic: []string{"security"}, Enabled: map[string]string{"main": "/r"}}}
	s := &Server{Work: work, Global: g, Version: "test", SubmitWait: 500 * time.Millisecond, PreviewWait: 300 * time.Millisecond,
		Namespace: func(n names.Namespace) Namespace {
			if n == ns {
				return f
			}
			return nil
		}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	url := srv.URL + Path
	return work, f, &mcp.Client{HTTP: srv.Client(), URL: url, Token: mainTok}, &mcp.Client{HTTP: srv.Client(), URL: url, Token: ciTok}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, c *mcp.Client, tool string, args any) (string, string) {
	t.Helper()
	out, err := c.Call(context.Background(), tool, args)
	var te *mcp.ToolError
	if errors.As(err, &te) {
		return "", te.Text
	}
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out, ""
}

// TestToken: without a token, with a wrong one or a pending registration's, nothing is served (401, naming nothing);
// an agent that is registered but not enabled is refused by decide, and the refusal recorded.
func TestToken(t *testing.T) {
	work, f, mainc, cic := fixture(t)
	pending, _, err := agents.Register(work, ns, "main", "/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"", "plinth_wrong", pending, "Bearer"} {
		c := &mcp.Client{HTTP: mainc.HTTP, URL: mainc.URL, Token: tok}
		if _, err := c.Call(context.Background(), "guide", nil); err == nil || !strings.Contains(err.Error(), "HTTP 401") ||
			strings.Contains(err.Error(), "main") || strings.Contains(err.Error(), "devops") {
			t.Errorf("token %q: %v", tok, err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, mainc.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := mainc.HTTP.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("tools/list without a token: %v %v", resp, err)
	}
	if _, msg := call(t, cic, "guide", nil); !strings.Contains(msg, "refused by rule target.valid: agent ci is registered but not enabled") ||
		!strings.Contains(msg, "next: tell the user") {
		t.Errorf("ci: %q", msg)
	}
	if ev := f.recorded(); len(ev) != 1 || ev[0] != "ci guide ci deny target.valid" {
		t.Errorf("events: %q", ev)
	}
	if out, msg := call(t, mainc, "guide", nil); msg != "" || !strings.Contains(out, "## This namespace: michael/devops") {
		t.Errorf("main: %q %q", out, msg)
	}
}

// TestGuideAndTemplate: the guide explains both ways to run a script, Plinth runs the default, and where the
// image comes from and what to do when a script needs a tool; its namespace part
// says the modes, the automatic reviews and the policy URLs (an inline definition only named); ask_template is the
// namespace's ask-template.yml as it is.
func TestGuideAndTemplate(t *testing.T) {
	work, f, mainc, _ := fixture(t)
	out, _ := call(t, mainc, "guide", nil)
	for _, want := range []string{"# Plinth: how you work with it", "## Two ways to run a script", "Use Plinth runs unless the user asks for plain AAA",
		"## Plinth runs", "## Plain AAA", "`aaa_new`", "network (`NETWORK=1`", "not allowed here",
		"A run is stopped after 120 minutes.", "every Plinth run's script gets before the user decides: `security`.",
		"- `security` (automatic):\n  - https://forgejo.example.org/ai/policies/raw/branch/main/security.md\n",
		"## The image your scripts run in", "`.plinth/Containerfile`", "plinthctl image publish -n <owner>/<repo>",
		"- `dlp`: defined in Plinth's config, not published"} {
		if !strings.Contains(out, want) {
			t.Errorf("guide: no %q", want)
		}
	}
	if strings.Contains(out, "Find secrets.") {
		t.Error("the guide quotes an inline definition")
	}
	f.cfg.Network = true
	if out, _ := call(t, mainc, "guide", nil); !strings.Contains(out, "the user accepts each run): allowed;") {
		t.Error("network allowed isn't said")
	}
	tmpl, _ := call(t, mainc, "ask_template", nil)
	b, err := os.ReadFile(filepath.Join(work, "michael", "devops", "ask-template.yml"))
	if err != nil || tmpl != string(b) || !strings.Contains(tmpl, "kind: AskTemplate") {
		t.Errorf("ask_template: %v %q", err, tmpl)
	}
	if _, msg := call(t, mainc, "guide", map[string]any{"run": "x"}); !strings.Contains(msg, `unknown field "run"`) {
		t.Errorf("an unknown argument: %q", msg)
	}
}

// TestSubmit: a script is checked at once (size, mode), written for the runner under a name it doesn't read until it
// is complete, the runner kicked, and its outcome given: stored, refused with the rule and the next step, or pending.
func TestSubmit(t *testing.T) {
	work, f, mainc, _ := fixture(t)
	subs := filepath.Join(work, "michael", "devops", "submissions", "main")
	script := "#!/bin/bash\n# AAA script: x\nNETWORK=0\n"
	idRe := regexp.MustCompile(`^[0-9a-f]{16}-42\.sh$`)
	var seen string
	f.take = func(id string) (runner.Submitted, bool) {
		entries, _ := os.ReadDir(subs)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), id) && idRe.MatchString(e.Name()) {
				b, _ := os.ReadFile(filepath.Join(subs, e.Name()))
				seen = string(b)
				return runner.Submitted{Run: "calm-otter-42", Rule: "allow"}, true
			}
		}
		return runner.Submitted{}, false
	}
	out, msg := call(t, mainc, "ask_submit", map[string]any{"script": script, "issue": "42"})
	if msg != "" || !strings.HasPrefix(out, "stored as calm-otter-42 - next: ") || seen != script || f.kicks != 1 {
		t.Errorf("stored: %q %q, the runner read %q, %d kicks", out, msg, seen, f.kicks)
	}
	// Layout is advice: a flat loop is stored all the same, with a note after the reply.
	out, msg = call(t, mainc, "ask_submit", map[string]any{"script": script + "for p in a b; do\nsay x\ndone\n", "issue": "42"})
	if msg != "" || !strings.HasPrefix(out, "stored as calm-otter-42 - next: ") || !strings.Contains(out, "\n- layout warning (not a finding;") ||
		!strings.HasSuffix(out, "\nline 5: expected 2 spaces, found 0") {
		t.Errorf("stored with a layout warning: %q %q", out, msg)
	}
	// A plinthctl call in a script that isn't privileged fails in its container: advice too, stored all the same.
	out, msg = call(t, mainc, "ask_submit", map[string]any{"script": script + "PRIVILEGED=0\n" + asklint.PlinthLibLine + "\nplinthctl status\n", "issue": "42"})
	if msg != "" || !strings.HasPrefix(out, "stored as calm-otter-42 - next: ") || !strings.Contains(out, "\n- plinthctl warning (not a finding;") ||
		!strings.HasSuffix(out, "plinthctl is not available in the run's container (PRIVILEGED=0), so this call fails") {
		t.Errorf("stored with a plinthctl warning: %q %q", out, msg)
	}
	f.take = func(string) (runner.Submitted, bool) {
		return runner.Submitted{Rule: "ask.lint", Reason: "ask lint --plinth: line 2: no header"}, true
	}
	if _, msg := call(t, mainc, "ask_submit", map[string]any{"script": script}); msg !=
		"refused by rule ask.lint: ask lint --plinth: line 2: no header - next: fix what it found and submit the script again (ask_submit); guide says what a script must be" {
		t.Errorf("refused by the runner: %q", msg)
	}
	f.take = nil
	out, _ = call(t, mainc, "ask_submit", map[string]any{"script": script})
	m := regexp.MustCompile(`^pending ([0-9a-f]{16}): `).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("pending: %q", out)
	}
	if status, _ := call(t, mainc, "ask_status", nil); !strings.Contains(status, "pending "+m[1]+" - next: wait") {
		t.Errorf("ask_status of a pending submission: %q", status)
	}
	// Refused at once, nothing written: too many lines, a mode the namespace doesn't allow, a bad issue.
	before, _ := os.ReadDir(subs)
	for args, want := range map[string]string{
		strings.Repeat("echo\n", 501):                "refused by rule ask.size: the script has 501 lines, at most 500",
		"#!/bin/bash\nNETWORK=1\n":                   "refused by rule ask.mode: the script asks for network mode",
		"#!/bin/bash\nPRIVILEGED=1\nINTERACTIVE=0\n": "refused by rule ask.mode: the script asks for privileged mode",
	} {
		if _, msg := call(t, mainc, "ask_submit", map[string]any{"script": args}); !strings.HasPrefix(msg, want) {
			t.Errorf("%q: %q, want %q", args[:12], msg, want)
		}
	}
	if _, msg := call(t, mainc, "ask_submit", map[string]any{"script": script, "issue": "4a"}); !strings.Contains(msg, `issue "4a"`) {
		t.Errorf("a bad issue: %q", msg)
	}
	if _, msg := call(t, mainc, "ask_submit", map[string]any{"script": strings.Repeat("x", maxScript+1)}); !strings.Contains(msg, "at most 1048576") {
		t.Errorf("too large: %q", msg)
	}
	if after, _ := os.ReadDir(subs); len(after) != len(before) {
		t.Errorf("a refused script was written: %d -> %d files", len(before), len(after))
	}
	if ev := strings.Join(f.recorded(), "\n"); strings.Count(ev, "main ask.submit main deny ") != 3 {
		t.Errorf("events:\n%s", ev)
	}
}

// TestSubmitIssue: ask_submit's issue is a number (an issue of the namespace's repo), owner/repo#N or a page on this
// Forgejo. It goes into the script as ISSUE="<page>" before the library line; the RUN_NAME ends in the number only for
// an issue of the namespace's repo. Another host, a malformed value, or a script that says another ISSUE is refused.
func TestSubmitIssue(t *testing.T) {
	work, f, mainc, _ := fixture(t)
	subs := filepath.Join(work, "michael", "devops", "submissions", "main")
	script := "#!/bin/bash\n# AAA script: x\nNETWORK=0\n" + asklint.PlinthLibLine + "\n"
	var name, seen string
	f.take = func(id string) (runner.Submitted, bool) {
		entries, _ := os.ReadDir(subs)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), id) {
				b, _ := os.ReadFile(filepath.Join(subs, e.Name()))
				name, seen = e.Name(), string(b)
				return runner.Submitted{Run: "calm-otter", Rule: "allow"}, true
			}
		}
		return runner.Submitted{}, false
	}
	const other, own = "https://forgejo.example.org/michael/plinth/issues/80", "https://forgejo.example.org/michael/devops/issues/42"
	for _, c := range []struct{ issue, file, page string }{
		{"michael/plinth#80", `^[0-9a-f]{16}\.sh$`, other},
		{other, `^[0-9a-f]{16}\.sh$`, other},
		{"michael/devops#42", `^[0-9a-f]{16}-42\.sh$`, own},
		{"42", `^[0-9a-f]{16}-42\.sh$`, own},
	} {
		name, seen = "", ""
		out, msg := call(t, mainc, "ask_submit", map[string]any{"script": script, "issue": c.issue})
		if msg != "" || !strings.HasPrefix(out, "stored as ") || !regexp.MustCompile(c.file).MatchString(name) ||
			!strings.Contains(seen, "ISSUE=\""+c.page+"\"\n"+asklint.PlinthLibLine) {
			t.Errorf("issue %q: %q %q, stored as %q:\n%s", c.issue, out, msg, name, seen)
		}
	}
	for _, bad := range []string{"https://evil.example.org/michael/plinth/issues/80", "michael/plinth", "80a", "michael/plinth#8x", "http://forgejo.example.org/michael/plinth/issues/80"} {
		if _, msg := call(t, mainc, "ask_submit", map[string]any{"script": script, "issue": bad}); !strings.Contains(msg, "issue "+strconv.Quote(bad)) {
			t.Errorf("issue %q: %q", bad, msg)
		}
	}
	says := strings.Replace(script, asklint.PlinthLibLine, `ISSUE="https://forgejo.example.org/michael/plinth/issues/1"`+"\n"+asklint.PlinthLibLine, 1)
	if _, msg := call(t, mainc, "ask_submit", map[string]any{"script": says, "issue": "michael/plinth#80"}); !strings.Contains(msg, "the script says ISSUE=") {
		t.Errorf("a script that says another ISSUE: %q", msg)
	}
	name, seen = "", ""
	if out, msg := call(t, mainc, "ask_submit", map[string]any{"script": says, "issue": "michael/plinth#1"}); msg != "" || !strings.HasPrefix(out, "stored as ") || seen != says {
		t.Errorf("a script that says the same ISSUE: %q %q, stored %q", out, msg, seen)
	}
}

// TestStatusAndAnswer: ask_status lists the agent's own runs with their states and the next step, and its refused
// submissions; answer_read gives the released answer only, records the read, and otherwise says the run's state.
func TestStatusAndAnswer(t *testing.T) {
	work, f, mainc, _ := fixture(t)
	runs := filepath.Join(work, "michael", "devops", "runs")
	ok := "## Verdict\nOK to run\n"
	write(t, filepath.Join(runs, "main", "calm-otter", "ask.sh"), "x")
	write(t, filepath.Join(runs, "main", "calm-otter", "review.security.md"), ok)
	write(t, filepath.Join(runs, "main", "keen-owl", "ask.sh"), "x")
	write(t, filepath.Join(runs, "main", "keen-owl", "review.security.md"), ok)
	write(t, filepath.Join(runs, "main", "keen-owl", "answer.FAIL.released.yml"), "apiVersion: plinth/v1\nkind: Answer\n")
	write(t, filepath.Join(runs, "main", "bold-bison", "ask.sh"), "x")
	write(t, filepath.Join(runs, "main", "bold-bison", "review.security.md"), ok)
	write(t, filepath.Join(runs, "main", "bold-bison", "answer.dlp-ok.yml"), "secret")
	write(t, filepath.Join(work, "michael", "devops", "requests", "main", "keen-owl", "exec.finished"), "")
	write(t, filepath.Join(work, "michael", "devops", "requests", "main", "bold-bison", "exec.finished"), "")
	write(t, filepath.Join(runs, "ci", "lone-yew", "ask.sh"), "x")
	write(t, filepath.Join(work, "michael", "devops", "rejected", "main", "0123456789abcdef", "reason.md"),
		"# Rejected submission 0123456789abcdef\n\nRule: ask.lint\n\nReason: ask lint --plinth: no header\n\nPolicy: x\n")

	status, _ := call(t, mainc, "ask_status", nil)
	for _, want := range []string{
		"calm-otter  ask: approved  answer: - - next: wait: the user runs it when they choose",
		"keen-owl  ask: ran  answer: released, gate - next: answer_read keen-owl",
		"bold-bison  ask: ran  answer: dlp-ok - next: wait: the user releases the answer, then answer_read",
		"refused 0123456789abcdef (", ") by rule ask.lint: ask lint --plinth: no header - next: fix what it found",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("ask_status: no %q in\n%s", want, status)
		}
	}
	if strings.Contains(status, "lone-yew") || strings.Contains(status, "secret") {
		t.Errorf("ask_status shows another agent's run or content:\n%s", status)
	}
	if one, _ := call(t, mainc, "ask_status", map[string]any{"run": "keen-owl"}); one != "keen-owl  ask: ran  answer: released, gate - next: answer_read keen-owl" {
		t.Errorf("ask_status of one run: %q", one)
	}
	if _, msg := call(t, mainc, "ask_status", map[string]any{"run": "lone-yew"}); !strings.Contains(msg, "you have no run lone-yew") {
		t.Errorf("another agent's run: %q", msg)
	}

	if out, _ := call(t, mainc, "answer_read", map[string]any{"run": "keen-owl"}); out != "apiVersion: plinth/v1\nkind: Answer\n" {
		t.Errorf("answer_read of a released answer: %q", out)
	}
	if out, msg := call(t, mainc, "answer_read", map[string]any{"run": "bold-bison"}); msg != "" || strings.Contains(out, "secret") ||
		!strings.HasPrefix(out, "not released yet: bold-bison  ask: ran  answer: dlp-ok") {
		t.Errorf("answer_read before Done: %q %q", out, msg)
	}
	if _, msg := call(t, mainc, "answer_read", map[string]any{"run": "lone-yew"}); !strings.Contains(msg, "refused by rule state.valid: run lone-yew does not exist") {
		t.Errorf("answer_read of another agent's run: %q", msg)
	}
	if _, msg := call(t, mainc, "answer_read", map[string]any{"run": "../x"}); !strings.Contains(msg, "is not a RUN_NAME") {
		t.Errorf("answer_read of a bad name: %q", msg)
	}
	if err := os.Remove(filepath.Join(runs, "main", "keen-owl", "answer.FAIL.released.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(runs, "main", "bold-bison", "answer.dlp-ok.yml"), filepath.Join(runs, "main", "keen-owl", "answer.released.yml")); err != nil {
		t.Fatal(err)
	}
	if out, msg := call(t, mainc, "answer_read", map[string]any{"run": "keen-owl"}); strings.Contains(out, "secret") || !strings.Contains(msg, "symlink") {
		t.Errorf("answer_read through a symlink: %q %q", out, msg)
	}
	ev := strings.Join(f.recorded(), "\n")
	if !strings.Contains(ev, "main answer.read main/keen-owl allow allow") || strings.Contains(ev, "bold-bison") ||
		!strings.Contains(ev, "main answer.read main/lone-yew deny state.valid") {
		t.Errorf("events:\n%s", ev)
	}
}

// TestForgedRelease: a script writes answer.released.yml into its own run directory. Only the controller's exec
// request, which the script can't reach, says the run was started and its exec ended: while the exec is unfinished, or
// when no exec request exists, ask_status doesn't call the answer released and answer_read gives nothing.
func TestForgedRelease(t *testing.T) {
	work, _, mainc, _ := fixture(t)
	runs := filepath.Join(work, "michael", "devops", "runs")
	reqs := filepath.Join(work, "michael", "devops", "requests")
	ok := "## Verdict\nOK to run\n"
	for _, run := range []string{"proud-otter", "brave-lynx"} {
		write(t, filepath.Join(runs, "main", run, "ask.sh"), "x")
		write(t, filepath.Join(runs, "main", run, "review.security.md"), ok)
		write(t, filepath.Join(runs, "main", run, "running.yml"), "")
		write(t, filepath.Join(runs, "main", run, "answer.released.yml"), "secret")
	}
	write(t, filepath.Join(reqs, "main", "proud-otter", "exec.running"), "") // the exec runs, or a restart left it unfinished
	for _, run := range []string{"proud-otter", "brave-lynx"} {
		status, _ := call(t, mainc, "ask_status", nil)
		if strings.Contains(status, run+"  ask: ran  answer: released") || strings.Contains(status, "answer_read "+run) {
			t.Errorf("ask_status of the forged %s:\n%s", run, status)
		}
		if out, msg := call(t, mainc, "answer_read", map[string]any{"run": run}); strings.Contains(out, "secret") || strings.Contains(msg, "secret") ||
			!strings.HasPrefix(out, "not released yet: "+run) {
			t.Errorf("answer_read of the forged %s: %q %q", run, out, msg)
		}
	}
	// The exec ended: the controller moved the forged file out (moveUnexpected) and Done released the real answer.
	if err := os.Rename(filepath.Join(reqs, "main", "proud-otter", "exec.running"), filepath.Join(reqs, "main", "proud-otter", "exec.finished")); err != nil {
		t.Fatal(err)
	}
	if out, _ := call(t, mainc, "answer_read", map[string]any{"run": "proud-otter"}); out != "secret" {
		t.Errorf("answer_read after the exec ended: %q", out)
	}
}

// TestNotServed: a namespace the controller doesn't serve now refuses every call, with the next step.
func TestNotServed(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	write(t, filepath.Join(work, config.FileName), globalCfg)
	tok, _, err := agents.Register(work, ns, "main", "/r")
	if err != nil {
		t.Fatal(err)
	}
	g, err := config.LoadGlobal(work, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{Work: work, Global: g, Namespace: func(names.Namespace) Namespace { return nil }}).Handler())
	t.Cleanup(srv.Close)
	c := &mcp.Client{HTTP: srv.Client(), URL: srv.URL + Path, Token: tok}
	if _, err := c.Call(context.Background(), "ask_status", nil); err == nil || !strings.Contains(err.Error(), "doesn't serve your namespace michael/devops right now") {
		t.Errorf("not served: %v", err)
	}
}

// TestPlainAAA: an agent's plain AAA runs, in its registered repo's ai/work. aaa_new creates ai/work and a run
// from the plain template; aaa_lint checks its ask.sh as ask lint does; aaa_status lists the runs there and in done/
// with their states; aaa_read gives only a scanned answer, otherwise its state with read-answer's exit code; a
// symlinked run, answer or ai/work leads nowhere.
func TestPlainAAA(t *testing.T) {
	work, f, mainc, cic := fixture(t)
	repo := t.TempDir()
	tok, _, err := agents.Register(work, ns, "dev", repo)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Enabled["dev"] = repo
	dev := &mcp.Client{HTTP: mainc.HTTP, URL: mainc.URL, Token: tok}
	aw := filepath.Join(repo, "ai", "work")

	// No ai/work yet.
	if out, msg := call(t, dev, "aaa_status", nil); msg != "" || out != "no runs in ai/work yet - next: aaa_new starts one (guide explains)" {
		t.Errorf("no ai/work: %q, %q", out, msg)
	}
	if out, msg := call(t, dev, "aaa_read", map[string]any{"run": "calm-otter"}); msg != "" || !strings.Contains(out, "no such run (your repo has no ai/work yet)") {
		t.Errorf("no ai/work, aaa_read: %q, %q", out, msg)
	}

	// aaa_new, with and without an issue.
	created := regexp.MustCompile(`^created ai/work/([a-z]+-[a-z]+(-42)?)/ask\.sh in your repo \(RUN_NAME ([a-z-0-9]+)\) - next: `)
	newRun := func(args map[string]any) string {
		t.Helper()
		out, msg := call(t, dev, "aaa_new", args)
		m := created.FindStringSubmatch(out)
		if msg != "" || m == nil || m[1] != m[3] {
			t.Fatalf("aaa_new %v: %q, %q", args, out, msg)
		}
		return m[1]
	}
	plainRun := newRun(nil)
	issueRun := newRun(map[string]any{"issue": "42"})
	if !strings.HasSuffix(issueRun, "-42") {
		t.Errorf("with issue 42: %s", issueRun)
	}
	b, err := os.ReadFile(filepath.Join(aw, plainRun, "ask.sh"))
	if fi, _ := os.Stat(filepath.Join(aw, plainRun, "ask.sh")); err != nil || fi.Mode().Perm() != 0o700 ||
		!strings.Contains(string(b), `RUN_NAME="`+plainRun+`"`) || !strings.Contains(string(b), "source <(plinthctl ask lib)") {
		t.Errorf("ask.sh: %v, %q", err, b)
	}
	if _, msg := call(t, dev, "aaa_new", map[string]any{"issue": "4a"}); !strings.Contains(msg, `issue "4a"`) {
		t.Errorf("issue 4a: %q", msg)
	}
	if got := f.recorded(); len(got) != 2 || got[0] != "dev aaa.new dev/ai/work/"+plainRun+" allow allow" {
		t.Errorf("events: %q", got)
	}

	// aaa_lint: the template as aaa_new wrote it still holds its placeholders; filled in, it passes.
	if out, msg := call(t, dev, "aaa_lint", map[string]any{"run": plainRun}); msg != "" || !strings.Contains(out, "finding(s):\n") ||
		!strings.Contains(out, "no header: line 2 must be") || !strings.HasSuffix(out, "- next: fix them in ai/work/"+plainRun+"/ask.sh, then aaa_lint "+plainRun+" again") {
		t.Errorf("aaa_lint of the template: %q, %q", out, msg)
	}
	filled := strings.NewReplacer("<one line: what this round is for>", "disks", "   (or: CHANGES: <exact list>)", "",
		"#   1 <section>  2 <section>  3 <section>", "#   1 disks", `"1. <section>"`, `"1. disks"`).Replace(string(b))
	write(t, filepath.Join(aw, plainRun, "ask.sh"), filled)
	if out, msg := call(t, dev, "aaa_lint", map[string]any{"run": plainRun}); msg != "" || !strings.HasPrefix(out, "ai/work/"+plainRun+"/ask.sh passes ask lint - next: ") {
		t.Errorf("aaa_lint of a filled-in script: %q, %q", out, msg)
	}
	// A layout warning is advice: the script passes, and the reply says what is off.
	write(t, filepath.Join(aw, plainRun, "ask.sh"), filled+"for p in a b; do\nsay \"$p\"\ndone\n")
	if out, msg := call(t, dev, "aaa_lint", map[string]any{"run": plainRun}); msg != "" || !strings.HasPrefix(out, "ai/work/"+plainRun+"/ask.sh passes ask lint - next: ") ||
		!strings.Contains(out, "\n- layout warning (not a finding;") || !strings.Contains(out, "expected 2 spaces, found 0") {
		t.Errorf("aaa_lint of a flat loop: %q, %q", out, msg)
	}
	if _, msg := call(t, dev, "aaa_lint", map[string]any{"run": "no-such"}); !strings.Contains(msg, "no ai/work/no-such/ask.sh in your repo") {
		t.Errorf("aaa_lint of no run: %q", msg)
	}
	if len(f.recorded()) != 2 {
		t.Errorf("aaa_lint is recorded: %q", f.recorded())
	}

	// Every state.
	answer := func(run string) string {
		a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: run},
			Sections: []doc.Section{{Title: "1\n", Output: "found\n"}}, Result: doc.ResultFail, Failure: "gate\n",
			Status: &doc.AnswerStatus{DLP: &doc.DLP{Result: doc.ResultOK}}}
		return string(a.Encode())
	}
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.yml"), answer("lone-yew"))
	write(t, filepath.Join(outside, "sly-cat", "answer.dlp-ok.yml"), answer("sly-cat"))
	for _, file := range []struct{ name, body string }{
		{issueRun + "/running.yml", "secret"},
		{"bold-bison/answer.new.yml", "secret"},
		{"keen-owl/answer.DLP-FAIL.yml", "secret"},
		{"tidy-wren/answer.INVALID.yml", "secret"},
		{"done/warm-fox/answer.FAIL.dlp-ok.yml", answer("warm-fox")},
		{"lone-yew/ask.sh", "x"},
	} {
		write(t, filepath.Join(aw, file.name), file.body)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.yml"), filepath.Join(aw, "lone-yew", "answer.dlp-ok.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "sly-cat"), filepath.Join(aw, "sly-cat")); err != nil {
		t.Fatal(err)
	}
	out, msg := call(t, dev, "aaa_status", nil)
	for _, want := range []string{
		plainRun + "  state: not-run - next: write ai/work/" + plainRun + "/ask.sh if you haven't",
		issueRun + "  state: running - next: wait until the user says \"" + issueRun + " done\"",
		"bold-bison  state: new - next: ask the user to run plinthctl finish bold-bison",
		"keen-owl  state: DLP-FAIL - next: tell the user",
		"tidy-wren  state: INVALID - next: tell the user",
		"warm-fox  state: dlp-ok, gate, in done/ - next: aaa_read warm-fox",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("aaa_status: no %q in\n%s", want, out)
		}
	}
	if msg != "" || strings.Contains(out, "secret") || strings.Contains(out, "sly-cat") {
		t.Errorf("aaa_status: %q, %q", out, msg)
	}
	if out, _ := call(t, dev, "aaa_status", map[string]any{"run": "bold-bison"}); strings.Count(out, "\n") != 0 || !strings.HasPrefix(out, "bold-bison  state: new") {
		t.Errorf("aaa_status of one run: %q", out)
	}
	if _, msg := call(t, dev, "aaa_status", map[string]any{"run": "no-such"}); !strings.Contains(msg, "no run no-such in ai/work") {
		t.Errorf("aaa_status of no run: %q", msg)
	}
	for _, c := range []struct {
		run, want string
	}{
		{plainRun, "not run yet (no answer in " + plainRun + ") - next: ask the user to run it (from ai/work: ./" + plainRun + "/ask.sh), then aaa_read again"},
		{issueRun, "still running (running.yml exists) - not read; next: wait until the user says \"" + issueRun + " done\", then aaa_read again; if it was killed, a rerun is a new run (aaa_new) (read-answer's exit code 10)"},
		{"bold-bison", "finished, not scanned yet - not read; next: ask the user to run plinthctl finish bold-bison"},
		{"keen-owl", "DLP scan FAILED - not read; next: tell the user, who reviews the answer and decides (nothing here overrides it) (read-answer's exit code 11)"},
		{"tidy-wren", "INVALID (not a valid document: a bug, never data) - not read; next: tell the user; a rerun is a new run (aaa_new) (read-answer's exit code 14)"},
		{"sly-cat", "no such run - next: check the RUN_NAME (aaa_new printed it), or ask the user whether it ran (read-answer's exit code 13)"},
	} {
		if out, msg := call(t, dev, "aaa_read", map[string]any{"run": c.run}); msg != "" || !strings.Contains(out, c.want) || strings.Contains(out, "secret") {
			t.Errorf("aaa_read %s: %q, %q", c.run, out, msg)
		}
	}
	if out, msg := call(t, dev, "aaa_read", map[string]any{"run": "lone-yew"}); out != "" || !strings.Contains(msg, "symlink") {
		t.Errorf("a symlinked answer: %q, %q", out, msg)
	}
	if out, msg := call(t, dev, "aaa_lint", map[string]any{"run": "sly-cat"}); out != "" || !strings.Contains(msg, "symlink") {
		t.Errorf("aaa_lint of a symlinked run: %q, %q", out, msg)
	}
	if out, msg := call(t, dev, "aaa_read", map[string]any{"run": "warm-fox"}); msg != "" || out != answer("warm-fox") {
		t.Errorf("a scanned answer: %q, %q", out, msg)
	}
	if got := f.recorded(); got[len(got)-1] != "dev aaa.read dev/ai/work/warm-fox allow allow" {
		t.Errorf("the read isn't recorded: %q", got)
	}
	if _, msg := call(t, dev, "aaa_read", map[string]any{"run": "../x"}); !strings.Contains(msg, "is not a RUN_NAME") {
		t.Errorf("../x: %q", msg)
	}
	if _, msg := call(t, dev, "aaa_read", nil); !strings.Contains(msg, "run is required") {
		t.Errorf("no run: %q", msg)
	}

	// A repo whose ai/work is a symlink out of it: nothing is created or read there.
	repo2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo2, "ai"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo2, "ai", "work")); err != nil {
		t.Fatal(err)
	}
	tok2, _, err := agents.Register(work, ns, "ops", repo2)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Enabled["ops"] = repo2
	ops := &mcp.Client{HTTP: mainc.HTTP, URL: mainc.URL, Token: tok2}
	for _, tool := range []string{"aaa_new", "aaa_status"} {
		if _, msg := call(t, ops, tool, nil); !strings.Contains(msg, "ai/work in your repo can't be opened") || !strings.Contains(msg, "symlink") {
			t.Errorf("%s in a symlinked ai/work: %q", tool, msg)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 2 {
		t.Errorf("written through the symlink: %v", entries)
	}

	// Not enabled: refused, and recorded.
	if _, msg := call(t, cic, "aaa_status", nil); !strings.Contains(msg, "refused by rule target.valid") {
		t.Errorf("ci: %q", msg)
	}
	if got := f.recorded(); got[len(got)-1] != "ci aaa.status ci deny target.valid" {
		t.Errorf("ci's refusal: %q", got)
	}
}

// TestPreview: ask_preview shows ask lint, the DLP masks and the reviews of a script and stores nothing, records
// each preview, and stops an agent past plinth.preview.limit per hour.
func TestPreview(t *testing.T) {
	fixtureLimit = 2
	t.Cleanup(func() { fixtureLimit = 0 })
	work, f, mainc, _ := fixture(t)
	f.preview = []runner.PreviewReview{
		{Type: "security", Verdict: "Run after fixes", Findings: []string{"line 4 (high): curl | sh"}},
		{Type: "dlp", Err: errors.New("Ollama isn't configured")},
	}
	script := "#!/bin/bash\n# AAA script: x\nNETWORK=0\npassword=hunter22xx\n"
	out, msg := call(t, mainc, "ask_preview", map[string]any{"script": script})
	if msg != "" {
		t.Fatalf("preview: %q", msg)
	}
	for _, want := range []string{"nothing was stored, nothing runs", "- ask lint: ", "- DLP masks: 1 value(s) look like a secret",
		"- review security: Run after fixes, 1 finding(s):\n  - line 4 (high): curl | sh\n", "- review dlp: no review (Ollama isn't configured)\n",
		"remember: record in ai/memory/plinth-review-failures.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if len(f.seen) != 1 || strings.Contains(string(f.seen[0]), "hunter22xx") || !strings.Contains(string(f.seen[0]), "password=<masked>") {
		t.Errorf("the reviews were given %q, want the script with its secret masked", f.seen)
	}
	if ev := f.recorded(); len(ev) != 1 || ev[0] != "main ask.preview main allow allow" {
		t.Errorf("events: %q", ev)
	}
	if _, err := os.Stat(filepath.Join(work, "michael", "devops", "submissions", "main")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(work, "michael", "devops", "submissions", "main"))
		if len(entries) != 0 {
			t.Errorf("a preview stored %d file(s)", len(entries))
		}
	}
	// The limit is 2 here: the second preview passes, the third is refused and recorded, and nothing reaches the reviews.
	if _, msg := call(t, mainc, "ask_preview", map[string]any{"script": script}); msg != "" {
		t.Fatalf("the second preview: %q", msg)
	}
	if _, msg := call(t, mainc, "ask_preview", map[string]any{"script": script}); !strings.HasPrefix(msg, "at most 2 previews per hour (plinth.preview.limit); the next is free in 60 minutes - next: don't retry in a loop;") {
		t.Errorf("the third: %q", msg)
	}
	if ev := f.recorded(); len(f.seen) != 2 || ev[len(ev)-1] != "main ask.preview main deny ask.preview.limit" {
		t.Errorf("%d reviews, events %q", len(f.seen), ev)
	}
	if _, msg := call(t, mainc, "ask_preview", map[string]any{"script": strings.Repeat("echo\n", 501)}); !strings.Contains(msg, "ask.size") {
		t.Errorf("a script over the size limit: %q", msg)
	}
}

// TestPreviewWindow: previews older than an hour don't count, and a refusal says when the oldest one leaves the hour.
func TestPreviewWindow(t *testing.T) {
	g := &config.Global{}
	g.Plinth.Preview.Limit = 2
	s := &Server{Global: g}
	now := time.Now()
	for i, c := range []struct {
		at   time.Duration
		ok   bool
		wait time.Duration
	}{{0, true, 0}, {time.Minute, true, 0}, {2 * time.Minute, false, 58 * time.Minute}, {61 * time.Minute, true, 0}} {
		if ok, wait := s.allowPreview("main", now.Add(c.at)); ok != c.ok || wait != c.wait {
			t.Errorf("call %d at +%v: %v, wait %v, want %v, %v", i, c.at, ok, wait, c.ok, c.wait)
		}
	}
	if ok, _ := s.allowPreview("ci", now); !ok {
		t.Error("another agent's previews count")
	}
	for d, want := range map[time.Duration]string{time.Second: "1 minute", time.Minute: "1 minute", time.Minute + time.Second: "2 minutes", 58 * time.Minute: "58 minutes"} {
		if got := minutes(d); got != want {
			t.Errorf("minutes(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestPreviewNeedsOllama: without ollama.host a preview is refused and recorded, and no review is tried.
func TestPreviewNeedsOllama(t *testing.T) {
	fixtureOllama = ""
	t.Cleanup(func() { fixtureOllama = "http://ollama.example:11434" })
	_, f, mainc, _ := fixture(t)
	_, msg := call(t, mainc, "ask_preview", map[string]any{"script": "#!/bin/bash\n"})
	if !strings.HasPrefix(msg, "refused by rule ask.preview.ollama: no review can be made: ollama.host is not set - next: ") {
		t.Errorf("without Ollama: %q", msg)
	}
	if ev := f.recorded(); len(ev) != 1 || ev[0] != "main ask.preview main deny ask.preview.ollama" || len(f.seen) != 0 {
		t.Errorf("events %q, %d reviews", ev, len(f.seen))
	}
}

// TestPreviewPending: reviews that outlast the wait answer pending <id>; asking with the id gives pending again until
// they are done and then the result, as often as asked. A preview is the agent's own, and one id comes with one script.
func TestPreviewPending(t *testing.T) {
	_, f, mainc, _ := fixture(t)
	f.gate = make(chan struct{})
	f.preview = []runner.PreviewReview{{Type: "security", Verdict: "OK to run"}}
	script := "#!/bin/bash\n# AAA script: x\nNETWORK=0\n"
	out, msg := call(t, mainc, "ask_preview", map[string]any{"script": script})
	id := ""
	if p := strings.Fields(out); len(p) > 1 {
		id = p[1]
	}
	if msg != "" || !strings.HasPrefix(out, "pending ") || !strings.Contains(out, "call ask_preview with id "+id) || !strings.HasPrefix(id, "pv-") || len(id) != 11 {
		t.Fatalf("slow reviews: %q %q", out, msg)
	}
	if ev := f.recorded(); len(ev) != 0 {
		t.Errorf("recorded before the reviews were done: %q", ev)
	}
	if out, msg := call(t, mainc, "ask_preview", map[string]any{"id": id}); msg != "" || !strings.HasPrefix(out, "pending "+id+" ") {
		t.Errorf("still running: %q %q", out, msg)
	}
	close(f.gate)
	for range 2 { // the result stays
		out, msg = call(t, mainc, "ask_preview", map[string]any{"id": id})
		if msg != "" || !strings.Contains(out, "- review security: OK to run, no findings\n") {
			t.Errorf("done: %q %q", out, msg)
		}
	}
	if ev := f.recorded(); len(ev) != 1 || ev[0] != "main ask.preview main allow allow" {
		t.Errorf("events: %q", ev)
	}
	if _, msg := call(t, mainc, "ask_preview", map[string]any{"id": "pv-00000000"}); !strings.HasPrefix(msg, `no preview "pv-00000000" of yours`) {
		t.Errorf("an unknown id: %q", msg)
	}
	if _, msg := call(t, mainc, "ask_preview", map[string]any{"id": id, "script": script}); !strings.Contains(msg, "not both") {
		t.Errorf("both: %q", msg)
	}
}
