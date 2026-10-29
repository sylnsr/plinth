package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/decide"
	"plinth/internal/doc"
	"plinth/internal/events"
	"plinth/internal/imagetest"
	"plinth/internal/ollama/ollamatest"
	"plinth/internal/plain"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
)

// startFiles separates, in the reason of an exec.start event, the ask.sh's sha256 from the names in the run directory
// at the start (the runner's).
const startFiles = "; run directory: "

// execCfg is globalCfg plus a secret from the environment, which a script must never see.
const execCfg = globalCfg + `forgejo-token: "${RUNNER_TEST_TOKEN}"
`

// submitted takes in a script whose steps (after the template's) are body and returns its run, review-failed (no
// Ollama).
func submitted(t *testing.T, work, body string) *runstate.Run {
	t.Helper()
	return submittedAs(t, work, "", body)
}

// submittedAs is submitted with a mode line set, such as NETWORK=1.
func submittedAs(t *testing.T, work, mode, body string) *runstate.Run {
	t.Helper()
	return submittedStatus(t, work, mode, body, "review-failed")
}

// submittedStatus is submittedAs for a run that ends its reviews with status (review-failed or approved).
func submittedStatus(t *testing.T, work, mode, body, status string) *runstate.Run {
	t.Helper()
	script := string(filledTemplate(t)) + body
	if name, _, ok := strings.Cut(mode, "="); ok {
		script = strings.Replace(script, name+"=0", mode, 1)
	}
	writeFile(t, filepath.Join(work, "michael", "devops", runner.Submissions, "main", "0123456789abcdef.sh"), []byte(script), 0o600)
	var run *runstate.Run
	eventually(t, "the submission reviewed: "+status, func() bool {
		runs, err := runstate.List(work, ns, "main", auto(work), nil)
		if err == nil && len(runs) == 1 && runs[0].AskStatus() == status {
			run = runs[0]
		}
		return run != nil
	})
	return run
}

// answerOf decodes the answer document at path.
func answerOf(t *testing.T, path string) *doc.Answer {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var a doc.Answer
	if err := doc.Decode(b, &a); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return &a
}

// outputOf is the output of every section of the answer document at path, joined.
func outputOf(t *testing.T, path string) string {
	t.Helper()
	var out string
	for _, s := range answerOf(t, path).Sections {
		out += s.Output
	}
	return out
}

// answerDoc is a valid answer document of run, as the AAA library writes it: one section with output, result ok.
func answerDoc(run, output string) []byte {
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: run},
		Sections: []doc.Section{{Title: "1. x\n", Output: output}}, Result: doc.ResultOK}
	return a.Encode()
}

func queue(t *testing.T, work, run, action string) {
	t.Helper()
	if err := request.Queue(work, ns, "main", run, action); err != nil {
		t.Fatalf("queue %s: %v", action, err)
	}
}

// settled waits until the run has no queued, starting or running request left and returns its .FAILED ones (action ->
// content).
func settled(t *testing.T, work, run string) map[string]string {
	t.Helper()
	failed := map[string]string{}
	eventually(t, "the requests of "+run+" carried out", func() bool {
		reqs, err := request.List(work, ns, "main", run)
		if err != nil {
			return false
		}
		clear(failed)
		for _, q := range reqs {
			switch q.State {
			case request.Failed:
				failed[q.Action] = q.Reason
			case request.Finished:
			default:
				return false
			}
		}
		return true
	})
	return failed
}

// finished reports whether the run's request for action is finished: carried out, and kept until Scrub.
func finished(work, run, action string) bool {
	return exists(filepath.Join(work, request.RunDir(ns, "main", run), action+"."+request.Finished))
}

func load(t *testing.T, work, run string) *runstate.Run {
	t.Helper()
	r, err := runstate.Load(work, ns, "main", run, auto(work), nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// auto is the global config's reviewers.automatic.
func auto(work string) []string {
	g, err := config.LoadGlobal(work, env)
	if err != nil {
		return nil
	}
	return g.Reviewers.Automatic
}

func eventList(t *testing.T, work string) []string {
	t.Helper()
	var out []string
	if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
		out = append(out, e.Caller+" "+e.Action+" "+e.Outcome+" "+e.Rule)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// lastEvent is the reason of the namespace's last event action ("" for none).
func lastEvent(t *testing.T, work, action string) string {
	t.Helper()
	var reason string
	if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
		if e.Action == action {
			reason = e.Reason
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return reason
}

func TestExec(t *testing.T) {
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(execCfg), 0o600)
	t.Setenv("RUNNER_TEST_TOKEN", "config-secret")
	t.Setenv("PLINTH_TOKEN", "agent-secret")
	t.Setenv("AAA_QUIET", "0")
	dlpDown := execEnabled(t, work)
	start(t, Options{Work: work})
	// The script reports what it sees, leaves a secret in its answer, writes a state file itself and leaves a process
	// behind.
	r := submitted(t, work, `say "env: cfg=${RUNNER_TEST_TOKEN-unset} quiet=$(env | grep -c '^A[A]A_QUIET=1$') prefixed=$(env | grep -c '^PL[I]NTH_')"
say "db password=hunter2"
echo forged > override.dlp.md
sleep 60.1234 &
setsid sleep 60.5678 &
`)
	run, dir := r.Name, filepath.Join(work, runstate.RunDir(ns, "main", r.Name))

	// Exec of a run whose review failed needs override.review: the request fails with the rule.
	queue(t, work, run, decide.Exec)
	if f := settled(t, work, run); !strings.Contains(f[decide.Exec], "Rule: review.failed") {
		t.Fatalf("exec without override: %v", f)
	}
	// While the DLP scan can't ask the model, the answer stays unscanned and the run running; it is scanned once the
	// model answers.
	dlpDown.Store(true)
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	unavailable := func() int {
		n := 0
		for _, e := range eventList(t, work) {
			if e == "runner dlp.scan unavailable dlp.unavailable" {
				n++
			}
		}
		return n
	}
	eventually(t, "dlp.scan unavailable", func() bool { return unavailable() > 0 })
	time.Sleep(3 * time.Second) // the scan starts again every second
	if r := load(t, work, run); r.AnswerStatus() != "scanning" || r.AnswerFile() != "answer.new.yml" ||
		!exists(filepath.Join(work, request.RunDir(ns, "main", run), decide.Exec+"."+request.Running)) || unavailable() != 1 {
		t.Fatalf("while the model can't be asked: answer %s, files %v, %d dlp.scan unavailable", r.AnswerStatus(), r.Files, unavailable())
	}
	dlpDown.Store(false)
	if f := settled(t, work, run); len(f) != 1 || !strings.Contains(f[decide.Exec], "Rule: review.failed") {
		t.Fatalf("override and exec: %v (the earlier .FAILED stays)", f)
	}
	if !finished(work, run, decide.OverrideReview) || !finished(work, run, decide.Exec) {
		t.Fatal("override.review and exec are not finished")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "override.review.md"))
	if !strings.Contains(string(b), "review(s) security") || !strings.Contains(string(b), "Event: seq ") {
		t.Errorf("override.review.md = %q", b)
	}

	// The answer was scanned: fixed patterns masked, the original kept in raw/, DLP-FAIL: the model's replies don't fit.
	r = load(t, work, run)
	if r.AnswerStatus() != "dlp-failed" || r.AskStatus() != "ran" {
		t.Fatalf("after exec: ask %s, answer %s, files %v", r.AskStatus(), r.AnswerStatus(), r.Files)
	}
	answer, _ := os.ReadFile(filepath.Join(dir, "answer.DLP-FAIL.yml"))
	a := answerOf(t, filepath.Join(dir, "answer.DLP-FAIL.yml"))
	for _, want := range []string{"env: cfg=unset quiet=1 prefixed=0\n", "db password=<masked>\n"} {
		if !strings.Contains(a.Sections[len(a.Sections)-1].Output, want) {
			t.Errorf("answer: no %q in\n%s", want, answer)
		}
	}
	if line := runstate.DLPLine(answer); !strings.HasPrefix(line, "DLP scan: FAILED - Ollama (model dlp): the model's answer is not the expected JSON") ||
		!strings.HasSuffix(line, "Fixed patterns: 1 mask(s).") || a.Result != doc.ResultOK {
		t.Errorf("answer: DLP line %q, result %q", line, a.Result)
	}
	raw, _ := os.ReadFile(filepath.Join(work, "michael", "devops", runner.Raw, "main", run, "answer.new.yml"))
	if !strings.Contains(string(raw), "db password=hunter2") || strings.Contains(string(raw), "status:") {
		t.Errorf("raw original = %q", raw)
	}
	if lib, _ := os.ReadFile(filepath.Join(work, "michael", "devops", runner.Raw, "main", run, runner.LibFile)); !bytes.Equal(lib, plain.ControllerLib(container.BinPath)) {
		t.Errorf("the run's AAA library = %q", lib)
	}
	// The state file the script wrote is moved out; what it left running is gone.
	if forged, err := os.ReadFile(filepath.Join(work, "michael", "devops", runner.Raw, "main", run, "unexpected", "override.dlp.md")); err != nil || string(forged) != "forged\n" {
		t.Errorf("unexpected/override.dlp.md: %q, %v", forged, err)
	}
	if r.Facts.DLPOverridden {
		t.Error("the script's own override.dlp.md counts")
	}
	// What it left running is gone, in its process group or not (setsid): the container ends with its script, and
	// everything in it.
	eventually(t, "the script's background process killed", func() bool { return len(sleepers("60.1234")) == 0 })
	eventually(t, "the process that left the group killed", func() bool { return len(sleepers("60.5678")) == 0 })

	// Done needs override.dlp after a failed scan; then the answer is released.
	queue(t, work, run, decide.Done)
	if f := settled(t, work, run); !strings.Contains(f[decide.Done], "Rule: dlp.failed") {
		t.Fatalf("done without override: %v", f)
	}
	queue(t, work, run, decide.OverrideDLP)
	queue(t, work, run, decide.Done)
	settled(t, work, run)
	if !finished(work, run, decide.OverrideDLP) || !finished(work, run, decide.Done) {
		t.Fatal("override.dlp and done are not finished")
	}
	if r = load(t, work, run); r.AnswerStatus() != "released" || !r.Facts.DLPOverridden {
		t.Errorf("after done: %s, %+v", r.AnswerStatus(), r.Facts)
	}
	// Exec once more is refused: it has an answer, so it is done.
	queue(t, work, run, decide.Exec)
	if f := settled(t, work, run); !strings.Contains(f[decide.Exec], "is done: it has an answer") {
		t.Errorf("second exec: %v", f)
	}

	// Scrub deletes the run, its original and its requests; the event log keeps every step.
	queue(t, work, run, decide.Scrub)
	eventually(t, "the run scrubbed", func() bool {
		return !exists(dir) && !exists(filepath.Join(work, "michael", "devops", runner.Raw, "main", run)) &&
			!exists(filepath.Join(work, request.RunDir(ns, "main", run)))
	})
	got := eventList(t, work)
	for _, want := range []string{"user exec require-approval review.failed", "user override.review allow allow", "user exec allow allow", "runner exec.start ok allow",
		"runner exec.end answered allow", "runner dlp.scan failed dlp.failed", "user done require-approval dlp.failed",
		"user override.dlp allow allow", "user done allow allow", "user exec deny state.valid", "user scrub allow allow"} {
		if !slices.Contains(got, want) {
			t.Errorf("no event %q in\n%s", want, strings.Join(got, "\n"))
		}
	}
	if _, err := events.Verify(work, filepath.Join("michael", "devops")); err != nil {
		t.Error(err)
	}
}

func TestExecTimeout(t *testing.T) {
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(globalCfg+"  runner:\n    exec-timeout-minutes: 1\n"), 0o600)
	execEnabled(t, work)
	// 10 s: the timeout counts from the podman client's start, and a busy host (go test ./... runs packages side by
	// side) has taken over 5 s to start the container (fleet-ridge-19).
	start(t, Options{Work: work, ExecTimeoutUnit: 10 * time.Second, KillGrace: time.Second})
	// It ignores SIGTERM, so SIGKILL ends it (the word trap in two pieces: ask lint refuses a script's own trap).
	r := submitted(t, work, "t=tr\nt+=ap\n\"$t\" '' TERM\n: > started\nsleep 30\n")
	queue(t, work, r.Name, decide.OverrideReview)
	queue(t, work, r.Name, decide.Exec)
	settled(t, work, r.Name)
	if !exists(filepath.Join(work, runstate.RunDir(ns, "main", r.Name), "started")) {
		t.Fatalf("the script never ran before the timeout: %s, log:\n%s", load(t, work, r.Name).AskStatus(), logText(work))
	}
	if r = load(t, work, r.Name); r.AskStatus() != "no-answer" {
		t.Errorf("after the timeout: %s, files %v", r.AskStatus(), r.Files)
	}
	got := eventList(t, work)
	if !slices.Contains(got, "runner exec.end no answer allow") {
		t.Errorf("no exec.end no answer in %v", got)
	}
	if !strings.Contains(logText(work), "stopped after 10s (plinth.runner.exec-timeout-minutes): exit 137 (signal 9, killed)") {
		t.Errorf("log:\n%s", logText(work))
	}
}

// TestLeftovers: what a stopped controller left is finished at the next start.
func TestLeftovers(t *testing.T) {
	work := setup(t)
	enabled(t, work, "main")
	nsDir := filepath.Join(work, "michael", "devops")
	script := filledTemplate(t)
	sum := sha256.Sum256(script)
	mk := func(run string, files ...string) {
		writeFile(t, filepath.Join(nsDir, "runs", "main", run, "ask.sh"), script, 0o700)
		writeFile(t, filepath.Join(nsDir, "runs", "main", run, "ask.sh.sha256"), []byte(hex.EncodeToString(sum[:])+"  ask.sh\n"), 0o600)
		writeFile(t, filepath.Join(nsDir, "runs", "main", run, "review.security.md"), []byte("## Verdict\nOK to run\n"), 0o600)
		for _, f := range files {
			body := []byte("token=abc123\n")
			if strings.HasSuffix(f, ".new.yml") {
				body = answerDoc(run, "token=abc123\n")
			}
			writeFile(t, filepath.Join(nsDir, "runs", "main", run, f), body, 0o600)
		}
		writeFile(t, filepath.Join(nsDir, request.Dir, "main", run, "exec.running"), nil, 0o600)
	}
	mk("keen-mink", runstate.Running)  // the controller stopped while it ran
	mk("bold-acorn", "answer.new.yml") // the controller stopped before the scan
	mk("dim-moth", "answer.new.yml")   // the same, an answer that isn't a valid document: INVALID, never scanned
	writeFile(t, filepath.Join(nsDir, "runs", "main", "dim-moth", "answer.new.yml"), []byte("token=abc123\n"), 0o600)
	writeFile(t, filepath.Join(nsDir, "runs", "main", ".scrub.gone-owl", "ask.sh"), script, 0o700)
	writeFile(t, filepath.Join(nsDir, runner.Raw, "main", "gone-owl", "answer.new.yml"), nil, 0o600)
	// Scripts that wrote state files themselves before the controller stopped: moved out at the next start, with the
	// names their exec.start recorded, or, without such a list, all but what a run holds before its start.
	evlog, err := events.Open(work, filepath.Join("michael", "devops"), 10000)
	if err != nil {
		t.Fatal(err)
	}
	logged := func(run, action, outcome, reason string) {
		t.Helper()
		if _, err := evlog.Append(events.Event{Caller: runner.Caller, Action: action, Target: "main/" + run, Outcome: outcome, Rule: "allow", Reason: reason}); err != nil {
			t.Fatal(err)
		}
	}
	started := "ask.sh sha256 x" + startFiles + "ask.sh ask.sh.sha256 review.security.md"
	// proud-otter's answer would be released without a scan or Done; brave-lynx's DLP scan overridden without the user.
	mk("proud-otter", runstate.Running, "answer.released.yml")
	logged("proud-otter", "exec.start", "ok", started)
	mk("brave-lynx", "answer.new.yml", "override.dlp.md")
	logged("brave-lynx", "exec.start", "ok", started)
	// amber-crane has no exec.start that names the files; quiet-wren's finishExec got as far as exec.end, so its
	// DLP-FAIL answer is the controller's own.
	mk("amber-crane", runstate.Running, "answer.dlp-ok.yml")
	mk("quiet-wren", "answer.DLP-FAIL.yml")
	logged("quiet-wren", "exec.start", "ok", started)
	logged("quiet-wren", "exec.end", "answered", "exit 0")
	// lone-heron's request is .running, so its script started, though it left nothing in the run directory (it removed
	// running.yml itself): finished without an answer, not carried out again, running.yml written back. Its failed
	// review can't be overridden any more, though the loop decides that before it finishes the exec.
	mk("lone-heron", "review.dlp.FAILED.md")
	writeFile(t, filepath.Join(nsDir, request.Dir, "main", "lone-heron", decide.OverrideReview+"."+request.Queued), nil, 0o600)

	start(t, Options{Work: work})
	for _, run := range []string{"keen-mink", "dim-moth", "proud-otter", "amber-crane", "quiet-wren", "lone-heron"} {
		settled(t, work, run)
		if !finished(work, run, decide.Exec) {
			t.Errorf("%s: the exec request is not finished", run)
		}
	}
	// Without Ollama the DLP scan can't ask the model: bold-acorn's and brave-lynx's answers stay unscanned, their execs
	// unfinished, until it can.
	eventually(t, "dlp.scan unavailable of bold-acorn and brave-lynx", func() bool {
		n := 0
		for _, e := range eventList(t, work) {
			if e == "runner dlp.scan unavailable dlp.unavailable" {
				n++
			}
		}
		return n == 2
	})
	for _, run := range []string{"bold-acorn", "brave-lynx"} {
		if r := load(t, work, run); r.AnswerStatus() != "scanning" || finished(work, run, decide.Exec) {
			t.Errorf("%s: %s, exec finished %v", run, r.AnswerStatus(), finished(work, run, decide.Exec))
		}
	}
	if r := load(t, work, "keen-mink"); r.AskStatus() != "no-answer" {
		t.Errorf("keen-mink: %s", r.AskStatus())
	}
	if a := answerOf(t, filepath.Join(nsDir, "runs", "main", "bold-acorn", "answer.new.yml")); len(a.Sections) != 1 || a.Sections[0].Output != "token=abc123\n" {
		t.Errorf("bold-acorn's answer = %+v, want it as the script left it", a)
	}
	if r := load(t, work, "dim-moth"); r.AnswerStatus() != "invalid" || r.AnswerFile() != "answer.INVALID.yml" {
		t.Errorf("dim-moth: %s, files %v", r.AnswerStatus(), r.Files)
	}
	if b, _ := os.ReadFile(filepath.Join(nsDir, "runs", "main", "dim-moth", "answer.INVALID.yml")); string(b) != "token=abc123\n" {
		t.Errorf("dim-moth's INVALID answer = %q, want it as the script left it", b)
	}

	for run, forged := range map[string]string{"proud-otter": "answer.released.yml", "brave-lynx": "override.dlp.md", "amber-crane": "answer.dlp-ok.yml"} {
		if !exists(filepath.Join(nsDir, runner.Raw, "main", run, "unexpected", forged)) || exists(filepath.Join(nsDir, "runs", "main", run, forged)) {
			t.Errorf("%s: its %s was not moved to unexpected/", run, forged)
		}
	}
	if r := load(t, work, "proud-otter"); r.Facts.Answer != "" || r.AskStatus() != "no-answer" {
		t.Errorf("proud-otter: answer %q, %s", r.Facts.Answer, r.AskStatus())
	}
	if r := load(t, work, "brave-lynx"); r.AnswerStatus() != "scanning" || r.Facts.DLPOverridden {
		t.Errorf("brave-lynx: %s, overridden %v", r.AnswerStatus(), r.Facts.DLPOverridden)
	}
	if r := load(t, work, "amber-crane"); r.AskStatus() != "no-answer" || r.Reviews["security"] != "ok" {
		t.Errorf("amber-crane: %s, reviews %v (review.security.md is the controller's: it stays)", r.AskStatus(), r.Reviews)
	}
	if r := load(t, work, "quiet-wren"); r.AnswerStatus() != "dlp-failed" {
		t.Errorf("quiet-wren: %s, files %v", r.AnswerStatus(), r.Files)
	}
	eventually(t, "the scrub finished", func() bool {
		return !exists(filepath.Join(nsDir, "runs", "main", ".scrub.gone-owl")) && !exists(filepath.Join(nsDir, runner.Raw, "main", "gone-owl"))
	})
	// One exec.end per run, quiet-wren's the one from before the start; no run is started again.
	ends, starts := map[string][]string{}, map[string]int{}
	if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
		run := strings.TrimPrefix(e.Target, "main/")
		switch e.Action {
		case "exec.end":
			ends[run] = append(ends[run], e.Outcome+": "+e.Reason)
		case "exec.start":
			starts[run]++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]string{"keen-mink": "no answer: ", "bold-acorn": "answered: ", "dim-moth": "answered: ", "proud-otter": "no answer: ", "brave-lynx": "answered: ",
		"amber-crane": "no answer: ", "quiet-wren": "answered: exit 0", "lone-heron": "no answer: "} {
		if len(ends[run]) != 1 || !strings.HasPrefix(ends[run][0], want) {
			t.Errorf("%s: exec.end %q, want one %q...", run, ends[run], want)
		}
	}
	if starts["lone-heron"] != 0 || load(t, work, "lone-heron").Facts.Answer != "" {
		t.Errorf("lone-heron was carried out again: %d exec.start", starts["lone-heron"])
	}
	if r := load(t, work, "lone-heron"); r.AskStatus() != "no-answer" || !r.Has(runstate.Running) {
		t.Errorf("lone-heron: %s, files %v; want no-answer, running.yml written back", r.AskStatus(), r.Files)
	}
	if len(ends["lone-heron"]) == 1 && !strings.Contains(ends["lone-heron"][0], "it removed running.yml itself, written back") {
		t.Errorf("lone-heron's exec.end doesn't say running.yml was written back: %q", ends["lone-heron"])
	}
	if f := settled(t, work, "lone-heron"); !strings.Contains(f[decide.OverrideReview], "Rule: state.valid") {
		t.Errorf("lone-heron's override.review after the start: %v", f)
	}
	if len(ends["proud-otter"]) == 1 && !strings.Contains(ends["proud-otter"][0], "it wrote answer.released.yml itself, moved to raw/main/proud-otter/unexpected/") {
		t.Errorf("proud-otter's exec.end doesn't name the moved file: %q", ends["proud-otter"])
	}
	if got := eventList(t, work); !slices.Contains(got, "runner dlp.scan invalid answer.invalid") {
		t.Errorf("no dlp.scan invalid in %v", got)
	}
}

// TestExecStop: a controller stopped while a script runs finishes that exec before it ends: the state file the script wrote
// itself is moved out, exec.end recorded, the request deleted, so nothing is left for the next start. controller
// stop refuses while it runs and stops it with force.
func TestExecStop(t *testing.T) {
	work := setup(t)
	execEnabled(t, work)
	_, stop := start(t, Options{Work: work})
	// The name in two pieces: ask lint refuses a script that names an answer file, which the controller moves out anyway.
	r := submitted(t, work, "n=answer.released\necho forged > \"$n.yml\"\nsleep 30\n")
	dir := filepath.Join(work, runstate.RunDir(ns, "main", r.Name))
	queue(t, work, r.Name, decide.OverrideReview)
	queue(t, work, r.Name, decide.Exec)
	eventually(t, "the script running", func() bool { return exists(filepath.Join(dir, "answer.released.yml")) })
	// controller stop: refused while the script runs, naming it; with force, as Ctrl-C.
	if st, err := Query(work); err != nil || !slices.Equal(st.Running, []string{"michael/devops main/" + r.Name}) {
		t.Errorf("status while it runs: %+v, %v", st, err)
	}
	var refused *StopRefused
	if err := RequestStop(work, false); !errors.As(err, &refused) || refused.Reason != "still running: michael/devops main/"+r.Name {
		t.Errorf("a stop while it runs: %v", err)
	}
	if running, _ := Running(work); !running {
		t.Fatal("a refused stop stopped the controller")
	}
	if err := RequestStop(work, true); err != nil {
		t.Fatal(err)
	}
	// The script gets SIGTERM, which bash holds while sleep runs, then SIGKILL killGrace (30 s) later: more than eventually waits.
	for end := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if running, _ := Running(work); !running {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the controller still holds the lock a minute after a forced stop")
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(work, "michael", "devops", runner.Raw, "main", r.Name, "unexpected", "answer.released.yml")) || exists(filepath.Join(dir, "answer.released.yml")) {
		t.Error("the script's answer.released.yml was not moved to unexpected/ at the stop")
	}
	if r = load(t, work, r.Name); r.Facts.Answer != "" || r.AskStatus() != "no-answer" {
		t.Errorf("after the stop: answer %q, %s", r.Facts.Answer, r.AskStatus())
	}
	if !finished(work, r.Name, decide.Exec) {
		t.Error("the exec request is not finished after the stop")
	}
	if got := eventList(t, work); !slices.Contains(got, "runner exec.end no answer allow") {
		t.Errorf("no exec.end no answer in %v", got)
	}
}

// TestExecRemovesRunning: a script that removes its running.yml still runs as far as anyone asks (the controller's
// record, not the file), so its run can't be scrubbed meanwhile; once it has ended without an answer (the AAA library
// writes no new one), running.yml is back: the run ran, without an answer; it may run again (TestExecAgain).
func TestExecRemovesRunning(t *testing.T) {
	work := setup(t)
	execEnabled(t, work)
	start(t, Options{Work: work})
	r := submitted(t, work, "n=running\nrm -f \"$n.yml\"\nfor i in $(seq 400); do [[ -e go ]] && break; sleep 0.05; done\nexit 0\n")
	run, dir := r.Name, filepath.Join(work, runstate.RunDir(ns, "main", r.Name))
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	// exec.running comes after the controller wrote running.yml: without it now, the script removed it.
	eventually(t, "the script running, its running.yml removed", func() bool {
		return exists(filepath.Join(work, request.RunDir(ns, "main", run), decide.Exec+"."+request.Running)) && !exists(filepath.Join(dir, runstate.Running))
	})
	live, err := runstate.Load(work, ns, "main", run, []string{"security"}, Live(work, ns))
	if err != nil {
		t.Fatal(err)
	}
	if live.AskStatus() != "running" {
		t.Errorf("while it runs: %s, want running", live.AskStatus())
	}
	queue(t, work, run, decide.Scrub)
	eventually(t, "the scrub refused", func() bool {
		return exists(filepath.Join(work, request.RunDir(ns, "main", run), decide.Scrub+"."+request.Failed))
	})
	writeFile(t, filepath.Join(dir, "go"), nil, 0o600) // the script ends, without an answer
	f := settled(t, work, run)
	if !strings.Contains(f[decide.Scrub], "Rule: state.valid") {
		t.Errorf("scrub while it ran: %v", f)
	}
	if r = load(t, work, run); r.AskStatus() != "no-answer" || !r.Has(runstate.Running) {
		t.Errorf("after it ended: %s, files %v; want no-answer, running.yml written back", r.AskStatus(), r.Files)
	}
	var end string
	if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
		if e.Action == "exec.end" {
			end = e.Outcome + ": " + e.Reason
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(end, "no answer: ") || !strings.Contains(end, "it removed running.yml itself, written back") {
		t.Errorf("exec.end = %q", end)
	}
}

// TestExecAgain: only an answer says a run is done. A run whose exec ended without one runs again, as attempt 2:
// exec.start says so, what attempt 1 left in running.yml is kept as raw/<agent>/<RUN>/running.1.yml, and a state file
// attempt 2 wrote itself goes to unexpected.2/; once the run has an answer, exec is refused.
func TestExecAgain(t *testing.T) {
	work := setup(t)
	execEnabled(t, work)
	start(t, Options{Work: work})
	r := submitted(t, work, "say partial\n[[ -e again ]] || kill -9 $$\n: > report.md\n")
	run, dir := r.Name, filepath.Join(work, runstate.RunDir(ns, "main", r.Name))
	raw := filepath.Join(work, "michael", "devops", runner.Raw, "main", run)
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	settled(t, work, run)
	eventually(t, "the exec finished", func() bool { return finished(work, run, decide.Exec) })
	if r = load(t, work, run); r.AskStatus() != "no-answer" {
		t.Fatalf("attempt 1: %s, files %v; want no-answer", r.AskStatus(), r.Files)
	}
	writeFile(t, filepath.Join(dir, "again"), nil, 0o600)
	queue(t, work, run, decide.Exec)
	settled(t, work, run)
	eventually(t, "attempt 2 finished", func() bool { return load(t, work, run).Facts.Answer != "" })
	if s := lastEvent(t, work, "exec.start"); !strings.Contains(s, "; mode normal; attempt 2, after 1 without an answer; image ") {
		t.Errorf("exec.start = %q", s)
	}
	if b, err := os.ReadFile(filepath.Join(raw, "running.1.yml")); err != nil || !strings.Contains(string(b), "partial") {
		t.Errorf("attempt 1's running.yml: %q, %v", b, err)
	}
	if !exists(filepath.Join(raw, "unexpected.2", "report.md")) || exists(filepath.Join(dir, "report.md")) {
		t.Error("attempt 2's report.md was not moved to unexpected.2/")
	}
	if s := lastEvent(t, work, "exec.end"); !strings.Contains(s, "moved to raw/main/"+run+"/unexpected.2/") {
		t.Errorf("exec.end = %q", s)
	}
	if got := eventList(t, work); slices.Index(got, "runner exec.end no answer allow") < 0 || slices.Index(got, "runner exec.end answered allow") < 0 {
		t.Errorf("events:\n%s", strings.Join(got, "\n"))
	}
	queue(t, work, run, decide.Exec)
	if f := settled(t, work, run); !strings.Contains(f[decide.Exec], "is done: it has an answer") {
		t.Errorf("a third exec: %v", f)
	}
}

// TestLeftoverStarting: an exec request a stopped controller left .starting never started its script, so it is carried out
// again; the empty running.yml that controller had written goes first, or the run would count as ran.
func TestLeftoverStarting(t *testing.T) {
	work := setup(t)
	execEnabled(t, work)
	// Filled in as a submission does: the AAA library refuses a script without its RUN_NAME.
	script := bytes.Replace(filledTemplate(t), []byte(`RUN_NAME=""`), []byte(`RUN_NAME="calm-finch"`), 1)
	sum := sha256.Sum256(script)
	dir := filepath.Join(work, runstate.RunDir(ns, "main", "calm-finch"))
	writeFile(t, filepath.Join(dir, "ask.sh"), script, 0o700)
	writeFile(t, filepath.Join(dir, "ask.sh.sha256"), []byte(hex.EncodeToString(sum[:])+"  ask.sh\n"), 0o600)
	writeFile(t, filepath.Join(dir, "review.security.md"), []byte("## Verdict\nOK to run\n"), 0o600)
	writeFile(t, filepath.Join(dir, runstate.Running), nil, 0o600)
	writeFile(t, filepath.Join(work, request.RunDir(ns, "main", "calm-finch"), decide.Exec+"."+request.Starting), nil, 0o600)

	start(t, Options{Work: work})
	if f := settled(t, work, "calm-finch"); len(f) != 0 {
		t.Fatalf("carried out again: %v", f)
	}
	if r := load(t, work, "calm-finch"); r.AnswerStatus() != "dlp-failed" {
		t.Errorf("after the exec: %s, %s, files %v", r.AskStatus(), r.AnswerStatus(), r.Files)
	}
	if !finished(work, "calm-finch", decide.Exec) {
		t.Error("the exec request is not finished")
	}
	if got := eventList(t, work); !slices.Contains(got, "user exec allow allow") || !slices.Contains(got, "runner exec.end answered allow") {
		t.Errorf("events: %v", got)
	}
}

// TestOneBadRun: a run that can't be read, and a script that hard-links its answer, never stop the namespace: the bad
// run is skipped and logged, the refused answer moved aside, and the other runs go on (Scan 3).
func TestOneBadRun(t *testing.T) {
	work := setup(t)
	execEnabled(t, work)
	nsDir := filepath.Join(work, "michael", "devops")
	// al's odd-owl has two answer files, so it can't be read; a request for it fails on its own.
	for _, f := range []string{"ask.sh", "answer.dlp-ok.yml", "answer.released.yml"} {
		writeFile(t, filepath.Join(nsDir, "runs", "al", "odd-owl", f), nil, 0o600)
	}
	if err := request.Queue(work, ns, "al", "odd-owl", decide.Done); err != nil {
		t.Fatal(err)
	}
	start(t, Options{Work: work})

	// The script makes its answer a hard link to another file and is killed before the AAA library's EXIT trap (the
	// name in two pieces: ask lint refuses a script that names an answer file).
	r := submitted(t, work, "echo x > a.txt\nn=answer.new\nln a.txt \"$n.yml\"\nkill -9 $$\n")
	queue(t, work, r.Name, decide.OverrideReview)
	queue(t, work, r.Name, decide.Exec)
	if f := settled(t, work, r.Name); len(f) != 0 {
		t.Fatalf("override and exec: %v", f)
	}
	if r = load(t, work, r.Name); r.Facts.Answer != "" || r.AskStatus() != "no-answer" {
		t.Errorf("after exec: answer %q, %s, files %v", r.Facts.Answer, r.AskStatus(), r.Files)
	}
	if !exists(filepath.Join(nsDir, runner.Raw, "main", r.Name, "unexpected", "answer.new.yml")) {
		t.Error("the hard-linked answer was not moved to unexpected/")
	}
	if got := eventList(t, work); !slices.Contains(got, "runner exec.end answer refused allow") {
		t.Errorf("no exec.end answer refused in %v", got)
	}
	eventually(t, "odd-owl's request failed and logged", func() bool {
		reqs, err := request.List(work, ns, "al", "odd-owl")
		return err == nil && len(reqs) == 1 && reqs[0].State == request.Failed && logLines(work, 1, `msg="run is broken, skipped"`, "agent=al", "run=odd-owl")()
	})
}

// sleepers returns the PIDs of the live processes running "sleep <arg>", from /proc: a unique arg marks a test's own.
func sleepers(arg string) []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil && string(b) == "sleep\x00"+arg+"\x00" {
			out = append(out, pid)
		}
	}
	return out
}

// TestExecContainer: a run sees nothing of <work> but its own run directory, none of the controller's environment but
// plinth.exec.env-file's variables, only its own processes, no capability and no network; its ask.sh is the checked
// copy, read-only; a process it leaves behind, even outside its process group, ends with it. It runs the pinned image,
// and exec.start names it, its commit, and that the drift check couldn't reach Forgejo.
func TestExecContainer(t *testing.T) {
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(strings.Replace(execCfg, "  exec:\n", "  exec:\n    env-file: exec.env\n", 1)), 0o600)
	// The script gets <work>'s path from the env file: it ends in .plinth, and ask lint refuses a script that names it.
	writeFile(t, filepath.Join(work, "michael", "devops", "exec.env"), []byte("FROM_ENV_FILE=yes\nW="+work+"\n"), 0o600)
	t.Setenv("RUNNER_TEST_TOKEN", "config-secret")
	t.Setenv("HOST_ONLY", "from-the-host")
	execEnabled(t, work)
	start(t, Options{Work: work})
	r := submitted(t, work, `say "w: ${W:+set}"
say "config: $([[ -e "$W/config.yml" ]] && echo seen || echo hidden)"
say "requests: $([[ -e "$W/michael/devops/requests" ]] && echo seen || echo hidden)"
say "procs: $(ls /proc | grep -c '^[0-9]')"
say "uid: $(id -u)"
say "caps: $(grep CapEff /proc/self/status)"
say "write ask: $( (echo x >> ask.sh) 2> /dev/null && echo yes || echo refused)"
say "write usr: $( (: > /usr/x) 2> /dev/null && echo yes || echo refused)"
say "env: file=${FROM_ENV_FILE-unset} host=${HOST_ONLY-unset} cfg=${RUNNER_TEST_TOKEN-unset}"
say "net: $(ls /sys/class/net | tr '\n' ' ')"
setsid sleep 61.2468 &
`)
	queue(t, work, r.Name, decide.OverrideReview)
	queue(t, work, r.Name, decide.Exec)
	if f := settled(t, work, r.Name); len(f) != 0 {
		t.Fatalf("override and exec: %v", f)
	}
	dir := filepath.Join(work, runstate.RunDir(ns, "main", r.Name))
	answer := outputOf(t, filepath.Join(dir, "answer.DLP-FAIL.yml"))
	for _, want := range []string{"w: set", "config: hidden", "requests: hidden", "uid: " + strconv.Itoa(os.Getuid()),
		"caps: CapEff:\t0000000000000000", "write ask: refused", "write usr: refused", "env: file=yes host=unset cfg=unset",
		"net: lo \n"} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer: no %q in\n%s", want, answer)
		}
	}
	var procs int
	for _, l := range strings.Split(answer, "\n") {
		if n, ok := strings.CutPrefix(l, "procs: "); ok {
			procs, _ = strconv.Atoi(n)
		}
	}
	if procs == 0 || procs > 8 {
		t.Errorf("the run saw %d processes, want only its own few", procs)
	}
	eventually(t, "the process the script left outside its group killed", func() bool { return len(sleepers("61.2468")) == 0 })
	// The copy that ran is the checked ask.sh, kept out of the run's reach.
	ran, _ := os.ReadFile(filepath.Join(work, "michael", "devops", runner.Raw, "main", r.Name, "ask.sh"))
	if script, _ := os.ReadFile(filepath.Join(dir, "ask.sh")); len(ran) == 0 || string(ran) != string(script) {
		t.Errorf("raw/main/%s/ask.sh = %q", r.Name, ran)
	}
	pin := pinned(t, strings.Repeat("c", 40), strings.Repeat("e", 40))
	if start := lastEvent(t, work, "exec.start"); !strings.Contains(start, "; image "+pin.Ref+"; commit "+pin.Commit+"; drift not checked"+startFiles) {
		t.Errorf("exec.start %q", start)
	}
}

// TestLeftoverContainer: a container an earlier controller of this <work> left is removed at the start, before its exec
// is finished without an answer; one of another <work> stays.
func TestLeftoverContainer(t *testing.T) {
	img := needPodman(t)
	work := setup(t)
	enabled(t, work, "main")
	other := filepath.Join(t.TempDir(), "other")
	for _, w := range []string{work, other} {
		base := t.TempDir()
		writeFile(t, filepath.Join(base, "run", "ask.sh"), []byte("sleep 60\n"), 0o600)
		r := container.Run{Work: w, Name: ns.String() + "/main/old-wolf", Image: img, Dir: filepath.Join(base, "run"),
			Ask: filepath.Join(base, "run", "ask.sh"), Home: filepath.Join(base, "home"), Timeout: 2 * time.Minute, Memory: "512m"}
		t.Cleanup(func() { container.Remove(r.Work, r.Name) })
		cmd, err := container.Command(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
	}
	listed := func(w string) bool {
		left, err := container.Leftovers(w)
		return err == nil && slices.Contains(left, ns.String()+"/main/old-wolf")
	}
	eventually(t, "both containers running", func() bool { return listed(work) && listed(other) })
	start(t, Options{Work: work})
	eventually(t, "this <work>'s container removed", func() bool { return !listed(work) })
	if !listed(other) {
		t.Error("the container of another <work> was removed")
	}
	if !logLines(work, 1, `msg="removed a container left by an earlier controller"`, "container=michael/devops/main/old-wolf")() {
		t.Errorf("log:\n%s", logText(work))
	}
}

// TestExecNetwork: a network run waits for accept.network, which the controller records with the event's receipt; then it
// runs with a network of its own and no ssh: nothing of the user's ~/.ssh reaches it, and /aaa holds only the run
// directory, the AAA library and its text filter; exec.start names the mode. The script avoids the word ssh, which ask lint refuses.
func TestExecNetwork(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".ssh", "known_hosts"), []byte("host-key\n"), 0o600)
	writeFile(t, filepath.Join(home, ".ssh", "id_ed25519"), []byte("PERSONAL KEY\n"), 0o600)
	writeFile(t, filepath.Join(home, ".ssh", "config"), []byte("Host *\n  StrictHostKeyChecking no\n"), 0o600)
	prev := userHome
	t.Cleanup(func() { userHome = prev })
	userHome = func() (string, error) { return home, nil }
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(strings.Replace(globalCfg, "  exec:\n", "  exec:\n    network: true\n", 1)), 0o600)
	execEnabled(t, work)
	if _, err := exec.LookPath("pasta"); err != nil {
		if os.Getenv("PLINTH_TEST_PODMAN") == "required" {
			t.Fatal("PLINTH_TEST_PODMAN=required, but there is no pasta (package passt)")
		}
		t.Skip("no pasta")
	}
	start(t, Options{Work: work})
	r := submittedAs(t, work, "NETWORK=1", `say "net: $(ls /sys/class/net | tr '\n' ' ')"
say "home: $(ls -A "$HOME" | tr '\n' ' ')"
say "aaa: $(ls -A /aaa | tr '\n' ' ')"
say "dot-ssh mounts: $(grep -c -F .ssh /proc/self/mountinfo)"
`)
	run, dir := r.Name, filepath.Join(work, runstate.RunDir(ns, "main", r.Name))
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	if f := settled(t, work, run); !strings.Contains(f[decide.Exec], "Rule: exec.mode") || !strings.Contains(f[decide.Exec], "needs accept.network") {
		t.Fatalf("exec before accept.network: %v", f)
	}
	queue(t, work, run, decide.AcceptNetwork)
	queue(t, work, run, decide.Exec)
	if f := settled(t, work, run); len(f) != 1 {
		t.Fatalf("accept.network and exec: %v (the earlier .FAILED stays)", f)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, runstate.AcceptNetwork)); !strings.Contains(string(b), "a network of its own") || !strings.Contains(string(b), "Event: seq ") ||
		!strings.Contains(string(b), "; no ssh (nothing of ~/.ssh") {
		t.Errorf("accept.network.md = %q", b)
	}
	answer := outputOf(t, filepath.Join(dir, "answer.DLP-FAIL.yml"))
	for _, want := range []string{"home: \n", "aaa: lib.bash plinthctl run \n", "dot-ssh mounts: 0\n"} {
		if !strings.Contains(answer, want) {
			t.Errorf("no %q in the answer:\n%s", want, answer)
		}
	}
	if strings.Contains(string(answer), "net: lo \n") {
		t.Errorf("no network:\n%s", answer)
	}
	var start string
	if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
		if e.Action == "exec.start" {
			start = e.Reason
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(start, "; mode network; image ") || strings.Contains(start, "ssh") {
		t.Errorf("exec.start = %q", start)
	}
	if got := eventList(t, work); !slices.Contains(got, "user exec require-approval exec.mode") || !slices.Contains(got, "user accept.network allow allow") {
		t.Errorf("events:\n%s", strings.Join(got, "\n"))
	}
}

// TestSubmitMode: a script asking for a mode its namespace doesn't allow is rejected (ask.mode), as is one that is
// interactive but not privileged (ask lint).
func TestSubmitMode(t *testing.T) {
	work := setup(t)
	enabled(t, work, "main")
	start(t, Options{Work: work})
	script := string(filledTemplate(t))
	subs := filepath.Join(work, "michael", "devops", runner.Submissions, "main")
	writeFile(t, filepath.Join(subs, "00000000000000a1.sh"), []byte(strings.Replace(script, "NETWORK=0", "NETWORK=1", 1)), 0o600)
	writeFile(t, filepath.Join(subs, "00000000000000a2.sh"), []byte(strings.Replace(script, "INTERACTIVE=0", "INTERACTIVE=1", 1)), 0o600)
	for id, want := range map[string]string{"00000000000000a1": "Rule: ask.mode", "00000000000000a2": "INTERACTIVE=1 needs PRIVILEGED=1"} {
		reason := filepath.Join(work, "michael", "devops", runner.Rejected, "main", id, "reason.md")
		eventually(t, id+" rejected", func() bool { return exists(reason) })
		if b, _ := os.ReadFile(reason); !strings.Contains(string(b), want) {
			t.Errorf("%s: reason.md = %q, want %q", id, b, want)
		}
	}
}

// TestExecMemory: a run over plinth.exec.memory is killed in its container, and exec.end says so, from podman inspect.
func TestExecMemory(t *testing.T) {
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(strings.Replace(execCfg, "  exec:\n", "  exec:\n    memory: 64m\n", 1)), 0o600)
	execEnabled(t, work)
	start(t, Options{Work: work})
	r := submitted(t, work, `x=$(head -c 200000000 /dev/zero | tr '\0' a)
say "survived"
`)
	queue(t, work, r.Name, decide.OverrideReview)
	queue(t, work, r.Name, decide.Exec)
	if f := settled(t, work, r.Name); len(f) != 0 {
		t.Fatalf("override and exec: %v", f)
	}
	var end string
	if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
		if e.Action == "exec.end" {
			end = e.Reason
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(end, "exit 137 (signal 9, killed); over its memory limit (plinth.exec.memory 64m)") {
		t.Errorf("exec.end: %q", end)
	}
}

// TestExecCreateFails: a container podman can't create ends the run with podman's own message in exec.end, since no
// script ran. The failure here is the run's container name in use, by a container without the
// controller's labels: that container stays, since it isn't the run's.
func TestExecCreateFails(t *testing.T) {
	img := needPodman(t)
	work := setup(t)
	execEnabled(t, work)
	start(t, Options{Work: work})
	r := submitted(t, work, `say "ran"
`)
	name := container.ContainerName(work, ns.String()+"/main/"+r.Name)
	if out, err := exec.Command("podman", "create", "--name", name, img, "true").CombinedOutput(); err != nil {
		t.Fatalf("podman create: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("podman", "rm", "--force", "--ignore", name).Run() })
	queue(t, work, r.Name, decide.OverrideReview)
	queue(t, work, r.Name, decide.Exec)
	if f := settled(t, work, r.Name); len(f) != 0 {
		t.Fatalf("override and exec: %v", f)
	}
	if end := lastEvent(t, work, "exec.end"); !strings.HasPrefix(end, "the container wasn't created: Error: ") ||
		!strings.Contains(end, name) || !strings.Contains(end, "already in use") {
		t.Errorf("exec.end: %q", end)
	}
	if answers, _ := filepath.Glob(filepath.Join(work, runstate.RunDir(ns, "main", r.Name), "answer*")); len(answers) != 0 {
		t.Errorf("the script ran: %v", answers)
	}
	if err := exec.Command("podman", "container", "exists", name).Run(); err != nil {
		t.Errorf("the container that held the name is gone: %v", err)
	}
}

// TestExecPin: a run gets the pin only: none, one not on this machine, or one whose labels aren't the pin's fails the
// exec before the start, saying what to do.
func TestExecPin(t *testing.T) {
	good := pinned(t, strings.Repeat("c", 40), strings.Repeat("e", 40))
	work := setup(t)
	enabled(t, work, "main")
	start(t, Options{Work: work})
	r := submitted(t, work, "")
	queue(t, work, r.Name, decide.OverrideReview)
	for _, c := range []struct {
		pin  config.ImagePin
		want string
	}{
		{config.ImagePin{}, "no image pinned: plinthctl image publish -n michael/devops"},
		{config.ImagePin{Ref: testImages + "@sha256:" + strings.Repeat("0", 64), Commit: good.Commit, Tree: good.Tree},
			"image not pulled: run plinthctl image pull -n michael/devops"},
		{config.ImagePin{Ref: good.Ref, Commit: strings.Repeat("d", 40), Tree: good.Tree}, "its label org.opencontainers.image.revision is"},
		{config.ImagePin{Ref: good.Ref, Commit: good.Commit, Tree: strings.Repeat("d", 40)}, "its label plinth.tree is"},
	} {
		if c.pin != (config.ImagePin{}) {
			setPin(t, work, c.pin)
		}
		queue(t, work, r.Name, decide.Exec)
		if f := settled(t, work, r.Name); !strings.Contains(f[decide.Exec], c.want) {
			t.Errorf("pin %+v: %v, want %q", c.pin, f, c.want)
		}
	}
	if s := lastEvent(t, work, "exec.start"); s != "" {
		t.Errorf("a run started: %s", s)
	}
}

// TestExecDrift: before a run the controller compares the .plinth/ tree of the default branch on Forgejo with the pin's:
// no drift, or a drift, which goes into the event log (image.drift) and to stderr; the run gets the pin either way.
// The stand-in for Forgejo is package imagetest's.
func TestExecDrift(t *testing.T) {
	needPodman(t)
	f := imagetest.NewForge(t, ns, "plinth-token")
	head := f.Commit(t, "FROM scratch\n", "image")
	out, err := exec.Command("git", "-C", f.Bare, "rev-parse", "main:"+container.RepoDir).Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(string(out))
	cfg := strings.Replace(globalCfg, "https://127.0.0.1:9", f.URL, 1) + "forgejo-token: plinth-token\n"
	other := pinned(t, strings.Repeat("c", 40), strings.Repeat("e", 40))
	for _, c := range []struct {
		pin   config.ImagePin
		drift string // "" for none
	}{
		{pinned(t, head, tree), ""},
		{other, "main at " + head + " has .plinth tree " + tree + ", the pin has " + other.Tree},
	} {
		work := setup(t)
		writeFile(t, filepath.Join(work, config.FileName), []byte(cfg), 0o600)
		enabled(t, work, "main")
		setPin(t, work, c.pin)
		fakeModel(t, work)
		stderr, stop := start(t, Options{Work: work, ForgejoCAFile: f.CA})
		r := submitted(t, work, "")
		queue(t, work, r.Name, decide.OverrideReview)
		queue(t, work, r.Name, decide.Exec)
		if f := settled(t, work, r.Name); len(f) != 0 {
			t.Fatalf("override and exec: %v\n%s", f, logText(work))
		}
		want := "; no drift"
		if c.drift != "" {
			want = "; drift: " + c.drift
		}
		s := lastEvent(t, work, "exec.start")
		if !strings.Contains(s, "; image "+c.pin.Ref+"; commit "+c.pin.Commit+want+startFiles) {
			t.Errorf("exec.start %q, want %q", s, want)
		}
		if check, ok := runner.PinCheck(s, c.pin.Ref); !ok || check != strings.TrimPrefix(want, "; ") {
			t.Errorf("PinCheck = %q, %v", check, ok)
		}
		if _, ok := runner.PinCheck(s, other.Ref+"x"); ok {
			t.Error("PinCheck of another image")
		}
		if got := lastEvent(t, work, "image.drift"); got != c.drift {
			t.Errorf("image.drift %q, want %q", got, c.drift)
		}
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		if warned := strings.Contains(stderr.String(), ".plinth/ changed since the image was published (ns=michael/devops reason="+c.drift+")"); warned != (c.drift != "") {
			t.Errorf("stderr:\n%s", stderr.String())
		}
	}
}

// TestExecDLPModel: the DLP scan's model step runs in the background, and the lines the model flags are masked in
// answer.dlp-ok.yml. A stop cuts it short without a result: the exec request stays .running, and the next start scans
// the answer again.
func TestExecDLPModel(t *testing.T) {
	work := setup(t)
	stopGateway(t, work)
	fake := ollamatest.New(t)
	asked := make(chan struct{}, 10)
	fake.SetReply(func(ctx context.Context, c ollamatest.Chat) (int, string) {
		if !isDLP(c) {
			return http.StatusOK, `{"verdict":"do-not-run","findings":[]}`
		}
		asked <- struct{}{}
		<-ctx.Done() // until the controller gives up
		return http.StatusServiceUnavailable, "gone"
	})
	cfg := globalCfg + "ollama:\n  host: " + fake.URL + "\n  models:\n    review: rev\n    dlp: dlp\n  retry-seconds: 0\n"
	writeFile(t, filepath.Join(work, config.FileName), []byte(cfg), 0o600)
	execEnabled(t, work)
	_, stop := start(t, Options{Work: work})
	r := submitted(t, work, "say \"luks: Zq8vX2pLm9TtR4wE\"\n")
	dir, reqs := filepath.Join(work, runstate.RunDir(ns, "main", r.Name)), filepath.Join(work, request.RunDir(ns, "main", r.Name))
	queue(t, work, r.Name, decide.OverrideReview)
	queue(t, work, r.Name, decide.Exec)
	select {
	case <-asked:
	case <-time.After(30 * time.Second):
		t.Fatal("the model was never asked")
	}
	if !exists(filepath.Join(dir, "answer.new.yml")) || !exists(filepath.Join(reqs, decide.Exec+"."+request.Running)) {
		t.Fatal("while the model step runs: no answer.new.yml, or the exec request isn't .running")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	scanned := func() bool {
		return slices.ContainsFunc(eventList(t, work), func(e string) bool { return strings.Contains(e, " dlp.scan ") })
	}
	if !exists(filepath.Join(dir, "answer.new.yml")) || !exists(filepath.Join(reqs, decide.Exec+"."+request.Running)) || scanned() {
		t.Fatal("a scan the stop cut short was applied")
	}

	fake.SetReply(ollamatest.DLP(func(text string) string {
		if strings.Contains(text, "Zq8vX2pLm9") {
			return "encryption_key"
		}
		return ""
	}))
	start(t, Options{Work: work})
	if f := settled(t, work, r.Name); len(f) != 0 {
		t.Fatalf("after the restart: %v", f)
	}
	answer, _ := os.ReadFile(filepath.Join(dir, "answer.dlp-ok.yml"))
	if out := outputOf(t, filepath.Join(dir, "answer.dlp-ok.yml")); strings.Contains(string(answer), "Zq8vX2pLm9") || !strings.Contains(out, " masked by dlp-scan: encryption_key]\n") ||
		!strings.HasPrefix(runstate.DLPLine(answer), "DLP scan: ok (model dlp, 1 chunk(s), ") {
		t.Errorf("answer.dlp-ok.yml:\n%s", answer)
	}
	if raw, _ := os.ReadFile(filepath.Join(work, "michael", "devops", runner.Raw, "main", r.Name, "answer.new.yml")); !strings.Contains(string(raw), "Zq8vX2pLm9") {
		t.Errorf("raw original = %q", raw)
	}
	if got := eventList(t, work); !slices.Contains(got, "runner dlp.scan ok allow") {
		t.Errorf("no dlp.scan ok in %v", got)
	}
	if r = load(t, work, r.Name); r.AnswerStatus() != "dlp-ok" {
		t.Errorf("answer status %s", r.AnswerStatus())
	}
}

// TestExecReload: an answer that waits for the model holds back no reload of its namespace: its config.yml changes,
// the runner is attached again, and the new one scans the answer once the model is back, with no second dlp.scan
// unavailable for the exec.
func TestExecReload(t *testing.T) {
	work := setup(t)
	dlpDown := execEnabled(t, work)
	dlpDown.Store(true)
	start(t, Options{Work: work})
	run := submitted(t, work, "say hello\n").Name
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	unavailable := func() int {
		n := 0
		for _, e := range eventList(t, work) {
			if e == "runner dlp.scan unavailable dlp.unavailable" {
				n++
			}
		}
		return n
	}
	eventually(t, "dlp.scan unavailable", func() bool { return unavailable() > 0 })
	nsCfg := filepath.Join(work, "michael", "devops", config.FileName)
	b, err := os.ReadFile(nsCfg)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, nsCfg, append(b, "# changed while the answer waits for the model\n"...), 0o600)
	eventually(t, "the namespace attached again", func() bool {
		return logLines(work, 1, "msg=detached", "ns=michael/devops", `why="its config.yml changed"`)() &&
			logLines(work, 2, "msg=attached", "ns=michael/devops")()
	})
	time.Sleep(3 * time.Second) // the new runner's scans can't ask the model either
	if r := load(t, work, run); r.AnswerStatus() != "scanning" || finished(work, run, decide.Exec) || unavailable() != 1 {
		t.Fatalf("after the reload: answer %s, exec finished %v, %d dlp.scan unavailable", r.AnswerStatus(), finished(work, run, decide.Exec), unavailable())
	}
	dlpDown.Store(false)
	if f := settled(t, work, run); len(f) != 0 {
		t.Fatalf("once the model is back: %v", f)
	}
	if r := load(t, work, run); r.AnswerStatus() != "dlp-failed" || unavailable() != 1 {
		t.Errorf("once the model is back: answer %s, %d dlp.scan unavailable", r.AnswerStatus(), unavailable())
	}
}

// TestExecOutage: a real outage, Ollama refusing connections (not an HTTP status): the answer stays unscanned, one
// dlp.scan unavailable says why and since when (runner.ScanWaiting), and once Ollama is back the scan runs.
func TestExecOutage(t *testing.T) {
	work := setup(t)
	fake := ollamatest.New(t)
	fake.SetReply(func(_ context.Context, c ollamatest.Chat) (int, string) {
		if !isDLP(c) {
			return http.StatusServiceUnavailable, "down"
		}
		return http.StatusOK, "not the expected JSON"
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // refused until it listens again
	writeFile(t, filepath.Join(work, config.FileName), []byte(globalCfg+"ollama:\n  host: http://"+addr+"\n  models:\n    review: rev\n    dlp: dlp\n  retry-seconds: 0\n"), 0o600)
	stopGateway(t, work)
	execEnabled(t, work)
	start(t, Options{Work: work})
	run := submitted(t, work, "say hello\n").Name
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	eventually(t, "dlp.scan unavailable", func() bool { return slices.Contains(eventList(t, work), "runner dlp.scan unavailable dlp.unavailable") })
	w, err := runner.ScanWaiting(work, ns, "main", run)
	if r := load(t, work, run); err != nil || r.AnswerStatus() != "scanning" || w.Since == "" || !strings.Contains(w.Reason, "unreachable") {
		t.Fatalf("while Ollama refuses: answer %s, waiting %+v, %v", r.AnswerStatus(), w, err)
	}
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listening again on %s: %v", addr, err)
	}
	srv := &http.Server{Handler: fake.Config.Handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if f := settled(t, work, run); len(f) != 0 {
		t.Fatalf("once Ollama is back: %v", f)
	}
	n := 0
	for _, e := range eventList(t, work) {
		if e == "runner dlp.scan unavailable dlp.unavailable" {
			n++
		}
	}
	if r := load(t, work, run); r.AnswerStatus() != "dlp-failed" || n != 1 {
		t.Errorf("once Ollama is back: answer %s, %d dlp.scan unavailable", r.AnswerStatus(), n)
	}
}

// TestExecScanErrors: an error of an Ollama that took the request fails the scan at once (HTTP 500), and one that
// doesn't reply in time is asked ScanAttempts times in all, each timeout in the event log, then fails it.
func TestExecScanErrors(t *testing.T) {
	work := setup(t)
	var slow atomic.Bool
	fake := ollamatest.New(t)
	fake.SetReply(func(ctx context.Context, c ollamatest.Chat) (int, string) {
		switch {
		case !isDLP(c):
			return http.StatusServiceUnavailable, "down"
		case slow.Load():
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
			return http.StatusOK, `{"findings":[]}`
		}
		return http.StatusInternalServerError, "out of memory"
	})
	writeFile(t, filepath.Join(work, config.FileName), []byte(globalCfg+"ollama:\n  host: "+fake.URL+"\n  models:\n    review: rev\n    dlp: dlp\n  timeout-seconds: 1\n  retry-seconds: 0\n"), 0o600)
	stopGateway(t, work)
	execEnabled(t, work)
	start(t, Options{Work: work})
	outcomes := func(run string) []string {
		var out []string
		if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
			if e.Target == "main/"+run && e.Action == "dlp.scan" {
				out = append(out, e.Outcome+": "+e.Reason)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// HTTP 500: the scan ran and failed, at once.
	run := submitted(t, work, "say hello\n").Name
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	settled(t, work, run)
	if got := outcomes(run); load(t, work, run).AnswerStatus() != "dlp-failed" || len(got) != 1 || !strings.HasPrefix(got[0], "failed: Ollama: HTTP 500: out of memory") {
		t.Errorf("HTTP 500: %s, dlp.scan %q", load(t, work, run).AnswerStatus(), got)
	}

	// No reply within ollama.timeout-seconds: ScanAttempts attempts, then DLP-FAIL.
	slow.Store(true)
	before := len(fake.Chats())
	writeFile(t, filepath.Join(work, "michael", "devops", runner.Submissions, "main", "fedcba9876543210.sh"), []byte(string(filledTemplate(t))+"say again\n"), 0o600)
	var second string
	eventually(t, "the second submission stored", func() bool {
		runs, err := runstate.List(work, ns, "main", auto(work), nil)
		for _, r := range runs {
			if err == nil && r.Name != run && r.AskStatus() == "review-failed" {
				second = r.Name
			}
		}
		return second != ""
	})
	queue(t, work, second, decide.OverrideReview)
	queue(t, work, second, decide.Exec)
	settled(t, work, second)
	got := outcomes(second)
	dlpChats := 0
	for _, c := range fake.Chats()[before:] {
		if isDLP(c) {
			dlpChats++
		}
	}
	if len(got) != runner.ScanAttempts || !strings.HasPrefix(got[0], "timeout: Ollama: no reply within 1s (ollama.timeout-seconds); attempt 1 of 3") ||
		!strings.HasPrefix(got[1], "timeout: ") || !strings.HasPrefix(got[2], "failed: Ollama: no reply within 1s (ollama.timeout-seconds), 3 attempts") ||
		dlpChats != runner.ScanAttempts {
		t.Errorf("timeouts: dlp.scan %q, %d DLP chats", got, dlpChats)
	}
	if a := answerOf(t, filepath.Join(work, runstate.RunDir(ns, "main", second), "answer.DLP-FAIL.yml")); !strings.Contains(a.Status.DLP.Reason, ", 3 attempts") {
		t.Errorf("the answer's reason: %q", a.Status.DLP.Reason)
	}
}

// TestExecOutageAfterTimeout: the model can't be asked, then times out, then can't be asked again. The running
// runner records the second outage too, and the run says it waits for an unavailable model, not for a timed-out one;
// once the model replies, the scan passes.
func TestExecOutageAfterTimeout(t *testing.T) {
	work := setup(t)
	const down, slow, up = 0, 1, 2
	var model atomic.Int32
	fake := ollamatest.New(t)
	fake.SetReply(func(ctx context.Context, c ollamatest.Chat) (int, string) {
		switch {
		case !isDLP(c) || model.Load() == down:
			return http.StatusServiceUnavailable, "down"
		case model.Load() == slow:
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
		return http.StatusOK, `{"findings":[]}`
	})
	writeFile(t, filepath.Join(work, config.FileName), []byte(globalCfg+"ollama:\n  host: "+fake.URL+"\n  models:\n    review: rev\n    dlp: dlp\n  timeout-seconds: 1\n  retry-seconds: 0\n"), 0o600)
	stopGateway(t, work)
	execEnabled(t, work)
	start(t, Options{Work: work})
	scans := func(run string) (outcomes, times []string) {
		if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
			if e.Target == "main/"+run && e.Action == "dlp.scan" {
				outcomes, times = append(outcomes, e.Outcome), append(times, e.Time)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return outcomes, times
	}
	run := submitted(t, work, "say hello\n").Name
	queue(t, work, run, decide.OverrideReview)
	queue(t, work, run, decide.Exec)
	for _, step := range []struct {
		model int32
		want  []string
	}{
		{down, []string{"unavailable"}},
		{slow, []string{"unavailable", "timeout"}},
		{down, []string{"unavailable", "timeout", "unavailable"}},
	} {
		model.Store(step.model)
		eventually(t, fmt.Sprintf("dlp.scan %q", step.want), func() bool {
			got, _ := scans(run)
			return slices.Equal(got, step.want)
		})
	}
	_, times := scans(run)
	w, err := runner.ScanWaiting(work, ns, "main", run)
	if s := w.String(); err != nil || w.Since != times[2] || w.Timeouts != 1 ||
		!strings.HasPrefix(s, "waiting for the model: unavailable since "+times[2]+" (") || !strings.HasSuffix(s, "HTTP 503: down); 1 of 3 attempts timed out before") {
		t.Errorf("unavailable again: %+v, %q, %v", w, w.String(), err)
	}
	model.Store(up)
	if f := settled(t, work, run); len(f) != 0 {
		t.Fatalf("once the model replies: %v", f)
	}
	if got, _ := scans(run); load(t, work, run).AnswerStatus() != "dlp-ok" || len(got) != 4 || got[3] != "ok" {
		t.Errorf("once the model replies: answer %s, dlp.scan %q", load(t, work, run).AnswerStatus(), got)
	}
}

// fakeModel gives the controller an Ollama through its gateway, unless <work>/config.yml names one already: its reviews
// fail (HTTP 503), as without one, and its DLP replies never fit, so a scan runs and fails (DLP-FAIL). While the flag it
// returns is set, the DLP scan can't ask it either (HTTP 503): the answer stays unscanned.
func fakeModel(t *testing.T, work string) *atomic.Bool {
	t.Helper()
	down := &atomic.Bool{}
	p := filepath.Join(work, config.FileName)
	cfg, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(cfg, []byte("ollama:")) || bytes.Contains(cfg, []byte("\nollama:")) {
		return down
	}
	stopGateway(t, work)
	fake := ollamatest.New(t)
	fake.SetReply(func(_ context.Context, c ollamatest.Chat) (int, string) {
		if !isDLP(c) || down.Load() {
			return http.StatusServiceUnavailable, "down"
		}
		return http.StatusOK, "not the expected JSON"
	})
	cfg = append(cfg, "ollama:\n  host: "+fake.URL+"\n  models:\n    review: rev\n    dlp: dlp\n  retry-seconds: 0\n"...)
	writeFile(t, p, cfg, 0o600)
	return down
}

// isDLP reports whether a chat is the DLP scan's (the reviews have their own prompts).
func isDLP(c ollamatest.Chat) bool {
	return strings.HasPrefix(c.System, "You are a data-loss-prevention scanner")
}

// TestReviews: the automatic reviews run on Ollama in the background: of the script once it is stored, and of the
// answer once its scan passed, for the types whose prompt reviews answers; review.<type> reviews the script again and
// replaces the type's review.
func TestReviews(t *testing.T) {
	work := setup(t)
	stopGateway(t, work)
	fake := ollamatest.New(t)
	verdict := make(chan string, 1)
	verdict <- "ok-to-run"
	fake.SetReply(func(_ context.Context, c ollamatest.Chat) (int, string) {
		switch {
		case isDLP(c):
			return ollamatest.DLP(nil)(context.Background(), c)
		case strings.Contains(c.Prompt, "<<<BEGIN ANSWER>>>"):
			return http.StatusOK, `{"verdict":"ok-to-run","findings":[]}`
		}
		v := <-verdict
		verdict <- v
		return http.StatusOK, `{"verdict":"` + v + `","findings":[{"line":2,"severity":"low","text":"fine,\nreally"}]}`
	})
	cfg := strings.Replace(globalCfg, "  automatic: [security]\n", "    dlp:\n      definition: \"Review the answer between <<<BEGIN ANSWER>>> and <<<END ANSWER>>>.\"\n  automatic: [security, dlp]\n", 1) +
		"ollama:\n  host: " + fake.URL + "\n  models:\n    review: rev\n    dlp: dlp\n  retry-seconds: 0\n"
	writeFile(t, filepath.Join(work, config.FileName), []byte(cfg), 0o600)
	execEnabled(t, work)
	start(t, Options{Work: work})
	r := submittedStatus(t, work, "", "say \"ok\"\n", "approved")
	dir := filepath.Join(work, runstate.RunDir(ns, "main", r.Name))
	b, _ := os.ReadFile(filepath.Join(dir, "review.security.md"))
	if !strings.HasPrefix(string(b), "# Review security of ask.sh\n\n## Verdict\nOK to run\n\n## Findings\n- line 2 (low): fine, really\n") ||
		!strings.Contains(string(b), "on rev, ") {
		t.Errorf("review.security.md:\n%s", b)
	}
	// A review's file comes before its event, so the run can be approved while the event is still being written.
	eventually(t, "both reviews in the event log", func() bool {
		got := eventList(t, work)
		return slices.Contains(got, "runner review.security ok allow") && slices.Contains(got, "runner review.dlp ok allow")
	})

	queue(t, work, r.Name, decide.Exec)
	if f := settled(t, work, r.Name); len(f) != 0 {
		t.Fatalf("exec: %v", f)
	}
	eventually(t, "the answer reviewed by dlp", func() bool { return exists(filepath.Join(dir, "review.answer.dlp.md")) })
	if exists(filepath.Join(dir, "review.answer.security.md")) || exists(filepath.Join(dir, "review.answer.security.FAILED.md")) {
		t.Error("security, whose prompt doesn't review answers, reviewed the answer")
	}

	<-verdict
	verdict <- "do-not-run"
	queue(t, work, r.Name, decide.Review+".security")
	if f := settled(t, work, r.Name); len(f) != 0 {
		t.Fatalf("review.security: %v", f)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "review.security.md")); !strings.Contains(string(b), "## Verdict\nDo not run\n") {
		t.Errorf("after review.security:\n%s", b)
	}
	if got := eventList(t, work); !slices.Contains(got, "runner review.security failed review.failed") {
		t.Errorf("no failed review.security in %v", got)
	}
}
