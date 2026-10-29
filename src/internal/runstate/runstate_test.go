package runstate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"plinth/internal/names"
)

var ns = names.Namespace{Owner: "michael", Repo: "devops"}

var automatic = []string{"security", "dlp"}

// mkrun creates runs/<agent>/<run>/ with the given files (name -> content) and sets their time to at.
func mkrun(t *testing.T, work, agent, run string, at time.Time, files map[string]string) {
	t.Helper()
	dir := filepath.Join(work, RunDir(ns, agent, run))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, body := range files {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

const ok = "## Verdict\nOK to run\n"

func TestStates(t *testing.T) {
	work := t.TempDir()
	at := time.Now()
	for _, c := range []struct {
		run    string
		files  map[string]string
		live   bool
		ask    string
		answer string
		check  func(r *Run) bool
	}{
		{"brave-ibex", map[string]string{"ask.sh": "", "ask.sh.sha256": ""}, false, "reviewing", "", func(r *Run) bool {
			return slices.Equal(r.Facts.ReviewsPending, automatic) && !r.Approved()
		}},
		{"calm-otter", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok}, false, "approved", "", func(r *Run) bool {
			return r.Approved() && r.Reviews["dlp"] == "ok"
		}},
		{"eager-lynx", map[string]string{"ask.sh": "", "review.security.FAILED.md": "", "review.dlp.md": ok}, false, "review-failed", "", func(r *Run) bool {
			return slices.Equal(r.Facts.ReviewsFailed, []string{"security"}) && !r.Approved()
		}},
		{"gentle-wren", map[string]string{"ask.sh": "", "review.security.md": "x\n## Verdict\n\nDo not run\n", "review.dlp.md": ok}, false, "review-failed", "", func(r *Run) bool {
			return r.Reviews["security"] == "failed"
		}},
		{"happy-yak", map[string]string{"ask.sh": "", "review.security.FAILED.md": "", "review.dlp.md": ok, "override.review.md": ""}, false, "overridden", "", func(r *Run) bool {
			return r.Approved() && r.Facts.ReviewOverridden
		}},
		{"keen-tapir", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "running.yml": ""}, true, "running", "", func(r *Run) bool {
			return r.Facts.Running && !r.Facts.Ran
		}},
		{"keen-mink", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "running.yml": ""}, false, "no-answer", "", func(r *Run) bool {
			return !r.Facts.Running && r.Facts.Ran
		}},

		// Its script removed running.yml: still running, as the controller's record says.
		{"keen-heron", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok}, true, "running", "", func(r *Run) bool {
			return r.Facts.Running && !r.Facts.Ran
		}},
		{"bold-acorn", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "answer.new.yml": ""}, false, "ran", "scanning", nil},
		{"bold-aspen", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "answer.FAIL.dlp-ok.yml": ""}, false, "ran", "dlp-ok (gate)", func(r *Run) bool {
			return r.Gate && r.AnswerFile() == "answer.FAIL.dlp-ok.yml"
		}},
		{"bold-birch", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "answer.DLP-FAIL.yml": ""}, false, "ran", "dlp-failed", nil},
		{"bold-cedar", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "answer.FAIL.INVALID.yml": ""}, false, "ran", "invalid (gate)", func(r *Run) bool {
			return r.Facts.Answer == "INVALID"
		}},
		{"bold-bison", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "answer.DLP-FAIL.yml": "", "override.dlp.md": "", "report.md": ""}, false, "ran", "overridden", func(r *Run) bool {
			return r.Facts.DLPOverridden && r.Facts.Reported
		}},
		{"bold-brook", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "answer.released.yml": "", "review.answer.dlp.FAILED.md": ""}, false, "ran", "released", func(r *Run) bool {
			return r.Reviews["answer.dlp"] == "failed" && len(r.Facts.ReviewsFailed) == 0
		}},
		{"calm-cedar", map[string]string{"ask.sh": "", "review.security.md": ok, "review.dlp.md": ok, "accept.network.md": ""}, false, "approved", "", func(r *Run) bool {
			return r.Facts.NetworkAccepted
		}},
	} {
		mkrun(t, work, "main", c.run, at, c.files)
		r, err := Load(work, ns, "main", c.run, automatic, func(_, _ string) bool { return c.live })
		if err != nil {
			t.Fatalf("%s: %v", c.run, err)
		}
		if !r.Facts.Exists || r.AskStatus() != c.ask || r.AnswerStatus() != c.answer || (c.check != nil && !c.check(r)) {
			t.Errorf("%s: ask %q answer %q facts %+v reviews %v; want ask %q answer %q", c.run, r.AskStatus(), r.AnswerStatus(), r.Facts, r.Reviews, c.ask, c.answer)
		}
	}
	// A run that doesn't exist, and a run directory reached through a symlink.
	if r, err := Load(work, ns, "main", "lost-owl", automatic, nil); err != nil || r.Facts.Exists {
		t.Errorf("missing run: %+v, %v", r, err)
	}
	if err := os.Symlink(filepath.Join(work, RunDir(ns, "main", "calm-otter")), filepath.Join(work, RunDir(ns, "main", "sly-fox"))); err != nil {
		t.Fatal(err)
	}
	if r, err := Load(work, ns, "main", "sly-fox", automatic, nil); err != nil || !r.Facts.Symlink {
		t.Errorf("symlinked run: %+v, %v", r, err)
	}
	// Two answers: the run's state can't be read.
	mkrun(t, work, "main", "odd-owl", at, map[string]string{"ask.sh": "", "answer.new.yml": "", "answer.released.yml": ""})
	if _, err := Load(work, ns, "main", "odd-owl", automatic, nil); err == nil || !strings.Contains(err.Error(), "2 answer files") {
		t.Errorf("two answers: %v", err)
	}
}

func TestListAndNames(t *testing.T) {
	work := t.TempDir()
	now := time.Now()
	mkrun(t, work, "main", "old-run", now.Add(-time.Hour), map[string]string{"ask.sh": ""})
	mkrun(t, work, "main", "new-run", now, map[string]string{"ask.sh": ""})
	mkrun(t, work, "al", "done-run", now.Add(time.Minute), map[string]string{"ask.sh": "", "answer.released.yml": ""})
	// Broken: two answer files. It is listed (last of the unreleased: no time), never an error for the whole list.
	mkrun(t, work, "main", "odd-owl", now, map[string]string{"ask.sh": "", "answer.new.yml": "", "answer.dlp-ok.yml": ""})
	for _, d := range []string{".scrub.gone-run", ".new.half-run.0123456789abcdef", "NotARun"} {
		if err := os.MkdirAll(filepath.Join(work, "michael", "devops", Dir, "main", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := List(work, ns, "", automatic, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.Agent+"/"+r.Name)
	}
	// Not released first, newest first; released last.
	if want := []string{"main/new-run", "main/old-run", "main/odd-owl", "al/done-run"}; !slices.Equal(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
	for _, r := range runs {
		if broken := r.Name == "odd-owl"; (r.Broken != nil) != broken || (r.AskStatus() == "broken") != broken {
			t.Errorf("%s: Broken %v, status %s", r.Name, r.Broken, r.AskStatus())
		}
	}
	if runs, _ := List(work, ns, "al", automatic, nil); len(runs) != 1 {
		t.Errorf("List for al = %d runs", len(runs))
	}
	taken, err := Names(work, ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"old-run", "new-run", "done-run", "gone-run", "half-run"} {
		if !taken[s] {
			t.Errorf("%s not taken: %v", s, taken)
		}
	}
}

func TestHeader(t *testing.T) {
	work := t.TempDir()
	mkrun(t, work, "main", "keen-tapir", time.Now(), map[string]string{"ask.sh": "#!/bin/bash\n# AAA script: check the disks\n# READ-ONLY\n#   1 disks\nset -uo pipefail\n# later comment\nINTERACTIVE=1\nNETWORK=0\nPRIVILEGED=1\n"})
	h, mode, err := Header(work, ns, "main", "keen-tapir")
	if err != nil || !slices.Equal(h, []string{"AAA script: check the disks", "READ-ONLY", "  1 disks"}) || mode != (Mode{Interactive: true, Privileged: true}) || mode.Name() != "privileged" {
		t.Errorf("Header = %q, %+v, %v", h, mode, err)
	}
}

func TestScript(t *testing.T) {
	work := t.TempDir()
	script := "#!/bin/bash\nINTERACTIVE=1   # asks\nNETWORK=1\nPRIVILEGED=0\nsource \"$AAA_LIB\"; [[ \"${AAA_LOADED:-}\" == 1 ]] || exit 2\n"
	sum := sha256.Sum256([]byte(script))
	mkrun(t, work, "main", "keen-tapir", time.Now(), map[string]string{"ask.sh": script, "ask.sh.sha256": hex.EncodeToString(sum[:]) + "  ask.sh\n"})
	if mode, changed, err := Script(work, ns, "main", "keen-tapir"); err != nil || mode != (Mode{Interactive: true, Network: true}) || mode.Name() != "network" || changed {
		t.Errorf("Script = %+v, %v, %v", mode, changed, err)
	}
	if b, got, err := CheckScript(work, ns, "main", "keen-tapir"); err != nil || got != hex.EncodeToString(sum[:]) || sha256.Sum256(b) != sum {
		t.Errorf("CheckScript = %q, %v", got, err)
	}
	mkrun(t, work, "main", "keen-tapir", time.Now(), map[string]string{"ask.sh": "#!/bin/bash\nINTERACTIVE=0\n"})
	if mode, changed, err := Script(work, ns, "main", "keen-tapir"); err != nil || mode != (Mode{}) || mode.Name() != "normal" || !changed {
		t.Errorf("Script of a changed script = %+v, %v, %v", mode, changed, err)
	}
	if _, _, err := CheckScript(work, ns, "main", "keen-tapir"); err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Errorf("CheckScript of a changed script: %v", err)
	}
	mkrun(t, work, "main", "calm-otter", time.Now(), map[string]string{"ask.sh": script})
	if _, changed, _ := Script(work, ns, "main", "calm-otter"); !changed {
		t.Error("a script without its sum counts as changed")
	}
	if _, _, err := CheckScript(work, ns, "main", "calm-otter"); err == nil {
		t.Error("CheckScript passed a script without its sum")
	}
	for name, want := range map[string]bool{"running.yml": true, "override.review.md": true, "report.md": true, "review.security.FAILED.md": true,
		"review.answer.dlp.md": true, "accept.network.md": true, "answer.FAIL.released.yml": true, "answer.INVALID.yml": true,
		"answer.FAIL.released.md": false, "ask.sh": false, "ask.sh.sha256": false, "notes.txt": false, "answer.md": false, "answer.yml": false} {
		if StateFile(name) != want {
			t.Errorf("StateFile(%q) = %v", name, !want)
		}
	}
}

func TestBeforeExec(t *testing.T) {
	for name, want := range map[string]bool{"ask.sh": true, "review.security.md": true, "override.review.md": true, "accept.network.md": true,
		"running.yml": false, "override.dlp.md": false, "answer.new.yml": false, "review.answer.dlp.md": false} {
		if BeforeExec(name) != want {
			t.Errorf("BeforeExec(%q) = %v", name, !want)
		}
	}
}

// TestSaveAndReportFiles: Save zips all but running.yml; Report names the failed scans and attaches ask.sh, the review
// files and only a failed answer.
func TestSaveAndReportFiles(t *testing.T) {
	files := func(list ...string) []File {
		var out []File
		for _, n := range list {
			out = append(out, File{Name: n})
		}
		return out
	}
	r := &Run{Files: files("answer.DLP-FAIL.yml", "ask.sh", "ask.sh.sha256", "override.dlp.md", "review.answer.dlp.md", "review.security.FAILED.md", Running)}
	r.Facts.Answer, r.Facts.ReviewsFailed = "DLP-FAIL", []string{"security"}
	if got := r.SaveFiles(); !slices.Equal(got, []string{"answer.DLP-FAIL.yml", "ask.sh", "ask.sh.sha256", "override.dlp.md", "review.answer.dlp.md", "review.security.FAILED.md"}) {
		t.Errorf("SaveFiles = %v", got)
	}
	if got := r.FailedChecks(); !slices.Equal(got, []string{"review security", "DLP scan"}) {
		t.Errorf("FailedChecks = %v", got)
	}
	if got := r.ReportFiles(); !slices.Equal(got, []string{"ask.sh", "review.answer.dlp.md", "review.security.FAILED.md", "answer.DLP-FAIL.yml"}) {
		t.Errorf("ReportFiles = %v", got)
	}
	r = &Run{Files: files("answer.dlp-ok.yml", "ask.sh", "review.security.md")}
	r.Facts.Answer, r.Facts.ReviewsFailed = "dlp-ok", []string{"security"} // a Do not run verdict
	if got := r.ReportFiles(); !slices.Equal(got, []string{"ask.sh", "review.security.md"}) {
		t.Errorf("ReportFiles of a passed answer = %v", got)
	}
}
