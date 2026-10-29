package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"plinth/internal/agents"
)

// gitRepo creates a git repo with the given origin ("" for none) and returns its top level.
func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	cmds := [][]string{{"init", "-q"}}
	if origin != "" {
		cmds = append(cmds, []string{"remote", "add", "origin", origin})
	}
	for _, args := range cmds {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	top, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return top
}

func TestRegister(t *testing.T) {
	repo := gitRepo(t, "ssh://git@forgejo.example.org:2222/michael/devops.git")
	sub := filepath.Join(repo, "ai", "agents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), ".plinth")
	dir := filepath.Join(work, "michael", "devops", "state", "agents")

	// stdout is the token and nothing else, so $(...) captures exactly it.
	code, out, errs := run("register", "--repo", sub, "--agent", "main", "--work", work)
	if code != exitOK {
		t.Fatalf("register: %d %q", code, errs)
	}
	if !regexp.MustCompile(`^plinth_[A-Za-z0-9_-]{43}\n$`).MatchString(out) {
		t.Fatalf("stdout is not just the token: %q", out)
	}
	tok1 := strings.TrimSpace(out)
	if b, _ := os.ReadFile(filepath.Join(dir, "main.sha256")); string(b) != agents.Fingerprint(tok1)+"\n" {
		t.Errorf("main.sha256 = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.repo")); string(b) != repo+"\n" {
		t.Errorf("main.repo = %q, want the repo's top level %s", b, repo)
	}
	// stderr: only what the user acts on. The rest is in the log.
	for _, want := range []string{"fill in " + filepath.Join(work, "config.yml"), "controller is not running"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errs)
		}
	}
	for _, notWant := range []string{"created ", "registered agent", filepath.Join(work, "michael", "devops", "config.yml")} {
		if strings.Contains(errs, notWant) {
			t.Errorf("stderr holds %q, which belongs in the log:\n%s", notWant, errs)
		}
	}
	logs, _ := filepath.Glob(filepath.Join(work, "logs", "plinth-*.log"))
	if len(logs) != 1 {
		t.Fatalf("log files: %v, want one", logs)
	}
	logText, _ := os.ReadFile(logs[0])
	for _, want := range []string{"msg=created component=cli ", "command=register path=" + work + "\n", "command=register path=" + filepath.Join(work, "michael", "devops", "config.yml") + "\n",
		"agent=main ns=michael/devops repo=" + repo + "\n"} {
		if !strings.Contains(string(logText), want) {
			t.Errorf("log lacks %q:\n%s", want, logText)
		}
	}
	if strings.Contains(string(logText), tok1) {
		t.Error("the token is in the log")
	}
	if strings.Contains(errs, tok1) {
		t.Error("the token is on stderr")
	}

	// Again: a new token replaces the old one; nothing is created.
	code, out, errs = run("register", "--repo", repo, "--agent", "main", "--work", work)
	tok2 := strings.TrimSpace(out)
	if code != exitOK || tok2 == tok1 || strings.Contains(errs, "created ") {
		t.Errorf("second register: %d, same token %v\n%s", code, tok2 == tok1, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.sha256")); string(b) != agents.Fingerprint(tok2)+"\n" {
		t.Error("main.sha256 still holds the old token's sha256")
	}

	// -n overrides the origin remote.
	if code, _, errs := run("register", "--repo", repo, "--agent", "al", "-n", "ai/docs", "--work", work); code != exitOK {
		t.Errorf("register -n: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(work, "ai", "docs", "state", "agents", "al.sha256")); err != nil {
		t.Errorf("-n was not used: %v", err)
	}

	// agent token-rotate: a new token, stdout only, the same repo.
	code, out, errs = run("agent", "token-rotate", "-n", "michael/devops", "--work", work, "main")
	tok3 := strings.TrimSpace(out)
	if code != exitOK || !regexp.MustCompile(`^plinth_[A-Za-z0-9_-]{43}\n$`).MatchString(out) || tok3 == tok2 {
		t.Errorf("agent token-rotate: %d %q\n%s", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.sha256")); string(b) != agents.Fingerprint(tok3)+"\n" {
		t.Error("main.sha256 doesn't hold the rotated token's sha256")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.repo")); string(b) != repo+"\n" {
		t.Errorf("main.repo after agent token-rotate = %q", b)
	}
	if strings.Contains(errs, tok3) || !strings.Contains(errs, "controller is not running") {
		t.Errorf("agent token-rotate stderr:\n%s", errs)
	}

	// agent token-remove, by --repo and by -n.
	if code, _, errs := run("agent", "token-remove", "--repo", repo, "--work", work, "main"); code != exitOK {
		t.Errorf("agent token-remove: %d %q", code, errs)
	}
	if _, err := os.Lstat(filepath.Join(dir, "main.sha256")); err == nil {
		t.Error("main.sha256 still there after agent token-remove")
	}
	if code, _, errs := run("agent", "token-remove", "--repo", repo, "--work", work, "main"); code != exitFail || !strings.Contains(errs, "not registered") {
		t.Errorf("agent token-remove twice: %d %q", code, errs)
	}
	if code, _, errs := run("agent", "token-remove", "-n", "ai/docs", "--work", work, "al"); code != exitOK {
		t.Errorf("agent token-remove -n: %d %q", code, errs)
	}
}

func TestRegisterRefuses(t *testing.T) {
	noOrigin := gitRepo(t, "")
	work := t.TempDir()
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"register", "--agent", "main"}, exitUsage, "--repo is required"},
		{[]string{"register", "--repo", noOrigin}, exitUsage, "--agent is required"},
		{[]string{"register", "--repo", noOrigin, "--agent", "Main"}, exitUsage, "not a valid agent name"},
		{[]string{"register", "--repo", noOrigin, "--agent", "user"}, exitUsage, "not a valid agent name"},
		{[]string{"register", "--repo", noOrigin, "--agent", "worker"}, exitUsage, "not user, worker, runner, controller, messages"},
		{[]string{"register", "--repo", noOrigin, "--agent", "main", "extra"}, exitUsage, "too many arguments"},
		{[]string{"register", "--repo", noOrigin, "--agent", "main", "--work", work}, exitFail, "give -n <owner>/<repo>"},
		{[]string{"register", "--repo", t.TempDir(), "--agent", "main", "--work", work}, exitFail, "not inside a git repo"},
		{[]string{"register", "--repo", noOrigin, "--agent", "main", "-n", "../x", "--work", work}, exitFail, "not <owner>/<repo>"},
		{[]string{"agent", "token-remove", "-n", "michael/devops", "--work", work}, exitUsage, "one <agent> is required"},
		{[]string{"agent", "token-remove", "-n", "michael/devops", "--work", work, "main"}, exitFail, "not registered"},
		{[]string{"agent", "token-rotate", "-n", "michael/devops", "--work", work}, exitUsage, "one <agent> is required"},
		{[]string{"agent", "token-rotate", "-n", "michael/devops", "--work", work, "Main"}, exitUsage, "not an agent name"},
		{[]string{"agent", "token-rotate", "-n", "michael/devops", "--work", work, "main"}, exitFail, "plinthctl register registers it"},
	} {
		code, out, errs := run(c.args...)
		if code != c.code || !strings.Contains(errs, c.msg) || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit %d and %q", c.args, code, out, errs, c.code, c.msg)
		}
	}
}

// writeConfig writes <work>/config.yml (creating <work>) with forgejo-url and one review type.
func writeConfig(t *testing.T, work, forgejoURL string) {
	t.Helper()
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "forgejo-url: " + forgejoURL + "\nreviewers:\n  type:\n    security:\n      definition: review it\n  automatic: [security]\n"
	if err := os.WriteFile(filepath.Join(work, "config.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterChecksOrigin(t *testing.T) {
	repo := gitRepo(t, "ssh://git@forgejo.example.org:2222/michael/devops.git")

	// origin on forgejo-url's host: registered, no note.
	work := filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://Forgejo.Example.org")
	if code, _, errs := run("register", "--repo", repo, "--agent", "main", "--work", work); code != exitOK || strings.Contains(errs, "not checked") {
		t.Errorf("origin on forgejo-url's host: %d %q", code, errs)
	}

	// origin elsewhere: refused before anything is written, unless -n names the namespace.
	work = filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://other.example.org")
	code, out, errs := run("register", "--repo", repo, "--agent", "main", "--work", work)
	if code != exitFail || out != "" || !strings.Contains(errs, "is on forgejo.example.org, not on forgejo-url's host other.example.org") {
		t.Errorf("origin on another host: %d %q %q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(work, "michael")); err == nil {
		t.Error("a refused register set up the namespace")
	}
	if code, _, errs := run("register", "--repo", repo, "--agent", "main", "-n", "michael/devops", "--work", work); code != exitOK {
		t.Errorf("-n skips the check: %d %q", code, errs)
	}

	// A config with problems: registered all the same, with a note.
	work = filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "not-a-url")
	if code, _, errs := run("register", "--repo", repo, "--agent", "main", "--work", work); code != exitOK || !strings.Contains(errs, "origin was not checked against forgejo-url") {
		t.Errorf("config with problems: %d %q", code, errs)
	}

	// The error never quotes a remote URL, which may hold user:token.
	secret := gitRepo(t, "https://user:s3cret@other.example.org/michael/devops.git")
	work = filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://forgejo.example.org")
	if code, _, errs := run("register", "--repo", secret, "--agent", "main", "--work", work); code != exitFail || strings.Contains(errs, "s3cret") {
		t.Errorf("origin with a token: %d %q", code, errs)
	}
}

func TestConfigCheck(t *testing.T) {
	repo := gitRepo(t, "ssh://git@forgejo.example.org:2222/michael/devops.git")
	work := filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://forgejo.example.org")
	if code, _, errs := run("register", "--repo", repo, "--agent", "main", "--work", work); code != exitOK {
		t.Fatalf("register: %d %q", code, errs)
	}
	code, out, errs := run("config", "check", "--work", work)
	if code != exitOK || !strings.Contains(out, "ok: "+filepath.Join(work, "config.yml")) ||
		!strings.Contains(out, "(michael/devops)") || !strings.Contains(out, "reviewers.automatic: [security] (global)") {
		t.Errorf("config check: %d\n%s%s", code, out, errs)
	}
	nsConfig := filepath.Join(work, "michael", "devops", "config.yml")
	if err := os.WriteFile(nsConfig, []byte("reviewers:\n  automatic: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("config", "check", "--work", work, "-n", "michael/devops"); code != exitOK || !strings.Contains(out, "reviewers.automatic: [] (namespace)") {
		t.Errorf("config check after automatic: []: %d\n%s", code, out)
	}
	if err := os.WriteFile(nsConfig, []byte("forgejo-url: https://x.example.org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("config", "check", "--work", work); code != exitFail || !strings.Contains(out, "field forgejo-url not found") {
		t.Errorf("config check with a global key in a namespace: %d\n%s", code, out)
	}
	writeConfig(t, work, "not-a-url")
	if code, out, _ := run("config", "check", "--work", work); code != exitFail || !strings.Contains(out, "1 namespace config(s) not checked") {
		t.Errorf("config check with a broken global config: %d\n%s", code, out)
	}
}
