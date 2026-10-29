package decide

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"plinth/internal/auth"
	"plinth/internal/names"
)

var ns = names.Namespace{Owner: "michael", Repo: "devops"}

var (
	user      = auth.HostUser()
	agentMain = auth.Submission("main")
	agentAl   = auth.Submission("al")
)

// req is a request about main's run keen-tapir, which is queued and reviewed, with the given changes.
func req(caller auth.Caller, action string, change func(r *Request)) Request {
	r := Request{Caller: caller, Namespace: ns, Action: action, Agent: "main", RunName: "keen-tapir",
		ReviewTypes: []string{"security", "dlp"}, Run: Run{Exists: true}}
	if change != nil {
		change(&r)
	}
	return r
}

func TestDecide(t *testing.T) {
	for _, c := range []struct {
		name    string
		r       Request
		outcome Outcome
		rule    string
	}{
		// caller.user-only
		{"agent execs", req(agentMain, Exec, nil), Deny, "caller.user-only"},
		{"agent releases its own answer", req(agentMain, Done, func(r *Request) { r.Run.Answer = "dlp-ok" }), Deny, "caller.user-only"},
		{"agent overrides a review", req(agentMain, OverrideReview, nil), Deny, "caller.user-only"},
		{"agent scrubs logs", Request{Caller: agentMain, Action: LogsScrub}, Deny, "caller.user-only"},
		{"a caller of no kind", req(auth.Caller{}, Exec, nil), Deny, "caller.user-only"},
		{"an agent named user", req(auth.Submission("user"), Exec, nil), Deny, "caller.user-only"},
		// caller.own-runs
		{"agent reads another agent's answer", req(agentAl, AnswerRead, func(r *Request) { r.Run.Answer = "released" }), Deny, "caller.own-runs"},
		{"agent asks about another agent's run", req(agentAl, AskStatus, nil), Deny, "caller.own-runs"},
		{"agent reads an answer naming no agent", req(agentMain, AnswerRead, func(r *Request) { r.Agent = ""; r.Run.Answer = "released" }), Deny, "caller.own-runs"},
		{"agent asks about runs naming no agent", req(agentMain, AskStatus, func(r *Request) { r.Agent, r.RunName = "", "" }), Deny, "caller.own-runs"},
		{"agent submits for another agent", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "al",
			Submission: Submission{Script: []byte("echo\n")}}, Deny, "caller.own-runs"},
		// target.valid
		{"bad run name", req(user, Exec, func(r *Request) { r.RunName = "../x" }), Deny, "target.valid"},
		{"bad agent name", req(user, Exec, func(r *Request) { r.Agent = "Main" }), Deny, "target.valid"},
		{"bad caller", req(auth.Submission("plinth_tok3n"), AskTemplate, func(r *Request) { r.Agent = "plinth_tok3n" }), Deny, "target.valid"},
		{"removed agent", req(agentMain, AskTemplate, func(r *Request) { r.Unknown = true }), Deny, "target.valid"},
		{"agent not enabled", req(agentMain, AskTemplate, func(r *Request) { r.NotEnabled = true }), Deny, "target.valid"},
		{"the user is never not enabled", req(user, Exec, func(r *Request) { r.NotEnabled = true }), Allow, "allow"},
		{"a caller of no kind asks for the template", Request{Caller: auth.Caller{}, Namespace: ns, Action: AskTemplate}, Deny, "target.valid"},
		{"an agent named user asks for the template", Request{Caller: auth.Submission("user"), Namespace: ns, Action: AskTemplate, Agent: "user"}, Deny, "target.valid"},
		{"no namespace", req(user, Exec, func(r *Request) { r.Namespace = names.Namespace{} }), Deny, "target.valid"},
		{"undefined review type", req(user, Review, func(r *Request) { r.ReviewType = "nope" }), Deny, "target.valid"},
		{"symlink in the run", req(user, Exec, func(r *Request) { r.Run.Symlink = true }), Deny, "target.valid"},
		// state.valid
		{"exec a run that doesn't exist", req(user, Exec, func(r *Request) { r.Run.Exists = false }), Deny, "state.valid"},
		{"exec twice", req(user, Exec, func(r *Request) { r.Run.Ran, r.Run.Started, r.Run.Answer = true, true, "dlp-ok" }), Deny, "state.valid"},
		{"exec of a scanning answer", req(user, Exec, func(r *Request) { r.Run.Ran, r.Run.Answer = true, "new" }), Deny, "state.valid"},
		{"exec again, no answer", req(user, Exec, func(r *Request) { r.Run.Ran, r.Run.Started = true, true }), Allow, "allow"},
		{"exec again, the run directory emptied", req(user, Exec, func(r *Request) { r.Run.Started = true }), Allow, "allow"},
		{"exec while running", req(user, Exec, func(r *Request) { r.Run.Running = true }), Deny, "state.valid"},
		{"exec with an unfinished exec", req(user, Exec, func(r *Request) { r.Run.Started, r.Run.Unfinished = true, true }), Deny, "state.valid"},
		{"done without a scanned answer", req(user, Done, func(r *Request) { r.Run.Answer = "new" }), Deny, "state.valid"},
		{"done twice", req(user, Done, func(r *Request) { r.Run.Answer = "released" }), Deny, "state.valid"},
		{"report without a failed scan", req(user, Report, func(r *Request) { r.Run.Answer = "dlp-ok" }), Deny, "state.valid"},
		{"report twice", req(user, Report, func(r *Request) { r.Run.Answer = "DLP-FAIL"; r.Run.Reported = true }), Deny, "state.valid"},
		{"override a review that passed", req(user, OverrideReview, nil), Deny, "state.valid"},
		{"override a review once the script started, the run directory emptied", req(user, OverrideReview, func(r *Request) {
			r.Run.ReviewsFailed, r.Run.Started = []string{"dlp"}, true
		}), Deny, "state.valid"},
		{"override dlp that passed", req(user, OverrideDLP, func(r *Request) { r.Run.Answer = "dlp-ok" }), Deny, "state.valid"},
		{"scrub while running", req(user, Scrub, func(r *Request) { r.Run.Running = true }), Deny, "state.valid"},
		{"exec a changed script", req(user, Exec, func(r *Request) { r.Run.ScriptChanged = true }), Deny, "state.valid"},
		{"exec a script that fails ask lint", req(user, Exec, func(r *Request) { r.Run.Lint = "no library line" }), Deny, "ask.lint"},
		{"release an INVALID answer", req(user, Done, func(r *Request) { r.Run.Answer = "INVALID" }), Deny, "state.valid"},
		{"save an INVALID answer", req(user, Save, func(r *Request) { r.RunName, r.Run.Answer = "keen-tapir-12", "INVALID" }), Deny, "state.valid"},
		{"save a run without an issue", req(user, Save, func(r *Request) { r.Run.Answer, r.Repo = "released", "michael/runs" }), Allow, "allow"},
		{"save without a repo", req(user, Save, func(r *Request) { r.Run.Answer = "released" }), Deny, "repo.set"},
		{"save a released answer", req(user, Save, func(r *Request) { r.RunName, r.Run.Answer, r.Repo = "keen-tapir-12", "released", "michael/runs" }), Allow, "allow"},
		{"exec an interactive script", req(user, Exec, func(r *Request) { r.Run.Interactive = true }), Deny, "state.valid"},
		// ask.lint, ask.size
		{"submission fails lint", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "main",
			Submission: Submission{Script: []byte("#!/bin/bash\n"), LintErr: errors.New("no RUN_NAME")}}, Deny, "ask.lint"},
		{"submission too long", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "main",
			Submission: Submission{Script: []byte(strings.Repeat("echo\n", 501))}}, Deny, "ask.size"},
		{"submission not UTF-8", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "main",
			Submission: Submission{Script: []byte("echo \xff\n")}}, Deny, "ask.size"},
		{"submission of 500 lines", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "main",
			Submission: Submission{Script: []byte(strings.Repeat("echo\n", 500))}}, Allow, "allow"},
		{"preview too long", Request{Caller: agentMain, Namespace: ns, Action: AskPreview, Agent: "main",
			Submission: Submission{Script: []byte(strings.Repeat("echo\n", 501))}}, Deny, "ask.size"},
		{"preview of another agent", Request{Caller: agentMain, Namespace: ns, Action: AskPreview, Agent: "al",
			Submission: Submission{Script: []byte("echo\n")}}, Deny, "caller.own-runs"},
		{"preview", Request{Caller: agentMain, Namespace: ns, Action: AskPreview, Agent: "main",
			Submission: Submission{Script: []byte("echo\n")}}, Allow, "allow"},
		// review.pending, review.failed
		{"exec during a review", req(user, Exec, func(r *Request) { r.Run.ReviewsPending = []string{"security"} }), Deny, "review.pending"},
		{"exec after a failed review", req(user, Exec, func(r *Request) { r.Run.ReviewsFailed = []string{"security"} }), RequireApproval, "review.failed"},
		{"exec after an overridden review", req(user, Exec, func(r *Request) {
			r.Run.ReviewsFailed, r.Run.ReviewOverridden = []string{"security"}, true
		}), Allow, "allow"},
		{"override a failed review", req(user, OverrideReview, func(r *Request) { r.Run.ReviewsFailed = []string{"dlp"} }), Allow, "allow"},
		// ask.mode, exec.mode
		{"submission asks for the network, not allowed", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "main",
			Submission: Submission{Script: []byte("echo\n"), Network: true}}, Deny, "ask.mode"},
		{"submission asks for the network, allowed", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "main", Network: true,
			Submission: Submission{Script: []byte("echo\n"), Network: true}}, Allow, "allow"},
		{"submission asks for privileged mode, not allowed", Request{Caller: agentMain, Namespace: ns, Action: AskSubmit, Agent: "main", Network: true,
			Submission: Submission{Script: []byte("echo\n"), Privileged: true}}, Deny, "ask.mode"},
		{"exec a network run before accept.network", req(user, Exec, func(r *Request) { r.Network, r.Run.Network = true, true }), RequireApproval, "exec.mode"},
		{"exec a network run after accept.network", req(user, Exec, func(r *Request) {
			r.Network, r.Run.Network, r.Run.NetworkAccepted = true, true, true
		}), Allow, "allow"},
		{"exec a network run the namespace no longer allows", req(user, Exec, func(r *Request) { r.Run.Network, r.Run.NetworkAccepted = true, true }), Deny, "exec.mode"},
		{"exec a privileged run", req(user, Exec, func(r *Request) { r.Privileged, r.Run.Privileged = true, true }), Deny, "exec.mode"},
		{"exec a privileged run from the terminal", req(user, Exec, func(r *Request) { r.Privileged, r.Run.Privileged, r.Terminal = true, true, true }), Allow, "allow"},
		{"exec an interactive run from the terminal", req(user, Exec, func(r *Request) {
			r.Privileged, r.Run.Privileged, r.Run.Interactive, r.Terminal = true, true, true, true
		}), Allow, "allow"},
		{"exec a privileged run from the terminal, not allowed", req(user, Exec, func(r *Request) { r.Run.Privileged, r.Terminal = true, true }), Deny, "exec.mode"},
		{"exec a normal run from the terminal", req(user, Exec, func(r *Request) { r.Privileged, r.Terminal = true, true }), Deny, "exec.mode"},
		{"accept the network of a privileged run", req(user, AcceptNetwork, func(r *Request) {
			r.Network, r.Privileged, r.Run.Network, r.Run.Privileged = true, true, true, true
		}), Deny, "exec.mode"},
		{"accept a network run", req(user, AcceptNetwork, func(r *Request) { r.Network, r.Run.Network = true, true }), Allow, "allow"},
		{"accept a network run the namespace doesn't allow", req(user, AcceptNetwork, func(r *Request) { r.Run.Network = true }), Deny, "exec.mode"},
		{"agent accepts a network run", req(agentMain, AcceptNetwork, func(r *Request) { r.Network, r.Run.Network = true, true }), Deny, "caller.user-only"},
		{"accept a normal run", req(user, AcceptNetwork, func(r *Request) { r.Network = true }), Deny, "state.valid"},
		{"accept twice", req(user, AcceptNetwork, func(r *Request) { r.Network, r.Run.Network, r.Run.NetworkAccepted = true, true, true }), Deny, "state.valid"},
		{"accept after the start", req(user, AcceptNetwork, func(r *Request) { r.Network, r.Run.Network, r.Run.Started = true, true, true }), Deny, "state.valid"},
		{"accept a changed script", req(user, AcceptNetwork, func(r *Request) { r.Network, r.Run.Network, r.Run.ScriptChanged = true, true, true }), Deny, "state.valid"},
		// dlp.failed
		{"done after a failed scan", req(user, Done, func(r *Request) { r.Run.Answer = "DLP-FAIL" }), RequireApproval, "dlp.failed"},
		{"save after a failed scan", req(user, Save, func(r *Request) { r.RunName, r.Run.Answer = "keen-tapir-12", "DLP-FAIL" }), RequireApproval, "dlp.failed"},
		{"done after an overridden scan", req(user, Done, func(r *Request) { r.Run.Answer, r.Run.DLPOverridden = "DLP-FAIL", true }), Allow, "allow"},
		// answer.unreleased
		{"answer before done", req(agentMain, AnswerRead, func(r *Request) { r.Run.Answer = "dlp-ok" }), Deny, "answer.unreleased"},
		{"answer of a run without one", req(agentMain, AnswerRead, func(r *Request) { r.Run.Ran, r.Run.Started = true, true }), Deny, "answer.unreleased"},
		{"answer after done", req(agentMain, AnswerRead, func(r *Request) { r.Run.Answer, r.Run.Started = "released", true }), Allow, "allow"},
		{"a released answer while the script runs", req(agentMain, AnswerRead, func(r *Request) { r.Run.Answer, r.Run.Started, r.Run.Running = "released", true, true }), Deny, "answer.unreleased"},
		{"a released answer, the exec unfinished", req(agentMain, AnswerRead, func(r *Request) { r.Run.Answer, r.Run.Started, r.Run.Unfinished = "released", true, true }), Deny, "answer.unreleased"},
		{"a released answer, no exec request", req(agentMain, AnswerRead, func(r *Request) { r.Run.Answer = "released" }), Deny, "answer.unreleased"},
		// repo.set
		{"report without a repo", req(user, Report, func(r *Request) { r.Run.Answer = "DLP-FAIL" }), Deny, "repo.set"},
		{"report", req(user, Report, func(r *Request) { r.Run.Answer = "DLP-FAIL"; r.Repo = "michael/scans" }), Allow, "allow"},
		// allowed
		{"exec a reviewed run", req(user, Exec, nil), Allow, "allow"},
		{"done", req(user, Done, func(r *Request) { r.Run.Answer = "dlp-ok" }), Allow, "allow"},
		{"review", req(user, Review, func(r *Request) { r.ReviewType = "dlp" }), Allow, "allow"},
		{"logs scrub", Request{Caller: user, Action: LogsScrub}, Allow, "allow"},
		{"agent's own status", req(agentMain, AskStatus, nil), Allow, "allow"},
		{"template", Request{Caller: agentMain, Namespace: ns, Action: AskTemplate, Agent: "main"}, Allow, "allow"},
		{"guide", Request{Caller: agentMain, Namespace: ns, Action: Guide, Agent: "main"}, Allow, "allow"},
		{"guide, agent not enabled", Request{Caller: agentMain, Namespace: ns, Action: Guide, Agent: "main", NotEnabled: true}, Deny, "target.valid"},
		{"aaa new", Request{Caller: agentMain, Namespace: ns, Action: AAANew, Agent: "main"}, Allow, "allow"},
		{"aaa read", Request{Caller: agentMain, Namespace: ns, Action: AAARead, Agent: "main"}, Allow, "allow"},
		{"aaa status", Request{Caller: agentMain, Namespace: ns, Action: AAAStatus, Agent: "main"}, Allow, "allow"},
		{"aaa lint", Request{Caller: agentMain, Namespace: ns, Action: AAALint, Agent: "main"}, Allow, "allow"},
		{"aaa new, no agent", Request{Caller: agentMain, Namespace: ns, Action: AAANew}, Deny, "caller.own-runs"},
		{"aaa read, agent not enabled", Request{Caller: agentMain, Namespace: ns, Action: AAARead, Agent: "main", NotEnabled: true}, Deny, "target.valid"},
		// default.deny: messages until they are specified, and anything unknown
		{"message", Request{Caller: agentMain, Namespace: ns, Action: MessageSend, Agent: "main"}, Deny, "default.deny"},
		{"approve", Request{Caller: user, Namespace: ns, Action: Approve}, Deny, "default.deny"},
		{"unknown action", Request{Caller: user, Namespace: ns, Action: "delete-everything"}, Deny, "default.deny"},
		{"no action", Request{Caller: user, Namespace: ns}, Deny, "default.deny"},
	} {
		d := Decide(c.r)
		if d.Outcome != c.outcome || d.Rule != c.rule {
			t.Errorf("%s: %s by %s (%s), want %s by %s", c.name, d.Outcome, d.Rule, d.Reason, c.outcome, c.rule)
		}
		if d.Reason == "" || d.Policy == "" {
			t.Errorf("%s: a decision without a reason or policy: %+v", c.name, d)
		}
	}
}

func TestRuleOrder(t *testing.T) {
	want := []string{"caller.user-only", "caller.own-runs", "target.valid", "state.valid", "ask.lint", "ask.size",
		"ask.mode", "review.pending", "review.failed", "exec.mode", "dlp.failed", "answer.unreleased", "repo.set", "allow", "default.deny"}
	if got := Rules(); !slices.Equal(got, want) {
		t.Errorf("rules = %v\nwant    %v", got, want)
	}
}
