package controller

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"plinth/internal/logfile"
)

// TestNotifier: Once prints the sentence to stderr and writes a WARN line with the key and the fields, once per
// distinct notice; Clear ends an open notice with an INFO line, and only an open one; a key an earlier controller left
// open (seed) is ended by a Clear too.
func TestNotifier(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, logfile.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	lg := logfile.New(work, logfile.Controller)
	n := &notifier{w: &stderr, log: lg, last: map[string]string{}, open: map[string]bool{}}
	raise := func() {
		n.Once("ollama-gateway", "the Ollama gateway does not run", "err", errors.New("boom"), "effect", "the reviews fail",
			"next", "plinthctl ollama-mcp start")
	}
	raise()
	raise()
	if want := "plinthctl controller: the Ollama gateway does not run: boom (the reviews fail); next: plinthctl ollama-mcp start\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	open := func() []logfile.Notice {
		t.Helper()
		got, err := logfile.OpenNotices(work, time.Now(), 14)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := open(); len(got) != 1 || got[0].Key != "ollama-gateway" || got[0].Last.Msg != "the Ollama gateway does not run" ||
		got[0].Last.Get("err") != "boom" || got[0].Next() != "plinthctl ollama-mcp start" || got[0].Last.Level != "WARN" {
		t.Fatalf("open after Once: %+v", got)
	}
	n.Clear("ollama-gateway")
	n.Clear("ollama-gateway") // nothing open: no second line
	n.Clear("never-raised")
	if got := open(); len(got) != 0 {
		t.Errorf("open after Clear: %+v", got)
	}
	b, _ := os.ReadFile(filepath.Join(work, logfile.Dir, logfile.Name(time.Now())))
	if c := strings.Count(string(b), "cleared=true"); c != 1 || strings.Count(string(b), "key=ollama-gateway") != 2 {
		t.Errorf("log lines:\n%s", b)
	}
	// An earlier controller's open key: seed makes it known, so Clear ends it.
	lg.Warn("left open", "key", "old")
	if got := open(); len(got) != 1 {
		t.Fatalf("old notice: %+v", got)
	}
	n2 := &notifier{w: &stderr, log: lg, last: map[string]string{}, open: map[string]bool{}}
	n2.seed([]string{"old"})
	n2.Clear("old")
	if got := open(); len(got) != 0 {
		t.Errorf("after seed and Clear: %+v", got)
	}
}
