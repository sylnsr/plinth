// Package forgejo reads what Plinth needs from Forgejo's API with Plinth's token (repository read): a repo's default
// branch, that branch's head and protection, and the hash of a folder at a commit. It writes only issues,
// comments and their attachments (issue read and write), for Save and Report. Spec: plinthctl.README.md, "The image",
// Save and Report.
package forgejo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"plinth/internal/gitrepo"
	"plinth/internal/names"
)

// maxBody bounds an API answer.
const maxBody = 1 << 20

// Client is Forgejo's API at URL (forgejo-url), with Token (forgejo-token; "" for none).
type Client struct {
	URL    string
	Token  string
	CAFile string // the CA bundle New was given: git fetches from the same server
	http   *http.Client
}

// New returns a client for forgejo-url. caFile is a CA bundle for the server's certificate, "" for the system's (tests
// set one). It follows no redirect: one would take the token's header elsewhere.
func New(forgejoURL, token, caFile string) (*Client, error) {
	u, err := url.Parse(forgejoURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, errors.New("forgejo-url is not an http(s) URL with a host and no user") // never quote it
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificate", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{URL: strings.TrimSuffix(forgejoURL, "/"), Token: token, CAFile: caFile, http: &http.Client{
		Transport:     tr,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Close closes the client's idle connections.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// RepoURL is the namespace's repo for git: <forgejo-url>/<owner>/<repo>.git.
func (c *Client) RepoURL(ns names.Namespace) string {
	return c.URL + "/" + ns.Owner + "/" + ns.Repo + ".git"
}

// get reads the API path (below /api/v1) into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, out)
}

// do sends a request with body (of contentType; nil for none) to the API path (below /api/v1) and reads the answer
// into out: 200, or 201 for a POST.
func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error quotes the URL
		}
		return fmt.Errorf("Forgejo API %s: %v", path, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("Forgejo API %s: %v", path, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && method == http.MethodGet:
		return fmt.Errorf("Forgejo API %s: not found, or Plinth's token can't read it", path)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("Forgejo API %s: %s: not found, or Plinth's token can't write there", path, resp.Status)
	case resp.StatusCode != http.StatusOK && !(method == http.MethodPost && resp.StatusCode == http.StatusCreated):
		return fmt.Errorf("Forgejo API %s: %s", path, resp.Status)
	case len(answer) > maxBody:
		return fmt.Errorf("Forgejo API %s: over %d bytes", path, maxBody)
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("Forgejo API %s: %v", path, err)
	}
	return nil
}

// DefaultBranch is the repo's default branch, as Forgejo names it.
func (c *Client) DefaultBranch(ctx context.Context, ns names.Namespace) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, "/repos/"+ns.Owner+"/"+ns.Repo, &r); err != nil {
		return "", err
	}
	if !gitrepo.ValidBranch(r.DefaultBranch) {
		return "", fmt.Errorf("%s: the default branch %q is not a branch name", ns, r.DefaultBranch)
	}
	return r.DefaultBranch, nil
}

// Branch is a branch's head and the part of its protection Plinth's read token sees: the whitelists and file patterns
// need admin access to read.
type Branch struct {
	Name              string
	Commit            string
	Protected         bool
	RequiredApprovals int
}

// Branch reads branch of ns.
func (c *Client) Branch(ctx context.Context, ns names.Namespace, branch string) (Branch, error) {
	if !gitrepo.ValidBranch(branch) {
		return Branch{}, fmt.Errorf("branch %q: not a branch name", branch)
	}
	var r struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
		Protected         bool `json:"protected"`
		RequiredApprovals int  `json:"required_approvals"`
	}
	if err := c.get(ctx, "/repos/"+ns.Owner+"/"+ns.Repo+"/branches/"+branch, &r); err != nil {
		return Branch{}, err
	}
	return Branch{Name: r.Name, Commit: r.Commit.ID, Protected: r.Protected, RequiredApprovals: r.RequiredApprovals}, nil
}

// Tree is the git hash of the tree path, an entry at the top of commit's tree in ns ("" when commit has no such
// tree).
func (c *Client) Tree(ctx context.Context, ns names.Namespace, commit, path string) (string, error) {
	if !objectRe.MatchString(commit) {
		return "", fmt.Errorf("commit %q: not a git object id", commit)
	}
	var r struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := c.get(ctx, "/repos/"+ns.Owner+"/"+ns.Repo+"/git/trees/"+commit, &r); err != nil {
		return "", err
	}
	for _, e := range r.Tree {
		if e.Path == path {
			if e.Type != "tree" || !objectRe.MatchString(e.SHA) {
				return "", nil
			}
			return e.SHA, nil
		}
	}
	if r.Truncated {
		return "", fmt.Errorf("%s at %s: Forgejo's list of the top tree is cut short before %s", ns, commit, path)
	}
	return "", nil
}

// objectRe is a git object id: SHA-1 or SHA-256.
var objectRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
