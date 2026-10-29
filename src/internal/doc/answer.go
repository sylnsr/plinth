package doc

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"plinth/internal/names"
)

// Answer is kind Answer: what an AAA run wrote through the AAA library (running.yml, then answer[.FAIL].<state>.yml),
// and the status Plinth adds. The library writes it append-only, sections first and result last, so a run killed
// on the way still leaves a document that decodes.
type Answer struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   AnswerMeta    `yaml:"metadata"`
	Sections   []Section     `yaml:"sections"`
	Failure    string        `yaml:"failure,omitempty"`
	Result     string        `yaml:"result,omitempty"` // ok | fail; "": the script didn't finish
	Status     *AnswerStatus `yaml:"status,omitempty"`
}

// AnswerMeta is an answer's metadata.
type AnswerMeta struct {
	Run     string `yaml:"run"`
	Started string `yaml:"started,omitempty"` // RFC 3339
}

// Section is one section of an answer: its title and what the script said in it.
type Section struct {
	Title  string `yaml:"title"`
	Output string `yaml:"output"`
}

// AnswerStatus is Plinth's part of an answer; a script never writes it.
type AnswerStatus struct {
	DLP *DLP `yaml:"dlp,omitempty"`
}

// DLP is the DLP scan's result in an answer's status.
type DLP struct {
	Result string `yaml:"result"` // ok | fail
	Model  string `yaml:"model,omitempty"`
	Chunks int    `yaml:"chunks"`
	Fixed  int    `yaml:"fixed"`  // fixed-pattern masks
	Masked int    `yaml:"masked"` // lines the model flagged
	Reason string `yaml:"reason,omitempty"`
}

// The results.
const (
	ResultOK   = "ok"
	ResultFail = "fail"
)

// Check checks what Decode can't: the envelope, the run's name, the time, the result and the status. fromScript is
// a document as the script left it (running.yml, answer[.FAIL].new.yml), which has no status: that is Plinth's.
func (a *Answer) Check(fromScript bool) error {
	if err := checkEnvelope(a.APIVersion, a.Kind, KindAnswer); err != nil {
		return err
	}
	if !names.ValidRun(a.Metadata.Run) {
		return fmt.Errorf("metadata.run %q is not a run name", a.Metadata.Run)
	}
	if s := a.Metadata.Started; s != "" {
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return fmt.Errorf("metadata.started %q is not an RFC 3339 time", s)
		}
	}
	switch a.Result {
	case "", ResultOK, ResultFail:
	default:
		return fmt.Errorf("result %q, want %s or %s", a.Result, ResultOK, ResultFail)
	}
	if a.Failure != "" && a.Result != ResultFail {
		return errors.New("a failure without result: fail")
	}
	if a.Status == nil {
		return nil
	}
	if fromScript {
		return errors.New("a status: that is Plinth's, a script doesn't write it")
	}
	if d := a.Status.DLP; d != nil && d.Result != ResultOK && d.Result != ResultFail {
		return fmt.Errorf("status.dlp.result %q, want %s or %s", d.Result, ResultOK, ResultFail)
	}
	return nil
}

// Texts are the answer's texts from the script, for the DLP scan to mask: every section's title and output, and the
// failure.
func (a *Answer) Texts() []*string {
	var out []*string
	for i := range a.Sections {
		out = append(out, &a.Sections[i].Title, &a.Sections[i].Output)
	}
	return append(out, &a.Failure)
}

// Encode writes the answer as the AAA library does, with its status after the result: every text a literal block
// (AppendText), so the document decodes whatever its texts hold. A text decodes back ending in one \n (an empty one
// stays empty).
func (a *Answer) Encode() []byte {
	b := fmt.Appendf(nil, "apiVersion: %s\nkind: %s\nmetadata:\n  run: %s\n", APIVersion, KindAnswer, quote(a.Metadata.Run))
	if a.Metadata.Started != "" {
		b = fmt.Appendf(b, "  started: %s\n", quote(a.Metadata.Started))
	}
	if len(a.Sections) == 0 {
		b = append(b, "sections: []\n"...)
	} else {
		b = append(b, "sections:\n"...)
	}
	for _, s := range a.Sections {
		b = append(b, "  - title: |2\n"...)
		b = AppendText(b, []byte(s.Title), 6)
		b = append(b, "    output: |2\n"...)
		b = AppendText(b, []byte(s.Output), 6)
	}
	if a.Failure != "" {
		b = append(b, "failure: |2\n"...)
		b = AppendText(b, []byte(a.Failure), 2)
	}
	if a.Result != "" {
		b = fmt.Appendf(b, "result: %s\n", quote(a.Result))
	}
	if a.Status != nil && a.Status.DLP != nil {
		d := a.Status.DLP
		b = append(b, "status:\n"...)
		b = fmt.Appendf(b, "  dlp:\n    result: %s\n", quote(d.Result))
		if d.Model != "" {
			b = fmt.Appendf(b, "    model: %s\n", quote(d.Model))
		}
		b = fmt.Appendf(b, "    chunks: %d\n    fixed: %d\n    masked: %d\n", d.Chunks, d.Fixed, d.Masked)
		if d.Reason != "" {
			b = append(b, "    reason: |2\n"...)
			b = AppendText(b, []byte(d.Reason), 6)
		}
	}
	return b
}

// ParseEnded decodes b, an answer as the script left it once it ended (answer[.FAIL].new.yml), and checks it: a valid
// document from a script (Check), with a result, of run, its result fail exactly when its name says .FAIL (failed).
// The sections the script never said anything in (the AAA library's untitled first one, when the script starts with
// section) are dropped.
func ParseEnded(b []byte, run string, failed bool) (*Answer, error) {
	var a Answer
	if err := Decode(b, &a); err != nil {
		return nil, err
	}
	if err := a.Check(true); err != nil {
		return nil, err
	}
	switch {
	case a.Result == "":
		return nil, errors.New("no result: the script didn't end through the AAA library")
	case a.Metadata.Run != run:
		return nil, fmt.Errorf("metadata.run %q is not the run %s", a.Metadata.Run, run)
	case (a.Result == ResultFail) != failed:
		return nil, fmt.Errorf("result %s doesn't match the file's name", a.Result)
	}
	a.Sections = slices.DeleteFunc(a.Sections, func(s Section) bool { return s.Title == "" && s.Output == "" })
	return &a, nil
}
