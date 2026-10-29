package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plinth/internal/events"
)

func TestEvents(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join("michael", "devops")
	if err := os.MkdirAll(filepath.Join(work, dir, events.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := events.Open(work, dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"main/keen-tapir", "al/brave-ibex", "main/keen-tapir"} {
		if _, err := l.Append(events.Event{Caller: "user", Action: "exec", Target: target, Outcome: "allow", Rule: "allow", Reason: "every rule passed"}); err != nil {
			t.Fatal(err)
		}
	}
	if code, out, errs := run("events", "verify", "-n", "michael/devops", "--work", work); code != exitOK || !strings.Contains(out, "3 event(s), the chain is intact") {
		t.Errorf("verify: %d %q %q", code, out, errs)
	}
	code, out, _ := run("events", "show", "-n", "michael/devops", "--work", work, "keen-tapir")
	if code != exitOK || strings.Count(out, "\n") != 2 || strings.Contains(out, "brave-ibex") || !strings.HasPrefix(out, "1 ") {
		t.Errorf("show keen-tapir: %d %q", code, out)
	}
	if _, out, _ := run("events", "show", "-n", "michael/devops", "--work", work); strings.Count(out, "\n") != 3 {
		t.Errorf("show all: %q", out)
	}
	t.Chdir(t.TempDir()) // outside any checkout: -n is required
	for _, args := range [][]string{{"events", "verify", "--work", work}, {"events", "show", "-n", "michael/devops", "--work", work, "../x"}} {
		if code, _, _ := run(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}

	// A changed line: verify names it, show stops before it.
	p := filepath.Join(work, dir, events.Dir, "1.jsonl")
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "al/brave-ibex", "al/brave-ibez", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run("events", "verify", "-n", "michael/devops", "--work", work); code != exitFail || !strings.Contains(errs, "1.jsonl line 3") {
		t.Errorf("verify after tampering: %d %q", code, errs)
	}
	code, out, errs := run("events", "show", "-n", "michael/devops", "--work", work)
	if code != exitFail || strings.Count(out, "\n") != 2 || !strings.Contains(errs, "the chain is broken") {
		t.Errorf("show after tampering: %d %q %q", code, out, errs)
	}
}
