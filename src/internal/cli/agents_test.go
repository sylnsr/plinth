package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"plinth/internal/agents"
	"plinth/internal/controller"
	"plinth/internal/names"
)

func TestAgentEnable(t *testing.T) {
	origin := "ssh://git@forgejo.example.org:2222/michael/devops.git"
	repoA, repoB := gitRepo(t, origin), gitRepo(t, origin)
	work := filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://forgejo.example.org")
	ns := names.Namespace{Owner: "michael", Repo: "devops"}
	dir := filepath.Join(work, "michael", "devops", "state", "agents")
	nsArgs := []string{"-n", "michael/devops", "--work", work}
	agent := func(answer string, args ...string) (int, string, string) {
		stdin = strings.NewReader(answer)
		t.Cleanup(func() { stdin = os.Stdin })
		last := len(args) - 1 // flags before <agent>: flag parsing stops at the first argument
		return run(append(append(append([]string{"agent"}, args[:last]...), nsArgs...), args[last])...)
	}
	list := func() string {
		code, out, errs := run("agent", "list", "-n", "michael/devops", "--work", work)
		if code != exitOK {
			t.Fatalf("agent list: %d %q", code, errs)
		}
		return strings.Join(strings.Fields(out), " ")
	}
	nsConfig := func() string {
		b, _ := os.ReadFile(filepath.Join(work, "michael", "devops", "config.yml"))
		return string(b)
	}

	// Step one: registered, not enabled. The user is told how to enable it.
	code, _, errs := run("register", "--repo", repoA, "--agent", "main", "--work", work)
	if code != exitOK || !strings.Contains(errs, "registered but not enabled in michael/devops") || !strings.Contains(errs, "plinthctl agent enable -n michael/devops main") {
		t.Fatalf("register: %d %q", code, errs)
	}
	if got := list(); got != "AGENT STATE REPO main pending "+repoA {
		t.Errorf("list after register: %q", got)
	}

	// Step two: enable shows the repo and asks; anything but "enable" changes nothing.
	code, out, _ := agent("no\n", "enable", "main")
	if code != exitFail || !strings.Contains(out, repoA) || !strings.Contains(out, "Nothing changed") || strings.Contains(nsConfig(), "main:") {
		t.Errorf("enable, answered no: %d %q\n%s", code, out, nsConfig())
	}
	if code, out, errs := agent("yes\n", "enable", "main"); code != exitOK || !strings.Contains(out, "Enabled main in michael/devops for "+repoA) {
		t.Fatalf("enable: %d %q %q", code, out, errs)
	}
	if !strings.Contains(nsConfig(), "main: "+repoA) {
		t.Errorf("not in the namespace config:\n%s", nsConfig())
	}
	if got := list(); got != "AGENT STATE REPO main enabled "+repoA {
		t.Errorf("list after enable: %q", got)
	}

	// From here on the controller's lock is held, as a running controller holds it: the log says whether it serves the new
	// token now or only once the agent is enabled.
	lock, err := os.OpenFile(filepath.Join(work, filepath.FromSlash(controller.LockFile)), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	logged := func(line string) int {
		logs, _ := filepath.Glob(filepath.Join(work, "logs", "plinth-*.log"))
		n := 0
		for _, l := range logs {
			b, _ := os.ReadFile(l)
			n += strings.Count(string(b), `msg="`+line+`" `)
		}
		return n
	}

	// Registering again from the same repo: a new token, still enabled, no note.
	if code, _, errs := run("register", "--repo", repoA, "--agent", "main", "--work", work); code != exitOK || strings.Contains(errs, "not enabled") {
		t.Errorf("register again: %d %q", code, errs)
	}

	// From another repo: the registration waits beside the current one, whose token keeps working.
	before, _ := os.ReadFile(filepath.Join(dir, "main.sha256"))
	code, _, errs = run("register", "--repo", repoB, "--agent", "main", "--work", work)
	if code != exitOK || !strings.Contains(errs, "waits beside it until you run: plinthctl agent enable -n michael/devops main") {
		t.Errorf("register from another repo: %d %q", code, errs)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "main.sha256")); string(after) != string(before) {
		t.Error("the current registration's token changed")
	}
	if n, m := logged("the controller is running and serves the new token"), logged("the controller is running and serves the new token once the agent is enabled (plinthctl agent enable)"); n != 1 || m != 1 {
		t.Errorf("controller lines in the log: %d served now (want 1, the same repo), %d once enabled (want 1, the pending one)", n, m)
	}
	if got := list(); got != "AGENT STATE REPO main enabled "+repoA+" pending "+repoB {
		t.Errorf("list with a pending registration: %q", got)
	}
	// Enabling it makes it current.
	code, out, errs = agent("yes\n", "enable", "main")
	if code != exitOK || !strings.Contains(out, "pending registration from\n  "+repoB) || !strings.Contains(out, "token stops working") {
		t.Fatalf("enable the pending one: %d %q %q", code, out, errs)
	}
	if reg, err := agents.Get(work, ns, "main"); err != nil || reg.Repo != repoB || reg.Pending != "" {
		t.Errorf("after enabling the pending one: %+v %v", reg, err)
	}
	if !strings.Contains(nsConfig(), "main: "+repoB) || strings.Contains(nsConfig(), repoA) {
		t.Errorf("namespace config after enabling the pending one:\n%s", nsConfig())
	}

	// Disable: still registered, not enabled; twice is no error.
	if code, out, _ := agent("", "disable", "main"); code != exitOK || !strings.Contains(out, "Disabled main") {
		t.Errorf("disable: %d %q", code, out)
	}
	if got := list(); got != "AGENT STATE REPO main pending "+repoB {
		t.Errorf("list after disable: %q", got)
	}
	if code, out, _ := agent("", "disable", "main"); code != exitOK || !strings.Contains(out, "is not enabled") {
		t.Errorf("disable twice: %d %q", code, out)
	}

	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"enable", "al"}, exitFail, "agent al is not registered in michael/devops"},
		{[]string{"enable", "user"}, exitUsage, "not an agent name"},
		{[]string{"disable", "main.pending"}, exitUsage, "not an agent name"},
	} {
		if code, _, errs := agent("yes\n", c.args...); code != c.code || !strings.Contains(errs, c.want) {
			t.Errorf("agent %v: %d %q, want %d %q", c.args, code, errs, c.code, c.want)
		}
	}
	t.Chdir(t.TempDir()) // outside any checkout: no namespace to default to
	if code, _, errs := run("agent", "enable", "--work", work, "main"); code != exitUsage || !strings.Contains(errs, "-n <owner>/<repo> is required") {
		t.Errorf("enable without -n: %d %q", code, errs)
	}
}

// TestAgentGuide: agent guide prints the guide's fixed part and the namespace's own, as the guide tool gives it.
func TestAgentGuide(t *testing.T) {
	work := filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://forgejo.example.org") // security, automatic, defined inline
	if _, _, err := agents.Register(work, names.Namespace{Owner: "michael", Repo: "devops"}, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("agent", "guide", "-n", "michael/devops", "--work", work)
	for _, want := range []string{"# Plinth: how you work with it\n", "## This namespace: michael/devops\n", "network (`NETWORK=1`",
		": not allowed here; privileged", "Automatic reviews, which every Plinth run's script gets before the user decides: `security`.",
		"- `security` (automatic): defined in Plinth's config, not published"} {
		if code != exitOK || !strings.Contains(out, want) {
			t.Errorf("agent guide: %d %q: no %q in\n%s", code, errs, want, out)
		}
	}
	if code, _, errs := run("agent", "guide", "-n", "michael/nope", "--work", work); code != exitFail || !strings.Contains(errs, "michael/nope is not set up") {
		t.Errorf("a namespace that isn't set up: %d %q", code, errs)
	}
}
