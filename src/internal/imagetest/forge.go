// Package imagetest is a stand-in for Forgejo for the tests of the image commands and the controller's drift check:
// one local https server with the API Plinth reads, git over https (git http-backend) and the least of a container
// registry, behind the auth Forgejo asks for. Only tests import it.
package imagetest

import (
	"encoding/base64"
	"encoding/json"
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

	"plinth/internal/names"
)

// DefaultImage is the build's own image: present wherever plinthctl was built.
const DefaultImage = "docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89"

// Registry login of the test registry.
const (
	User     = "tester"
	Password = "secret"
)

// NeedTools returns the image a test builds on. Without podman, skopeo, git or the image the test skips, unless
// PLINTH_TEST_PODMAN=required, which makes that a failure.
func NeedTools(t *testing.T) string {
	t.Helper()
	required := os.Getenv("PLINTH_TEST_PODMAN") == "required"
	for _, tool := range []string{"podman", "skopeo", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			if required {
				t.Fatalf("PLINTH_TEST_PODMAN=required, but there is no %s", tool)
			}
			t.Skip("no " + tool)
		}
	}
	img := os.Getenv("PLINTH_TEST_IMAGE")
	if img == "" {
		img = DefaultImage
	}
	if err := exec.Command("podman", "image", "exists", img).Run(); err != nil {
		if required {
			t.Fatalf("PLINTH_TEST_PODMAN=required, but image %s is not present", img)
		}
		t.Skipf("image %s is not present", img)
	}
	return img
}

// Forge is the stand-in for Forgejo, serving one namespace whose default branch is main.
type Forge struct {
	URL   string // https://127.0.0.1:<port>
	Host  string // 127.0.0.1:<port>: the registry's host too
	CA    string // a PEM file with the server's certificate
	Reg   *Registry
	NS    names.Namespace
	Token string // Plinth's token: the API wants it as "token <t>", git as Basic <t>:x-oauth-basic
	Src   string // a checkout of the repo; Commit pushes from it
	Bare  string // the repo the server serves

	mu        sync.Mutex
	protected bool
}

// NewForge starts the server for ns, with an empty repo (Commit adds to it) and its branch protected.
func NewForge(t *testing.T, ns names.Namespace, token string) *Forge {
	t.Helper()
	root := t.TempDir()
	f := &Forge{Reg: NewRegistry(User, Password), NS: ns, Token: token, Src: t.TempDir(), protected: true,
		Bare: filepath.Join(root, ns.Owner, ns.Repo+".git")}
	git(t, f.Src, "init", "-q", "-b", "main")
	gitPath, _ := exec.LookPath("git")
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(token+":x-oauth-basic"))
	api := "/api/v1/repos/" + ns.String()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/"):
			f.Reg.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/v1/"):
			if r.Header.Get("Authorization") != "token "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case api:
				json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
			case api + "/branches/main":
				head, _ := exec.Command("git", "-C", f.Bare, "rev-parse", "main").Output()
				f.mu.Lock()
				protected := f.protected
				f.mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]string{"id": strings.TrimSpace(string(head))},
					"protected": protected, "required_approvals": 1})
			default:
				commit, ok := strings.CutPrefix(r.URL.Path, api+"/git/trees/")
				if !ok || strings.HasPrefix(commit, "-") {
					http.NotFound(w, r)
					return
				}
				out, err := exec.Command("git", "-C", f.Bare, "ls-tree", commit).Output()
				if err != nil {
					http.NotFound(w, r)
					return
				}
				var tree []map[string]string
				for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") { // <mode> <type> <sha>\t<path>
					meta, path, _ := strings.Cut(l, "\t")
					if m := strings.Fields(meta); len(m) == 3 {
						tree = append(tree, map[string]string{"path": path, "type": m[1], "sha": m[2]})
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"sha": commit, "tree": tree, "truncated": false})
			}
		default:
			if r.Header.Get("Authorization") != basic {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			backend.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f.URL, f.Host = srv.URL, strings.TrimPrefix(srv.URL, "https://")
	f.CA = filepath.Join(t.TempDir(), "ca.pem")
	write(t, f.CA, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	return f
}

// Protect sets what the branch API says about main's protection.
func (f *Forge) Protect(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.protected = on
}

// Repo is the namespace's image repo in the test registry.
func (f *Forge) Repo() string {
	return f.Host + "/" + strings.ToLower(f.NS.Owner) + "/plinth-" + strings.ToLower(f.NS.Repo)
}

// Trust makes podman, skopeo and this process trust the server's certificate, as they trust Forgejo's (Go programs
// take their roots from SSL_CERT_FILE; skopeo 1.13 ignores CONTAINERS_REGISTRIES_CONF, so insecure = true in a
// registries.conf never reaches it: cosmic-ivy-19).
func (f *Forge) Trust(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", f.CA)
}

// Login logs the user in to the registry, as podman login would: REGISTRY_AUTH_FILE, which podman and skopeo read.
// Logout points it at a file with no login.
func (f *Forge) Login(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte(User + ":" + Password))
	file := filepath.Join(t.TempDir(), "auth.json")
	write(t, file, `{"auths":{"`+f.Host+`":{"auth":"`+auth+`"}}}`)
	t.Setenv("REGISTRY_AUTH_FILE", file)
}

// Logout: see Login.
func (f *Forge) Logout(t *testing.T) {
	file := filepath.Join(t.TempDir(), "none.json")
	write(t, file, `{"auths":{}}`)
	t.Setenv("REGISTRY_AUTH_FILE", file)
}

// Commit writes .plinth/Containerfile in the checkout, commits it and pushes it to main on the server; it returns the
// new head.
func (f *Forge) Commit(t *testing.T, containerfile, msg string) string {
	t.Helper()
	write(t, filepath.Join(f.Src, ".plinth", "Containerfile"), containerfile)
	git(t, f.Src, "add", ".plinth")
	git(t, f.Src, "commit", "-q", "-m", msg)
	if _, err := os.Stat(f.Bare); err != nil {
		git(t, f.Src, "clone", "-q", "--bare", f.Src, f.Bare)
	} else {
		git(t, f.Src, "push", "-q", f.Bare, "main")
	}
	return git(t, f.Bare, "rev-parse", "main")
}

// RemoveImages removes this machine's images of the namespace (by their plinth.image label): a test's cleanup.
func RemoveImages(ns names.Namespace) {
	out, _ := exec.Command("podman", "images", "--quiet", "--filter", "label=plinth.image="+ns.String()).Output()
	for _, id := range strings.Fields(string(out)) {
		exec.Command("podman", "rmi", "--force", id).Run()
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.org"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
