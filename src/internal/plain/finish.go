package plain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"plinth/internal/dlp"
	"plinth/internal/doc"
	"plinth/internal/names"
	"plinth/internal/ollama"
	"plinth/internal/safefs"
)

// The states of a finished answer document, the part of its name before .yml: as the controller's.
const (
	StateNew     = "new"      // the script ended (the AAA library's EXIT trap); plinthctl finish not run yet
	StateDLPOK   = "dlp-ok"   // the DLP scan passed: the only state read-answer prints
	StateDLPFail = "DLP-FAIL" // the DLP scan ran and failed: the agent never reads it
	StateInvalid = "INVALID"  // not a valid document: a bug, never data
)

// Finished is what Finish did.
type Finished struct {
	State  string // StateDLPOK, StateDLPFail or StateInvalid
	File   string // the answer, in dir/done/<RUN>/
	Reason string // why INVALID or DLP-FAIL
	DLP    dlp.Result
	Raw    string // the untouched original, for dlp-ok and DLP-FAIL
}

// Finish is finish: the end of a plain AAA run once its script has ended, run by the user. The run is
// dir/<RUN>/ with answer[.FAIL].new.yml, which the AAA library's EXIT trap left. Finish decodes it strictly; one that
// doesn't decode or check (doc.ParseEnded: a result, the run's name, .FAIL matching result: fail) becomes
// answer[.FAIL].INVALID.yml. Otherwise the DLP scan masks the document's texts (dlp.ScanTexts) and Finish writes it
// again, with status.dlp, as answer[.FAIL].dlp-ok.yml, or as .DLP-FAIL.yml when the model's replies didn't fit; the
// untouched original goes to rawDir/<RUN>/<name>.orig (mode 0600) first. Either way the run directory then moves to
// dir/done/<RUN>/. A scan that couldn't ask the model (the gateway not running, Ollama unreachable, not configured)
// changes nothing and is an error: run Finish again once it is up.
func Finish(ctx context.Context, c ollama.Chatter, model, rawDir, dir, run string) (*Finished, error) {
	if !names.ValidRun(run) {
		return nil, fmt.Errorf("%q is not a RUN_NAME", run)
	}
	if !filepath.IsAbs(rawDir) {
		return nil, fmt.Errorf("plinth.aaa-raw-dir %q: not an absolute path", rawDir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	done := filepath.Join(doneDir, run)
	if fi, err := safefs.Lstat(root, run); err != nil || !fi.IsDir() {
		if present(root, done) {
			return nil, fmt.Errorf("%s: finished already (%s)", run, filepath.Join(dir, done))
		}
		return nil, fmt.Errorf("%s: no run directory %s", run, filepath.Join(dir, run))
	}
	if present(root, done) {
		return nil, fmt.Errorf("%s: %s exists already", run, filepath.Join(dir, done))
	}
	if present(root, filepath.Join(run, "running.yml")) {
		return nil, fmt.Errorf("%s: still running, or killed (running.yml exists): nothing to finish; a rerun is a new run", run)
	}
	var name, fail string
	for _, s := range []string{"", ".FAIL"} {
		if n := "answer" + s + "." + StateNew + ".yml"; present(root, filepath.Join(run, n)) {
			if name != "" {
				return nil, fmt.Errorf("%s: both answer.new.yml and answer.FAIL.new.yml exist", run)
			}
			name, fail = n, s
		}
	}
	if name == "" {
		return nil, fmt.Errorf("%s: nothing to finish (no answer[.FAIL].new.yml: not run yet, or not by the AAA library)", run)
	}
	file := filepath.Join(run, name)
	fi, err := safefs.Lstat(root, file)
	if err != nil {
		return nil, err
	}
	if err := safefs.Regular(root, file); err != nil {
		return nil, err
	}
	var a *doc.Answer
	var b []byte
	var bad error
	if fi.Size() > doc.MaxSize {
		bad = fmt.Errorf("%d bytes, more than the %d a document may have", fi.Size(), doc.MaxSize)
	} else if b, err = safefs.ReadFile(root, file, doc.MaxSize); err != nil {
		return nil, err
	} else {
		a, bad = doc.ParseEnded(b, run, fail != "")
	}
	if bad != nil {
		f := &Finished{State: StateInvalid, Reason: bad.Error()}
		f.File, err = settle(root, dir, run, file, "answer"+fail+"."+StateInvalid+".yml", nil)
		return f, err
	}

	res, err := dlp.ScanAnswer(ctx, c, model, a)
	var re *dlp.ReplyError
	if err != nil && !errors.As(err, &re) {
		return nil, fmt.Errorf("the DLP scan didn't run, nothing was changed (run finish again once it can): %w", err)
	}
	f := &Finished{State: StateDLPOK, DLP: res}
	if err != nil {
		f.State, f.Reason = StateDLPFail, err.Error()
	}
	if f.Raw, err = keepRaw(rawDir, run, name, b); err != nil {
		return nil, err
	}
	f.File, err = settle(root, dir, run, file, "answer"+fail+"."+f.State+".yml", a.Encode())
	return f, err
}

// keepRaw writes b, the untouched original of run's file name, to rawDir/<run>/<name>.orig (mode 0600) and returns its
// path.
func keepRaw(rawDir, run, name string, b []byte) (string, error) {
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		return "", err
	}
	raw, err := os.OpenRoot(rawDir)
	if err != nil {
		return "", err
	}
	defer raw.Close()
	if _, err := safefs.EnsureDir(raw, run); err != nil {
		return "", err
	}
	if err := safefs.Replace(raw, run, name+".orig", b, 0o600); err != nil {
		return "", fmt.Errorf("keeping the original: %w", err)
	}
	return filepath.Join(rawDir, run, name+".orig"), nil
}

// settle gives run's answer file its final name: renamed to final, or, with body, final written (mode 0600) and file
// removed; then the run directory moves to done/. It returns the answer's path in done/.
func settle(root *os.Root, dir, run, file, final string, body []byte) (string, error) {
	if body == nil {
		if err := safefs.Rename(root, file, filepath.Join(run, final)); err != nil {
			return "", err
		}
	} else {
		if err := safefs.WriteNew(root, run, final, body, 0o600); err != nil {
			return "", err
		}
		if err := safefs.Remove(root, file); err != nil {
			return "", err
		}
	}
	if _, err := safefs.EnsureDir(root, doneDir); err != nil {
		return "", err
	}
	if err := safefs.Rename(root, run, filepath.Join(doneDir, run)); err != nil {
		return "", err
	}
	return filepath.Join(dir, doneDir, run, final), nil
}
