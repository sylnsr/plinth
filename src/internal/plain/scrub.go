package plain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"plinth/internal/names"
	"plinth/internal/safefs"
)

// Names of the scripts the scrubbers write into the work directory.
const (
	ScrubScript   = "scrub.sh"
	HandoffScript = "handoff-scrubber.sh"
	doneDir       = "done"
)

// ScrubEntry is one entry of done/ and what scrub.sh does with it.
type ScrubEntry struct {
	Name string
	What string // "scrub", "keep" or "skip"
	Why  string // for keep and skip
}

// Scrub writes dir/scrub.sh (mode 0700): per finished run in dir/done/ (a real directory with a run's name), an rm of
// its run directory and one of its untouched original in rawDir/<RUN>/ (a full scrub, plinthctl.README.md, No backend).
// Without all, a run whose name ends in -<digits> stays: it belongs to an issue. Anything else in done/ (a file, a
// symlink) is skipped. It deletes nothing itself, and returns every entry of done/ with what
// scrub.sh does with it. now dates the script.
func Scrub(dir, rawDir string, all bool, now time.Time) ([]ScrubEntry, error) {
	if !filepath.IsAbs(dir) || !filepath.IsAbs(rawDir) {
		return nil, fmt.Errorf("scrub: %q and %q must be absolute paths", dir, rawDir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var entries []ScrubEntry
	fi, err := safefs.Lstat(root, doneDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case !fi.IsDir():
		return nil, fmt.Errorf("%s: not a directory", filepath.Join(dir, doneDir))
	default:
		d, err := root.Open(doneDir)
		if err != nil {
			return nil, err
		}
		des, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return nil, err
		}
		for _, e := range des {
			n := e.Name()
			switch {
			case !e.IsDir() || !names.ValidRun(n):
				entries = append(entries, ScrubEntry{n, "skip", "not a run directory"})
			case !all && names.Stem(n) != n:
				entries = append(entries, ScrubEntry{n, "keep", "belongs to issue " + n[strings.LastIndexByte(n, '-')+1:]})
			default:
				entries = append(entries, ScrubEntry{Name: n, What: "scrub"})
			}
		}
	}
	var b strings.Builder
	n := 0
	for _, e := range entries {
		if e.What == "scrub" {
			n++
		}
	}
	scope := "without an issue number"
	if all {
		scope = "(--all: with an issue number or not)"
	}
	fmt.Fprintf(&b, "#!/bin/bash\n# Written by plinthctl scrubber on %s. Read it before you run it: it deletes, for good.\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "# The finished runs in %s %s: %d, each with its untouched original in %s.\n", filepath.Join(dir, doneDir), scope, n, rawDir)
	b.WriteString("set -u\n")
	fmt.Fprintf(&b, "cd -- %s || exit 1\n", shellQuote(filepath.Join(dir, doneDir)))
	for _, e := range entries {
		if e.What == "scrub" {
			fmt.Fprintf(&b, "rm -rf -- %s %s\n", e.Name, shellQuote(filepath.Join(rawDir, e.Name)))
		}
	}
	b.WriteString("echo \"scrub done\"\n")
	if err := safefs.Replace(root, ".", ScrubScript, []byte(b.String()), 0o700); err != nil {
		return nil, err
	}
	return entries, nil
}

// handoffRe is a handoff branch the script may name: handoff/ and git ref name parts of safe characters.
var handoffRe = regexp.MustCompile(`^handoff(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$`)

// HandoffScrub writes dir/handoff-scrubber.sh (mode 0700), which deletes the handoff branch branch in the repo dir
// belongs to, locally and on its origin, then itself; a step that fails keeps the script, to run again (each step
// skips a branch that is gone already). branch must be handoff/<...> with plain ref-name parts. It deletes nothing
// itself. repo is the repo directory, absolute.
func HandoffScrub(dir, repo, branch string, now time.Time) error {
	if !handoffRe.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") ||
		strings.HasSuffix(branch, ".") {
		return fmt.Errorf("branch %q: not handoff/<name> (letters, digits, . _ - and / only)", branch)
	}
	if !filepath.IsAbs(repo) {
		return fmt.Errorf("handoff-scrubber: %q must be an absolute path", repo)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	q := shellQuote(branch)
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/bash\n# Written by plinthctl handoff-scrubber on %s. Read it before you run it.\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "# Deletes the branch %s here and on origin, then this script; a step that fails keeps it, to run again.\n", branch)
	b.WriteString("set -u\n")
	fmt.Fprintf(&b, "cd -- %s || exit 1\n", shellQuote(repo))
	b.WriteString("ok=1\n")
	fmt.Fprintf(&b, "if git show-ref --verify --quiet refs/heads/%s; then git branch -D %s || ok=0; fi\n", branch, q)
	b.WriteString("git ls-remote --exit-code --heads origin " + q + " >/dev/null\n")
	b.WriteString("case $? in\n")
	fmt.Fprintf(&b, "  0) git push origin --delete %s || ok=0 ;;\n", q)
	b.WriteString("  2) ;; # not on origin (any more)\n")
	b.WriteString("  *) echo \"origin can't be reached: the branch on origin stays\" >&2; ok=0 ;;\n")
	b.WriteString("esac\n")
	b.WriteString("[[ $ok == 1 ]] || { echo \"handoff-scrubber.sh: a step failed; it stays, to run again\" >&2; exit 1; }\n")
	b.WriteString("rm -f -- \"${BASH_SOURCE[0]}\"\n")
	fmt.Fprintf(&b, "echo \"deleted %s\"\n", branch)
	return safefs.Replace(root, ".", HandoffScript, []byte(b.String()), 0o700)
}

// shellQuote quotes s for bash: in single quotes, where each single quote of s ends the quoting, comes escaped, and
// starts it again.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
