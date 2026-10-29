package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiff: the diff of .plinth/ between two commits, or every file in full against the empty tree; cut at max; an
// external diff or a textconv the repo's config names never runs.
func TestDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "-b", "main")
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		gitCmd(t, repo, "add", "-A")
		gitCmd(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "-m", msg)
		return strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
	}
	write(".plinth/Containerfile", "FROM x\nRUN one\n")
	write("README", "outside .plinth\n")
	one := commit("one")
	write(".plinth/Containerfile", "FROM x\nRUN two\n")
	write(".plinth/setup.sh", "echo setup\n")
	write("README", "changed outside\n")
	two := commit("two")

	// Commands the repo names for a diff: none may run.
	ran := filepath.Join(t.TempDir(), "ran")
	gitCmd(t, repo, "config", "diff.external", "touch "+ran)
	gitCmd(t, repo, "config", "diff.evil.textconv", "touch "+ran+";cat")
	write(".git/info/attributes", "* diff=evil\n")

	got, cut, err := Diff(repo, one, two, ".plinth", 1<<20)
	if err != nil || cut {
		t.Fatalf("Diff = %v, cut %v", err, cut)
	}
	for _, want := range []string{"-RUN one", "+RUN two", "+echo setup", "b/.plinth/setup.sh"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "README") {
		t.Errorf("a file outside .plinth/ in\n%s", got)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("a command the repo names ran")
	}
	if full, _, err := Diff(repo, "", one, ".plinth", 1<<20); err != nil || !strings.Contains(full, "+FROM x") || !strings.Contains(full, "+RUN one") {
		t.Errorf("against the empty tree: %v\n%s", err, full)
	}
	if part, cut, err := Diff(repo, one, two, ".plinth", 64); err != nil || !cut || len(part) > 64 {
		t.Errorf("cut at 64: %d bytes, cut %v, %v", len(part), cut, err)
	}
	if _, _, err := Diff(repo, strings.Repeat("f", 40), two, ".plinth", 1<<20); err == nil || !strings.Contains(err.Error(), "is not in the repo") {
		t.Errorf("an unknown from: %v", err)
	}
	if _, _, err := Diff(repo, "--output=/tmp/x", two, ".plinth", 1<<20); err == nil {
		t.Error("an option as from was taken")
	}
	if _, _, err := Diff(repo, one, two, "../x", 1<<20); err == nil {
		t.Error("a path outside the tree was taken")
	}
}
