package plain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"plinth/internal/dlp"
	"plinth/internal/doc"
	"plinth/internal/names"
	"plinth/internal/ollama"
	"plinth/internal/safefs"
)

// ErrScanned: the answer document's status.dlp.result is ok already; DLPScan leaves it alone.
var ErrScanned = errors.New("already scanned")

// errNotFinished: dlp-scan was given a run's running.yml or answer[.FAIL].new.yml, which finish scans.
var errNotFinished = errors.New("an answer that isn't finished: plinthctl finish <RUN> scans it")

// DLPScan is dlp-scan: the DLP scan of an answer document, file, any path below a run directory (ai/work/<RUN>/
// or done/<RUN>/, which names the run): decoded strictly, its texts masked (dlp.ScanAnswer), the untouched original
// kept in rawDir/<RUN>/<name>.orig (mode 0600), and the file replaced, its mode kept, by the document written again
// with status.dlp. One whose status.dlp.result is ok already is left alone (ErrScanned); a scan that fails leaves the
// file untouched, and the error says why.
func DLPScan(ctx context.Context, c ollama.Chatter, model, rawDir, file string) (dlp.Result, error) {
	var res dlp.Result
	if !filepath.IsAbs(rawDir) {
		return res, fmt.Errorf("plinth.aaa-raw-dir %q: not an absolute path", rawDir)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return res, err
	}
	dir, name := filepath.Split(abs)
	run := filepath.Base(dir)
	if !names.ValidRun(run) {
		return res, fmt.Errorf("%s: not in a run directory (its directory's name is not a run name)", file)
	}
	if filepath.Ext(name) != ".yml" {
		return res, fmt.Errorf("%s: not an answer document (.yml)", file)
	}
	if name == "running.yml" || strings.HasSuffix(name, "."+StateNew+".yml") {
		return res, errNotFinished
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return res, err
	}
	defer root.Close()
	if err := safefs.Regular(root, name); err != nil {
		return res, err
	}
	fi, err := root.Lstat(name)
	if err != nil {
		return res, err
	}
	if fi.Size() > doc.MaxSize {
		return res, fmt.Errorf("%d bytes, more than the %d a document may have", fi.Size(), doc.MaxSize)
	}
	b, err := safefs.ReadFile(root, name, doc.MaxSize)
	if err != nil {
		return res, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return res, errors.New("the file is empty")
	}
	var a doc.Answer
	err = doc.Decode(b, &a)
	if err == nil {
		err = a.Check(false)
	}
	if err != nil {
		return res, fmt.Errorf("not a valid answer document: %w", err)
	}
	if a.Status != nil && a.Status.DLP != nil && a.Status.DLP.Result == doc.ResultOK {
		return res, ErrScanned
	}
	if res, err = dlp.ScanAnswer(ctx, c, model, &a); err != nil {
		return res, err
	}
	if _, err := keepRaw(rawDir, run, name, b); err != nil {
		return res, err
	}
	return res, safefs.Replace(root, ".", name, a.Encode(), fi.Mode().Perm())
}
