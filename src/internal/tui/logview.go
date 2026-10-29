package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"plinth/internal/logfile"
	"plinth/internal/term"
)

// The Logs screen shows the general log as lines, newest first, across the daily files, at most logLimit of the
// lines that match its filters. The filters combine; each one that is set shows in the breadcrumb bar, and Esc clears the
// last one set (first the search).

// logLimit is the most lines the Logs screen holds.
const logLimit = 5000

var levelRank = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

// logQuery is the Logs screen's filters except the search, which is the view's own (the file filter uses it too).
type logQuery struct {
	level     string // the lowest level shown: DEBUG, INFO, WARN or ERROR; "" for all
	notices   bool   // only the lines of the keys still open (Open notices)
	component string
	date      string // YYYY-MM-DD, local
	hour      string // 00 to 23, local
	ns, agent string // the run whose lines show (the Run screen's L)
	run       string
	order     []string // the filters in the order they were set: level, component, date, hour, run
}

// set notes that filter kind was set last.
func (q *logQuery) set(kind string) {
	q.order = append(slices.DeleteFunc(q.order, func(s string) bool { return s == kind }), kind)
}

// clearLast clears the filter set last and reports whether there was one.
func (q *logQuery) clearLast() bool {
	if len(q.order) == 0 {
		return false
	}
	switch kind := q.order[len(q.order)-1]; kind {
	case "level":
		q.level, q.notices = "", false
	case "component":
		q.component = ""
	case "date":
		q.date = ""
	case "hour":
		q.hour = ""
	case "run":
		q.ns, q.agent, q.run = "", "", ""
	}
	q.order = q.order[:len(q.order)-1]
	return true
}

// chips are the filters as the breadcrumb bar shows them, in the order they were set.
func (q logQuery) chips() []string {
	var out []string
	for _, kind := range q.order {
		switch kind {
		case "level":
			if q.notices {
				out = append(out, "notices")
			} else {
				out = append(out, ">="+strings.ToLower(q.level))
			}
		case "component":
			out = append(out, q.component)
		case "date":
			out = append(out, q.date)
		case "hour":
			out = append(out, q.hour+"h")
		case "run":
			out = append(out, "run:"+q.run)
		}
	}
	return out
}

// sig names the query, search included: the lines are loaded again when it changes.
func (q logQuery) sig(search string) string {
	return fmt.Sprintf("%q|%q|%v|%q|%q|%q|%q|%q|%q", search, q.level, q.notices, q.component, q.date, q.hour, q.ns, q.agent, q.run)
}

// logLine is a line of the log that parsed: the entry and the line as written (what the search looks in).
type logLine struct {
	e   logfile.Entry
	raw string
}

// match reports whether l passes the filters; open holds the keys of the open notices (for Open notices).
func (q logQuery) match(l logLine, search string, open map[string]bool) bool {
	e := l.e
	t := e.Time.In(time.Local)
	switch {
	case search != "" && !strings.Contains(strings.ToLower(l.raw), strings.ToLower(search)):
	case q.level != "" && levelRank[e.Level] < levelRank[q.level]:
	case q.notices && !open[e.Get("key")]:
	case q.component != "" && e.Component != q.component:
	case q.date != "" && t.Format("2006-01-02") != q.date:
	case q.hour != "" && t.Format("15") != q.hour:
	case q.ns != "" && e.Get("ns") != q.ns:
	case q.agent != "" && e.Get("agent") != q.agent:
	case q.run != "" && e.Get("run") != q.run:
	default:
		return true
	}
	return false
}

// logState is the Logs screen's state: the filters, whether the file list shows instead, and the lines loaded.
type logState struct {
	q     logQuery
	files bool
	lines []logLine
	more  bool   // lines beyond logLimit matched
	sig   string // the query the lines are of; "" forces a load
	qsig  string // the query of the lines last loaded, to tell a new query from a refresh
}

// logs is the view's Logs state, made when first used.
func (v *view) logsState() *logState {
	if v.logs == nil {
		v.logs = &logState{}
	}
	return v.logs
}

// logLines reads the lines that match, newest first, from files (the log's files, newest first), at most logLimit; more
// says there were others. A line that isn't in the log format is skipped.
func (s *Store) logLines(files []logFile, q logQuery, search string, expires int) (out []logLine, more bool, err error) {
	var open map[string]bool
	if q.notices {
		notices, err := logfile.OpenNotices(s.Work, s.Now(), expires)
		if err != nil {
			return nil, false, err
		}
		open = map[string]bool{}
		for _, n := range notices {
			open[n.Key] = true
		}
	}
	for _, f := range files {
		if q.date != "" && f.name != logfile.Name(mustDay(q.date)) {
			continue
		}
		b, _, err := s.readBelow(filepath.Join(logfile.Dir, f.name), maxFile)
		if err != nil {
			return out, false, err
		}
		lines := strings.Split(string(b), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if lines[i] == "" {
				continue
			}
			e, ok := logfile.Parse(lines[i])
			if !ok {
				continue
			}
			l := logLine{e, lines[i]}
			if !q.match(l, search, open) {
				continue
			}
			if len(out) == logLimit {
				return out, true, nil
			}
			out = append(out, l)
		}
	}
	return out, false, nil
}

// mustDay is the day of a YYYY-MM-DD date; the zero time for anything else (no file has that name).
func mustDay(date string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	return t
}

// reloadLines loads the lines of v again when its query changed or was forced (sig ""), and keeps the cursor on the same
// line, or at the top when it was there, so a screen at the top follows the new lines.
func (m *Model) reloadLines(v *view) {
	st := v.logsState()
	sig := st.q.sig(v.search)
	if st.sig == sig {
		return
	}
	changed := st.qsig != sig // a new query, not just a refresh of the log
	var sel string
	if !changed && v.cursor > 0 && v.cursor < len(st.lines) {
		sel = st.lines[v.cursor].raw
	}
	lines, more, err := m.store.logLines(m.logFiles, st.q, v.search, m.snap.g.Plinth.Notice.Expires)
	if err != nil {
		m.say(errStyle, err.Error())
	}
	if changed {
		v.cursor, v.top = 0, 0
	}
	st.lines, st.more, st.sig, st.qsig = lines, more, sig, sig
	if sel != "" {
		if i := slices.IndexFunc(lines, func(l logLine) bool { return l.raw == sel }); i >= 0 {
			v.cursor = i
		}
	}
}

// levelStyle draws the level token: error red, warn amber, info plain, debug dim.
func levelStyle(level string) term.Style {
	switch level {
	case "ERROR":
		return errStyle
	case "WARN":
		return term.Style{FG: term.Yellow}
	case "DEBUG":
		return dimStyle
	}
	return normal
}

// logRow is one line of the Logs screen: the time, the level, the component, the message, then the fields in dim, cut at
// the edge. Everything from the file goes through the sanitizer.
func logRow(l logLine, w int, selected bool) *term.Line {
	st, lv, dim := actStyle, levelStyle(l.e.Level), dimStyle
	if selected {
		st, lv, dim = selStyle, selStyle, selStyle
	}
	row := text(l.e.Time.In(time.Local).Format("01-02 15:04"), st).PadTo(12, st).Append(untrusted(l.e.Level, lv)).PadTo(18, st).
		Append(untrusted(l.e.Component, st)).PadTo(33, st).Append(untrusted(l.e.Msg, st))
	var fields []string
	for _, f := range l.e.Fields {
		fields = append(fields, f.Key+"="+f.Value)
	}
	if len(fields) > 0 {
		row.Text("  ", st).Append(untrusted(strings.Join(fields, " "), dim))
	}
	return row.Cut(0, w).PadTo(w, st)
}

// logText is a line in full for the pager: the fixed keys, then each field on a line of its own.
func logText(e logfile.Entry) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "time:       %s\nlevel:      %s\ncomponent:  %s\npid:        %s\nmessage:    %s\n", e.Time.Format(time.RFC3339), e.Level, e.Component, e.PID, e.Msg)
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "%-11s %s\n", f.Key+":", strings.ReplaceAll(f.Value, "\n", "\n            "))
	}
	return []byte(b.String())
}

// logDates are the days that have a log file, newest first: the Date picker's entries.
func (m *Model) logDates() []string {
	var out []string
	for _, f := range m.logFiles {
		if d, ok := strings.CutPrefix(f.name, "plinth-"); ok {
			if d, ok := strings.CutSuffix(d, ".log"); ok && len(d) == 10 {
				out = append(out, d)
			}
		}
	}
	return out
}

// logPicker opens the picker of a Logs filter: picking an entry sets it and notes it as the last set.
func (m *Model) logPicker(label string, values []string, set func(q *logQuery, s string) string) {
	v := m.top()
	m.pick = &picker{label: label, all: values, pick: func(s string) {
		st := v.logsState()
		st.q.set(set(&st.q, s))
		v.cursor, v.top = 0, 0
	}}
}

// logsKey is a key on the Logs screen: the filters (l, c, d, h, /), f for the file list, Enter for a line in full.
func (m *Model) logsKey(v *view, k term.Key) {
	if m.screenKey(k) {
		return
	}
	st := v.logsState()
	if st.files {
		m.logFilesKey(v, k)
		return
	}
	switch {
	case k.Code == term.KeyEsc && v.search != "":
		v.search = ""
	case k.Code == term.KeyEsc && st.q.clearLast():
		v.cursor, v.top = 0, 0
	case k.Code == term.KeyEsc || (k.Code == term.KeyRune && k.Rune == 'q'):
		m.pop()
	case k.Code == term.KeyEnter && v.cursor < len(st.lines):
		m.openText("Log line", "line", logText(st.lines[v.cursor].e), 0)
	case k.Code == term.KeyRune && k.Rune == '/':
		m.typing = true
		v.search = ""
	case k.Code == term.KeyRune && k.Rune == 'f':
		st.files, v.cursor, v.top = true, 0, 0
		v.search = ""
	case k.Code == term.KeyRune && k.Rune == 'l':
		m.logPicker("level:", []string{"Open notices", "error", "warn", "info", "debug"}, func(q *logQuery, s string) string {
			q.notices, q.level = s == "Open notices", ""
			if !q.notices {
				q.level = strings.ToUpper(s)
			}
			return "level"
		})
	case k.Code == term.KeyRune && k.Rune == 'c':
		comps := []string{logfile.AgentMCP, logfile.CLI, logfile.Controller, logfile.DLP, logfile.OllamaGateway, logfile.Reviews, logfile.Runner, logfile.TUI}
		m.logPicker("component:", comps, func(q *logQuery, s string) string { q.component = s; return "component" })
	case k.Code == term.KeyRune && k.Rune == 'd':
		m.logPicker("date:", m.logDates(), func(q *logQuery, s string) string { q.date = s; return "date" })
	case k.Code == term.KeyRune && k.Rune == 'h':
		var hours []string
		for h := range 24 {
			hours = append(hours, fmt.Sprintf("%02dh", h))
		}
		m.logPicker("hour:", hours, func(q *logQuery, s string) string { q.hour = strings.TrimSuffix(s, "h"); return "hour" })
	case k.Code == term.KeyCtrl && k.Rune == 'd':
		m.scrubLogs()
	}
}

// logFilesKey is a key on the file list of the Logs screen (f): Enter opens a file in the pager, f or Esc goes back to the
// lines, / filters the names.
func (m *Model) logFilesKey(v *view, k term.Key) {
	st := v.logsState()
	files := m.logRows(v)
	switch {
	case k.Code == term.KeyEsc && v.search != "":
		v.search = ""
	case k.Code == term.KeyEsc || (k.Code == term.KeyRune && (k.Rune == 'q' || k.Rune == 'f')):
		st.files, v.cursor, v.top = false, 0, 0
		v.search = ""
	case k.Code == term.KeyEnter && v.cursor < len(files):
		f := files[v.cursor]
		b, cut, err := m.store.readLog(f.name)
		if err != nil {
			m.say(errStyle, err.Error())
			return
		}
		m.openText("Log "+f.name, f.name, b, cut)
	case k.Code == term.KeyRune && k.Rune == '/':
		m.typing = true
		v.search = ""
	case k.Code == term.KeyCtrl && k.Rune == 'd':
		m.scrubLogs()
	}
}

// logsView is the Logs screen: the lines with their filters, or, after f, the log's files with the old ones marked.
func (m *Model) logsView(v *view, w, rows int) (title, body []*term.Line) {
	st := v.logsState()
	if st.files {
		return m.logFilesView(v, w, rows)
	}
	m.reloadLines(v)
	t := text("Logs", headStyle).Text(titleGap, normal).Untrusted(m.store.Work+"/logs", normal)
	switch {
	case st.more:
		t.Text(fmt.Sprintf(titleGap+"the newest %d lines that match", len(st.lines)), normal)
	case len(st.lines) == 1:
		t.Text(titleGap+"1 line", normal)
	default:
		t.Text(fmt.Sprintf(titleGap+"%d lines", len(st.lines)), normal)
	}
	if v.search != "" && !m.typing {
		t.Text(titleGap+"search: ", normal).Untrusted(v.search, boldStyle)
	}
	title = []*term.Line{t}
	if len(st.lines) == 0 {
		return title, []*term.Line{text("No log line matches.", dimStyle)}
	}
	v.cursor = max(0, min(v.cursor, len(st.lines)-1))
	scroll(v, v.cursor, rows)
	for i := v.top; i < len(st.lines) && i < v.top+rows; i++ {
		body = append(body, logRow(st.lines[i], w, i == v.cursor))
	}
	return title, body
}
