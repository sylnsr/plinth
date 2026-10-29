package gitrepo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceFromURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ssh://git@forgejo.example.org:2222/michael/plinth.git", "michael/plinth"},
		{"git@forgejo.example.org:michael/devops.git", "michael/devops"},
		{"https://forgejo.example.org/michael/devops", "michael/devops"},
		{"https://forgejo.example.org/sub/path/michael/devops/", "michael/devops"},
		{"/srv/git/michael/devops.git", "michael/devops"},
		{"file:///srv/git/ai/docs.git", "ai/docs"},
	} {
		ns, err := NamespaceFromURL(c.in)
		if err != nil || ns.String() != c.want {
			t.Errorf("NamespaceFromURL(%q) = %v, %v; want %s", c.in, ns, err, c.want)
		}
	}
	for _, in := range []string{"", "devops", "git@host:devops.git", "https://host/x/..", "https://host/%zz/x"} {
		if ns, err := NamespaceFromURL(in); err == nil {
			t.Errorf("NamespaceFromURL(%q) = %v, want an error", in, ns)
		}
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q")
	sub := filepath.Join(repo, "ai", "agents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{repo, sub} {
		if top, err := TopLevel(dir); err != nil || top != want {
			t.Errorf("TopLevel(%s) = %q, %v; want %s", dir, top, err, want)
		}
	}
	if _, err := TopLevel(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not inside a git repo") {
		t.Errorf("TopLevel outside a repo = %v", err)
	}
	if _, err := OriginNamespace(repo); err == nil || !strings.Contains(err.Error(), "no origin remote") {
		t.Errorf("OriginNamespace without origin = %v", err)
	}
	gitCmd(t, repo, "remote", "add", "origin", "ssh://git@forgejo.example.org:2222/michael/devops.git")
	if ns, err := OriginNamespace(repo); err != nil || ns.String() != "michael/devops" {
		t.Errorf("OriginNamespace = %v, %v", ns, err)
	}
}

func TestRemoteHost(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ssh://git@Forgejo.Example.org:2222/michael/plinth.git", "forgejo.example.org"},
		{"git@forgejo.example.org:michael/devops.git", "forgejo.example.org"},
		{"https://forgejo.example.org/michael/devops", "forgejo.example.org"},
		{"https://forgejo.example.org:8443/michael/devops", "forgejo.example.org"},
		{"/srv/git/michael/devops.git", ""},
		{"file:///srv/git/michael/devops.git", ""},
		{"devops", ""},
	} {
		if got := RemoteHost(c.in); got != c.want {
			t.Errorf("RemoteHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestErrorsNeverQuoteTheURL(t *testing.T) {
	for _, in := range []string{"https://user:s3cret@host/%zz/x", "https://user:s3cret@host/x", "https://user:s3cret@host/x/.."} {
		if _, err := NamespaceFromURL(in); err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("NamespaceFromURL(%q) = %v: want an error that doesn't quote the URL", in, err)
		}
	}
}

// TestFiles: a tree's files are read as committed, and no command the repo's .git/ names runs: not a filter (which git
// archive would run), not an fsmonitor, not the ssh command of a partial clone's lazy fetch; replace objects and
// inherited GIT_* variables don't count; a symlink and a size over the limit are refused.
func TestFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q")
	write := func(name, s string, mode os.FileMode) {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(".plinth/Containerfile", "FROM x\n", 0o600)
	write(".plinth/sub/run.sh", "echo run\n", 0o700)
	write("other", "other\n", 0o600)
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "-m", "one")

	// Trees for the cases below, written before the checkout turns hostile: one whose blob is missing, as in a partial
	// clone, and one with a symlink.
	other := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD:other"))
	cf := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD:.plinth/Containerfile"))
	lazy := strings.TrimSpace(gitIn(t, repo, "100644 blob "+strings.Repeat("1", 40)+"\tgone\n", "mktree", "--missing"))
	link := strings.TrimSpace(gitIn(t, repo, "120000 blob "+other+"\tlink\n", "mktree"))
	gitCmd(t, repo, "replace", cf, other)

	// What an agent could set in its checkout's .git/: each would touch a marker file.
	marker := filepath.Join(t.TempDir(), "ran")
	touch := "sh -c 'touch " + marker + "; cat'"
	gitCmd(t, repo, "config", "filter.x.smudge", touch)
	gitCmd(t, repo, "config", "diff.x.textconv", touch)
	write(".git/info/attributes", "* filter=x diff=x\n", 0o600)
	gitCmd(t, repo, "config", "core.fsmonitor", touch)
	gitCmd(t, repo, "config", "core.sshCommand", touch)
	gitCmd(t, repo, "config", "remote.origin.url", "ssh://forgejo.example.org/michael/devops.git")
	gitCmd(t, repo, "config", "remote.origin.promisor", "true")
	gitCmd(t, repo, "config", "extensions.partialClone", "origin")
	gitCmd(t, repo, "config", "core.repositoryformatversion", "1")
	t.Setenv("GIT_DIR", "/nonexistent")

	_, tree, err := Tree(repo, "HEAD", ".plinth")
	if err != nil {
		t.Fatal(err)
	}
	files, err := Files(repo, tree, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = fmt.Sprintf("%v %q", f.Exec, f.Data)
	}
	want := map[string]string{"Containerfile": `false "FROM x\n"`, "sub/run.sh": `true "echo run\n"`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Files = %v, want %v", got, want)
	}
	if _, err := Files(repo, tree, 10); err == nil || !strings.Contains(err.Error(), "over 10 bytes") {
		t.Errorf("over the limit: %v", err)
	}

	// No lazy fetch of the missing blob.
	if _, err := Files(repo, lazy, 1<<20); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing blob: %v", err)
	}

	// A symlink is refused.
	if _, err := Files(repo, link, 1<<20); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("symlink: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a command from the repo's .git/ ran")
	}
}

// gitOut runs git in dir and returns its stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitIn(t, dir, "", args...)
}

// gitIn runs git in dir with stdin and returns its stdout.
func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
