package events

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nsDir = "michael/devops"

// setup returns a <work> with <ns-dir>/events/.
func setup(t *testing.T) (work, dir string) {
	t.Helper()
	work = t.TempDir()
	dir = filepath.Join(work, "michael", "devops", Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return work, dir
}

func open(t *testing.T, work string, maxLines int) *Log {
	t.Helper()
	l, err := Open(work, filepath.FromSlash(nsDir), maxLines)
	if err != nil {
		t.Fatal(err)
	}
	l.now = func() time.Time { return time.Date(2026, 10, 12, 19, 0, 0, 0, time.FixedZone("CDT", -5*3600)) }
	return l
}

func appendN(t *testing.T, l *Log, n int) []Receipt {
	t.Helper()
	var rs []Receipt
	for i := range n {
		r, err := l.Append(Event{Caller: "user", Action: "exec", Target: "main/keen-tapir", Outcome: "allow", Rule: "allow",
			Reason: "every rule passed " + strings.Repeat("x", i), Config: ConfigHash("a", "b")})
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	return rs
}

func verify(t *testing.T, work string) (int64, error) {
	t.Helper()
	return Verify(work, filepath.FromSlash(nsDir))
}

func TestAppendRotateVerify(t *testing.T) {
	work, dir := setup(t)
	l := open(t, work, 2)
	rs := appendN(t, l, 5)
	if rs[4].Seq != 5 {
		t.Errorf("receipt = %+v", rs[4])
	}
	for _, f := range []string{"1.jsonl", "3.jsonl", "5.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "1.jsonl"))
	first, _, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(first, `{"seq":1,"time":"2026-10-13T00:00:00Z","caller":"user",`) || !strings.HasSuffix(first, `"prev":"genesis"}`) {
		t.Errorf("first line = %s", first)
	}
	if rs[0].Hash != Hash([]byte(first)) {
		t.Errorf("receipt hash %s is not the line's hash", rs[0].Hash)
	}
	if n, err := verify(t, work); err != nil || n != 5 {
		t.Fatalf("Verify = %d, %v", n, err)
	}
	// A new Open continues the chain where it ended, across the file boundary.
	l = open(t, work, 2)
	appendN(t, l, 2)
	if n, err := verify(t, work); err != nil || n != 7 {
		t.Fatalf("Verify after reopening = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "7.jsonl")); err != nil {
		t.Errorf("rotation after reopening: %v", err)
	}
}

func TestVerifyFindsTampering(t *testing.T) {
	for _, c := range []struct {
		name   string
		tamper func(dir string) error
		want   string
	}{
		{"changed reason", func(dir string) error {
			return edit(dir, "1.jsonl", func(s string) string {
				return strings.Replace(s, "every rule passed x", "every rule passed y", 1)
			})
		}, "3.jsonl line 1: seq 3: prev doesn't match"},
		{"removed line", func(dir string) error {
			return edit(dir, "3.jsonl", func(s string) string { _, rest, _ := strings.Cut(s, "\n"); return rest })
		}, "3.jsonl line 1: seq 4, want 3"},
		{"removed file", func(dir string) error { return os.Remove(filepath.Join(dir, "3.jsonl")) }, "5.jsonl: named 5, but the line before it is seq 2"},
		{"extra field", func(dir string) error {
			return edit(dir, "5.jsonl", func(s string) string { return strings.Replace(s, `{"seq":5,`, `{"seq":5,"x":1,`, 1) })
		}, `unknown field "x"`},
		{"stray file", func(dir string) error { return os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600) }, "notes.txt is not an event file"},
		{"torn last line", func(dir string) error { return appendRaw(dir, "5.jsonl", `{"seq":6,"ti`) }, "5.jsonl line 2: a torn last line"},
	} {
		work, dir := setup(t)
		appendN(t, open(t, work, 2), 5)
		if err := c.tamper(dir); err != nil {
			t.Fatal(err)
		}
		_, err := verify(t, work)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Verify = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestOpenCutsATornLine(t *testing.T) {
	work, dir := setup(t)
	appendN(t, open(t, work, 10), 3)
	if err := appendRaw(dir, "1.jsonl", `{"seq":4,"time":"20`); err != nil {
		t.Fatal(err)
	}
	l := open(t, work, 10)
	if r := appendN(t, l, 1); r[0].Seq != 4 {
		t.Errorf("seq after the torn line = %d, want 4", r[0].Seq)
	}
	if n, err := verify(t, work); err != nil || n != 4 {
		t.Errorf("Verify = %d, %v", n, err)
	}
	// A torn line that was the only line of a new file: the file is left empty and the chain goes on in it.
	work, dir = setup(t)
	appendN(t, open(t, work, 2), 2)
	if err := appendRaw(dir, "3.jsonl", `{"seq":3`); err != nil {
		t.Fatal(err)
	}
	l = open(t, work, 2)
	if r := appendN(t, l, 1); r[0].Seq != 3 {
		t.Errorf("seq = %d, want 3", r[0].Seq)
	}
	if n, err := verify(t, work); err != nil || n != 3 {
		t.Errorf("Verify = %d, %v", n, err)
	}
}

func TestOpenRefusesDamage(t *testing.T) {
	work, dir := setup(t)
	appendN(t, open(t, work, 10), 2)
	if err := appendRaw(dir, "1.jsonl", "not json\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(work, filepath.FromSlash(nsDir), 10); err == nil || !strings.Contains(err.Error(), "doesn't parse") {
		t.Errorf("Open on a damaged last line: %v", err)
	}
	if _, err := Open(work, filepath.FromSlash(nsDir), 0); err == nil {
		t.Error("Open took 0 lines per file")
	}
	var p *Problem
	if _, err := verify(t, work); !errors.As(err, &p) || p.Line != 3 {
		t.Errorf("Verify = %v, want a Problem on line 3", err)
	}
}

func TestEmptyLog(t *testing.T) {
	work, _ := setup(t)
	if n, err := verify(t, work); err != nil || n != 0 {
		t.Errorf("Verify of an empty log = %d, %v", n, err)
	}
	if n, err := Verify(t.TempDir(), filepath.FromSlash(nsDir)); err != nil || n != 0 {
		t.Errorf("Verify without events/ = %d, %v", n, err)
	}
	if ConfigHash("a", "b") == ConfigHash("b", "a") || ConfigHash("a", "b") != ConfigHash("a", "b") {
		t.Error("ConfigHash must depend on which file is which, and only on them")
	}
}

func edit(dir, name string, f func(string) string) error {
	p := filepath.Join(dir, name)
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(f(string(b))), 0o600)
}

func appendRaw(dir, name, s string) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}
