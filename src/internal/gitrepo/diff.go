package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// ValidBranch reports whether name is a branch name Plinth takes: what Fetch and the Forgejo API are given.
func ValidBranch(name string) bool {
	return branchRe.MatchString(name) && !strings.Contains(name, "..") && !strings.HasSuffix(name, ".lock") &&
		!strings.HasSuffix(name, "/") && !strings.Contains(name, "//")
}

// Diff returns git diff from..to of path in the repo at dir (Plinth's own bare repo, from Fetch), at most max bytes,
// and whether it was cut there: what image publish shows the user before they type yes. An empty from diffs
// against the empty tree, so every file shows in full. No external diff and no textconv, which would run commands the
// repo's config names; a binary file shows as "Binary files ... differ". The text is the repo's: a caller that prints
// it to a terminal takes out control characters.
func Diff(dir, from, to, path string, max int) (string, bool, error) {
	if (from != "" && !objectRe.MatchString(from)) || !objectRe.MatchString(to) {
		return "", false, fmt.Errorf("diff %q..%q: not commit ids", from, to)
	}
	if !validPath(path) {
		return "", false, fmt.Errorf("diff of %q: not a path in the tree", path)
	}
	if from == "" {
		empty, err := git(dir, "hash-object", "-t", "tree", "/dev/null")
		if err != nil || !objectRe.MatchString(empty) {
			return "", false, fmt.Errorf("the empty tree: %v", err)
		}
		from = empty
	} else if _, err := git(dir, "cat-file", "-e", from+"^{commit}"); err != nil {
		return "", false, fmt.Errorf("commit %s is not in the repo", from)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out := &limited{max: max}
	var errb bytes.Buffer
	cmd := command(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--no-color", from, to, "--", path)
	cmd.Stdout, cmd.Stderr = out, &errb
	if err := cmd.Run(); err != nil && !out.over {
		return "", false, fmt.Errorf("git diff: %s", firstLine(errb.String(), err))
	}
	return out.b.String(), out.over, nil
}
