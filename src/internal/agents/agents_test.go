package agents

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"plinth/internal/names"
	"plinth/internal/safefs"
)

var ns = names.Namespace{Owner: "michael", Repo: "devops"}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewToken(t *testing.T) {
	re := regexp.MustCompile(`^plinth_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for range 100 {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(tok) {
			t.Fatalf("token %q has the wrong form", tok)
		}
		if seen[tok] {
			t.Fatal("the same token twice")
		}
		seen[tok] = true
	}
	if got := Fingerprint("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("Fingerprint(abc) = %s", got)
	}
}

func TestRegister(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	dir := filepath.Join(work, "michael", "devops", "state", "agents")

	tok1, created, err := Register(work, ns, "main", "/home/u/git/devops")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".", "config.yml", "michael/devops/config.yml", "michael/devops/state/agents"} {
		found := false
		for _, c := range created {
			found = found || c == filepath.FromSlash(p)
		}
		if !found {
			t.Errorf("%s not reported as created: %v", p, created)
		}
	}
	if got := read(t, filepath.Join(dir, "main.sha256")); got != Fingerprint(tok1)+"\n" {
		t.Errorf("main.sha256 = %q, want the token's sha256", got)
	}
	if got := read(t, filepath.Join(dir, "main.repo")); got != "/home/u/git/devops\n" {
		t.Errorf("main.repo = %q", got)
	}
	for _, f := range []string{"main.sha256", "main.repo"} {
		if fi, _ := os.Stat(filepath.Join(dir, f)); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, fi.Mode().Perm())
		}
	}
	// The token itself is stored nowhere below <work>.
	filepath.WalkDir(work, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(read(t, p), tok1) {
			t.Errorf("%s holds the token", p)
		}
		return nil
	})

	// Registering again from the same repo: a new token, the old one's sha256 is gone, nothing new created (from another
	// repo it waits as pending: TestPending).
	tok2, created, err := Register(work, ns, "main", "/home/u/git/devops")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 == tok1 || len(created) != 0 {
		t.Errorf("second Register: same token %v, created %v", tok2 == tok1, created)
	}
	if got := read(t, filepath.Join(dir, "main.sha256")); got != Fingerprint(tok2)+"\n" {
		t.Error("main.sha256 does not hold the new token's sha256")
	}
	if got := read(t, filepath.Join(dir, "main.repo")); got != "/home/u/git/devops\n" {
		t.Errorf("main.repo = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "main.pending.sha256")); err == nil {
		t.Error("a pending registration from the same repo")
	}

	// A second agent in the same namespace leaves the first one alone.
	tok3, _, err := Register(work, ns, "al", "/home/u/git/devops")
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "main.sha256")) != Fingerprint(tok2)+"\n" || read(t, filepath.Join(dir, "al.sha256")) != Fingerprint(tok3)+"\n" {
		t.Error("registering al changed main")
	}
}

func TestRegisterRefuses(t *testing.T) {
	work := t.TempDir()
	for _, c := range []struct{ agent, repo string }{
		{"Main", "/r"}, {"../x", "/r"}, {"", "/r"}, {"main", "relative"}, {"main", "/r\nx"},
		{"user", "/r"}, {"worker", "/r"}, {"messages", "/r"}, // reserved names
	} {
		if _, _, err := Register(work, ns, c.agent, c.repo); err == nil {
			t.Errorf("Register(%q, %q) did not fail", c.agent, c.repo)
		}
	}
	if _, _, err := Register(work, names.Namespace{Owner: "..", Repo: "x"}, "main", "/r"); err == nil {
		t.Error("Register accepted owner ..")
	}
	// A symlinked agents directory, even one inside <work>, is never written through.
	if _, _, err := Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "michael", "devops", "state", "agents")
	if err := os.Rename(dir, dir+"-real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("agents-real", dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Register(work, ns, "main", "/r"); !errors.Is(err, safefs.ErrSymlink) {
		t.Errorf("Register through a symlinked agents directory = %v, want ErrSymlink", err)
	}
}

// TestRotate: a new token for the current registration, the same repo, the pending registration's token untouched; an
// agent that isn't registered gets none.
func TestRotate(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "michael", "devops", "state", "agents")
	if _, err := Rotate(work, ns, "main"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("Rotate of an unregistered agent = %v", err)
	}
	tok1, _, err := Register(work, ns, "main", "/r")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Register(work, ns, "main", "/x"); err != nil {
		t.Fatal(err)
	}
	pending := read(t, filepath.Join(dir, "main"+PendingSuffix+".sha256"))
	tok2, err := Rotate(work, ns, "main")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 == tok1 || read(t, filepath.Join(dir, "main.sha256")) != Fingerprint(tok2)+"\n" {
		t.Error("main.sha256 doesn't hold the new token's sha256")
	}
	if r, _ := Get(work, ns, "main"); r != (Registration{Repo: "/r", Pending: "/x"}) {
		t.Errorf("Get after Rotate = %+v", r)
	}
	if read(t, filepath.Join(dir, "main"+PendingSuffix+".sha256")) != pending {
		t.Error("Rotate replaced the pending registration's token")
	}
}

func TestRemove(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "michael", "devops", "state", "agents")
	if _, _, err := Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Register(work, ns, "al", "/r"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(work, ns, "main"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"main.sha256", "main.repo"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s still there", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "al.sha256")); err != nil {
		t.Errorf("removing main removed al: %v", err)
	}
	if err := Remove(work, ns, "main"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("Remove of an unregistered agent = %v", err)
	}
}

func TestList(t *testing.T) {
	work := t.TempDir()
	if got, err := List(work, ns); err != nil || len(got) != 0 {
		t.Errorf("List of a namespace not set up = %v, %v", got, err)
	}
	for _, a := range []string{"main", "al"} {
		if _, _, err := Register(work, ns, a, "/r"); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(work, "michael", "devops", "state", "agents")
	// Not agents: a bad name, a reserved name, a symlink, a .repo without a .sha256, a directory.
	for _, f := range []string{"Bad.sha256", "user.sha256", "x.repo"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("main.sha256", filepath.Join(dir, "ghost.sha256")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.sha256"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := List(work, ns); err != nil || strings.Join(got, ",") != "al,main" {
		t.Errorf("List = %v, %v; want [al main]", got, err)
	}
	if err := Remove(work, ns, "al"); err != nil {
		t.Fatal(err)
	}
	if got, _ := List(work, ns); strings.Join(got, ",") != "main" {
		t.Errorf("List after remove = %v", got)
	}
}

// TestPending: a register from another repo waits beside the current registration, which keeps working, until Promote
// makes it current; Enabled needs the config to name the current registration's repo.
func TestPending(t *testing.T) {
	work := t.TempDir()
	if _, _, err := Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	sum := filepath.Join(work, "michael", "devops", "state", "agents", "main.sha256")
	before, _ := os.ReadFile(sum)
	if r, err := Get(work, ns, "main"); err != nil || r != (Registration{Repo: "/r"}) {
		t.Errorf("Get = %+v, %v", r, err)
	}
	if _, _, err := Register(work, ns, "main", "/x"); err != nil {
		t.Fatal(err)
	}
	if r, err := Get(work, ns, "main"); err != nil || r != (Registration{Repo: "/r", Pending: "/x"}) {
		t.Errorf("Get after a register from /x = %+v, %v", r, err)
	}
	if after, _ := os.ReadFile(sum); string(after) != string(before) {
		t.Error("a register from another repo replaced the current token")
	}
	if got, _ := List(work, ns); strings.Join(got, ",") != "main" {
		t.Errorf("List = %v: the pending slot counts as an agent", got)
	}
	for enabled, want := range map[string]bool{"/r": true, "/x": false, "": false} {
		if ok, err := Enabled(work, ns, "main", map[string]string{"main": enabled}); ok != want || err != nil {
			t.Errorf("Enabled for %q = %v, %v", enabled, ok, err)
		}
	}
	// The same repo again rotates the current token and leaves the pending one alone.
	if _, _, err := Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	if r, _ := Get(work, ns, "main"); r != (Registration{Repo: "/r", Pending: "/x"}) {
		t.Errorf("Get after rotating = %+v", r)
	}
	if err := Promote(work, ns, "main"); err != nil {
		t.Fatal(err)
	}
	if r, _ := Get(work, ns, "main"); r != (Registration{Repo: "/x"}) {
		t.Errorf("Get after Promote = %+v", r)
	}
	if err := Promote(work, ns, "main"); err == nil {
		t.Error("Promote without a pending registration passed")
	}
	if ok, _ := Enabled(work, ns, "main", map[string]string{"main": "/x"}); !ok {
		t.Error("not enabled for /x after Promote")
	}
	if _, _, err := Register(work, ns, "main", "/y"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(work, ns, "main"); err != nil {
		t.Fatal(err)
	}
	if r, _ := Get(work, ns, "main"); r != (Registration{}) {
		t.Errorf("Get after Remove = %+v", r)
	}
}

// TestByToken: a current token gives its agent and namespace, across namespaces; a pending registration's token, a
// rotated-out one, a removed agent's and anything else match nothing.
func TestByToken(t *testing.T) {
	work := t.TempDir()
	other := names.Namespace{Owner: "michael", Repo: "other"}
	mainTok, _, err := Register(work, ns, "main", "/r")
	if err != nil {
		t.Fatal(err)
	}
	ci, _, err := Register(work, other, "ci", "/o")
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := Register(work, ns, "main", "/x")
	if err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]string{mainTok: "michael/devops main", ci: "michael/other ci"} {
		if n, a, ok, err := ByToken(work, tok); err != nil || !ok || n.String()+" "+a != want {
			t.Errorf("ByToken: %v %q %v %v, want %s", n, a, ok, err, want)
		}
	}
	rotated, err := Rotate(work, other, "ci")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{pending, ci, "", "plinth_", "nope", Fingerprint(mainTok), strings.ToUpper(mainTok)} {
		if _, _, ok, err := ByToken(work, tok); ok || err != nil {
			t.Errorf("ByToken(%q) matched (%v)", tok, err)
		}
	}
	if _, a, ok, _ := ByToken(work, rotated); !ok || a != "ci" {
		t.Error("the rotated token doesn't match")
	}
	if err := Remove(work, ns, "main"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := ByToken(work, mainTok); ok {
		t.Error("a removed agent's token matched")
	}
}
