package logfile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"plinth/internal/safefs"
)

// A notice is what the controller tells the user to act on: a WARN or ERROR line with a key= field, written when
// the problem is raised, again when it is raised again, and ended by an INFO line with the same key and cleared=true. A
// key is open from its last line on, unless that line is a clear, and it stops being open Expires days after that line,
// so a problem nobody saw to doesn't stay for good (plinth.notice.expires).
//
// The fields msg, err, effect and next say what is wrong, why, what it costs and what to do.

// Notice is an open notice: its key and its last line.
type Notice struct {
	Key  string
	Last Entry
}

// Next is what the user does about it, "" when the line doesn't say.
func (n Notice) Next() string { return n.Last.Get("next") }

// maxFile is the most of one day's file OpenNotices reads.
const maxFile = 64 << 20

// keyedCache keeps the keyed lines of a day's file until its size or time changes, so a TUI that asks every few seconds
// doesn't read two weeks of log each time.
var keyedCache = struct {
	sync.Mutex
	files map[string]keyedFile
}{files: map[string]keyedFile{}}

type keyedFile struct {
	size  int64
	mod   time.Time
	lines []Entry
}

// keyedLines are the lines of the day's file that carry a key=, in order. A missing file has none.
func keyedLines(root *os.Root, name string) ([]Entry, error) {
	f, err := safefs.OpenFile(root, name, os.O_RDONLY)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	keyedCache.Lock()
	c, ok := keyedCache.files[name+"|"+root.Name()]
	keyedCache.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.lines, nil
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile))
	if err != nil {
		return nil, err
	}
	var lines []Entry
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, " key=") {
			continue
		}
		if e, ok := Parse(l); ok && e.Get("key") != "" {
			lines = append(lines, e)
		}
	}
	keyedCache.Lock()
	keyedCache.files[name+"|"+root.Name()] = keyedFile{fi.Size(), fi.ModTime(), lines}
	keyedCache.Unlock()
	return lines, nil
}

// OpenNotices returns the notices open at now, newest first: it reads the logs of the last expires days and today, takes
// the last line of each key, and keeps those that are WARN or ERROR, not a clear and not older than expires days.
func OpenNotices(work string, now time.Time, expires int) ([]Notice, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer root.Close()
	last := map[string]Entry{}
	for d := expires; d >= 0; d-- {
		lines, err := keyedLines(root, filepath.Join(Dir, Name(now.AddDate(0, 0, -d))))
		if err != nil {
			return nil, err
		}
		for _, e := range lines {
			last[e.Get("key")] = e
		}
	}
	limit := now.AddDate(0, 0, -expires)
	var out []Notice
	for k, e := range last {
		if (e.Level == "WARN" || e.Level == "ERROR") && e.Get("cleared") != "true" && !e.Time.Before(limit) {
			out = append(out, Notice{k, e})
		}
	}
	slices.SortFunc(out, func(a, b Notice) int {
		if c := b.Last.Time.Compare(a.Last.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out, nil
}
