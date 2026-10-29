package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestScrubberCommands: scrubber and handoff-scrubber write their scripts, say what they hold, and delete nothing.
func TestScrubberCommands(t *testing.T) {
	repo, work := t.TempDir(), t.TempDir()
	done := filepath.Join(repo, "ai", "work", "done")
	for _, d := range []string{"calm-otter", "brave-fox-12"} {
		if err := os.MkdirAll(filepath.Join(done, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errs := run("scrubber", "--repo", repo, "--work", work)
	if code != exitOK || !strings.Contains(out, "scrub  calm-otter\n") || !strings.Contains(out, "keep   brave-fox-12 (belongs to issue 12)") ||
		!strings.Contains(out, ": 1 to scrub, 1 kept, 0 skipped") {
		t.Fatalf("scrubber: %d %q %q", code, out, errs)
	}
	b, err := os.ReadFile(filepath.Join(repo, "ai", "work", "scrub.sh"))
	if err != nil || !strings.Contains(string(b), "rm -rf -- calm-otter '"+filepath.Join(work, "aaa-raw", "calm-otter")+"'") {
		t.Errorf("scrub.sh: %v\n%s", err, b)
	}
	if _, err := os.Stat(filepath.Join(done, "calm-otter")); err != nil {
		t.Error("scrubber deleted a run itself")
	}
	// plinth.aaa-raw-dir from <work>/config.yml, read alone.
	if err := os.WriteFile(filepath.Join(work, "config.yml"), []byte("plinth:\n  aaa-raw-dir: \"/srv/raw\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run("scrubber", "--repo", repo, "--work", work, "--all"); code != exitOK || !strings.Contains(out, ": 2 to scrub") {
		t.Errorf("scrubber --all: %d %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "ai", "work", "scrub.sh")); !strings.Contains(string(b), "'/srv/raw/brave-fox-12'") {
		t.Errorf("scrub.sh with the config's raw dir:\n%s", b)
	}
	if code, _, _ := run("scrubber", "--repo", repo, "extra"); code != exitUsage {
		t.Errorf("scrubber extra: %d", code)
	}

	if code, _, errs := run("handoff-scrubber", "--repo", repo); code != exitUsage || !strings.Contains(errs, "--branch is required") {
		t.Errorf("no --branch: %d %q", code, errs)
	}
	if code, _, errs := run("handoff-scrubber", "--repo", repo, "--branch", "main"); code != exitFail || !strings.Contains(errs, "not handoff/<name>") {
		t.Errorf("--branch main: %d %q", code, errs)
	}
	if code, out, errs := run("handoff-scrubber", "--repo", repo, "--branch", "handoff/x"); code != exitOK || !strings.Contains(out, "handoff-scrubber.sh") {
		t.Errorf("handoff-scrubber: %d %q %q", code, out, errs)
	}
}

// TestCompletion: the script is valid bash and asks plinthctl __complete, which completes command words, flags,
// namespaces and run names, and prints nothing it can't know. The words only here; TestCompletionMarks has the lines.
func TestCompletion(t *testing.T) {
	code, script, _ := run("completion", "bash")
	if code != exitOK || !strings.Contains(script, "complete -o default -F _plinthctl plinthctl") || !strings.Contains(script, CompleteArg) ||
		!strings.Contains(script, "compopt -o nosort") {
		t.Fatalf("completion bash: %d %q", code, script)
	}
	if out, err := exec.Command("bash", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Errorf("bash -n: %v\n%s", err, out)
	}
	work := t.TempDir()
	for _, d := range []string{"michael/devops/state", "michael/devops/runs/main/calm-otter", "michael/devops/runs/main/brave-fox-12",
		"michael/other/state", "michael/other/runs/main/keen-owl"} {
		if err := os.MkdirAll(filepath.Join(work, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		words []string
		want  string
	}{
		{[]string{"ge"}, "get\n"},
		{[]string{"get", "a"}, "ans\nask\n"},
		{[]string{"override", ""}, "dlp\nreview\n"},
		{[]string{"new-ask", "-"}, "--repo\n--template\n--work\n"},
		{[]string{"scrubber", "--"}, "--all\n--repo\n--work\n"},
		{[]string{"exec", "--work", work, "-n", ""}, "michael/devops\nmichael/other\n"},
		{[]string{"exec", "--work", work, "-n", "michael/devops", ""}, "brave-fox-12\ncalm-otter\n"},
		{[]string{"describe", "--work", work, "k"}, "keen-owl\n"},
		{[]string{"exec", "--work", work, "-n", "michael/devops", "calm-otter", ""}, ""},
		{[]string{"new-ask", "--repo", ""}, ""},
		{[]string{"help", "ima"}, "image\n"},
		{[]string{"nope", ""}, ""},
	} {
		if code, out, errs := run(append([]string{CompleteArg}, c.words...)...); code != exitOK || wordsOf(out) != c.want || errs != "" {
			t.Errorf("%q: %d %q %q, want %q", c.words, code, out, errs, c.want)
		}
	}
}
