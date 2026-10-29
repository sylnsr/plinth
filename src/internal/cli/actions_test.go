package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"plinth/internal/asklint"
	"plinth/internal/doc"
)

func TestRequests(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org") // automatic: [security]
	old := "#!/bin/bash\n# AAA script: check the disks\n# READ-ONLY\nAGENT_NAME=\"devops/main\"\nRUN_NAME=\"keen-tapir\"\nINTERACTIVE=0\nNETWORK=0\nPRIVILEGED=0\n"
	script := old + asklint.PlinthLibLine + "\n"
	sum := sha256.Sum256([]byte(script))
	stored := hex.EncodeToString(sum[:]) + "  ask.sh\n"
	oldSum := sha256.Sum256([]byte(old))
	fixtureRun(t, work, "keen-tapir", map[string]string{"ask.sh": script, "ask.sh.sha256": stored, "review.security.FAILED.md": "# Review security of keen-tapir: FAILED\n\nnot available without Ollama.\n"})
	fixtureRun(t, work, "calm-otter", map[string]string{"ask.sh": script, "ask.sh.sha256": stored, "review.security.md": "## Verdict\nOK to run\n"})
	fixtureRun(t, work, "eager-lynx", map[string]string{"ask.sh": strings.Replace(script, "INTERACTIVE=0", "INTERACTIVE=1", 1), "ask.sh.sha256": stored, "review.security.md": "## Verdict\nOK to run\n"})
	// A script that fails ask lint --plinth (one from before the answer documents: no library line).
	fixtureRun(t, work, "old-wren", map[string]string{"ask.sh": old, "ask.sh.sha256": hex.EncodeToString(oldSum[:]) + "  ask.sh\n", "review.security.md": "## Verdict\nOK to run\n"})
	failed := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: "bold-birch"},
		Sections: []doc.Section{{Output: "x\n"}}, Result: doc.ResultOK,
		Status: &doc.AnswerStatus{DLP: &doc.DLP{Result: doc.ResultFail, Reason: "not available without Ollama (ollama.host is not set)\n"}}}
	fixtureRun(t, work, "bold-birch", map[string]string{"ask.sh": script, "review.security.md": "ok", "answer.DLP-FAIL.yml": string(failed.Encode())})
	fixtureRun(t, work, "bold-bison", map[string]string{"ask.sh": script, "review.security.md": "ok", "answer.dlp-ok.yml": "the answer\n"})
	ns := []string{"-n", "michael/devops", "--work", work}
	req := func(run, action string) string {
		return filepath.Join(work, "michael", "devops", "requests", "main", run, action+".queued")
	}
	say := func(answer string, args ...string) (int, string, string) {
		stdin = strings.NewReader(answer)
		t.Cleanup(func() { stdin = os.Stdin })
		last := len(args) - 1 // flags before <RUN>: flag parsing stops at the first argument
		return run(append(append(slices.Clone(args[:last]), ns...), args[last])...)
	}

	// Refused at once, with the rule, as the controller would refuse it.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"exec", "keen-tapir"}, "keen-tapir: require-approval by rule review.failed"},
		{[]string{"exec", "eager-lynx"}, "deny by rule state.valid: run eager-lynx: ask.sh no longer matches"},
		{[]string{"exec", "old-wren"}, "deny by rule ask.lint: run old-wren: ask.sh fails ask lint --plinth (no AAA library line"},
		{[]string{"done", "calm-otter"}, "deny by rule state.valid: run calm-otter has no scanned answer"},
		{[]string{"done", "bold-birch"}, "require-approval by rule dlp.failed"},
		{[]string{"override", "dlp", "bold-bison"}, "has no failed DLP scan"},
		{[]string{"exec", "lost-owl"}, "no run lost-owl"},
	} {
		if code, _, errs := say("", c.args...); code != exitFail || !strings.Contains(errs, c.want) {
			t.Errorf("%v: %d %q, want %q", c.args, code, errs, c.want)
		}
	}
	// Allowed: queued, with a note while the controller isn't running.
	if code, out, errs := say("", "exec", "calm-otter"); code != exitOK || out != "Queued exec of calm-otter.\n" || !strings.Contains(errs, "controller is not running") {
		t.Errorf("exec: %d %q %q", code, out, errs)
	}
	if code, out, _ := say("", "exec", "calm-otter"); code != exitOK || !strings.Contains(out, "already queued") {
		t.Errorf("exec again: %d %q", code, out)
	}
	if code, _, _ := say("", "done", "bold-bison"); code != exitOK || !exists(req("bold-bison", "done")) {
		t.Errorf("done: %d", code)
	}
	// Two steps: what failed, then yes (a single y is no).
	code, out, _ := say("y\n", "override", "review", "keen-tapir")
	if code != exitFail || !strings.Contains(out, "security     review.security.FAILED.md: not available without Ollama.") || !strings.Contains(out, "Nothing queued.") || exists(req("keen-tapir", "override.review")) {
		t.Errorf("override review answered y: %d %q", code, out)
	}
	if code, out, _ := say("yes\n", "override", "review", "keen-tapir"); code != exitOK || !strings.Contains(out, "Queued override.review of keen-tapir.") {
		t.Errorf("override review: %d %q", code, out)
	}
	if code, out, _ := say("yes\n", "override", "dlp", "bold-birch"); code != exitOK || !strings.Contains(out, "failed: not available without Ollama (ollama.host is not set). Fixed patterns") {
		t.Errorf("override dlp: %d %q", code, out)
	}
	if code, out, _ := say("no\n", "scrub", "calm-otter"); code != exitFail || !strings.Contains(out, "Are you sure? Scrub calm-otter? (type yes)") || exists(req("calm-otter", "scrub")) {
		t.Errorf("scrub answered no: %d %q", code, out)
	}
	if code, _, _ := say("yes\n", "scrub", "calm-otter"); code != exitOK || !exists(req("calm-otter", "scrub")) {
		t.Errorf("scrub: %d", code)
	}
	// describe lists the requests, a failed one with its reason.
	if err := os.WriteFile(filepath.Join(work, "michael", "devops", "requests", "main", "calm-otter", "done.FAILED"), []byte("Rule: state.valid\nReason: no answer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(append([]string{"describe"}, append(ns, "calm-otter")...)...)
	for _, want := range []string{"Requests:\n", "  exec             queued\n", "  done             FAILED: Rule: state.valid; Reason: no answer;\n", "  scrub            queued\n"} {
		if code != exitOK || !strings.Contains(out, want) {
			t.Errorf("describe: no %q in\n%s", want, out)
		}
	}
}

// TestSaveAndReport: save and report need plinth.repo (a run needs no issue in its name); report names the repo and what it
// posts, masked first, then asks for the word; it refuses without the repo or Ollama.
func TestSaveAndReport(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org") // automatic: [security]
	script := "#!/bin/bash\n# AAA script: check the disks\n# READ-ONLY\nAGENT_NAME=\"devops/main\"\nRUN_NAME=\"keen-tapir\"\nINTERACTIVE=0\nNETWORK=0\nPRIVILEGED=0\n" +
		asklint.PlinthLibLine + "\n"
	fixtureRun(t, work, "keen-tapir", map[string]string{"ask.sh": script, "review.security.FAILED.md": "# Review security of keen-tapir: FAILED\n\nnot available without Ollama.\n"})
	fixtureRun(t, work, "bold-bison", map[string]string{"ask.sh": script, "review.security.md": "ok", "answer.dlp-ok.yml": "the answer\n"})
	fixtureRun(t, work, "brave-hare-12", map[string]string{"ask.sh": script, "review.security.md": "ok", "answer.dlp-ok.yml": "the answer\n"})
	ns := []string{"-n", "michael/devops", "--work", work}
	say := func(answer string, args ...string) (int, string, string) {
		stdin = strings.NewReader(answer)
		t.Cleanup(func() { stdin = os.Stdin })
		last := len(args) - 1
		return run(append(append(slices.Clone(args[:last]), ns...), args[last])...)
	}
	req := func(run, action string) string {
		return filepath.Join(work, "michael", "devops", "requests", "main", run, action+".queued")
	}
	cfg := func(extra string) {
		writeConfig(t, work, "https://forgejo.example.org")
		f, err := os.OpenFile(filepath.Join(work, "config.yml"), os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, err = f.WriteString(extra)
			f.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	if code, _, errs := say("", "save", "bold-bison"); code != exitFail || !strings.Contains(errs, "deny by rule repo.set") {
		t.Errorf("save without the repo: %d %q", code, errs)
	}
	if code, _, errs := say("yes\n", "report", "keen-tapir"); code != exitFail || !strings.Contains(errs, "deny by rule repo.set") {
		t.Errorf("report without the repo: %d %q", code, errs)
	}
	repo := "plinth:\n  repo: michael/runs\n"
	cfg(repo)
	// A run whose name ends in no issue is saved all the same: Save posts on the run's own issue in plinth.repo.
	if code, out, _ := say("", "save", "bold-bison"); code != exitOK || out != "Queued save of bold-bison.\n" || !exists(req("bold-bison", "save")) {
		t.Errorf("save without an issue: %d %q", code, out)
	}
	if code, out, _ := say("", "save", "brave-hare-12"); code != exitOK || out != "Queued save of brave-hare-12.\n" || !exists(req("brave-hare-12", "save")) {
		t.Errorf("save: %d %q", code, out)
	}
	if code, out, _ := say("yes\n", "report", "keen-tapir"); code != exitFail || !strings.Contains(out, "not available without Ollama") || exists(req("keen-tapir", "report")) {
		t.Errorf("report without Ollama: %d %q", code, out)
	}
	cfg("ollama:\n  host: http://127.0.0.1:9\n  models:\n    review: rev\n    dlp: dlp\n" + repo)
	code, out, _ := say("y\n", "report", "keen-tapir")
	for _, want := range []string{"Report posts on the run's issue in michael/runs:\n", `"michael/devops keen-tapir: <what the script says>", opened if the run has none,`,
		`a comment "Report of failed scans: review security" with the events that decided it,`, "attaches keen-tapir.report.zip:\n  ask.sh, review.security.FAILED.md\n", "Everything is masked first", "Report keen-tapir? (type yes): ", "Nothing queued."} {
		if code != exitFail || !strings.Contains(out, want) || exists(req("keen-tapir", "report")) {
			t.Errorf("report answered y: %d, no %q in %q", code, want, out)
		}
	}
	if code, out, _ := say("yes\n", "report", "keen-tapir"); code != exitOK || !strings.Contains(out, "Queued report of keen-tapir.") || !exists(req("keen-tapir", "report")) {
		t.Errorf("report: %d %q", code, out)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// TestAcceptNetwork: a network run waits for accept network, which says what the run can reach and asks for the word;
// a namespace that doesn't allow network mode refuses both.
func TestAcceptNetwork(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	writeConfig(t, work, "https://forgejo.example.org")
	script := "#!/bin/bash\n# AAA script: reach the host\n# READ-ONLY\nAGENT_NAME=\"devops/main\"\nRUN_NAME=\"keen-tapir\"\nINTERACTIVE=0\nNETWORK=1\nPRIVILEGED=0\n" +
		asklint.PlinthLibLine + "\n"
	sum := sha256.Sum256([]byte(script))
	fixtureRun(t, work, "keen-tapir", map[string]string{"ask.sh": script, "ask.sh.sha256": hex.EncodeToString(sum[:]) + "  ask.sh\n",
		"review.security.md": "## Verdict\nOK to run\n"})
	ns := []string{"-n", "michael/devops", "--work", work}
	say := func(answer string, args ...string) (int, string, string) {
		stdin = strings.NewReader(answer)
		t.Cleanup(func() { stdin = os.Stdin })
		last := len(args) - 1
		return run(append(append(slices.Clone(args[:last]), ns...), args[last])...)
	}
	queued := filepath.Join(work, "michael", "devops", "requests", "main", "keen-tapir", "accept.network.queued")

	if code, _, errs := say("yes\n", "accept", "network", "keen-tapir"); code != exitFail || !strings.Contains(errs, "deny by rule exec.mode") {
		t.Errorf("not allowed in the namespace: %d %q", code, errs)
	}
	if err := os.WriteFile(filepath.Join(work, "michael", "devops", "config.yml"), []byte("plinth:\n  exec:\n    network: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := say("", "exec", "keen-tapir"); code != exitFail || !strings.Contains(errs, "require-approval by rule exec.mode") || !strings.Contains(errs, "needs accept.network") {
		t.Errorf("exec before accept: %d %q", code, errs)
	}
	code, out, _ := say("y\n", "accept", "network", "keen-tapir")
	if code != exitFail || !strings.Contains(out, "is a network run (NETWORK=1)") || !strings.Contains(out, "no ssh: nothing of your ~/.ssh") || exists(queued) {
		t.Errorf("accept answered y: %d %q", code, out)
	}
	if code, out, _ := say("yes\n", "accept", "network", "keen-tapir"); code != exitOK || !strings.Contains(out, "Queued accept.network of keen-tapir.") || !exists(queued) {
		t.Errorf("accept: %d %q", code, out)
	}
	code, out, _ = run(append([]string{"describe"}, append(ns, "keen-tapir")...)...)
	if code != exitOK || !strings.Contains(out, "Mode:        network\n") || !strings.Contains(out, "Network:     not accepted yet") {
		t.Errorf("describe: %d\n%s", code, out)
	}
}
