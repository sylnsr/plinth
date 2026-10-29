package plain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"plinth/internal/doc"
	"plinth/internal/names"
	"plinth/internal/safefs"
)

// read-answer's exit codes: 0 printed; the rest print one fixed status line and nothing of any file.
const (
	AnswerRunning    = 10 // still running (running.yml): poll again, never read it
	AnswerDLPFail    = 11 // the DLP scan failed: not read, tell the user; nothing here overrides it
	AnswerNotScanned = 12 // finished, not scanned (answer[.FAIL].new.yml): the user runs plinthctl finish
	AnswerNone       = 13 // no such run, or not run yet
	AnswerInvalid    = 14 // the answer isn't a valid document (INVALID): a bug, never read; tell the user
)

// Answer is what ReadAnswer found: the answer to print (Body), or the status line and exit code instead.
type Answer struct {
	Body   []byte // nil unless Code is 0
	File   string // the answer's path, for Code 0
	Failed bool   // the run stopped at a gate (answer.FAIL.*)
	Status string // one fixed line for Code other than 0, never any file content
	Code   int
}

// Commands are what ReadAnswer's status lines tell the agent to run: plinthctl's commands (CLI), or the Plinth MCP
// server's tools for an agent without plinthctl.
type Commands struct {
	ReadAnswer string // "read-answer": read the answer again
	NewAsk     string // "plinthctl new-ask": start a new run
}

// CLI are plinthctl's commands.
var CLI = Commands{ReadAnswer: "read-answer", NewAsk: "plinthctl new-ask"}

// ReadAnswer is read-answer: the answer of run in the AAA work directory dir, dir/<RUN>/ or dir/done/<RUN>/, only when
// it is finished and its DLP scan passed: an answer document (answer[.FAIL].dlp-ok.yml) that decodes strictly
// with status.dlp.result ok. Its state comes from the file names, a failed DLP scan first, then INVALID, running, not
// scanned (answer[.FAIL].new.yml). Nothing is read through a symlink, and only these checks read the file before it is
// printed, inside this function, so the caller never sees an unscanned answer.
func ReadAnswer(dir, run string) (*Answer, error) {
	if !names.ValidRun(run) {
		return nil, fmt.Errorf("%q is not a RUN_NAME", run)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return ReadAnswerIn(root, run, CLI)
}

// ReadAnswerIn is ReadAnswer in the work directory root, opened by the caller (OpenWorkDir), its status lines naming
// cmd's commands. Answer.File is below root.Name().
func ReadAnswerIn(root *os.Root, run string, cmd Commands) (*Answer, error) {
	if !names.ValidRun(run) {
		return nil, fmt.Errorf("%q is not a RUN_NAME", run)
	}
	runDir := ""
	for _, d := range []string{run, filepath.Join(doneDir, run)} {
		if fi, err := root.Lstat(d); err == nil && fi.IsDir() {
			runDir = d
			break
		}
	}
	if runDir == "" {
		return &Answer{Status: fmt.Sprintf("%s: no such run - next: check the RUN_NAME (%s printed it), or ask the user whether it ran", run, cmd.NewAsk), Code: AnswerNone}, nil
	}
	has := func(state string) bool {
		return present(root, filepath.Join(runDir, "answer."+state+".yml")) || present(root, filepath.Join(runDir, "answer.FAIL."+state+".yml"))
	}
	switch {
	case has("DLP-FAIL"):
		return dlpFailed(run), nil
	case has("INVALID"):
		return invalid(run, cmd), nil
	case present(root, filepath.Join(runDir, "running.yml")):
		return &Answer{Status: fmt.Sprintf("%s: still running (running.yml exists) - not read; next: wait until the user says \"%s done\", then "+
			"%s again; if it was killed, a rerun is a new run (%s)", run, run, cmd.ReadAnswer, cmd.NewAsk), Code: AnswerRunning}, nil
	case has("new"):
		return &Answer{Status: fmt.Sprintf("%s: finished, not scanned yet - not read; next: ask the user to run plinthctl finish %s (again, if the "+
			"Ollama gateway or Ollama was down), then %s again", run, run, cmd.ReadAnswer), Code: AnswerNotScanned}, nil
	}
	found := ""
	for _, n := range []string{"answer.dlp-ok.yml", "answer.FAIL.dlp-ok.yml"} {
		if f := filepath.Join(runDir, n); present(root, f) {
			found = f
			break
		}
	}
	if found == "" {
		return &Answer{Status: fmt.Sprintf("%s: not run yet (no answer in %s) - next: ask the user to run it (from ai/work: ./%s/ask.sh), "+
			"then %s again once they say \"%s done\"", run, runDir, run, cmd.ReadAnswer, run), Code: AnswerNone}, nil
	}
	b, err := safefs.ReadFile(root, found, doc.MaxSize)
	if err != nil {
		return nil, err
	}
	var d doc.Answer
	err = doc.Decode(b, &d)
	if err == nil {
		err = d.Check(false)
	}
	if err != nil {
		return invalid(run, cmd), nil
	}
	if d.Status == nil || d.Status.DLP == nil || d.Status.DLP.Result != doc.ResultOK {
		return dlpFailed(run), nil
	}
	return &Answer{Body: b, File: filepath.Join(root.Name(), found), Failed: strings.Contains(filepath.Base(found), ".FAIL")}, nil
}

func dlpFailed(run string) *Answer {
	return &Answer{Status: run + ": DLP scan FAILED - not read; next: tell the user, who reviews the answer and decides (nothing here overrides it)", Code: AnswerDLPFail}
}

func invalid(run string, cmd Commands) *Answer {
	return &Answer{Status: run + ": the answer is INVALID (not a valid document: a bug, never data) - not read; next: tell the user; a rerun is a new run (" + cmd.NewAsk + ")", Code: AnswerInvalid}
}

// present reports whether name is there below root as anything, a symlink included: a state is a name.
func present(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
