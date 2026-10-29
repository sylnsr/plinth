package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/events"
	"plinth/internal/names"
)

// TestScanWaiting: what an unscanned answer waits for comes from the exec's dlp.scan events: the first
// unavailable says since when, timeouts count, an exec.start begins anew, a scan that ended clears it.
func TestScanWaiting(t *testing.T) {
	work := t.TempDir()
	ns := names.Namespace{Owner: "michael", Repo: "devops"}
	nsDir := filepath.Join(ns.Owner, ns.Repo)
	if err := os.MkdirAll(filepath.Join(work, nsDir, events.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	log, err := events.Open(work, nsDir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	add := func(run, action, outcome, reason string) {
		t.Helper()
		if _, err := log.Append(events.Event{Caller: Caller, Action: action, Target: "main/" + run, Outcome: outcome, Rule: "allow", Reason: reason}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(run string) Waiting {
		t.Helper()
		w, err := ScanWaiting(work, ns, "main", run)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	add("calm-otter", "exec.start", "ok", "ask.sh sha256 x")
	if w := get("calm-otter"); w.String() != "" {
		t.Errorf("before any scan: %+v", w)
	}
	add("calm-otter", "dlp.scan", "unavailable", "not available without Ollama (ollama.host is not set); the answer stays answer.new.yml, scanned again once the model can be asked; original sha256 x")
	first := get("calm-otter")
	add("calm-otter", "dlp.scan", "unavailable", "the Ollama gateway isn't running (plinthctl ollama-mcp start); the answer stays answer.new.yml, scanned again; original sha256 x")
	w := get("calm-otter")
	if w.Since == "" || w.Since != first.Since || w.Reason != "the Ollama gateway isn't running (plinthctl ollama-mcp start)" ||
		!strings.HasPrefix(w.String(), "waiting for the model: unavailable since "+w.Since+" (the Ollama gateway isn't running") {
		t.Errorf("unavailable: %+v, %q", w, w.String())
	}
	add("calm-otter", "dlp.scan", "timeout", "Ollama: no reply within 5m0s (ollama.timeout-seconds); attempt 1 of 3, the scan starts again; original sha256 x")
	if w := get("calm-otter"); w.Timeouts != 1 || w.String() != "waiting for the model: 1 of 3 attempts timed out (Ollama: no reply within 5m0s (ollama.timeout-seconds))" {
		t.Errorf("a timeout: %+v, %q", w, w.String())
	}
	add("calm-otter", "dlp.scan", "unavailable", "the Ollama gateway: connection refused; the answer stays answer.new.yml, scanned again once the model can be asked; original sha256 x")
	if w := get("calm-otter"); w.Since == "" || w.Timeouts != 1 ||
		w.String() != "waiting for the model: unavailable since "+w.Since+" (the Ollama gateway: connection refused); 1 of 3 attempts timed out before" {
		t.Errorf("unavailable after a timeout: %+v, %q", w, w.String())
	}
	add("calm-otter", "dlp.scan", "failed", "Ollama: no reply within 5m0s (ollama.timeout-seconds), 3 attempts")
	if w := get("calm-otter"); w.String() != "" {
		t.Errorf("after the scan ended: %+v", w)
	}
	add("keen-owl", "dlp.scan", "unavailable", "x; the answer stays answer.new.yml")
	add("keen-owl", "exec.start", "ok", "ask.sh sha256 x")
	if w := get("keen-owl"); w.String() != "" {
		t.Errorf("a new exec: %+v", w)
	}
}
