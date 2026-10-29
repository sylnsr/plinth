// Package agentmcp is the Plinth MCP server: the agents' way into Plinth. An agent reads the guide and the ask
// template, submits ask scripts, follows its own runs and reads their answers once the user has released them. Plain
// AAA goes through it too: an agent without plinthctl starts runs in its own repo's ai/work, follows them and
// reads their answers once scanned. Every call carries the agent's token, which alone decides the agent and its
// namespace; every tool asks decide first. It runs in the controller's process and only queues: a submission is a file
// the namespace's runner takes in (Listeners only queue). Spec: plinthctl.README.md, "Plinth MCP server"; the agent's
// setup: docs/agent-mcp.md.
package agentmcp

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"plinth/internal/agents"
	"plinth/internal/asklint"
	"plinth/internal/auth"
	"plinth/internal/config"
	"plinth/internal/decide"
	"plinth/internal/doc"
	"plinth/internal/events"
	"plinth/internal/logfile"
	"plinth/internal/mcp"
	"plinth/internal/names"
	"plinth/internal/plain"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
	"plinth/internal/workdir"
)

// Path is the MCP endpoint: http://<host>:<plinth.mcp.port>/mcp.
const Path = "/mcp"

// maxScript is the largest script ask_submit takes, as the runner reads it.
const maxScript = 1 << 20

// submitWait is how long ask_submit waits for the runner to take a submission in.
const submitWait = 30 * time.Second

// refusedFor is how long ask_status lists a refused submission.
const refusedFor = 24 * time.Hour

// instructions are the initialize result's: what a client tells its model about the server as a whole.
const instructions = "Plinth runs shell scripts you write, once the user approves them, and gives you their answers. " +
	"Call guide first if you don't know Plinth: it explains the workflow, the tools and how to write an ask script. " +
	"Preview a script with ask_preview before you submit it."

// seeGuide ends every tool's description.
const seeGuide = " Call guide first if you don't know Plinth."

// Namespace is what the server needs of an attached namespace: its runner (package runner).
type Namespace interface {
	Config() *config.Namespace
	Live() runstate.Live
	Record(caller, action, target, outcome, rule, reason string) (events.Receipt, error)
	Kick()
	Submitted(ctx context.Context, agent, id string) (runner.Submitted, bool)
	Preview(ctx context.Context, script []byte) []runner.PreviewReview
}

// Server is the Plinth MCP server of one controller.
type Server struct {
	Work    string         // absolute
	Global  *config.Global // as the controller loaded it
	Version string         // plinthctl's, for serverInfo

	// Namespace is the runner of an attached namespace, nil when the controller doesn't serve it now (its config doesn't
	// load, or it is being reloaded).
	Namespace func(ns names.Namespace) Namespace

	// Log gets one line per call and per refused token, never a token, an argument or a result.
	Log *logfile.Log

	// SubmitWait is how long ask_submit waits for the runner; zero for 30 s (tests shorten it).
	SubmitWait time.Duration

	// PreviewWait is how long ask_preview waits for the reviews before it answers pending; zero for 30 s (tests shorten it).
	PreviewWait time.Duration

	// previews are the times of each agent's ask_preview calls in the last hour (preview.go).
	previewMu   sync.Mutex
	previews    map[string][]time.Time
	previewJobs map[string]*previewJob // <agent>/<id> -> its preview, for an hour
}

// caller is the agent a request's token belongs to.
type caller struct {
	ns    names.Namespace
	agent string
}

type callerKey struct{}

var (
	issueRe          = regexp.MustCompile(`^[0-9]{1,9}$`)
	ownerRepoIssueRe = regexp.MustCompile(`^([^/\s#]+/[^/\s#]+)#([0-9]{1,9})$`)
	submittedRe      = regexp.MustCompile(`^([0-9a-f]{16})(-[0-9]{1,9})?\.sh$`)
	idRe             = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// Handler serves Path: the token check, then the MCP server with its tools: six for the namespace's runs, four for
// plain AAA in the agent's repo.
func (s *Server) Handler() http.Handler {
	m := &mcp.Server{Name: "plinth", Version: s.Version, Instructions: instructions, Log: s.Log, Tools: []mcp.Tool{
		{Name: "guide", ReadOnly: true, Schema: []byte(noArgs), Call: s.guide,
			Description: "Start here: what Plinth and its AAA workflow are, what each tool is for, how to write an ask script, " +
				"what the user does between your calls, and the policies your scripts are reviewed against (markdown)."},
		{Name: "ask_template", ReadOnly: true, Schema: []byte(noArgs), Call: s.askTemplate,
			Description: "Get the ask template: an AskTemplate YAML document whose spec.script is the script every ask starts " +
				"from. Keep its header, mode lines and library line as they are; write your steps after them, then check the script " +
				"with ask_preview before you ask_submit it." + seeGuide},
		{Name: "ask_submit", Schema: []byte(submitArgs), Call: s.askSubmit,
			Description: "Submit an ask script for the user to review and run. Returns its RUN_NAME once Plinth has checked " +
				"and stored it, or the rule that refused it and what to fix. Nothing runs until the user starts it. Without " +
				"a result within 30 s: pending <id>, which ask_status shows. Run ask_preview on the script first and submit it only " +
				"once the preview has nothing left to fix." + seeGuide},
		{Name: "ask_preview", Schema: []byte(previewArgs), Call: s.askPreview,
			Description: "Check an ask script before you submit it: ask lint, the DLP masks and the namespace's automatic security reviews, " +
				"as findings to fix. Nothing is stored and nothing runs; limited per hour. The reviews take a model run each: if it " +
				"answers pending <id>, call it again with that id (and no script) for the result. Use it to submit scripts that pass." + seeGuide},
		{Name: "ask_status", ReadOnly: true, Schema: []byte(runArg(false)), Call: s.askStatus,
			Description: "Your runs and their state, with what to do next: ask status (reviewing, approved, ran, ...) and " +
				"answer status (dlp-ok, released, ...); pending and refused submissions too. Names and states only, never " +
				"content. Give run for one run." + seeGuide},
		{Name: "answer_read", ReadOnly: true, Schema: []byte(runArg(true)), Call: s.answerRead,
			Description: "Read a run's answer once the user has released it: the Answer YAML document. Before that, one line " +
				"with the run's state and what to do next. Ask as often as you like." + seeGuide},
		{Name: "aaa_new", Schema: []byte(newArgs), Call: s.aaaNew,
			Description: "Plain AAA, the informal way: start a run in your own repo, ai/work/<RUN_NAME>/ask.sh from the plain " +
				"template, for you to write; the user runs it in their terminal. Returns the RUN_NAME and the path in your " +
				"repo. Use it only when the user asks for plain AAA; otherwise ask_submit." + seeGuide},
		{Name: "aaa_read", ReadOnly: true, Schema: []byte(runArg(true)), Call: s.aaaRead,
			Description: "Plain AAA: read the answer of a run in your repo's ai/work once the user has finished it and its DLP " +
				"scan passed: the Answer YAML document. Before that, one line with its state and what to do next." + seeGuide},
		{Name: "aaa_status", ReadOnly: true, Schema: []byte(runArg(false)), Call: s.aaaStatus,
			Description: "Plain AAA: the runs in your repo's ai/work and ai/work/done, each with its state and what to do next. " +
				"Names and states only, never content. Give run for one run." + seeGuide},
		{Name: "aaa_lint", ReadOnly: true, Schema: []byte(runArg(true)), Call: s.aaaLint,
			Description: "Plain AAA: check a run's ai/work/<RUN_NAME>/ask.sh as plinthctl ask lint does, before you ask the user " +
				"to run it: ok, or one line per finding to fix." + seeGuide},
	}}
	mux := http.NewServeMux()
	mux.Handle(Path, s.auth(m))
	return mux
}

const noArgs = `{"type":"object","properties":{},"additionalProperties":false}`

const submitArgs = `{"type":"object","properties":{` +
	`"script":{"type":"string","description":"the whole ask script, from #!/bin/bash on"},` +
	`"issue":{"type":"string","description":"the Forgejo issue the script is for, if any: its page (https://<forgejo>/<owner>/<repo>/issues/<N>), owner/repo#N, or a bare number for the issue of the namespace's repo (then its RUN_NAME ends in -<N>). It goes into the script as ISSUE=\"<page>\" as context: where Plinth posts is not decided by it"}` +
	`},"required":["script"],"additionalProperties":false}`

const newArgs = `{"type":"object","properties":{` +
	`"issue":{"type":"string","pattern":"^[0-9]{1,9}$","description":"the number of the Forgejo issue the run is for, if any: its RUN_NAME ends in -<issue>"}` +
	`},"additionalProperties":false}`

func runArg(required bool) string {
	req := ""
	if required {
		req = `,"required":["run"]`
	}
	return `{"type":"object","properties":{"run":{"type":"string","description":"the RUN_NAME"}}` + req + `,"additionalProperties":false}`
}

// auth lets only a request with a current agent token through, the agent and its namespace in its context. A token
// that matches nothing gets 401 and a message naming nothing.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns, agent, ok, err := agents.ByToken(s.Work, mcp.Bearer(r))
		if err != nil {
			s.Log.Error("checking a token failed", "err", err)
			http.Error(w, "can't check the token now", http.StatusInternalServerError)
			return
		}
		if !ok {
			s.Log.Warn("a request without a valid agent token", "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "an agent token is required (Authorization: Bearer <the token plinthctl register printed>)", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller{ns, agent})))
	})
}

// refusal is a decision other than allow, as the agent gets it: the rule, the reason and what to do next.
type refusal struct {
	d    decide.Decision
	next string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("refused by rule %s: %s - next: %s", r.d.Rule, r.d.Reason, r.next)
}

// turn is one tool call: its agent, the runner of its namespace and the namespace's config.
type turn struct {
	s   *Server
	c   caller
	n   Namespace
	cfg *config.Namespace
}

// begin finds the caller and its namespace's runner: a namespace the controller doesn't serve now refuses every call.
func (s *Server) begin(ctx context.Context) (*turn, error) {
	c, ok := ctx.Value(callerKey{}).(caller)
	if !ok {
		return nil, errors.New("no agent: the request didn't pass the token check")
	}
	var n Namespace
	if s.Namespace != nil {
		n = s.Namespace(c.ns)
	}
	if n == nil {
		return nil, fmt.Errorf("Plinth doesn't serve your namespace %s right now (its config doesn't load, or the controller is "+
			"reloading it) - next: try again in a minute; if it lasts, tell the user (plinthctl config check -n %s)", c.ns, c.ns)
	}
	return &turn{s: s, c: c, n: n, cfg: n.Config()}, nil
}

// decide asks decide about the agent's action (on run, when it names one) with the facts of the namespace, and records
// a refusal in the event log. A run's answer that isn't released yet (answer.unreleased) is its state, which the agent
// polls, so that refusal goes to the log only. It returns the run as loaded (nil without one).
func (k *turn) decide(action, run string, sub *decide.Submission) (*runstate.Run, error) {
	enabled, err := agents.Enabled(k.s.Work, k.c.ns, k.c.agent, k.cfg.Enabled)
	if err != nil {
		return nil, err
	}
	req := decide.Request{Caller: auth.Submission(k.c.agent), NotEnabled: !enabled, Namespace: k.c.ns, Action: action,
		Agent: k.c.agent, RunName: run, Network: k.cfg.Network, Privileged: k.cfg.Privileged,
		ReviewTypes: slices.Sorted(maps.Keys(k.s.Global.Reviewers.Type))}
	if sub != nil {
		req.Submission = *sub
	}
	var r *runstate.Run
	if run != "" {
		if !names.ValidRun(run) {
			return nil, fmt.Errorf("%q is not a RUN_NAME (<adjective>-<noun>[-<issue>]) - next: ask_status lists your runs", run)
		}
		if r, err = runstate.Load(k.s.Work, k.c.ns, k.c.agent, run, k.cfg.Automatic, k.n.Live()); err != nil {
			return nil, err
		}
		req.Run = r.Facts
		if r.Facts.Exists && !r.Facts.Symlink {
			if req.Run.Started, req.Run.Unfinished, err = request.Started(k.s.Work, k.c.ns, k.c.agent, run); err != nil {
				return nil, err
			}
			distrust(r, req.Run.Started, req.Run.Unfinished)
		}
	}
	d := decide.Decide(req)
	if d.Outcome == decide.Allow {
		return r, nil
	}
	target := k.c.agent
	if run != "" {
		target += "/" + run
	}
	if d.Rule == "answer.unreleased" {
		k.s.Log.With("ns", k.c.ns.String(), "agent", k.c.agent, "run", run).Info("refused", "action", action, "reason", d.Reason)
	} else if _, err := k.n.Record(k.c.agent, action, target, string(d.Outcome), d.Rule, d.Reason); err != nil {
		return nil, err
	}
	return r, &refusal{d, nextAfter(d)}
}

// distrust drops a released answer that only the run directory's file names vouch for: a script can write
// answer.released.yml into its own run directory. The answer counts as released once the controller's exec request (out
// of the script's reach) says the script was started and its exec ended. Decide checks the same, so ask_status never
// points the agent at an answer_read that is refused.
func distrust(r *runstate.Run, started, unfinished bool) {
	if r.Facts.Answer == "released" && !(started && !unfinished && !r.Facts.Running) {
		r.Facts.Answer = ""
	}
}

// nextAfter is what an agent does after a refusal.
func nextAfter(d decide.Decision) string {
	switch d.Rule {
	case "target.valid":
		if strings.Contains(d.Reason, "registered") {
			return "tell the user: you must be registered and enabled in this namespace (plinthctl agent enable)"
		}
		return "tell the user what you tried and this reason"
	case "caller.own-runs", "state.valid":
		return "ask_status lists your runs and their states"
	case "ask.lint", "ask.size":
		return "fix what it found and submit the script again (ask_submit); guide says what a script must be"
	case "ask.mode":
		return "write the script without that mode, or ask the user to allow it in the namespace"
	case "answer.unreleased":
		return "wait: ask_status says what the run waits for"
	}
	return "tell the user what you tried and this reason"
}

func (s *Server) guide(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	if err := mcp.DecodeArgs(args, &struct{}{}); err != nil {
		return "", err
	}
	if _, err := k.decide(decide.Guide, "", nil); err != nil {
		return "", err
	}
	return Guide(s.Global, k.c.ns, k.cfg), nil
}

func (s *Server) askTemplate(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	if err := mcp.DecodeArgs(args, &struct{}{}); err != nil {
		return "", err
	}
	if _, err := k.decide(decide.AskTemplate, "", nil); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(s.Work)
	if err != nil {
		return "", err
	}
	defer root.Close()
	b, err := safefs.ReadFile(root, filepath.Join(k.c.ns.Owner, k.c.ns.Repo, workdir.BaseTemplateName), maxScript)
	if err != nil {
		return "", fmt.Errorf("the namespace's ask template can't be read (%v) - next: tell the user", err)
	}
	return string(b), nil
}

func (s *Server) askSubmit(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Script string `json:"script"`
		Issue  string `json:"issue"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	switch {
	case a.Script == "":
		return "", errors.New("arguments: script is required (the whole ask script; ask_template gives the one to start from)")
	case len(a.Script) > maxScript:
		return "", fmt.Errorf("the script is %d bytes, at most %d - next: shorten it; guide says what a script must be", len(a.Script), maxScript)
	}
	suffix, page, err := k.resolveIssue(a.Issue)
	if err != nil {
		return "", err
	}
	script := []byte(a.Script)
	if page != "" {
		if script, err = asklint.WithIssue(script, page); err != nil {
			return "", fmt.Errorf("arguments: issue: %w", err)
		}
	}
	mode := runstate.ModeOf(script)
	if _, err := k.decide(decide.AskSubmit, "", &decide.Submission{Script: script, Network: mode.Network, Privileged: mode.Privileged}); err != nil {
		return "", err
	}
	id, err := k.store(script, suffix)
	if err != nil {
		return "", err
	}
	k.n.Kick()
	wait := s.SubmitWait
	if wait <= 0 {
		wait = submitWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for ctx.Err() == nil {
		n := s.Namespace(k.c.ns) // the runner may be replaced meanwhile (a reload): ask the current one
		if n == nil {
			time.Sleep(time.Second)
			continue
		}
		c, stop := context.WithTimeout(ctx, time.Second)
		got, ok := n.Submitted(c, k.c.agent, id)
		stop()
		if !ok {
			continue
		}
		if got.Run == "" {
			d := decide.Decision{Outcome: decide.Deny, Rule: got.Rule, Reason: got.Reason}
			return "", &refusal{d, nextAfter(d)}
		}
		return fmt.Sprintf("stored as %s - next: Plinth's automatic reviews check it, then the user decides whether it runs; "+
			"tell the user what it is for, follow it with ask_status (run %s) and read its answer with answer_read once it "+
			"is released", got.Run, got.Run) + warningNote(script), nil
	}
	return fmt.Sprintf("pending %s: Plinth hasn't taken it in yet - next: ask_status shows it, then its RUN_NAME or why it "+
		"was refused", id), nil
}

// resolveIssue reads ask_submit's issue argument: a bare number is an issue of the namespace's repo, and
// owner/repo#N or a page (https://<forgejo>/<owner>/<repo>/issues/<N>) an issue of any repo on this Plinth's Forgejo. It
// returns the number the RUN_NAME ends in (the issue's, when it is in the namespace's repo; "" for another repo's, whose
// number would mean something else here) and the issue's page for the script's ISSUE= line ("" when forgejo-url is
// not set and the argument is no page). The issue is context only: Save and Report post to plinth.repo.
func (k *turn) resolveIssue(arg string) (suffix, page string, err error) {
	if arg == "" {
		return "", "", nil
	}
	bad := fmt.Errorf("arguments: issue %q: a page https://<forgejo>/<owner>/<repo>/issues/<N> on this Plinth's Forgejo, "+
		"owner/repo#N, or a number (an issue of the namespace's repo)", arg)
	base := strings.TrimRight(k.s.Global.ForgejoURL, "/")
	var ref asklint.IssueRef
	switch m := ownerRepoIssueRe.FindStringSubmatch(arg); {
	case issueRe.MatchString(arg):
		ref = asklint.IssueRef{Repo: k.c.ns, Number: arg}
	case m != nil:
		repo, perr := names.ParseNamespace(m[1])
		if perr != nil {
			return "", "", bad
		}
		ref = asklint.IssueRef{Repo: repo, Number: m[2]}
	default:
		r, ok := asklint.ParseIssueURL(arg)
		if !ok || base == "" || !r.OnForgejo(base) {
			return "", "", bad
		}
		ref = r
	}
	switch {
	case ref.URL != "":
		page = ref.URL
	case base != "":
		page = base + "/" + ref.Repo.String() + "/issues/" + ref.Number
	}
	if ref.Repo == k.c.ns {
		suffix = ref.Number
	}
	return suffix, page, nil
}

// store writes the submission as <ns-dir>/submissions/<agent>/<id>[-<issue>].sh, first under a name the runner skips
// (a dot), so it never reads one half-written, and returns its id.
func (k *turn) store(script []byte, issue string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	name := id + ".sh"
	if issue != "" {
		name = id + "-" + issue + ".sh"
	}
	root, err := os.OpenRoot(k.s.Work)
	if err != nil {
		return "", err
	}
	defer root.Close()
	dir := filepath.Join(k.c.ns.Owner, k.c.ns.Repo, runner.Submissions)
	for _, d := range []string{dir, filepath.Join(dir, k.c.agent)} {
		if _, err := safefs.EnsureDir(root, d); err != nil {
			return "", err
		}
	}
	dir = filepath.Join(dir, k.c.agent)
	if err := safefs.WriteNew(root, dir, "."+name, script, 0o600); err != nil {
		return "", err
	}
	if err := safefs.Rename(root, filepath.Join(dir, "."+name), filepath.Join(dir, name)); err != nil {
		return "", err
	}
	k.s.Log.With("ns", k.c.ns.String(), "agent", k.c.agent).Info("submitted", "submission", id)
	return id, nil
}

func (s *Server) askStatus(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Run string `json:"run"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	r, err := k.decide(decide.AskStatus, a.Run, nil)
	if err != nil {
		return "", err
	}
	if r != nil {
		if !r.Facts.Exists {
			return "", fmt.Errorf("you have no run %s - next: ask_status without run lists your runs", a.Run)
		}
		return statusLine(r), nil
	}
	runs, err := runstate.List(s.Work, k.c.ns, k.c.agent, k.cfg.Automatic, k.n.Live())
	if err != nil {
		return "", err
	}
	var lines []string
	for _, r := range runs {
		if r.Facts.Exists && !r.Facts.Symlink {
			started, unfinished, err := request.Started(s.Work, k.c.ns, k.c.agent, r.Name)
			if err != nil {
				return "", err
			}
			distrust(r, started, unfinished)
		}
		lines = append(lines, statusLine(r))
	}
	more, err := k.submissions()
	if err != nil {
		return "", err
	}
	lines = append(lines, more...)
	if len(lines) == 0 {
		return "no runs yet - next: ask_template, write a script, ask_submit (guide explains)", nil
	}
	return strings.Join(lines, "\n"), nil
}

// submissions are the agent's submissions not taken in yet, and those refused in the last refusedFor, with the rule.
func (k *turn) submissions() ([]string, error) {
	root, err := os.OpenRoot(k.s.Work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var out []string
	pending, err := safefs.ReadDir(root, filepath.Join(k.c.ns.Owner, k.c.ns.Repo, runner.Submissions, k.c.agent))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range pending {
		if m := submittedRe.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
			out = append(out, fmt.Sprintf("pending %s - next: wait: Plinth takes it in at its next poll (then it has a RUN_NAME)", m[1]))
		}
	}
	dir := filepath.Join(k.c.ns.Owner, k.c.ns.Repo, runner.Rejected, k.c.agent)
	refused, err := safefs.ReadDir(root, dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range refused {
		info, err := e.Info()
		if err != nil || !e.IsDir() || time.Since(info.ModTime()) > refusedFor || !idRe.MatchString(e.Name()) {
			continue
		}
		b, err := safefs.ReadFile(root, filepath.Join(dir, e.Name(), "reason.md"), 64<<10)
		if err != nil {
			continue
		}
		d := decide.Decision{Outcome: decide.Deny, Rule: field(b, "Rule: "), Reason: field(b, "Reason: ")}
		out = append(out, fmt.Sprintf("refused %s (%s) by rule %s: %s - next: %s", e.Name(), info.ModTime().UTC().Format(time.RFC3339),
			d.Rule, d.Reason, nextAfter(d)))
	}
	return out, nil
}

// field is the rest of the first line of b that starts with prefix: a line of the runner's reason.md.
func field(b []byte, prefix string) string {
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, prefix); ok {
			return v
		}
	}
	return "?"
}

func (s *Server) answerRead(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Run string `json:"run"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Run == "" {
		return "", errors.New("arguments: run is required (the RUN_NAME; ask_status lists your runs)")
	}
	r, err := k.decide(decide.AnswerRead, a.Run, nil)
	var ref *refusal
	if errors.As(err, &ref) && ref.d.Rule == "answer.unreleased" && r != nil && r.Facts.Exists {
		return "not released yet: " + statusLine(r), nil // its state, not an error: the agent polls
	}
	if err != nil {
		return "", err
	}
	name := r.AnswerFile()
	if !strings.HasSuffix(name, ".released.yml") {
		return "", fmt.Errorf("run %s has no released answer - next: ask_status", a.Run)
	}
	root, err := os.OpenRoot(s.Work)
	if err != nil {
		return "", err
	}
	defer root.Close()
	b, err := safefs.ReadFile(root, filepath.Join(runstate.RunDir(k.c.ns, k.c.agent, a.Run), name), doc.MaxSize)
	if err != nil {
		return "", fmt.Errorf("the answer of %s can't be read (%v) - next: tell the user", a.Run, err)
	}
	if _, err := k.n.Record(k.c.agent, decide.AnswerRead, k.c.agent+"/"+a.Run, string(decide.Allow), "allow", name+" read"); err != nil {
		return "", err
	}
	return string(b), nil
}

// statusLine is one run as ask_status gives it: its name, ask status, answer status and what the agent does next.
func statusLine(r *runstate.Run) string {
	if r.Broken != nil {
		return fmt.Sprintf("%s  ask: broken - next: tell the user: Plinth can't read this run", r.Name)
	}
	ans := r.AnswerStatus()
	shown := cmp.Or(strings.Replace(ans, " (gate)", ", gate", 1), "-")
	return fmt.Sprintf("%s  ask: %s  answer: %s - next: %s", r.Name, r.AskStatus(), shown, nextFor(r))
}

// nextFor is what the agent does next for its run, from its states.
func nextFor(r *runstate.Run) string {
	switch ans, _, _ := strings.Cut(r.AnswerStatus(), " "); ans {
	case "released":
		return "answer_read " + r.Name
	case "scanning":
		return "wait: Plinth's DLP scan checks the answer, then the user releases it"
	case "dlp-ok", "overridden":
		return "wait: the user releases the answer, then answer_read"
	case "dlp-failed":
		return "wait: the DLP scan flagged the answer; the user reviews it and decides (it may never be released)"
	case "invalid":
		return "tell the user: the answer isn't a valid document (a bug); a rerun is a new ask_submit"
	}
	switch r.AskStatus() {
	case "running":
		return "wait: it runs"
	case "no-answer":
		return "wait: its run ended without an answer; the user may run it again"
	case "reviewing":
		return "wait: the automatic reviews run, then the user decides whether it runs"
	case "review-failed":
		return "wait: a review failed and the user decides; to change the script, submit a new version (ask_submit) and tell the user; record what the review refused in ai/memory/plinth-review-failures.md (guide: Previewing a script)"
	}
	return "wait: the user runs it when they choose"
}

// aaaCommands are what plain AAA's status lines tell an agent of this server to run.
var aaaCommands = plain.Commands{ReadAnswer: "aaa_read", NewAsk: "aaa_new"}

// repo is the agent's registered repo: its plain AAA runs are in its ai/work.
func (k *turn) repo() (string, error) {
	reg, err := agents.Get(k.s.Work, k.c.ns, k.c.agent)
	if err != nil {
		return "", err
	}
	if reg.Repo == "" {
		return "", fmt.Errorf("no repo is registered for you in %s - next: tell the user (plinthctl register --repo <your repo> --agent %s)", k.c.ns, k.c.agent)
	}
	return reg.Repo, nil
}

// aaaTarget is a plain AAA run as the event log names it, apart from the namespace's runs of the same name.
func aaaTarget(agent, run string) string { return agent + "/ai/work/" + run }

// validRun refuses a run argument that isn't a RUN_NAME, as decide does for the namespace's runs.
func validRun(run, next string) error {
	if !names.ValidRun(run) {
		return fmt.Errorf("%q is not a RUN_NAME (<adjective>-<noun>[-<issue>]) - next: %s", run, next)
	}
	return nil
}

func (s *Server) aaaNew(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Issue string `json:"issue"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Issue != "" && !issueRe.MatchString(a.Issue) {
		return "", fmt.Errorf("arguments: issue %q: the issue's number, digits only", a.Issue)
	}
	if _, err := k.decide(decide.AAANew, "", nil); err != nil {
		return "", err
	}
	repo, err := k.repo()
	if err != nil {
		return "", err
	}
	root, err := plain.OpenWorkDir(repo, true)
	if err != nil {
		return "", fmt.Errorf("ai/work in your repo can't be opened (%v) - next: tell the user", err)
	}
	defer root.Close()
	run, err := plain.NewAskIn(root, s.Work, "", a.Issue)
	if err != nil {
		return "", fmt.Errorf("the run can't be created (%v) - next: tell the user", err)
	}
	if _, err := k.n.Record(k.c.agent, decide.AAANew, aaaTarget(k.c.agent, run), string(decide.Allow), "allow",
		"ai/work/"+run+"/ask.sh created in "+repo); err != nil {
		return "", err
	}
	return fmt.Sprintf("created ai/work/%s/ask.sh in your repo (RUN_NAME %s) - next: write your steps into it after its library "+
		"line (guide: Writing an ask script), check it with aaa_lint %s, then tell the user to run it from ai/work (./%s/ask.sh) "+
		"and then plinthctl finish %s; once they say \"%s done\", aaa_read %s", run, run, run, run, run, run, run), nil
}

func (s *Server) aaaLint(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Run string `json:"run"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Run == "" {
		return "", errors.New("arguments: run is required (the RUN_NAME aaa_new gave you)")
	}
	if err := validRun(a.Run, "aaa_status lists the runs in ai/work"); err != nil {
		return "", err
	}
	if _, err := k.decide(decide.AAALint, "", nil); err != nil {
		return "", err
	}
	repo, err := k.repo()
	if err != nil {
		return "", err
	}
	missing := fmt.Errorf("no ai/work/%s/ask.sh in your repo - next: aaa_new starts a run; aaa_status lists the runs in ai/work", a.Run)
	root, err := plain.OpenWorkDir(repo, false)
	if errors.Is(err, fs.ErrNotExist) {
		return "", missing
	}
	if err != nil {
		return "", fmt.Errorf("ai/work in your repo can't be opened (%v) - next: tell the user", err)
	}
	defer root.Close()
	script, err := safefs.ReadFile(root, filepath.Join(a.Run, "ask.sh"), maxScript)
	if errors.Is(err, fs.ErrNotExist) {
		return "", missing
	}
	if err != nil {
		return "", fmt.Errorf("ai/work/%s/ask.sh can't be read (%v) - next: write it again as a regular file (no symlink, no hard link)", a.Run, err)
	}
	findings := asklint.Check(script, asklint.Options{RunName: a.Run})
	if len(findings) == 0 {
		return fmt.Sprintf("ai/work/%s/ask.sh passes ask lint - next: tell the user what it is for and ask them to run it from "+
			"ai/work (./%s/ask.sh), then plinthctl finish %s", a.Run, a.Run, a.Run) + warningNote(script), nil
	}
	return fmt.Sprintf("ai/work/%s/ask.sh: %d finding(s):\n%s%s\n- next: fix them in ai/work/%s/ask.sh, then aaa_lint %s again",
		a.Run, len(findings), strings.Join(findings, "\n"), warningNote(script), a.Run, a.Run), nil
}

// warningNote is the layout warnings of script and its plinthctl warnings as lines to append to a reply, or ""
// when there are none. They are advice: the script passes ask lint either way, since the user reads it before it runs.
func warningNote(script []byte) string {
	var out string
	if w := asklint.Warnings(script); len(w) > 0 {
		out += "\n- layout warning (not a finding; the script is indented 2 spaces per level inside if/for/while/case and { } blocks, " +
			"a broken pipe or \\ continuation 2 deeper):\n" + strings.Join(w, "\n")
	}
	if w := asklint.PlinthctlWarnings(script); len(w) > 0 {
		out += "\n- plinthctl warning (not a finding; a script with PRIVILEGED=0 runs in Plinth's container, where plinthctl is not " +
			"available; a script that needs it must say PRIVILEGED=1 and run in the user's terminal):\n" + strings.Join(w, "\n")
	}
	return out
}

func (s *Server) aaaRead(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Run string `json:"run"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Run == "" {
		return "", errors.New("arguments: run is required (the RUN_NAME; aaa_status lists the runs in ai/work)")
	}
	if err := validRun(a.Run, "aaa_status lists the runs in ai/work"); err != nil {
		return "", err
	}
	if _, err := k.decide(decide.AAARead, "", nil); err != nil {
		return "", err
	}
	repo, err := k.repo()
	if err != nil {
		return "", err
	}
	root, err := plain.OpenWorkDir(repo, false)
	if errors.Is(err, fs.ErrNotExist) {
		return a.Run + ": no such run (your repo has no ai/work yet) - next: aaa_new starts a run", nil
	}
	if err != nil {
		return "", fmt.Errorf("ai/work in your repo can't be opened (%v) - next: tell the user", err)
	}
	defer root.Close()
	ans, err := plain.ReadAnswerIn(root, a.Run, aaaCommands)
	if err != nil {
		return "", fmt.Errorf("the answer of %s can't be read (%v) - next: tell the user", a.Run, err)
	}
	if ans.Code != 0 {
		return fmt.Sprintf("%s (read-answer's exit code %d)", ans.Status, ans.Code), nil // its state, not an error: the agent polls
	}
	if _, err := k.n.Record(k.c.agent, decide.AAARead, aaaTarget(k.c.agent, a.Run), string(decide.Allow), "allow",
		filepath.Base(ans.File)+" read"); err != nil {
		return "", err
	}
	return string(ans.Body), nil
}

func (s *Server) aaaStatus(ctx context.Context, args json.RawMessage) (string, error) {
	k, err := s.begin(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Run string `json:"run"`
	}
	if err := mcp.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Run != "" {
		if err := validRun(a.Run, "aaa_status without run lists the runs in ai/work"); err != nil {
			return "", err
		}
	}
	if _, err := k.decide(decide.AAAStatus, "", nil); err != nil {
		return "", err
	}
	repo, err := k.repo()
	if err != nil {
		return "", err
	}
	var runs []plain.Run
	root, err := plain.OpenWorkDir(repo, false)
	switch {
	case errors.Is(err, fs.ErrNotExist): // no ai/work yet: no runs
	case err != nil:
		return "", fmt.Errorf("ai/work in your repo can't be opened (%v) - next: tell the user", err)
	default:
		defer root.Close()
		if runs, err = plain.RunsIn(root); err != nil {
			return "", err
		}
	}
	var lines []string
	for _, r := range runs {
		if a.Run == "" || r.Name == a.Run {
			lines = append(lines, aaaLine(r))
		}
	}
	switch {
	case len(lines) > 0:
		return strings.Join(lines, "\n"), nil
	case a.Run != "":
		return "", fmt.Errorf("no run %s in ai/work - next: aaa_status without run lists the runs there", a.Run)
	}
	return "no runs in ai/work yet - next: aaa_new starts one (guide explains)", nil
}

// aaaLine is one plain AAA run as aaa_status gives it: its name, its state from its file names (in done/ when the
// user moved it there) and what the agent does next.
func aaaLine(r plain.Run) string {
	state := r.State
	if r.Gate {
		state += ", gate"
	}
	if r.Done {
		state += ", in done/"
	}
	return fmt.Sprintf("%s  state: %s - next: %s", r.Name, state, aaaNext(r))
}

// aaaNext is what the agent does next for a plain AAA run, from its state.
func aaaNext(r plain.Run) string {
	switch r.State {
	case plain.StateNotRun:
		return fmt.Sprintf("write ai/work/%s/ask.sh if you haven't and check it (aaa_lint %s), then tell the user to run it from "+
			"ai/work (./%s/ask.sh) and then plinthctl finish %s", r.Name, r.Name, r.Name, r.Name)
	case plain.StateRunning:
		return fmt.Sprintf("wait until the user says \"%s done\", then aaa_read %s", r.Name, r.Name)
	case plain.StateNew:
		return fmt.Sprintf("ask the user to run plinthctl finish %s (it scans the answer), then aaa_read %s", r.Name, r.Name)
	case plain.StateDLPOK:
		return "aaa_read " + r.Name
	case plain.StateDLPFail:
		return "tell the user: the DLP scan flagged the answer; it isn't read, and nothing here overrides it"
	case plain.StateInvalid:
		return "tell the user: the answer isn't a valid document (a bug); a rerun is a new run (aaa_new)"
	}
	return "tell the user"
}
