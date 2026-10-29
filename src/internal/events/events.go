// Package events is a namespace's append-only, hash-chained event log in <ns-dir>/events/: one JSON object per line,
// each line holding the sha256 of the line before it (across files; "genesis" for the first), so no line can be
// changed, removed or reordered without Verify naming the first that doesn't match. A file holds at most max lines;
// the next line starts <first-seq>.jsonl. Files are never deleted. It holds names, decisions and hashes, never
// content. Spec: plinthctl.README.md, "Events".
package events

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"plinth/internal/safefs"
)

// Dir is the event directory below <ns-dir>.
const Dir = "events"

// Genesis is prev of the very first line.
const Genesis = "genesis"

// maxLine bounds one line when reading; a longer one is corrupt.
const maxLine = 64 << 10

var fileRe = regexp.MustCompile(`^([1-9][0-9]*)\.jsonl$`)

// Event is one line. The field order is the line's order.
type Event struct {
	Seq     int64  `json:"seq"`
	Time    string `json:"time"` // RFC 3339, UTC
	Caller  string `json:"caller"`
	Action  string `json:"action"`
	Target  string `json:"target"`
	Outcome string `json:"outcome"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason"`
	Config  string `json:"config"` // ConfigHash of the two config files in force
	Prev    string `json:"prev"`
}

// ConfigHash is the config field: the sha256 (hex) of the global and the namespace config files' sha256s, one per line.
func ConfigHash(global, namespace string) string {
	sum := sha256.Sum256([]byte(global + "\n" + namespace + "\n"))
	return hex.EncodeToString(sum[:])
}

// Hash is a line's hash: the sha256 (hex) of the line without its newline.
func Hash(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

// Log appends to one namespace's event log. One process writes it (the controller holds plinth.lock); a Log is safe for
// several goroutines.
type Log struct {
	work string // <work>
	dir  string // <owner>/<repo>/events, below work
	max  int
	now  func() time.Time

	mu    sync.Mutex
	seq   int64  // of the last line, 0 for none
	prev  string // hash of the last line, Genesis for none
	file  string // the file the next line goes to, "" to start a new one
	lines int    // in file
}

// Receipt identifies an appended line: its seq and hash. Overrides and released answers record it.
type Receipt struct {
	Seq  int64
	Hash string
}

// Open reads the tail of the namespace's log so Append can continue the chain. A last line cut short by a crash
// (no newline at the end) is removed: it was never complete, so nothing acted on it. Any other damage fails: the chain
// must not continue from a line that doesn't parse.
func Open(work, nsDir string, maxLines int) (*Log, error) {
	if maxLines < 1 {
		return nil, fmt.Errorf("events: %d lines per file: must be at least 1", maxLines)
	}
	l := &Log{work: work, dir: filepath.Join(nsDir, Dir), max: maxLines, now: time.Now, prev: Genesis}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files, err := list(root, l.dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return l, nil
	}
	last := files[len(files)-1]
	p := filepath.Join(l.dir, last.name)
	b, err := safefs.ReadFile(root, p, int64(maxLines+1)*maxLine)
	if err != nil {
		return nil, err
	}
	if i := bytes.LastIndexByte(b, '\n'); i != len(b)-1 {
		// A torn last line: cut it off (it has no newline, so it was never a complete event).
		f, err := safefs.OpenFile(root, p, os.O_WRONLY)
		if err != nil {
			return nil, err
		}
		err = f.Truncate(int64(i + 1))
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("%s: cutting off a torn last line: %w", p, err)
		}
		b = b[:i+1]
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if len(b) == 0 {
		// An empty file (its only line was torn): the chain continues from the file before.
		if len(files) == 1 {
			l.file = last.name
			return l, nil
		}
		l.file, l.lines = last.name, 0
		prevFile := filepath.Join(l.dir, files[len(files)-2].name)
		pb, err := safefs.ReadFile(root, prevFile, int64(maxLines+1)*maxLine)
		if err != nil {
			return nil, err
		}
		lines = bytes.Split(bytes.TrimSuffix(pb, []byte("\n")), []byte("\n"))
		p = prevFile
		if err := l.tail(p, lines[len(lines)-1]); err != nil {
			return nil, err
		}
		if l.seq+1 != last.first {
			return nil, fmt.Errorf("%s: named %d, but the line before it is seq %d", filepath.Join(l.dir, last.name), last.first, l.seq)
		}
		return l, nil
	}
	if err := l.tail(p, lines[len(lines)-1]); err != nil {
		return nil, err
	}
	l.file, l.lines = last.name, len(lines)
	return l, nil
}

// tail takes the chain's state from the last complete line.
func (l *Log) tail(file string, line []byte) error {
	var e Event
	if err := strict(line, &e); err != nil {
		return fmt.Errorf("%s: the last line doesn't parse (%v); plinthctl events verify names the damage", file, err)
	}
	l.seq, l.prev = e.Seq, Hash(line)
	return nil
}

// Append writes e as the next line, filling in Seq, Time and Prev, and returns its receipt once it is on disk
// (written in one write and synced). A file with max lines is followed by a new one, <seq>.jsonl.
func (l *Log) Append(e Event) (Receipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq, e.Prev = l.seq+1, l.prev
	e.Time = l.now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(e)
	if err != nil {
		return Receipt{}, err
	}
	root, err := os.OpenRoot(l.work)
	if err != nil {
		return Receipt{}, err
	}
	defer root.Close()
	file, lines := l.file, l.lines
	if file == "" || lines >= l.max {
		file, lines = strconv.FormatInt(e.Seq, 10)+".jsonl", 0
	}
	f, err := safefs.OpenCreate(root, l.dir, file, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Receipt{}, err
	}
	_, err = f.Write(append(line, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && lines == 0 {
		err = syncDir(root, l.dir) // a new file: its name must survive a crash too
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("%s: %w", filepath.Join(l.dir, file), err)
	}
	h := Hash(line)
	l.seq, l.prev, l.file, l.lines = e.Seq, h, file, lines+1
	return Receipt{Seq: e.Seq, Hash: h}, nil
}

// Problem is the first place where the chain doesn't match.
type Problem struct {
	File   string // below <ns-dir>/events
	Line   int    // 1-based; 0 for the file itself
	Reason string
}

func (p *Problem) Error() string {
	if p.Line == 0 {
		return fmt.Sprintf("%s: %s", p.File, p.Reason)
	}
	return fmt.Sprintf("%s line %d: %s", p.File, p.Line, p.Reason)
}

// Verify walks the whole chain of the namespace's log and returns the number of events, or the first Problem (or
// another error when the log can't be read).
func Verify(work, nsDir string) (int64, error) {
	var n int64
	err := Walk(work, nsDir, func(Event, []byte) error { n++; return nil })
	return n, err
}

// Walk calls fn for every event in order, after checking it against the chain; the first mismatch stops it with a
// *Problem. fn's error stops it too.
func Walk(work, nsDir string, fn func(e Event, line []byte) error) error {
	root, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := filepath.Join(nsDir, Dir)
	files, err := list(root, dir)
	if err != nil {
		return err
	}
	seq, prev := int64(0), Genesis
	for fi, f := range files {
		if f.first != seq+1 {
			return &Problem{File: f.name, Reason: fmt.Sprintf("named %d, but the line before it is seq %d", f.first, seq)}
		}
		fh, err := safefs.OpenFile(root, filepath.Join(dir, f.name), os.O_RDONLY)
		if err != nil {
			return err
		}
		r := bufio.NewReaderSize(fh, maxLine)
		for ln := 1; ; ln++ {
			line, err := r.ReadSlice('\n')
			if errors.Is(err, io.EOF) && len(line) == 0 {
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				fh.Close()
				return &Problem{File: f.name, Line: ln, Reason: "longer than a line can be"}
			}
			if err != nil && !errors.Is(err, io.EOF) {
				fh.Close()
				return err
			}
			if !bytes.HasSuffix(line, []byte("\n")) {
				fh.Close()
				if fi == len(files)-1 {
					return &Problem{File: f.name, Line: ln, Reason: "a torn last line (no newline); the controller cuts it off when it attaches the namespace"}
				}
				return &Problem{File: f.name, Line: ln, Reason: "no newline at the end of a file that isn't the last"}
			}
			line = line[:len(line)-1]
			var e Event
			if err := strict(line, &e); err != nil {
				fh.Close()
				return &Problem{File: f.name, Line: ln, Reason: fmt.Sprintf("doesn't parse: %v", err)}
			}
			if e.Seq != seq+1 {
				fh.Close()
				return &Problem{File: f.name, Line: ln, Reason: fmt.Sprintf("seq %d, want %d", e.Seq, seq+1)}
			}
			if e.Prev != prev {
				fh.Close()
				return &Problem{File: f.name, Line: ln, Reason: fmt.Sprintf("seq %d: prev doesn't match the line before it (changed, removed or reordered)", e.Seq)}
			}
			if err := fn(e, line); err != nil {
				fh.Close()
				return err
			}
			seq, prev = e.Seq, Hash(line)
		}
		fh.Close()
	}
	return nil
}

func syncDir(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// strict decodes one line: exactly the fields of Event, nothing after the object.
func strict(line []byte, e *Event) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(e); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

type evFile struct {
	name  string
	first int64
}

// list returns the event files of dir below root, ordered by their first seq. A missing dir has none; a file with
// another name, or anything that isn't a regular file, is refused.
func list(root *os.Root, dir string) ([]evFile, error) {
	fi, err := safefs.Lstat(root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []evFile
	for _, e := range entries {
		n := e.Name()
		if n == ".gitignore" || (strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".tmp")) {
			continue
		}
		m := fileRe.FindStringSubmatch(n)
		if m == nil || !e.Type().IsRegular() {
			return nil, fmt.Errorf("%s: %s is not an event file (<first-seq>.jsonl, a regular file)", dir, n)
		}
		first, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %v", dir, n, err)
		}
		out = append(out, evFile{n, first})
	}
	slices.SortFunc(out, func(a, b evFile) int { return cmp.Compare(a.first, b.first) })
	return out, nil
}
