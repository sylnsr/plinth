// Package gitrepo finds a repo's top level and the namespace its origin remote names, and reads a tree's files, by
// running git with fixed arguments (no shell, a timeout, no terminal prompts), locked down: the repo's .git/ may be an
// agent's to write, and git takes orders from it. Spec: plinthctl.README.md, "register" and "The image".
package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"plinth/internal/names"
)

// timeout bounds every git call; they only read local config.
const timeout = 10 * time.Second

// maxList bounds the list of a tree's files that Files reads.
const maxList = 1 << 20

// lockdownEnv and lockdownArgs go with every git call: no setting from outside the repo (system,
// global, inherited GIT_* variables), no replace objects, no lazy fetch of a partial clone and no transport at all
// (a fetch would run the ssh command .git/config names), no fsmonitor and no hooks, and no prompt for a password:
// GIT_TERMINAL_PROMPT=0 stops only the terminal's, so an empty GIT_ASKPASS stops the desktop's SSH_ASKPASS dialog
// (and a core.askPass), and credential.helper= any helper.
var (
	lockdownEnv = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_ASKPASS="}
	lockdownArgs = []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never",
		"-c", "credential.helper="}
)

// command is git -C dir args..., locked down.
func command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{"-C", dir}, lockdownArgs...), args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "SSH_ASKPASS") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, lockdownEnv...)
	return cmd
}

// git runs git -C dir args... and returns its trimmed stdout, or an error with git's first stderr line.
func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := command(ctx, dir, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), firstLine(errb.String(), err))
	}
	return strings.TrimSpace(out.String()), nil
}

// firstLine is the first line of git's stderr, or err when it said nothing.
func firstLine(stderr string, err error) string {
	msg, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	if msg == "" {
		msg = err.Error()
	}
	return msg
}

var objectRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Tree returns the commit ref names in the repo at top, and the tree of path in that commit: what a namespace's image
// is built from, as committed, never the working tree. An error when ref isn't a commit or path isn't a
// directory in it.
func Tree(top, ref, path string) (commit, tree string, err error) {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, ":^~ \n") {
		return "", "", fmt.Errorf("ref %q: not a branch, tag or commit name", ref)
	}
	if commit, err = git(top, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil || !objectRe.MatchString(commit) {
		return "", "", fmt.Errorf("%s: %s is not a commit", top, ref)
	}
	// <commit>:<path> takes all after the colon as the path, so the type is checked on its own.
	tree, err = git(top, "rev-parse", "--verify", "--quiet", commit+":"+path)
	if err == nil && objectRe.MatchString(tree) {
		if kind, err := git(top, "cat-file", "-t", tree); err == nil && kind == "tree" {
			return commit, tree, nil
		}
	}
	return "", "", fmt.Errorf("%s has no %s directory at %s", top, path, ref)
}

// File is a regular file of a tree, by its path inside the tree.
type File struct {
	Path string
	Exec bool // mode 100755
	Data []byte
}

// Files returns the regular files of tree (an id from Tree) in the repo at top, at most max bytes in all. It lists them
// with ls-tree and reads them with cat-file --batch, which apply no filter: git archive runs the filter commands .git/
// names. A symlink, a submodule or a path that leaves the tree is refused. git doesn't check that
// an object's bytes match its id, so these are the repo's files, not proof of a reviewed tree.
func Files(top, tree string, max int) ([]File, error) {
	if !objectRe.MatchString(tree) {
		return nil, fmt.Errorf("tree %q: not an object id", tree)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	list := &limited{max: maxList}
	var errb bytes.Buffer
	cmd := command(ctx, top, "ls-tree", "-r", "-z", tree)
	cmd.Stdout, cmd.Stderr = list, &errb
	if err := cmd.Run(); err != nil {
		if list.over {
			return nil, fmt.Errorf("git ls-tree %s: over %d bytes", tree, maxList)
		}
		return nil, fmt.Errorf("git ls-tree %s: %s", tree, firstLine(errb.String(), err))
	}
	var files []File
	var ids []string
	for _, entry := range strings.Split(strings.TrimSuffix(list.b.String(), "\x00"), "\x00") {
		if entry == "" {
			continue // an empty tree
		}
		meta, path, _ := strings.Cut(entry, "\t")
		f := strings.Fields(meta)
		if !validPath(path) {
			return nil, fmt.Errorf("%q: not a path inside the tree", path)
		}
		if len(f) != 3 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") || !objectRe.MatchString(f[2]) {
			return nil, fmt.Errorf("%q: not a regular file (%s)", path, meta)
		}
		files = append(files, File{Path: path, Exec: f[0] == "100755"})
		ids = append(ids, f[2])
	}
	if len(files) == 0 {
		return nil, nil
	}
	errb.Reset()
	cmd = command(ctx, top, "cat-file", "--batch")
	cmd.Stdin, cmd.Stderr = strings.NewReader(strings.Join(ids, "\n")+"\n"), &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := readBlobs(bufio.NewReader(stdout), files, ids, max); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("git cat-file %s: %v", tree, err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git cat-file %s: %s", tree, firstLine(errb.String(), err))
	}
	return files, nil
}

// readBlobs reads cat-file --batch's answer for ids, one per file, into the files: each a header "<id> blob <size>",
// the bytes and a newline. More than max bytes in all is an error.
func readBlobs(r *bufio.Reader, files []File, ids []string, max int) error {
	total := 0
	for i := range files {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("%q: %v", files[i].Path, err)
		}
		f := strings.Fields(hdr)
		if len(f) != 3 || f[0] != ids[i] || f[1] != "blob" {
			return fmt.Errorf("%q: %q", files[i].Path, strings.TrimSpace(hdr))
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size < 0 {
			return fmt.Errorf("%q: size %q", files[i].Path, f[2])
		}
		if total += size; total > max {
			return fmt.Errorf("over %d bytes", max)
		}
		b := make([]byte, size+1)
		if _, err := io.ReadFull(r, b); err != nil || b[size] != '\n' {
			return fmt.Errorf("%q: short read", files[i].Path)
		}
		files[i].Data = b[:size]
	}
	return nil
}

// validPath is a relative path with no empty, . or .. part.
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// limited takes at most max bytes; one more fails the write, which ends the copy.
type limited struct {
	b    bytes.Buffer
	max  int
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	if l.b.Len()+len(p) > l.max {
		l.over = true
		return 0, fmt.Errorf("over %d bytes", l.max)
	}
	return l.b.Write(p)
}

// TopLevel returns the absolute top-level directory of the git repo that contains dir.
func TopLevel(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	top, err := git(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repo (%v)", abs, err)
	}
	if !filepath.IsAbs(top) {
		return "", fmt.Errorf("git gave a relative top level %q for %s", top, abs)
	}
	return top, nil
}

// Origin returns the URL of the origin remote of the repo at top.
func Origin(top string) (string, error) {
	u, err := git(top, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("no origin remote in %s (%v)", top, err)
	}
	return u, nil
}

// OriginNamespace returns the namespace named by the origin remote of the repo at top.
func OriginNamespace(top string) (names.Namespace, error) {
	u, err := Origin(top)
	if err != nil {
		return names.Namespace{}, err
	}
	return NamespaceFromURL(u)
}

// RemoteHost returns the host name of a remote URL, without user or port, lower-cased: ssh://git@host:2222/o/r.git and
// git@host:o/r.git both give host. A local path has no host and gives "".
func RemoteHost(remote string) string {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	if strings.HasPrefix(remote, "/") {
		return ""
	}
	host, _, ok := strings.Cut(remote, ":")
	if !ok {
		return ""
	}
	if _, h, ok := strings.Cut(host, "@"); ok {
		host = h
	}
	return strings.ToLower(host)
}

// NamespaceFromURL takes <owner>/<repo> from the last two path segments of a remote URL, without a trailing .git:
// ssh://git@host:2222/owner/repo.git, git@host:owner/repo.git, https://host/owner/repo and local paths. Its errors never
// quote the URL, which may hold user:token.
func NamespaceFromURL(remote string) (names.Namespace, error) {
	p := remote
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return names.Namespace{}, fmt.Errorf("the remote URL doesn't parse") // url.Error would quote the URL, user:token included
		}
		p = u.Path
	} else if _, after, ok := strings.Cut(remote, ":"); ok && !strings.HasPrefix(remote, "/") {
		p = after // scp style: [user@]host:path
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 2 {
		return names.Namespace{}, fmt.Errorf("the remote URL has no <owner>/<repo> path")
	}
	ns := parts[len(parts)-2] + "/" + strings.TrimSuffix(parts[len(parts)-1], ".git")
	n, err := names.ParseNamespace(ns)
	if err != nil {
		return names.Namespace{}, fmt.Errorf("the remote URL names %v", err) // quotes only <owner>/<repo>, never the URL
	}
	return n, nil
}
