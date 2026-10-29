package gitrepo

import (
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gitServer serves the repos under root over https with git http-backend, answering only requests that carry token
// as Basic auth, and records the Authorization headers it saw. It returns the server and a CA file for its certificate.
func gitServer(t *testing.T, root, token string) (*httptest.Server, string, func() []string) {
	t.Helper()
	backend, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	var mu sync.Mutex
	var seen []string
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(token+":x-oauth-basic"))
	cgiHandler := &cgi.Handler{Path: backend, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// TestFetch: a branch is fetched over https into a new bare repo with the token as a header; no hooks; a commit off
// the branch isn't on it; a wrong token, a redirect, a URL that isn't https and a dir that exists all fail, and no
// message quotes the URL.
func TestFetch(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	gitCmd(t, filepath.Dir(src), "init", "-q", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", "f")
	gitCmd(t, src, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "-m", "one")
	gitCmd(t, src, "checkout", "-q", "-b", "side")
	gitCmd(t, src, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "--allow-empty", "-m", "side")
	side := strings.TrimSpace(gitOut(t, src, "rev-parse", "HEAD"))
	gitCmd(t, src, "checkout", "-q", "main")
	head := strings.TrimSpace(gitOut(t, src, "rev-parse", "HEAD"))
	if err := os.MkdirAll(filepath.Join(root, "michael"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "clone", "-q", "--bare", src, filepath.Join(root, "michael", "devops.git"))

	srv, ca, seen := gitServer(t, root, "plinth-token")
	remote := Remote{URL: srv.URL + "/michael/devops.git", Token: "plinth-token", CAFile: ca}
	dir := filepath.Join(t.TempDir(), "git")
	commit, err := Fetch(dir, remote, "main")
	if err != nil || commit != head {
		t.Fatalf("Fetch = %q, %v; want %s", commit, err, head)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks")); err == nil {
		t.Error("the bare repo has a hooks directory")
	}
	if !OnBranch(dir, "main", head) || OnBranch(dir, "main", side) {
		t.Error("OnBranch: the head is on main, the side commit isn't (it wasn't even fetched)")
	}
	if files, err := Files(dir, strings.TrimSpace(gitOut(t, dir, "rev-parse", head+"^{tree}")), 1<<20); err != nil || len(files) != 1 {
		t.Errorf("Files of the fetched tree: %v, %v", files, err)
	}

	bad := func(what string, d string, r Remote, branch string) {
		t.Helper()
		_, err := Fetch(d, r, branch)
		if err == nil {
			t.Errorf("%s: fetched", what)
		} else if strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "plinth-token") {
			t.Errorf("%s: the error quotes the URL or the token: %v", what, err)
		}
	}
	// A desktop's password dialog: SSH_ASKPASS is what git asks on a 401 when nothing else is set (it popped up).
	asked := filepath.Join(t.TempDir(), "asked")
	askpass := filepath.Join(t.TempDir(), "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\ntouch "+asked+"\necho guess\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_ASKPASS", askpass)
	t.Setenv("SSH_ASKPASS_REQUIRE", "force")
	t.Setenv("GIT_ASKPASS", askpass)
	fresh := func() string { return filepath.Join(t.TempDir(), "git") }
	bad("a wrong token", fresh(), Remote{URL: remote.URL, Token: "wrong", CAFile: ca}, "main")
	if _, err := os.Stat(asked); err == nil {
		t.Error("a wrong token made git ask for a password")
	}
	bad("no such branch", fresh(), remote, "nope")
	bad("the dir exists", dir, remote, "main")
	bad("http", fresh(), Remote{URL: strings.Replace(remote.URL, "https:", "http:", 1), Token: "plinth-token", CAFile: ca}, "main")
	bad("file", fresh(), Remote{URL: "file://" + root + "/michael/devops.git"}, "main")
	bad("a user in the URL", fresh(), Remote{URL: strings.Replace(remote.URL, "https://", "https://u:p@", 1), CAFile: ca}, "main")
	bad("an option for a branch", fresh(), remote, "--upload-pack=x")
	bad("a range for a branch", fresh(), remote, "main..side")

	// A redirect is never followed: the token's header would go to the other URL.
	before := len(seen())
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer redirect.Close()
	rca := filepath.Join(t.TempDir(), "rca.pem")
	if err := os.WriteFile(rca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	bad("a redirect", fresh(), Remote{URL: redirect.URL + "/michael/devops.git", Token: "plinth-token", CAFile: rca}, "main")
	if after := seen(); len(after) != before {
		t.Errorf("the redirect was followed: %d more requests reached the server", len(after)-before)
	}
}
