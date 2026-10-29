package forgejo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"plinth/internal/names"
)

// TestClient: the token goes as its header; the default branch, a branch's head and protection, and a folder's tree at
// a commit are read; a 404 says what may be wrong; a redirect is not followed; no error quotes the token.
func TestClient(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/repos/michael/devops":
			w.Write([]byte(`{"default_branch":"main","name":"devops"}`))
		case "/api/v1/repos/michael/devops/branches/main":
			w.Write([]byte(`{"name":"main","commit":{"id":"` + strings.Repeat("a", 40) + `"},"protected":true,"required_approvals":1}`))
		case "/api/v1/repos/michael/devops/git/trees/" + strings.Repeat("a", 40):
			w.Write([]byte(`{"sha":"` + strings.Repeat("a", 40) + `","tree":[{"path":"README","type":"blob","sha":"` + strings.Repeat("b", 40) +
				`"},{"path":".plinth","type":"tree","sha":"` + strings.Repeat("c", 40) + `"},{"path":"x","type":"blob","sha":"` + strings.Repeat("d", 40) + `"}],"truncated":false}`))
		case "/api/v1/repos/michael/devops/git/trees/" + strings.Repeat("e", 40):
			w.Write([]byte(`{"tree":[{"path":"README","type":"blob","sha":"` + strings.Repeat("b", 40) + `"}],"truncated":true}`))
		case "/api/v1/repos/michael/bad":
			w.Write([]byte(`{"default_branch":"--upload-pack=x"}`))
		case "/api/v1/repos/michael/moved":
			http.Redirect(w, r, "https://elsewhere.example.org/x", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL+"/", "s3cret", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := names.Namespace{Owner: "michael", Repo: "devops"}
	if b, err := c.DefaultBranch(ctx, ns); err != nil || b != "main" {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}
	if b, err := c.Branch(ctx, ns, "main"); err != nil || b != (Branch{Name: "main", Commit: strings.Repeat("a", 40), Protected: true, RequiredApprovals: 1}) {
		t.Errorf("Branch = %+v, %v", b, err)
	}
	for _, c2 := range []struct{ commit, path, want, err string }{
		{strings.Repeat("a", 40), ".plinth", strings.Repeat("c", 40), ""},
		{strings.Repeat("a", 40), "x", "", ""}, // a file, not a folder
		{strings.Repeat("a", 40), "none", "", ""},
		{strings.Repeat("e", 40), ".plinth", "", "cut short"},
		{"-x", ".plinth", "", "not a git object id"},
	} {
		if got, err := c.Tree(ctx, ns, c2.commit, c2.path); got != c2.want || (c2.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c2.err)) {
			t.Errorf("Tree(%s, %s) = %q, %v; want %q, %q", c2.commit, c2.path, got, err, c2.want, c2.err)
		}
	}
	if auth[0] != "token s3cret" {
		t.Errorf("Authorization = %q", auth[0])
	}
	if c.RepoURL(ns) != srv.URL+"/michael/devops.git" {
		t.Errorf("RepoURL = %q", c.RepoURL(ns))
	}
	for _, r := range []struct{ repo, want string }{
		{"gone", "not found, or Plinth's token can't read it"},
		{"bad", "is not a branch name"},
		{"moved", "302"},
	} {
		_, err := c.DefaultBranch(ctx, names.Namespace{Owner: "michael", Repo: r.repo})
		if err == nil || !strings.Contains(err.Error(), r.want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: %v, want %q", r.repo, err, r.want)
		}
	}
	if _, err := c.Branch(ctx, ns, "-x"); err == nil {
		t.Error("a branch starting with - was asked for")
	}
	if _, err := New("https://user:pw@example.org", "", ""); err == nil || strings.Contains(err.Error(), "pw") {
		t.Errorf("a URL with a password: %v", err)
	}
}

// TestPost: an issue, a comment and their attachments are posted as Forgejo's API wants them, with the token; a 403
// says the token can't write there; an issue that isn't a number is never asked for.
func TestPost(t *testing.T) {
	type seen struct{ method, path, query, auth, ctype, body string }
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), ""}
		if strings.HasPrefix(s.ctype, "multipart/form-data") {
			f, h, err := r.FormFile("attachment")
			if err != nil {
				t.Errorf("%s: %v", r.URL.Path, err)
				return
			}
			b, _ := io.ReadAll(f)
			s.body = h.Filename + ":" + string(b)
		} else {
			b, _ := io.ReadAll(r.Body)
			s.body = string(b)
		}
		got = append(got, s)
		switch r.URL.Path {
		case "/api/v1/repos/michael/failed-scans/issues":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":900,"number":7,"html_url":"https://f.example.org/michael/failed-scans/issues/7"}`))
		case "/api/v1/repos/michael/devops/issues/12/comments":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":555,"html_url":"https://f.example.org/michael/devops/issues/12#issuecomment-555"}`))
		case "/api/v1/repos/michael/failed-scans/issues/7/assets", "/api/v1/repos/michael/devops/issues/comments/555/assets":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":1,"name":"x"}`))
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, "s3cret", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	failed, devops := names.Namespace{Owner: "michael", Repo: "failed-scans"}, names.Namespace{Owner: "michael", Repo: "devops"}
	issue, err := c.CreateIssue(ctx, failed, "Failed DLP scan: michael/devops calm-otter", "body")
	if err != nil || issue != (Posted{ID: 900, Number: 7, URL: "https://f.example.org/michael/failed-scans/issues/7"}) {
		t.Errorf("CreateIssue = %+v, %v", issue, err)
	}
	if err := c.AttachToIssue(ctx, failed, issue.Number, "ask.sh.txt", []byte("#!/bin/bash\n")); err != nil {
		t.Errorf("AttachToIssue: %v", err)
	}
	comment, err := c.Comment(ctx, devops, "12", "### Run artifacts")
	if err != nil || comment.ID != 555 {
		t.Errorf("Comment = %+v, %v", comment, err)
	}
	if err := c.AttachToComment(ctx, devops, comment.ID, "calm-otter-12.zip", []byte("PK")); err != nil {
		t.Errorf("AttachToComment: %v", err)
	}
	want := []seen{
		{"POST", "/api/v1/repos/michael/failed-scans/issues", "", "token s3cret", "application/json", `{"body":"body","title":"Failed DLP scan: michael/devops calm-otter"}`},
		{"POST", "/api/v1/repos/michael/failed-scans/issues/7/assets", "name=ask.sh.txt", "token s3cret", "", "ask.sh.txt:#!/bin/bash\n"},
		{"POST", "/api/v1/repos/michael/devops/issues/12/comments", "", "token s3cret", "application/json", `{"body":"### Run artifacts"}`},
		{"POST", "/api/v1/repos/michael/devops/issues/comments/555/assets", "name=calm-otter-12.zip", "token s3cret", "", "calm-otter-12.zip:PK"},
	}
	for i := range got {
		if strings.HasPrefix(got[i].ctype, "multipart/form-data") {
			got[i].ctype = ""
		}
	}
	if len(got) != len(want) {
		t.Fatalf("requests %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := c.CreateIssue(ctx, names.Namespace{Owner: "michael", Repo: "closed"}, "t", "b"); err == nil ||
		!strings.Contains(err.Error(), "can't write there") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("a repo the token can't write: %v", err)
	}
	n := len(got)
	if _, err := c.Comment(ctx, devops, "../12", "x"); err == nil || len(got) != n {
		t.Errorf("an issue that isn't a number: %v, %d requests", err, len(got)-n)
	}
}
