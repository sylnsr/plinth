package plain

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"

	"plinth/internal/names"
)

// StateNotRun and StateRunning are Run.State without an answer: only ask.sh, or the script still runs (running.yml).
const (
	StateNotRun  = "not-run"
	StateRunning = "running"
)

// answerRe is a plain AAA answer document's name in any state.
var answerRe = regexp.MustCompile(`^answer(\.FAIL)?\.(new|dlp-ok|DLP-FAIL|INVALID)\.yml$`)

// stateOrder is the order ReadAnswer checks a run's states in: the first one a run has is its state.
var stateOrder = []string{StateDLPFail, StateInvalid, StateRunning, StateNew, StateDLPOK}

// Run is a plain AAA run as its file names describe it, for shell completion and aaa_status.
type Run struct {
	Name  string
	Done  bool     // in done/
	State string   // not-run, running, new, dlp-ok, DLP-FAIL or INVALID, in read-answer's order
	Gate  bool     // its answer is answer.FAIL.*
	Files []string // ask.sh and the answer document, those it has as regular files
}

// Runs lists the runs in the AAA work directory dir and in dir/done/, by name, from their file names only: no file
// is read. A symlinked run directory is no run; a state is a name, a symlink included, as ReadAnswer takes it.
func Runs(dir string) ([]Run, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return RunsIn(root)
}

// RunsIn is Runs in the work directory root, opened by the caller (OpenWorkDir).
func RunsIn(root *os.Root) ([]Run, error) {
	var out []Run
	for _, d := range []string{".", doneDir} {
		entries, err := fs.ReadDir(root.FS(), d)
		if err != nil {
			if d == doneDir {
				break
			}
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || !names.ValidRun(e.Name()) {
				continue
			}
			r := Run{Name: e.Name(), Done: d == doneDir, State: StateNotRun}
			files, _ := fs.ReadDir(root.FS(), path.Join(d, e.Name()))
			has := map[string]bool{}
			for _, f := range files {
				n := f.Name()
				if m := answerRe.FindStringSubmatch(n); m != nil {
					has[m[2]], r.Gate = true, m[1] != ""
				} else if n == "running.yml" {
					has[StateRunning] = true
					continue // finish's, never a file to give a command
				} else if n != "ask.sh" {
					continue
				}
				if f.Type().IsRegular() {
					r.Files = append(r.Files, n)
				}
			}
			if i := slices.IndexFunc(stateOrder, func(s string) bool { return has[s] }); i >= 0 {
				r.State = stateOrder[i]
			}
			out = append(out, r)
		}
	}
	return out, nil
}
