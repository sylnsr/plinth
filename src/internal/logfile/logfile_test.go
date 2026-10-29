package logfile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"plinth/internal/safefs"
)

func setup(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	return work
}

// at fixes the clock of l (and of the logs made from it, which share it).
func at(l *Log, t time.Time) { l.w.now = func() time.Time { return t } }

// TestLines: a line is slog's text format with the component and pid, the fields of With and of the call, in that
// order; a newline or a quote in a value can't break the line; a new day starts a new file; the mode is 0600.
func TestLines(t *testing.T) {
	work := setup(t)
	day := time.Date(2026, 10, 12, 23, 59, 0, 0, time.UTC)
	l := New(work, CLI).With("ns", "michael/devops")
	at(l, day)
	l.Info("created", "path", "a\nb \"c\"")
	at(l, day.Add(2*time.Minute)) // the next day: a new file
	l.Component(Runner).Warn("registered agent", "agent", "main", "run", "calm-otter")
	if err := l.Err(); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(filepath.Join(work, Dir, "plinth-2026-10-12.log"))
	b2, _ := os.ReadFile(filepath.Join(work, Dir, "plinth-2026-10-13.log"))
	pid := os.Getpid()
	want1 := "time=2026-10-12T23:59:00Z level=INFO msg=created component=cli pid=" + strconv.Itoa(pid) + " ns=michael/devops path=\"a\\nb \\\"c\\\"\"\n"
	if string(b1) != want1 {
		t.Errorf("first day's file = %q, want %q", b1, want1)
	}
	want2 := "time=2026-10-13T00:01:00Z level=WARN msg=\"registered agent\" component=runner pid=" + strconv.Itoa(pid) + " ns=michael/devops agent=main run=calm-otter\n"
	if string(b2) != want2 {
		t.Errorf("second day's file = %q, want %q", b2, want2)
	}
	if fi, _ := os.Stat(filepath.Join(work, Dir, "plinth-2026-10-12.log")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	var nilLog *Log
	nilLog.Info("dropped") // must not panic
	nilLog.With("a", "b").Component(DLP).Error("dropped")
	(&Log{}).Info("dropped")
}

// TestParse: Parse reads what the writer wrote, quotes and all, and refuses what isn't a line of it.
func TestParse(t *testing.T) {
	work := setup(t)
	l := New(work, Reviews).With("ns", "michael/devops", "run", "calm-otter")
	at(l, time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC))
	l.Error("review failed", "key", "review/security", "reason", "it said \"no\"\nand more")
	b, _ := os.ReadFile(filepath.Join(work, Dir, "plinth-2026-10-12.log"))
	e, ok := Parse(string(b))
	if !ok || e.Level != "ERROR" || e.Msg != "review failed" || e.Component != Reviews || e.PID != strconv.Itoa(os.Getpid()) ||
		!e.Time.Equal(time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("parsed %+v, %v", e, ok)
	}
	if e.Get("ns") != "michael/devops" || e.Get("run") != "calm-otter" || e.Get("key") != "review/security" ||
		e.Get("reason") != "it said \"no\"\nand more" || e.Get("absent") != "" {
		t.Errorf("fields %+v", e.Fields)
	}
	for _, bad := range []string{"", "2026-10-12T09:30:00Z 4211 controller: started", "time=x level=INFO", "level=INFO msg=\"open",
		"time=2026-10-12T09:30:00Z msg=x", "a b=c"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) is an entry", bad)
		}
	}
}

func TestKeepsFirstError(t *testing.T) {
	work := setup(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(work, Dir, Name(time.Now()))); err != nil {
		t.Fatal(err)
	}
	l := New(work, CLI)
	l.Info("x")
	l.Info("y")
	if err := l.Err(); !errors.Is(err, safefs.ErrSymlink) {
		t.Errorf("Err = %v, want ErrSymlink", err)
	}
}

func TestOldAndScrub(t *testing.T) {
	work := setup(t)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local)
	for _, n := range []string{"plinth-2026-09-11.log", "plinth-2026-09-12.log", "plinth-2026-09-13.log", "plinth-2026-10-12.log", "notes.txt", "plinth-bad.log"} {
		if err := os.WriteFile(filepath.Join(work, Dir, n), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 30 days before 2026-10-12 is 2026-09-12: only what lies before it is old.
	old, err := Old(work, 30, now)
	if err != nil || !slices.Equal(old, []string{"plinth-2026-09-11.log"}) {
		t.Errorf("Old(30) = %v, %v", old, err)
	}
	if _, err := Old(work, 0, now); err == nil {
		t.Error("Old accepted a max age of 0 days")
	}
	gone, err := Scrub(work, 1, now)
	if err != nil || !slices.Equal(gone, []string{"plinth-2026-09-11.log", "plinth-2026-09-12.log", "plinth-2026-09-13.log"}) {
		t.Errorf("Scrub(1) = %v, %v", gone, err)
	}
	left, _ := os.ReadDir(filepath.Join(work, Dir))
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"notes.txt", "plinth-2026-10-12.log", "plinth-bad.log"}) {
		t.Errorf("left after Scrub: %v", names)
	}
	// A symlink with a log name is never followed or deleted.
	if err := os.Symlink("/etc/passwd", filepath.Join(work, Dir, "plinth-2020-01-01.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := Scrub(work, 1, now); !errors.Is(err, safefs.ErrSymlink) {
		t.Errorf("Scrub over a symlink: %v, want ErrSymlink", err)
	}
	if old, err := Old(t.TempDir(), 30, now); err != nil || len(old) != 0 {
		t.Errorf("Old without a logs directory = %v, %v", old, err)
	}
}

// TestTail: the last n lines of one component, from the day before and the day, oldest first.
func TestTail(t *testing.T) {
	work := setup(t)
	day := time.Date(2026, 10, 22, 0, 0, 30, 0, time.Local)
	for i, c := range []struct {
		at        time.Time
		component string
	}{
		{day.Add(-time.Minute), Controller}, {day.Add(-time.Minute), CLI}, {day, Controller}, {day, Controller},
	} {
		l := New(work, c.component)
		at(l, c.at)
		l.Info("line", "n", i)
	}
	got, err := Tail(work, 2, day, Controller)
	if err != nil || len(got) != 2 || !strings.HasSuffix(got[0], " n=2") || !strings.HasSuffix(got[1], " n=3") {
		t.Errorf("last 2: %q, %v", got, err)
	}
	if got, _ := Tail(work, 10, day, Controller); len(got) != 3 || !strings.HasSuffix(got[0], " n=0") {
		t.Errorf("last 10, over two days: %q", got)
	}
	if got, err := Tail(t.TempDir(), 10, day, Controller); err != nil || len(got) != 0 {
		t.Errorf("no logs: %q, %v", got, err)
	}
}

// TestOpenNotices: a key is open from its last line on, if that line is WARN or ERROR and not a clear, and for
// expires days; lines without a key don't count; the newest comes first.
func TestOpenNotices(t *testing.T) {
	work := setup(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	day := func(ago int, hour int) time.Time {
		return now.AddDate(0, 0, -ago).Add(time.Duration(hour-12) * time.Hour)
	}
	write := func(ago, hour int, level, msg string, fields ...any) {
		l := New(work, Controller)
		at(l, day(ago, hour))
		switch level {
		case "WARN":
			l.Warn(msg, fields...)
		case "ERROR":
			l.Error(msg, fields...)
		default:
			l.Info(msg, fields...)
		}
	}
	write(3, 9, "WARN", "raised, then cleared", "key", "cleared-one")
	write(2, 9, "INFO", "notice cleared", "key", "cleared-one", "cleared", true)
	write(1, 8, "WARN", "open one", "key", "b", "next", "plinthctl x")
	write(5, 8, "ERROR", "raised long ago", "key", "d")
	write(0, 7, "ERROR", "raised again", "key", "d", "err", "boom")
	write(13, 8, "WARN", "just inside", "key", "e")
	write(20, 8, "WARN", "too old", "key", "c")
	write(0, 6, "WARN", "no key here")
	got, err := OpenNotices(work, now, 14)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, n := range got {
		keys = append(keys, n.Key)
	}
	if !slices.Equal(keys, []string{"d", "b", "e"}) {
		t.Fatalf("open: %v, want d, b, e (newest first)", keys)
	}
	if got[0].Last.Msg != "raised again" || got[0].Last.Level != "ERROR" || got[0].Last.Get("err") != "boom" || got[1].Next() != "plinthctl x" {
		t.Errorf("last lines: %+v", got)
	}
	if got, _ := OpenNotices(work, now, 2); len(got) != 2 { // d (today) and b (yesterday)
		t.Errorf("with 2 days: %d open, want 2", len(got))
	}
	if got, err := OpenNotices(t.TempDir(), now, 14); err != nil || len(got) != 0 {
		t.Errorf("no logs: %v, %v", got, err)
	}
	// The cache follows the file: a clear written later ends the notice.
	write(0, 10, "INFO", "notice cleared", "key", "d", "cleared", true)
	if got, _ := OpenNotices(work, now, 14); len(got) != 2 {
		t.Errorf("after a clear: %d open, want 2", len(got))
	}
}
