package plain

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestRuns: every run of the work directory and of done/, its state in read-answer's order from the file names, and
// only ask.sh and the answer as its files; a symlinked run directory is no run.
func TestRuns(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"calm-otter/ask.sh", "lone-yew/ask.sh", "lone-yew/answer.dlp-ok.yml", "lone-yew/running.yml",
		"done/deep-elm/answer.FAIL.dlp-ok.yml", "done/dark-sky/answer.DLP-FAIL.yml", "done/dark-sky/answer.dlp-ok.yml", "scrub.sh"} {
		write(t, filepath.Join(dir, f), "x")
	}
	if err := os.Symlink(filepath.Join(dir, "calm-otter"), filepath.Join(dir, "keen-owl")); err != nil {
		t.Fatal(err)
	}
	got, err := Runs(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Run{
		{Name: "calm-otter", State: StateNotRun, Files: []string{"ask.sh"}},
		{Name: "lone-yew", State: StateRunning, Files: []string{"answer.dlp-ok.yml", "ask.sh"}},
		{Name: "dark-sky", Done: true, State: StateDLPFail, Files: []string{"answer.DLP-FAIL.yml", "answer.dlp-ok.yml"}},
		{Name: "deep-elm", Done: true, State: StateDLPOK, Gate: true, Files: []string{"answer.FAIL.dlp-ok.yml"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if _, err := Runs(filepath.Join(dir, "none")); err == nil {
		t.Error("a missing work directory gave no error")
	}
}
