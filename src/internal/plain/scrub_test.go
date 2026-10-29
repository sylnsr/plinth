package plain

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestScrub: scrub.sh removes the finished runs in done/ and their untouched originals, keeps the ones of an issue
// (unless --all) and skips anything that isn't a run directory; Scrub itself deletes nothing.
func TestScrub(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(t.TempDir(), "raw it's") // quoting
	for _, d := range []string{"done/calm-otter", "done/brave-fox-12", "done/Not-A-Run", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "done", "ask-old-flat.sh"), "x")
	write(t, filepath.Join(raw, "calm-otter", "answer.new.yml.orig"), "original")
	write(t, filepath.Join(raw, "brave-fox-12", "answer.new.yml.orig"), "original")
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, "done", "keen-owl")); err != nil {
		t.Fatal(err)
	}
	entries, err := Scrub(dir, raw, false, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.What
	}
	want := map[string]string{"calm-otter": "scrub", "brave-fox-12": "keep", "Not-A-Run": "skip", "ask-old-flat.sh": "skip", "keen-owl": "skip"}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s: %q, want %q (all: %v)", n, got[n], w, got)
		}
	}
	if !exists(filepath.Join(dir, "done", "calm-otter")) {
		t.Fatal("Scrub deleted something itself")
	}
	script := filepath.Join(dir, ScrubScript)
	if fi, err := os.Stat(script); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("scrub.sh: %v, %v", fi, err)
	}
	if out, err := exec.Command("bash", script).CombinedOutput(); err != nil || !strings.Contains(string(out), "scrub done") {
		t.Fatalf("bash scrub.sh: %v\n%s", err, out)
	}
	for p, gone := range map[string]bool{"done/calm-otter": true, "done/brave-fox-12": false, "done/keen-owl": false, "elsewhere": false} {
		if exists(filepath.Join(dir, p)) == gone {
			t.Errorf("%s: gone %v, want %v", p, !gone, gone)
		}
	}
	if exists(filepath.Join(raw, "calm-otter")) || !exists(filepath.Join(raw, "brave-fox-12")) {
		t.Error("the originals: calm-otter's should be gone, brave-fox-12's kept")
	}

	// --all takes the runs of an issue too; a run without an original is fine.
	if entries, err = Scrub(dir, raw, true, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(entries, ScrubEntry{Name: "brave-fox-12", What: "scrub"}) {
		t.Errorf("--all: %v", entries)
	}
	write(t, filepath.Join(dir, "done", "lone-wren", "ask.sh"), "x")
	if _, err = Scrub(dir, raw, true, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", script).CombinedOutput(); err != nil {
		t.Fatalf("bash scrub.sh --all: %v\n%s", err, out)
	}
	if exists(filepath.Join(dir, "done", "brave-fox-12")) || exists(filepath.Join(dir, "done", "lone-wren")) || exists(filepath.Join(raw, "brave-fox-12")) {
		t.Error("--all left a run")
	}

	// No done/: nothing to scrub, and the script says so. Relative paths are refused.
	empty := t.TempDir()
	if entries, err := Scrub(empty, raw, false, time.Now()); err != nil || len(entries) != 0 || !exists(filepath.Join(empty, ScrubScript)) {
		t.Errorf("no done/: %v, %v", entries, err)
	}
	if _, err := Scrub("ai/work", raw, false, time.Now()); err == nil {
		t.Error("a relative work directory")
	}
}

// TestHandoffScrub: the script deletes the handoff branch locally and on origin, then itself; only handoff/ branch
// names with plain characters are accepted.
func TestHandoffScrub(t *testing.T) {
	for _, b := range []string{"main", "handoff", "handoff/", "handoff/../x", "handoff/-x", "handoff/x.lock", "handoff/a b",
		"handoff/x;rm", "handoff/x.", "handoff//x", "handoff/$(id)"} {
		if err := HandoffScrub(t.TempDir(), "/r", b, time.Now()); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := t.TempDir()
	origin, repo := filepath.Join(base, "origin.git"), filepath.Join(base, "repo")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(base, "init", "-q", "--bare", origin)
	git(base, "clone", "-q", origin, repo)
	git(repo, "commit", "-q", "--allow-empty", "-m", "first")
	git(repo, "push", "-q", "origin", "HEAD:main")
	const branch = "handoff/claude/plinth-coder-1"
	git(repo, "branch", branch)
	git(repo, "push", "-q", "origin", branch)
	work := filepath.Join(repo, "ai", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := HandoffScrub(work, repo, branch, time.Now()); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(work, HandoffScript)
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash handoff-scrubber.sh: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/" + branch}, {"-C", origin, "show-ref", "--verify", "--quiet", "refs/heads/" + branch}} {
		if exec.Command("git", args...).Run() == nil {
			t.Errorf("git %v: the branch is still there", args)
		}
	}
	if exists(script) {
		t.Error("the script didn't remove itself")
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
