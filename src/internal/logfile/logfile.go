// Package logfile writes Plinth's general log, the information that isn't for the terminal: one file per day,
// <work>/logs/plinth-YYYY-MM-DD.log, so a new day starts a new file (the rotation). A line is slog's text format:
// `time=<RFC 3339> level=<DEBUG|INFO|WARN|ERROR> msg="<text>" component=<name> pid=<n> [<field>=<value> ...]`, one
// line, strings quoted and escaped by the handler, so a message or a value can't break the line or hide a control
// character. component is one of the fixed set below; the fields a caller gives (ns, agent, run, key, rule, err ...) follow.
// Old files are deleted only by Scrub (web UI, TUI, plinthctl logs scrub), which takes plinth.log.max-age-days from the
// config. Never log a secret: no token, no key, no answer content. Spec: plinthctl.README.md, "Log".
package logfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"plinth/internal/safefs"
)

// Dir is the log directory below <work>.
const Dir = "logs"

// DefaultMaxAgeDays is plinth.log.max-age-days when the config doesn't set it.
const DefaultMaxAgeDays = 30

// The components a line can come from. A new one is added here and in the spec.
const (
	Controller    = "controller"
	Runner        = "runner"
	Reviews       = "reviews"
	DLP           = "dlp"
	OllamaGateway = "ollama-gateway"
	AgentMCP      = "agent-mcp"
	TUI           = "tui"
	CLI           = "cli"
)

const dateLayout = "2006-01-02"

var nameRe = regexp.MustCompile(`^plinth-([0-9]{4}-[0-9]{2}-[0-9]{2})\.log$`)

// Name is the log file for the day of t.
func Name(t time.Time) string { return "plinth-" + t.Format(dateLayout) + ".log" }

// dayWriter appends what the handler writes (one whole line per Write) to the log of now's day. The first failure is kept
// for Err and later lines are dropped: a failure never stops the caller.
type dayWriter struct {
	work   string
	now    func() time.Time
	mu     sync.Mutex
	err    error
	report func(error) // Watch: told of every failure, and with nil of the first success after one; failures don't latch
	failed bool
}

func (w *dayWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.work == "" || (w.err != nil && w.report == nil) {
		w.mu.Unlock()
		return len(p), nil
	}
	t := w.now()
	root, err := os.OpenRoot(w.work)
	if err == nil {
		err = safefs.Append(root, Dir, Name(t), p, 0o600)
		root.Close()
	}
	var tell func()
	switch {
	case err != nil:
		w.err = fmt.Errorf("log %s: %w", filepath.Join(w.work, Dir, Name(t)), err)
		w.failed = true
		if w.report != nil {
			e, r := w.err, w.report
			tell = func() { r(e) }
		}
	case w.failed:
		w.failed, w.err = false, nil
		if w.report != nil {
			r := w.report
			tell = func() { r(nil) }
		}
	}
	w.mu.Unlock()
	if tell != nil {
		go tell() // not in this call: report may log, and the handler holds its lock while it writes
	}
	return len(p), nil
}

// Log appends lines for one component. Its zero value and a nil *Log drop every line.
type Log struct {
	w         *dayWriter
	h         slog.Handler
	once      *onceState
	component string
	fields    []any
}

// onceState is what Once remembers, shared by a log and the logs made from it.
type onceState struct {
	mu   sync.Mutex
	last map[string]string
}

// New returns the log of component (one of the constants above) in work. Nothing is opened until a line is written.
func New(work, component string) *Log {
	w := &dayWriter{work: work, now: time.Now}
	return &Log{w: w, h: newHandler(w, func() time.Time { return w.now() }), once: &onceState{last: map[string]string{}}, component: component}
}

// NewTo returns a log that writes its lines to out, in the same format: the Ollama gateway's, in its container, whose
// output the controller reads with podman logs (it has no access to <work>).
func NewTo(out io.Writer, component string) *Log {
	return &Log{h: newHandler(out, time.Now), once: &onceState{last: map[string]string{}}, component: component}
}

func newHandler(out io.Writer, now func() time.Time) slog.Handler {
	return slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.String(slog.TimeKey, now().Format(time.RFC3339))
		}
		return a
	}})
}

// Watch makes this log, and the logs made from it, tell report about every write failure and, with nil, about the first
// success after one, and not stop at the first failure: a long-running process (the controller) goes on and says so.
func (l *Log) Watch(report func(error)) {
	if l == nil || l.w == nil {
		return
	}
	l.w.mu.Lock()
	l.w.report = report
	l.w.mu.Unlock()
}

// Component returns the log of another component that shares this one's file and fields.
func (l *Log) Component(component string) *Log {
	if l == nil || l.h == nil {
		return l
	}
	c := *l
	c.component = component
	return &c
}

// With returns the log with more fields on every line, given as alternating keys and values, as slog takes them.
func (l *Log) With(fields ...any) *Log {
	if l == nil || l.h == nil {
		return l
	}
	c := *l
	c.fields = append(append([]any(nil), l.fields...), fields...)
	return &c
}

func (l *Log) log(level slog.Level, msg string, fields []any) {
	if l == nil || l.h == nil {
		return
	}
	r := slog.NewRecord(time.Now(), level, msg, 0)
	r.Add("component", l.component, "pid", os.Getpid())
	r.Add(l.fields...)
	r.Add(fields...)
	_ = l.h.Handle(context.Background(), r)
}

// Debug, Info, Warn and Error append one line of that level; fields are alternating keys and values.
func (l *Log) Debug(msg string, fields ...any) { l.log(slog.LevelDebug, msg, fields) }
func (l *Log) Info(msg string, fields ...any)  { l.log(slog.LevelInfo, msg, fields) }
func (l *Log) Warn(msg string, fields ...any)  { l.log(slog.LevelWarn, msg, fields) }
func (l *Log) Error(msg string, fields ...any) { l.log(slog.LevelError, msg, fields) }

// Once appends a WARN line for key unless the same line (message and fields) was the last one logged for key, so a state
// that lasts is logged once; Clear forgets key, so its next Once logs again.
func (l *Log) Once(key, msg string, fields ...any) {
	if l == nil || l.h == nil {
		return
	}
	sig := msg + "\x00" + fmt.Sprint(fields...)
	l.once.mu.Lock()
	same := l.once.last[key] == sig
	l.once.last[key] = sig
	l.once.mu.Unlock()
	if !same {
		l.Warn(msg, fields...)
	}
}

// Clear forgets key.
func (l *Log) Clear(key string) {
	if l == nil || l.h == nil {
		return
	}
	l.once.mu.Lock()
	delete(l.once.last, key)
	l.once.mu.Unlock()
}

// Err returns the first write error (the latest, with Watch), or nil.

func (l *Log) Err() error {
	if l == nil || l.w == nil {
		return nil
	}
	l.w.mu.Lock()
	defer l.w.mu.Unlock()
	return l.w.err
}

// Old lists the log files in work that are older than maxAgeDays days at now: their date (from the name) lies more than
// maxAgeDays days before now's date. Today's file is never old; files with other names are never touched.
func Old(work string, maxAgeDays int, now time.Time) ([]string, error) {
	if maxAgeDays < 1 {
		return nil, fmt.Errorf("max age %d days: must be at least 1", maxAgeDays)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if _, err := safefs.Lstat(root, Dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	d, err := root.Open(Dir)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	y, m, dd := now.Date()
	cutoff := time.Date(y, m, dd, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -maxAgeDays)
	var old []string
	for _, n := range names {
		mm := nameRe.FindStringSubmatch(n)
		if mm == nil {
			continue
		}
		day, err := time.Parse(dateLayout, mm[1])
		if err == nil && day.Before(cutoff) {
			old = append(old, n)
		}
	}
	sort.Strings(old)
	return old, nil
}

// tailBytes is how much of the end of a day's file Tail reads.
const tailBytes = 1 << 20

// ControllerComponents are the components whose lines are the controller's: everything but the CLI's and the TUI's.
var ControllerComponents = []string{Controller, Runner, Reviews, DLP, OllamaGateway, AgentMCP}

// Tail returns the last n lines of the given components in the logs of now's day and the day before, oldest first: what
// controller status shows. A missing file has no lines; nothing is read through a symlink.
func Tail(work string, n int, now time.Time, components ...string) ([]string, error) {
	root, err := os.OpenRoot(work)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var out []string
	for _, day := range []time.Time{now.AddDate(0, 0, -1), now} {
		f, err := safefs.OpenFile(root, filepath.Join(Dir, Name(day)), os.O_RDONLY)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fi, err := f.Stat()
		if err == nil && fi.Size() > tailBytes {
			_, err = f.Seek(fi.Size()-tailBytes, io.SeekStart)
		}
		var b []byte
		if err == nil {
			b, err = io.ReadAll(io.LimitReader(f, tailBytes))
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if e, ok := Parse(l); ok && slices.Contains(components, e.Component) {

				out = append(out, l)
			}
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// Scrub deletes the files Old lists (regular files only, no symlink followed) and returns their names. It stops at the
// first file it can't delete; the names before it are deleted.
func Scrub(work string, maxAgeDays int, now time.Time) ([]string, error) {
	old, err := Old(work, maxAgeDays, now)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for i, n := range old {
		if err := safefs.Remove(root, filepath.Join(Dir, n)); err != nil {
			return old[:i], err
		}
	}
	return old, nil
}
