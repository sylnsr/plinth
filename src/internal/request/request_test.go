package request

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/asklint"
	"plinth/internal/config"
	"plinth/internal/decide"
	"plinth/internal/names"
	"plinth/internal/runstate"
	"plinth/internal/workdir"
)

var ns = names.Namespace{Owner: "michael", Repo: "devops"}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name, action, state string
		ok                  bool
	}{
		{"exec.queued", "exec", "queued", true},
		{"override.review.running", "override.review", "running", true},
		{"review.security.FAILED", "review.security", "FAILED", true},
		{"exec.starting", "exec", "starting", true},
		{"done.starting", "", "", false}, // only an exec has a script to start
		{"override.dlp.finished", "override.dlp", "finished", true},
		{"review.queued", "", "", false}, // a review names its type
		{"exec.done", "", "", false},
		{"rm.queued", "", "", false},
		{"exec", "", "", false},
		{".exec.queued.0123.tmp", "", "", false},
	} {
		a, s, ok := Parse(c.name)
		if a != c.action || s != c.state || ok != c.ok {
			t.Errorf("Parse(%q) = %q %q %v", c.name, a, s, ok)
		}
	}
	if !(Rank(decide.OverrideReview) < Rank(decide.Exec) && Rank(decide.AcceptNetwork) < Rank(decide.Exec) && Rank(decide.Exec) < Rank(decide.Done) && Rank("review.dlp") < Rank(decide.Exec) && Rank(decide.Done) < Rank(decide.Scrub)) {
		t.Error("overrides must come before exec, and scrub last")
	}
}

func TestQueueAndList(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "michael", "devops", Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Queue(work, ns, "main", "keen-tapir", decide.Done); err != nil {
		t.Fatal(err)
	}
	if err := Queue(work, ns, "main", "keen-tapir", decide.Done); !errors.Is(err, ErrQueued) {
		t.Errorf("queued twice: %v", err)
	}
	dir := filepath.Join(work, RunDir(ns, "main", "keen-tapir"))
	if err := os.WriteFile(filepath.Join(dir, "exec.running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Queue(work, ns, "main", "keen-tapir", decide.Exec); !errors.Is(err, ErrInProgress) {
		t.Errorf("queued while running: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, "exec.running"), filepath.Join(dir, "exec.starting")); err != nil {
		t.Fatal(err)
	}
	if err := Queue(work, ns, "main", "keen-tapir", decide.Exec); !errors.Is(err, ErrInProgress) {
		t.Errorf("queued while starting: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scrub.FAILED"), []byte("Rule: state.valid\nReason: run keen-tapir is running\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Queue(work, ns, "main", "keen-tapir", "rm -rf"); err == nil {
		t.Error("an unknown action was queued")
	}
	got, err := List(work, ns, "main", "keen-tapir")
	if err != nil || len(got) != 3 || got[0].Action != decide.Exec || got[1].Action != decide.Done || got[2].Action != decide.Scrub ||
		got[2].Reason != "Rule: state.valid; Reason: run keen-tapir is running;" {
		t.Errorf("List = %+v, %v", got, err)
	}
	if got, err := List(work, ns, "main", "calm-otter"); err != nil || got != nil {
		t.Errorf("List of a run without requests = %v, %v", got, err)
	}
}

func TestDecide(t *testing.T) {
	work := t.TempDir()
	g := &config.Global{Reviewers: config.GlobalReviewers{Type: map[string]config.ReviewType{"security": {}}}}
	cfg := &config.Namespace{Automatic: []string{"security"}}
	dir := filepath.Join(work, runstate.RunDir(ns, "main", "calm-otter"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := []byte(strings.NewReplacer("<one line: what this round is for>", "check the disks", "   (or: CHANGES: <exact list>)", "",
		"#   1 <section>  2 <section>  3 <section>", "#   1 disks").Replace(string(workdir.AskTemplate)))
	if f := asklint.Check(script, asklint.Options{Plinth: true}); len(f) > 0 {
		t.Fatalf("the filled template fails ask lint --plinth: %q", f)
	}
	sum := sha256.Sum256(script)
	for n, b := range map[string]string{"ask.sh": string(script), "ask.sh.sha256": hex.EncodeToString(sum[:]) + "  ask.sh\n", "review.security.md": "## Verdict\nOK to run\n"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(what, action string, outcome decide.Outcome, rule string) {
		t.Helper()
		d, _, err := Decide(work, ns, g, cfg, "main", "calm-otter", action, nil)
		if err != nil || d.Outcome != outcome || d.Rule != rule {
			t.Errorf("%s: %+v, %v; want %s by %s", what, d, err, outcome, rule)
		}
	}
	check("exec of an approved run", decide.Exec, decide.Allow, "allow")
	check("review of a defined type", "review.security", decide.Allow, "allow")
	check("review of an undefined type", "review.nope", decide.Deny, "target.valid")
	check("done without an answer", decide.Done, decide.Deny, "state.valid")
	// An exec request that is running says an exec isn't finished, though the run directory shows no sign; one
	// that is finished, with no answer, lets the run run again.
	reqs := filepath.Join(work, RunDir(ns, "main", "calm-otter"))
	if err := os.MkdirAll(reqs, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{Running, Finished} {
		if err := os.WriteFile(filepath.Join(reqs, "exec."+s), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if s == Running {
			check("exec with exec."+s, decide.Exec, decide.Deny, "state.valid")
		} else {
			check("exec with exec."+s+", no answer", decide.Exec, decide.Allow, "allow")
		}
		if err := os.Remove(filepath.Join(reqs, "exec."+s)); err != nil {
			t.Fatal(err)
		}
	}
	check("exec after the requests are gone", decide.Exec, decide.Allow, "allow")
	// The same for an override of a failed review: gathered for every action on a run, not only exec.
	if err := os.WriteFile(filepath.Join(dir, "review.dlp.FAILED.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check("override of a failed review", decide.OverrideReview, decide.Allow, "allow")
	if err := os.WriteFile(filepath.Join(reqs, "exec."+Finished), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check("override of a failed review with exec.finished", decide.OverrideReview, decide.Deny, "state.valid")
	for _, p := range []string{filepath.Join(dir, "review.dlp.FAILED.md"), filepath.Join(reqs, "exec."+Finished)} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ask.sh"), []byte("#!/bin/bash\nINTERACTIVE=1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	check("exec of a changed script", decide.Exec, decide.Deny, "state.valid")
	if d, _, _ := Decide(work, ns, g, cfg, "main", "calm-otter", decide.Exec, nil); d.Reason != "run calm-otter: ask.sh no longer matches the ask.sh.sha256 stored with it" {
		t.Errorf("reason: %q", d.Reason)
	}
	// A script that no longer passes ask lint --plinth, unchanged (one from before the answer documents): it doesn't
	// run.
	old := []byte("#!/bin/bash\nINTERACTIVE=0\n")
	oldSum := sha256.Sum256(old)
	for n, b := range map[string]string{"ask.sh": string(old), "ask.sh.sha256": hex.EncodeToString(oldSum[:]) + "  ask.sh\n"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check("exec of a script that fails ask lint", decide.Exec, decide.Deny, "ask.lint")
	if d, _, _ := Decide(work, ns, g, cfg, "main", "calm-otter", decide.Exec, nil); !strings.Contains(d.Reason, "fails ask lint --plinth (") || !strings.Contains(d.Reason, "next: ") {
		t.Errorf("reason: %q", d.Reason)
	}
}
