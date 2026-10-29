package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNamespaceDefault: without -n a command takes the namespace from the origin of the checkout it runs in, and says
// so on stderr; outside a checkout -n is required; a namespace <work> doesn't have, or an origin on another host than
// forgejo-url's, is refused; -n wins over the checkout.
func TestNamespaceDefault(t *testing.T) {
	repo := gitRepo(t, "ssh://git@forgejo.example.org:2222/michael/devops.git")
	sub := filepath.Join(repo, "ai", "work")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "w")
	writeConfig(t, work, "https://forgejo.example.org")

	t.Chdir(t.TempDir())
	if code, _, errs := run("agent", "list", "--work", work); code != exitUsage || !strings.Contains(errs, "-n <owner>/<repo> is required outside a checkout of the repo") {
		t.Errorf("outside a checkout: %d %q", code, errs)
	}
	t.Chdir(sub)
	if code, _, errs := run("agent", "list", "--work", work); code != exitFail || !strings.Contains(errs, "michael/devops, from the origin of "+repo+", is not a namespace in "+work) {
		t.Errorf("a namespace <work> doesn't have: %d %q", code, errs)
	}
	if code, _, errs := run("register", "--repo", repo, "--agent", "main", "--work", work); code != exitOK {
		t.Fatalf("register: %d %q", code, errs)
	}
	said := "namespace michael/devops (the origin of " + repo + ")"
	if code, out, errs := run("agent", "list", "--work", work); code != exitOK || !strings.Contains(errs, said) || !strings.Contains(out, "main") {
		t.Errorf("agent list in the checkout: %d %q %q", code, out, errs)
	}
	if code, _, errs := run("get", "--work", work); code != exitOK || !strings.Contains(errs, said) {
		t.Errorf("get in the checkout: %d %q", code, errs)
	}
	if code, _, errs := run("agent", "list", "-n", "michael/devops", "--work", work); code != exitOK || strings.Contains(errs, "the origin of") {
		t.Errorf("-n: %d %q", code, errs)
	}
	writeConfig(t, work, "https://other.example.org")
	if code, _, errs := run("agent", "list", "--work", work); code != exitFail || !strings.Contains(errs, "not on forgejo-url's host other.example.org") {
		t.Errorf("origin on another host: %d %q", code, errs)
	}
}
