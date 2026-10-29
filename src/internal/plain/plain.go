// Package plain holds the commands that need no backend: they work in a repo's AAA work directory, <repo>/ai/work,
// with no controller and no Forgejo. All file access in it goes through os.Root and package safefs, so no path leads
// outside it and no symlink is followed. Spec: plinthctl.README.md, "No backend (plain AAA)".
package plain

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"plinth/internal/doc"
	"plinth/internal/names"
	"plinth/internal/safefs"
	"plinth/internal/workdir"
)

// Names inside the work directory.
const (
	lockFile    = ".new-ask.lock" // new-ask's flock
	failedDir   = "failed"        // runs new-ask could not finish, kept for analysis
	maxTemplate = 1 << 20         // the largest template new-ask reads
)

// lockWait is how long new-ask waits for another new-ask's lock before it gives up; a variable for the tests.
var lockWait = 10 * time.Second

// WorkDir is a repo's AAA work directory, <repo>/ai/work.
func WorkDir(repo string) string { return filepath.Join(repo, "ai", "work") }

// OpenWorkDir opens repo's AAA work directory as a root of its own, for a caller that works in a repo it doesn't
// trust: the Plinth MCP server, in an agent's repo. Neither ai nor ai/work may be a symlink, and nothing below
// repo leads out of it (os.Root). With create it makes the ones that are missing (mode 0700); without, a missing one
// is fs.ErrNotExist.
func OpenWorkDir(repo string, create bool) (*os.Root, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	work := filepath.Join("ai", "work")
	for _, d := range []string{"ai", work} {
		if create {
			if _, err := safefs.EnsureDir(root, d); err != nil {
				return nil, err
			}
		} else if fi, err := safefs.Lstat(root, d); err != nil {
			return nil, err
		} else if !fi.IsDir() {
			return nil, fmt.Errorf("%s: not a directory", filepath.Join(repo, d))
		}
	}
	return root.OpenRoot(work)
}

// TakenStems lists every <adjective>-<noun> in use below dir: entries up to three levels down (dir/<RUN>/,
// dir/done/<RUN>/, dir/failed/<RUN>/, dir/<agent>/done/<RUN>/, ...). A stem is taken whatever its issue number and
// whichever agent uses it.
func TakenStems(dir string) (map[string]bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return takenStems(root.FS())
}

// takenStems counts every entry, so a file or symlink with a run's name never makes Mkdir fail later, but descends only
// into real directories: a symlink is never followed.
func takenStems(fsys fs.FS) (map[string]bool, error) {
	taken := map[string]bool{}
	level := []string{"."}
	for depth := 1; depth <= 3; depth++ {
		var next []string
		for _, d := range level {
			entries, err := fs.ReadDir(fsys, d)
			if err != nil {
				if d == "." {
					return nil, err
				}
				continue
			}
			for _, e := range entries {
				if s := names.Stem(e.Name()); s != "" {
					taken[s] = true
				}
				if e.IsDir() {
					next = append(next, path.Join(d, e.Name()))
				}
			}
		}
		level = next
	}
	return taken, nil
}

// RunName draws a RUN_NAME that is free in dir, with the issue number appended when issue is not "".
// It only draws: NewAsk is what reserves a name.
func RunName(dir, issue string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	return runName(root, issue)
}

func runName(root *os.Root, issue string) (string, error) {
	if issue != "" && !names.ValidIssue(issue) {
		return "", fmt.Errorf("issue %q is not a number", issue)
	}
	taken, err := takenStems(root.FS())
	if err != nil {
		return "", err
	}
	stem, err := names.Draw(taken, rand.IntN)
	if err != nil {
		return "", fmt.Errorf("%v - scrub %s", err, filepath.Join(root.Name(), "done"))
	}
	if issue != "" {
		return stem + "-" + issue, nil
	}
	return stem, nil
}

var runNameLine = regexp.MustCompile(`(?m)^RUN_NAME="[^"]*"`)

// writeAsk writes a new run's ask.sh; a variable so that tests can make it fail.
var writeAsk = safefs.WriteNew

// NewAsk creates dir/<RUN>/ask.sh (mode 0700) from a template's script with RUN_NAME filled in and returns its path.
// The template is the file the user names (an AskTemplate manifest of mode aaa, or a script as it is), else
// work/ask-template.yml when work is set and it exists (read only through safefs), else the embedded base
// (workdir.AAATemplate). NewAsk holds an exclusive flock on dir/.new-ask.lock while it draws the name and creates the
// run, so two calls never take the same pair; it gives up after lockWait. It never overwrites: a run directory that
// exists is an error. If ask.sh can't be written, the run directory moves to dir/failed/<RUN>/, kept for analysis; its
// name stays taken.
func NewAsk(dir, work, template, issue string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	run, err := NewAskIn(root, work, template, issue)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, run, "ask.sh"), nil
}

// NewAskIn is NewAsk in the work directory root, opened by the caller (OpenWorkDir); it returns the RUN_NAME.
func NewAskIn(root *os.Root, work, template, issue string) (string, error) {
	tpl, from, err := askScript(work, template)
	if err != nil {
		return "", err
	}
	if !runNameLine.Match(tpl) {
		return "", fmt.Errorf("template %s has no RUN_NAME=\"...\" line to fill in", from)
	}

	unlock, err := lock(root)
	if err != nil {
		return "", err
	}
	defer unlock()

	run, err := runName(root, issue)
	if err != nil {
		return "", err
	}
	if err := root.Mkdir(run, 0o700); err != nil {
		return "", err
	}
	body := runNameLine.ReplaceAllLiteral(tpl, []byte(`RUN_NAME="`+run+`"`))
	if err := writeAsk(root, run, "ask.sh", body, 0o700); err != nil {
		if merr := moveToFailed(root, run); merr != nil {
			return "", fmt.Errorf("%s: %w; moving it to %s/ failed too: %v", run, err, failedDir, merr)
		}
		return "", fmt.Errorf("%s: %w; moved to %s", run, err, filepath.Join(root.Name(), failedDir, run))
	}
	return run, nil
}

// askScript is the script new-ask starts from (see NewAsk) and where it came from.
func askScript(work, template string) ([]byte, string, error) {
	if template != "" {
		b, err := readTemplate(template)
		if err != nil {
			return nil, "", fmt.Errorf("template: %w", err)
		}
		s, err := scriptOf(b)
		if err != nil {
			return nil, "", fmt.Errorf("template %s: %w", template, err)
		}
		return s, template, nil
	}
	if work != "" {
		name := filepath.Join(work, workdir.BaseTemplateName)
		root, err := os.OpenRoot(work)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("template: %w", err)
		}
		if err == nil {
			defer root.Close()
			b, err := safefs.ReadFile(root, workdir.BaseTemplateName, maxTemplate)
			if err == nil {
				s, err := scriptOf(b)
				if err != nil {
					return nil, "", fmt.Errorf("template %s: %w", name, err)
				}
				return s, name, nil
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, "", fmt.Errorf("template %s: %w", name, err)
			}
		}
	}
	return workdir.AAATemplate, "the embedded template", nil
}

// scriptOf is the script of a template: an AskTemplate manifest's, of mode aaa, or a script (#!) as it is.
func scriptOf(b []byte) ([]byte, error) {
	if bytes.HasPrefix(b, []byte("#!")) {
		return b, nil
	}
	t, err := doc.ParseAskTemplate(b)
	if err != nil {
		return nil, err
	}
	if t.Spec.Mode != doc.ModeAAA {
		return nil, fmt.Errorf("mode %s: new-ask starts plain AAA runs, mode %s", t.Spec.Mode, doc.ModeAAA)
	}
	return []byte(t.Spec.Script), nil
}

// readTemplate reads the template the user named, as given, up to maxTemplate.
func readTemplate(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTemplate+1))
	if err == nil && len(b) > maxTemplate {
		err = fmt.Errorf("%s: larger than %d bytes", name, maxTemplate)
	}
	return b, err
}

// lock takes an exclusive flock on root's lock file and returns the function that releases it. It creates the file if
// it is missing, opens an existing one only through safefs (no symlink, no hard link), and fails after lockWait with
// a line naming the lock file, so a hung or stopped new-ask never makes the next one hang silently.
func lock(root *os.Root) (func(), error) {
	name := filepath.Join(root.Name(), lockFile)
	f, err := root.OpenFile(lockFile, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		f, err = safefs.OpenFile(root, lockFile, os.O_RDWR)
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", name, err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil // closing the file releases the lock
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			break
		}
		if time.Now().After(deadline) {
			err = fmt.Errorf("another new-ask has held it for %v; if none is running, check the process holding it", lockWait)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.Close()
	return nil, fmt.Errorf("lock %s: %w", name, err)
}

// moveToFailed moves the run directory run to failed/<run>, durably and without following a symlink.
func moveToFailed(root *os.Root, run string) error {
	if err := root.Mkdir(failedDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return safefs.Rename(root, run, filepath.Join(failedDir, run))
}
