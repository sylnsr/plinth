package gitrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// fetchTimeout bounds a fetch from Forgejo: the whole branch's history, over the network.
var fetchTimeout = 5 * time.Minute

// Remote is where Fetch takes a branch from.
type Remote struct {
	URL    string // https://<host>/<owner>/<repo>.git
	Token  string // sent only as the Authorization header to URL, never in argv or a config file; "" for none
	CAFile string // a CA bundle for the server's certificate; "" for the system's (tests set one)
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,199}$`)

// Fetch makes a bare repo at dir, which must not exist, fetches branch from r into it as refs/heads/<branch>, and
// returns the branch's head commit (a fresh fetch from Forgejo, never the agent's checkout). The
// repo is Plinth's own: no template (so no hooks), and every git call is locked down as all of gitrepo's are, with
// https the one protocol allowed, no redirect followed (one would take the token's header to another URL), and the
// token in the environment, scoped to r.URL. A failed fetch leaves dir for the caller to remove.
func Fetch(dir string, r Remote, branch string) (string, error) {
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("the remote is not an https URL without user, query or fragment") // never quote it
	}
	if !ValidBranch(branch) {
		return "", fmt.Errorf("branch %q: not a branch name", branch)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s: not an absolute path", dir)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s exists already", dir)
	}
	if _, err := git(filepath.Dir(dir), "init", "--bare", "--quiet", "--template=", dir); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	ref := "refs/heads/" + branch
	cmd := command(ctx, dir, "-c", "protocol.https.allow=always", "-c", "http.followRedirects=false", "-c", "gc.auto=0",
		"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", r.URL, "+"+ref+":"+ref)
	var cfg [][2]string
	if r.Token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(r.Token + ":x-oauth-basic"))
		cfg = append(cfg, [2]string{"http." + r.URL + ".extraHeader", "Authorization: Basic " + basic})
	}
	if r.CAFile != "" {
		cfg = append(cfg, [2]string{"http.sslCAInfo", r.CAFile})
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		cmd.Env = append(cmd.Env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	var msg bytes.Buffer
	cmd.Stderr = &msg
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git fetch %s: %s", branch, scrubURL(firstLine(msg.String(), err), r.URL))
	}
	commit, err := git(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || !objectRe.MatchString(commit) {
		return "", fmt.Errorf("git fetch %s: no commit at %s", branch, ref)
	}
	return commit, nil
}

// scrubURL takes every URL out of git's message (the remote's, or just its host, as in "could not read Username for
// 'https://host'"): Plinth's messages never quote a remote URL.
func scrubURL(msg, remote string) string {
	return urlRe.ReplaceAllString(strings.ReplaceAll(msg, remote, "<remote>"), "<remote>")
}

var urlRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"]+`)

// OnBranch reports whether commit is the head of branch, or one of its ancestors, in the repo at dir.
func OnBranch(dir, branch, commit string) bool {
	if !objectRe.MatchString(commit) || !branchRe.MatchString(branch) {
		return false
	}
	_, err := git(dir, "merge-base", "--is-ancestor", commit, "refs/heads/"+branch)
	return err == nil
}
