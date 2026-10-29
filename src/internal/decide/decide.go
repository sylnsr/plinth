// Package decide is Plinth's one gatekeeper: every MCP call and every request asks Decide before anything acts. The
// rules run in a fixed order; the first that denies or requires approval wins, and an action no rule allowed is denied
// (default.deny). Decide is pure: the caller gathers the facts (the run's state from its file names, the config) and
// Decide only reads them, so the whole policy is this one file. Spec: plinthctl.README.md, "Decision function".
package decide

import (
	"fmt"
	"slices"
	"unicode/utf8"

	"plinth/internal/auth"
	"plinth/internal/names"
)

// Outcome is what Decide says.
type Outcome string

const (
	Allow           Outcome = "allow"
	Deny            Outcome = "deny"
	RequireApproval Outcome = "require-approval"
)

// Actions. Agent actions are MCP tools; user actions are requests (and their commands).
const (
	Guide          = "guide" // an agent reads what Plinth is and how to work with it
	AskTemplate    = "ask.template"
	AskSubmit      = "ask.submit"
	AskPreview     = "ask.preview" // an agent has a script checked as ask_submit would, nothing stored
	AskStatus      = "ask.status"
	AnswerRead     = "answer.read"
	AAANew         = "aaa.new"    // an agent starts a plain AAA run in its own repo's ai/work
	AAARead        = "aaa.read"   // and reads its answer once the DLP scan passed
	AAAStatus      = "aaa.status" // and lists the runs there
	AAALint        = "aaa.lint"   // and checks a run's ask.sh as ask lint does
	AgentUp        = "agent.up"
	MessageSend    = "message.send"
	MessagePickup  = "message.pickup"
	Exec           = "exec"
	Done           = "done"
	Save           = "save"
	Scrub          = "scrub"
	Report         = "report"
	Review         = "review" // review.<type>: Request.ReviewType names the type
	OverrideReview = "override.review"
	OverrideDLP    = "override.dlp"
	AcceptNetwork  = "accept.network" // the user accepts that a network run gets the network
	Approve        = "approve"
	Reject         = "reject"
	LogsScrub      = "logs-scrub" // process-level
)

// userOnly are the actions only the user may take (rule caller.user-only).
var userOnly = []string{Exec, Done, Save, Scrub, Report, Review, OverrideReview, OverrideDLP, AcceptNetwork, Approve, Reject, LogsScrub}

// allowed are the actions the policy knows: once every rule above passes, they are allowed. The message actions are
// not here yet: until message ids and the catalogue are specified, default.deny refuses them.
var allowed = []string{Guide, AskTemplate, AskSubmit, AskPreview, AskStatus, AnswerRead, AAANew, AAARead, AAAStatus, AAALint, AgentUp,
	Exec, Done, Save, Scrub, Report, Review, OverrideReview, OverrideDLP, AcceptNetwork, LogsScrub}

// MaxScriptLines is the largest ask script a submission may have (rule ask.size).
const MaxScriptLines = 500

// Run is what the caller found about a run, from its file names only (plinthctl.README.md, Runs, States).
type Run struct {
	Exists           bool
	Symlink          bool     // something in the run's path is a symlink (target.valid)
	ReviewsPending   []string // automatic review types with no review file yet
	ReviewsFailed    []string // review.<type>.FAILED.md, or a Do not run verdict
	ReviewOverridden bool     // override.review.md
	Running          bool     // the controller's child runs it (the controller's record, not running.yml)
	Ran              bool     // it ran: an answer exists, or running.yml was left over (no answer)
	Answer           string   // "", "new", "dlp-ok", "DLP-FAIL", "INVALID" or "released": only an answer says the run is done
	DLPOverridden    bool     // override.dlp.md
	Reported         bool     // report.md
	NetworkAccepted  bool     // accept.network.md
	// From the exec request, for every action on a run.
	Started    bool // exec.running or exec.finished: the controller started the script, whatever the run directory says
	Unfinished bool // exec.running: an exec not finished yet; a stopped controller's is finished by the next one
	// From ask.sh itself, gathered for exec and accept.network only.
	Interactive   bool   // INTERACTIVE=1: it runs only in the user's terminal, never as the controller's child
	Network       bool   // NETWORK=1: network mode, once the user accepted it
	Privileged    bool   // PRIVILEGED=1: it runs only in the user's terminal, never as the controller's child
	ScriptChanged bool   // ask.sh no longer matches the ask.sh.sha256 stored with it
	Lint          string // ask lint --plinth's findings on ask.sh, "" when it passes (exec only)
}

// Submission is what the caller found about a submitted script (ask.submit).
type Submission struct {
	Script     []byte
	LintErr    error // ask lint --plinth; nil when it passed
	Network    bool  // it says NETWORK=1
	Privileged bool  // it says PRIVILEGED=1
}

// Request is one question to the gatekeeper.
type Request struct {
	Caller     auth.Caller     // from the channel that authenticated it (package auth), never from a name
	Unknown    bool            // the agent caller is not registered (any more): e.g. a submission left by a removed agent
	NotEnabled bool            // the agent caller is registered but not enabled for its registration's repo (agents.Enabled)
	Namespace  names.Namespace // zero for a process-level action
	Action     string
	Agent      string // the agent whose run (or whose turn) it is; for an agent caller, its own unless it asks about another
	RunName    string // the target run, when the action has one
	ReviewType string // for Review
	Run        Run
	Submission Submission
	// From the config.
	ReviewTypes []string // reviewers.type names
	Repo        string   // plinth.repo
	Network     bool     // plinth.exec.network: the namespace allows network mode
	Privileged  bool     // plinth.exec.privileged: the namespace allows privileged mode
	// Terminal: the action comes from the user's own terminal (plinthctl exec over the controller's socket), where a
	// privileged run runs, never as the controller's child.
	Terminal bool
}

// Decision is Decide's answer. Every refusal names its rule; the event log records the same id.
type Decision struct {
	Outcome Outcome
	Rule    string
	Reason  string
	Policy  string // where the rule is explained
}

// policy is the page every rule points at: the spec's table, where each rule id is listed.
const policy = "plinthctl.README.md, Decision function"

// rule is one row of the table: it returns a decision, or nil to pass the request on.
type rule struct {
	id    string
	check func(r *Request) *Decision
}

// rules is the whole policy, in order.
var rules = []rule{
	{"caller.user-only", func(r *Request) *Decision {
		if !r.Caller.IsUser() && slices.Contains(userOnly, r.Action) {
			return deny("only the user can %s", r.Action)
		}
		return nil
	}},
	{"caller.own-runs", func(r *Request) *Decision {
		// For an agent, the agent of the run (or turn) is always the caller itself: one that names another agent is
		// refused, and so is one that names none where the action is per agent, so a gap in the MCP code can't open
		// another agent's runs (Scan 3).
		perAgent := needsRun(r.Action) || slices.Contains([]string{AskStatus, AskSubmit, AskPreview, AAANew, AAARead, AAAStatus, AAALint}, r.Action)
		if !r.Caller.IsUser() && r.Agent != r.Caller.Agent() && (r.Agent != "" || perAgent) {
			return deny("a run of another agent")
		}
		return nil
	}},
	{"target.valid", func(r *Request) *Decision {
		if !r.Caller.IsUser() && (!r.Caller.IsAgent() || !names.ValidAgent(r.Caller.Agent())) {
			return deny("caller %q is neither the user nor an agent", r.Caller.Agent())
		}
		if !r.Caller.IsUser() && r.Unknown {
			return deny("agent %s is not registered", r.Caller.Agent())
		}
		if !r.Caller.IsUser() && r.NotEnabled {
			return deny("agent %s is registered but not enabled in this namespace (plinthctl agent enable)", r.Caller.Agent())
		}
		if r.Action != LogsScrub {
			if _, err := names.ParseNamespace(r.Namespace.String()); err != nil {
				return deny("namespace: %v", err)
			}
		}
		if r.Agent != "" && !names.ValidAgent(r.Agent) {
			return deny("agent %q: not an agent name", r.Agent)
		}
		if needsRun(r.Action) || r.RunName != "" {
			if !names.ValidRun(r.RunName) {
				return deny("run %q: not a run name", r.RunName)
			}
			if r.Agent == "" {
				return deny("run %s: no agent", r.RunName)
			}
		}
		if r.Action == Review && !slices.Contains(r.ReviewTypes, r.ReviewType) {
			return deny("review type %q is not defined in the config", r.ReviewType)
		}
		if r.Run.Symlink {
			return deny("a symlink in the run's path")
		}
		return nil
	}},
	{"state.valid", stateValid},
	{"ask.lint", func(r *Request) *Decision {
		if r.Action == AskSubmit && r.Submission.LintErr != nil {
			return deny("ask lint --plinth: %v", r.Submission.LintErr)
		}
		// A script that passed at its submission may not pass the lint of a later plinthctl.
		if r.Action == Exec && r.Run.Lint != "" {
			return deny("run %s: ask.sh fails ask lint --plinth (%s): it doesn't run; next: the agent writes it again from ask.template and submits it as a new run", r.RunName, r.Run.Lint)
		}
		return nil
	}},
	{"ask.size", func(r *Request) *Decision {
		if r.Action != AskSubmit && r.Action != AskPreview {
			return nil
		}
		s := r.Submission.Script
		if !utf8.Valid(s) {
			return deny("the script is not UTF-8")
		}
		if n := lines(s); n > MaxScriptLines {
			return deny("the script has %d lines, at most %d", n, MaxScriptLines)
		}
		return nil
	}},
	{"ask.mode", func(r *Request) *Decision {
		if r.Action != AskSubmit && r.Action != AskPreview {
			return nil
		}
		if r.Submission.Network && !r.Network {
			return deny("the script asks for network mode (NETWORK=1), which this namespace doesn't allow (plinth.exec.network)")
		}
		if r.Submission.Privileged && !r.Privileged {
			return deny("the script asks for privileged mode (PRIVILEGED=1), which this namespace doesn't allow (plinth.exec.privileged)")
		}
		return nil
	}},
	{"review.pending", func(r *Request) *Decision {
		if r.Action == Exec && len(r.Run.ReviewsPending) > 0 {
			return deny("automatic review still running: %v", r.Run.ReviewsPending)
		}
		return nil
	}},
	{"review.failed", func(r *Request) *Decision {
		if r.Action == Exec && len(r.Run.ReviewsFailed) > 0 && !r.Run.ReviewOverridden {
			return &Decision{Outcome: RequireApproval, Reason: fmt.Sprintf("failed review: %v; needs override.review", r.Run.ReviewsFailed)}
		}
		return nil
	}},
	{"exec.mode", func(r *Request) *Decision {
		if r.Action != Exec && r.Action != AcceptNetwork {
			return nil
		}
		if r.Action == AcceptNetwork && r.Run.Privileged {
			return deny("run %s is privileged (PRIVILEGED=1): it runs in your own terminal, with no network to accept", r.RunName)
		}
		if r.Run.Privileged && !r.Terminal {
			return deny("run %s is privileged (PRIVILEGED=1): it runs only in your own terminal, never as the controller's child (plinthctl exec, or e in the TUI)", r.RunName)
		}
		if r.Run.Privileged && !r.Privileged {
			return deny("run %s is privileged (PRIVILEGED=1), which this namespace doesn't allow (plinth.exec.privileged)", r.RunName)
		}
		if r.Terminal && !r.Run.Privileged {
			return deny("run %s isn't privileged: it runs as the controller's child, in its container (plinthctl exec queues it)", r.RunName)
		}
		if r.Run.Network && !r.Network {
			return deny("run %s is a network run (NETWORK=1), which this namespace doesn't allow (plinth.exec.network)", r.RunName)
		}
		if r.Action == Exec && r.Run.Network && !r.Run.NetworkAccepted {
			return &Decision{Outcome: RequireApproval, Reason: fmt.Sprintf("run %s is a network run (NETWORK=1); needs accept.network", r.RunName)}
		}
		return nil
	}},
	{"dlp.failed", func(r *Request) *Decision {
		if (r.Action == Done || r.Action == Save) && r.Run.Answer == "DLP-FAIL" && !r.Run.DLPOverridden {
			return &Decision{Outcome: RequireApproval, Reason: "the DLP scan failed; needs override.dlp"}
		}
		return nil
	}},
	{"answer.unreleased", func(r *Request) *Decision {
		switch {
		case r.Action != AnswerRead:
		case r.Run.Answer == "released" && r.Run.Started && !r.Run.Running && !r.Run.Unfinished:
			// The file name alone doesn't count: the script can write answer.released.yml into its own run
			// directory. Done releases only after the exec ended, and the controller's exec request says so.
		case r.Run.Answer == "" && (r.Run.Ran || r.Run.Started) && !r.Run.Running && !r.Run.Unfinished:
			return deny("the run has no answer: its exec ended without one; the user can run it again")
		default:
			return deny("the answer is not released (Done) yet")
		}
		return nil
	}},
	{"repo.set", func(r *Request) *Decision {
		if (r.Action == Save || r.Action == Report) && r.Repo == "" {
			return deny("no repo configured for saved runs and reports (plinth.repo)")
		}
		return nil
	}},
	{"allow", func(r *Request) *Decision {
		if slices.Contains(allowed, r.Action) {
			return &Decision{Outcome: Allow, Reason: "every rule passed"}
		}
		return nil
	}},
	{"default.deny", func(r *Request) *Decision {
		return deny("no rule allows %q", r.Action)
	}},
}

// Decide runs the rules in order and returns the first decision.
func Decide(r Request) Decision {
	for _, ru := range rules {
		if d := ru.check(&r); d != nil {
			d.Rule, d.Policy = ru.id, policy
			return *d
		}
	}
	panic("unreachable: default.deny decides everything") // the last rule never passes
}

// Rules lists the rule ids in order, for the spec test and the UIs.
func Rules() []string {
	ids := make([]string, len(rules))
	for i, ru := range rules {
		ids[i] = ru.id
	}
	return ids
}

// stateValid is rule state.valid: the run's state must allow the action.
func stateValid(r *Request) *Decision {
	if !needsRun(r.Action) {
		return nil
	}
	run := r.Run
	if !run.Exists {
		return deny("run %s does not exist", r.RunName)
	}
	scanned := run.Answer == "dlp-ok" || run.Answer == "DLP-FAIL"
	switch r.Action {
	case Exec:
		if run.Running {
			return deny("run %s is running", r.RunName)
		}
		if run.Unfinished {
			return deny("run %s: its last exec isn't finished yet (the controller finishes it, at its next start if it stopped)", r.RunName)
		}
		// Only an answer says the run is done: one whose exec ended without an answer runs again.
		if run.Answer != "" {
			return deny("run %s is done: it has an answer; a new version is a new submission", r.RunName)
		}
		if run.ScriptChanged {
			return deny("run %s: ask.sh no longer matches the ask.sh.sha256 stored with it", r.RunName)
		}

		if run.Interactive && !r.Terminal {
			return deny("run %s is interactive (INTERACTIVE=1): it runs only in your own terminal, never as the controller's child", r.RunName)
		}
	case Done:
		if run.Answer == "released" {
			return deny("run %s is released already", r.RunName)
		}
		if run.Answer == "INVALID" {
			return deny("run %s: its answer is INVALID, not a valid document (a bug, never data): it is never released; next: tell the agent, whose rerun is a new run", r.RunName)
		}
		if !scanned {
			return deny("run %s has no scanned answer", r.RunName)
		}
	case Save:
		if !scanned && run.Answer != "released" {
			return deny("run %s has no scanned answer", r.RunName)
		}
	case Scrub:
		if run.Running {
			return deny("run %s is running", r.RunName)
		}
	case Report:
		if run.Reported {
			return deny("run %s is reported already", r.RunName)
		}
		if len(run.ReviewsFailed) == 0 && run.Answer != "DLP-FAIL" {
			return deny("run %s has no failed scan", r.RunName)
		}
	case Review:
		if run.Running {
			return deny("run %s is running", r.RunName)
		}
	case OverrideReview:
		if run.ReviewOverridden {
			return deny("run %s: the review is overridden already", r.RunName)
		}
		if len(run.ReviewsFailed) == 0 {
			return deny("run %s has no failed review", r.RunName)
		}
		if run.Ran || run.Running || run.Started {
			return deny("run %s ran already", r.RunName)
		}
	case AcceptNetwork:
		if run.NetworkAccepted {
			return deny("run %s: network mode is accepted already", r.RunName)
		}
		if !run.Network {
			return deny("run %s is not a network run (no NETWORK=1)", r.RunName)
		}
		if run.Ran || run.Running || run.Started {
			return deny("run %s ran already", r.RunName)
		}
		if run.ScriptChanged {
			return deny("run %s: ask.sh no longer matches the ask.sh.sha256 stored with it", r.RunName)
		}
	case OverrideDLP:
		if run.DLPOverridden {
			return deny("run %s: the DLP scan is overridden already", r.RunName)
		}
		if run.Answer != "DLP-FAIL" {
			return deny("run %s has no failed DLP scan", r.RunName)
		}
	case AnswerRead:
		// answer.unreleased says why an existing run's answer can't be read yet.
	}
	return nil
}

// needsRun reports whether the action targets a run.
func needsRun(action string) bool {
	switch action {
	case Exec, Done, Save, Scrub, Report, Review, OverrideReview, OverrideDLP, AcceptNetwork, AnswerRead:
		return true
	}
	return false
}

func deny(format string, args ...any) *Decision {
	return &Decision{Outcome: Deny, Reason: fmt.Sprintf(format, args...)}
}

// lines counts the lines of s; a last line without a newline counts.
func lines(s []byte) int {
	n := 0
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	if len(s) > 0 && s[len(s)-1] != '\n' {
		n++
	}
	return n
}
