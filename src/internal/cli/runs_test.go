package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/events"
	"plinth/internal/runner"
	"plinth/internal/workdir"
)

// fixtureRun writes runs/main/<run>/ with the given files below work's michael/devops.
func fixtureRun(t *testing.T, work, run string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(work, "michael", "devops", "runs", "main", run)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGetAndDescribe(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org") // automatic: [security]
	script := "#!/bin/bash\n# AAA script: check the disks\n# READ-ONLY\nRUN_NAME=\"keen-tapir-73\"\n"
	fixtureRun(t, work, "keen-tapir-73", map[string]string{"ask.sh": script, "review.security.FAILED.md": "no Ollama"})
	fixtureRun(t, work, "calm-otter", map[string]string{"ask.sh": script, "review.security.md": "## Verdict\nOK to run\n"})
	fixtureRun(t, work, "bold-birch", map[string]string{"ask.sh": script, "review.security.md": "ok", "answer.DLP-FAIL.yml": "masked"})
	fixtureRun(t, work, "bold-bison", map[string]string{"ask.sh": script, "review.security.md": "ok", "answer.FAIL.dlp-ok.yml": "the answer\n"})
	fixtureRun(t, work, "bold-crane", map[string]string{"ask.sh": script, "review.security.md": "ok", "answer.INVALID.yml": "not a document\n"})
	ns := []string{"-n", "michael/devops", "--work", work}

	code, out, errs := run(append([]string{"get", "ask"}, ns...)...)
	if code != exitOK || !strings.Contains(out, "keen-tapir-73") || !strings.Contains(out, "review-failed") || !strings.Contains(out, "approved") {
		t.Errorf("get ask: %d %q %q", code, out, errs)
	}
	if code, out, _ := run(append([]string{"get", "ans"}, ns...)...); code != exitOK || strings.Contains(out, "calm-otter") ||
		!strings.Contains(out, "dlp-failed") || !strings.Contains(out, "dlp-ok (gate)") || !strings.Contains(out, "invalid") {
		t.Errorf("get ans: %d %q", code, out)
	}
	if code, out, _ := run(append([]string{"get"}, ns...)...); code != exitOK || !strings.Contains(out, "ASK STATUS") || !strings.Contains(out, "ANSWER STATUS") {
		t.Errorf("get: %d %q", code, out)
	}
	// One script: only an approved one is printed.
	if code, out, errs := run(append(append([]string{"get", "ask"}, ns...), "keen-tapir-73")...); code != exitFail || out != "" || !strings.Contains(errs, "Run not approved (review-failed)") {
		t.Errorf("get ask of an unapproved run: %d %q %q", code, out, errs)
	}
	if code, out, _ := run(append(append([]string{"get", "ask"}, ns...), "calm-otter")...); code != exitOK || out != script {
		t.Errorf("get ask of an approved run: %d %q", code, out)
	}
	// One answer: only a passed (or overridden) scan is printed.
	if code, out, errs := run(append(append([]string{"get", "ans"}, ns...), "bold-birch")...); code != exitFail || out != "" || !strings.Contains(errs, "DLP scan failed") ||
		!strings.Contains(errs, " next: ") {
		t.Errorf("get ans of a failed scan: %d %q %q", code, out, errs)
	}
	if code, out, errs := run(append(append([]string{"get", "ans"}, ns...), "bold-crane")...); code != exitFail || out != "" || !strings.Contains(errs, "INVALID") ||
		!strings.Contains(errs, " next: ") {
		t.Errorf("get ans of an INVALID answer: %d %q %q", code, out, errs)
	}
	if code, out, _ := run(append(append([]string{"get", "answer"}, ns...), "bold-bison")...); code != exitOK || out != "the answer\n" {
		t.Errorf("get answer of a passed scan: %d %q", code, out)
	}
	if code, _, errs := run(append(append([]string{"get", "ans"}, ns...), "lost-owl")...); code != exitFail || !strings.Contains(errs, "no run lost-owl") {
		t.Errorf("get ans of no run: %d %q", code, errs)
	}
	code, out, _ = run(append(append([]string{"describe"}, ns...), "keen-tapir-73")...)
	for _, want := range []string{"Ask status:  review-failed", "Issue:       73", "AAA script: check the disks", "security     failed (automatic)", "review.security.FAILED.md"} {
		if code != exitOK || !strings.Contains(out, want) {
			t.Errorf("describe: no %q in\n%s", want, out)
		}
	}
	t.Chdir(t.TempDir()) // outside any checkout: -n is required
	for _, args := range [][]string{{"get", "ask", "--work", work}, {"describe", "-n", "michael/devops", "--work", work}, {"get", "ask", "-n", "michael/devops", "--work", work, "Bad"}} {
		if code, _, _ := run(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestAskLint(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := filepath.Join(t.TempDir(), "keen-tapir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "ask.sh")
	s := strings.Replace(string(workdir.AskTemplate), "# AAA script: <one line: what this round is for>", "# AAA script: check", 1)
	s = strings.Replace(s, "# READ-ONLY diagnostics - nothing is started, stopped or changed.   (or: CHANGES: <exact list>)", "# READ-ONLY diagnostics - nothing is started, stopped or changed.", 1)
	s = strings.Replace(s, "#   1 <section>  2 <section>  3 <section>", "#   1 disks  2 smart", 1)
	s = strings.Replace(s, `RUN_NAME=""`, `RUN_NAME="keen-tapir"`, 1)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run("ask", "lint", "--plinth", "--work", "/nonexistent/.work", p); code != exitOK || out != "" {
		t.Errorf("lint of a good script: %d %q %q", code, out, errs)
	}
	if err := os.WriteFile(p, []byte(s+"cd ..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("ask", "lint", "--plinth", p); code != exitFail || !strings.Contains(out, "relative path") {
		t.Errorf("lint --plinth of cd ..: %d %q", code, out)
	}
	// Plain AAA's template, filled in the same way: without --plinth, cd .. is fine.
	a := strings.Replace(string(workdir.AAATemplate), "# AAA script: <one line: what this round is for>", "# AAA script: check", 1)
	a = strings.Replace(a, "# READ-ONLY diagnostics - nothing is started, stopped or changed.   (or: CHANGES: <exact list>)", "# READ-ONLY diagnostics - nothing is started, stopped or changed.", 1)
	a = strings.Replace(a, "#   1 <section>  2 <section>  3 <section>", "#   1 disks  2 smart", 1)
	a = strings.Replace(a, `RUN_NAME="<adjective>-<noun>-<issue>"`, `RUN_NAME="keen-tapir"`, 1)
	if err := os.WriteFile(p, []byte(a+"cd ..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("ask", "lint", p); code != exitOK {
		t.Errorf("plain lint of cd ..: %d %q", code, out)
	}
	// A layout warning is advice: the lint still passes (exit 0) and says what is off.
	if err := os.WriteFile(p, []byte(a+"for p in a b; do\nsay \"$p\"\ndone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("ask", "lint", p); code != exitOK || !strings.Contains(out, p+": layout warning: line ") || !strings.Contains(out, "expected 2 spaces, found 0") {
		t.Errorf("plain lint of a flat loop: %d %q", code, out)
	}
	// A plinthctl call in a script with PRIVILEGED=0 fails in its container: a warning, which doesn't change the
	// exit code. The unfilled template has findings of its own; the warning is printed all the same.
	if err := os.WriteFile(p, append(append([]byte{}, workdir.AskTemplate...), "plinthctl status\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := run("ask", "lint", "--plinth", "--work", t.TempDir(), p); !strings.Contains(out, p+": plinthctl warning: line ") ||
		!strings.Contains(out, "plinthctl is not available in the run's container (PRIVILEGED=0)") {
		t.Errorf("plinth lint of a plinthctl call: %q", out)
	}
}

// TestGetAnsWaiting: an answer whose scan can't ask the model says so, with since when and why, in get ans, its list
// and describe.
func TestGetAnsWaiting(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org")
	fixtureRun(t, work, "calm-otter", map[string]string{"ask.sh": "#!/bin/bash\n", "review.security.md": "ok", "answer.new.yml": "x"})
	nsDir := filepath.Join("michael", "devops")
	if err := os.MkdirAll(filepath.Join(work, nsDir, events.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	log, err := events.Open(work, nsDir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(events.Event{Caller: runner.Caller, Action: "dlp.scan", Target: "main/calm-otter", Outcome: "unavailable", Rule: "dlp.unavailable",
		Reason: "the Ollama gateway isn't running (plinthctl ollama-mcp start); the answer stays answer.new.yml, scanned again once the model can be asked; original sha256 x"}); err != nil {
		t.Fatal(err)
	}
	ns := []string{"-n", "michael/devops", "--work", work}
	if code, out, errs := run(append(append([]string{"get", "ans"}, ns...), "calm-otter")...); code != exitFail || out != "" ||
		!strings.Contains(errs, "calm-otter: the answer is not scanned yet, waiting for the model: unavailable since ") ||
		!strings.Contains(errs, "(the Ollama gateway isn't running (plinthctl ollama-mcp start)) - not shown; next: ") {
		t.Errorf("get ans: %d %q %q", code, out, errs)
	}
	if code, out, _ := run(append([]string{"get", "ans"}, ns...)...); code != exitOK || !strings.Contains(out, "waiting-for-model") {
		t.Errorf("get ans list: %d %q", code, out)
	}
	if code, out, _ := run(append(append([]string{"describe"}, ns...), "calm-otter")...); code != exitOK || !strings.Contains(out, "Answer:      waiting for the model: unavailable since ") {
		t.Errorf("describe: %d %q", code, out)
	}
}
