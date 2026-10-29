package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"plinth/internal/names"
	"plinth/internal/runner"
	"plinth/internal/runstate"
	"plinth/internal/term"
)

var (
	normal     = term.Style{}
	boldStyle  = term.Style{Bold: true}
	dimStyle   = term.Style{Dim: true}
	errStyle   = term.Style{FG: term.Red}
	headStyle  = term.Style{Bold: true, FG: term.Cyan}
	labelStyle = term.Style{FG: term.Cyan}
	selStyle   = term.Style{Reverse: true}

	// actStyle is the text one can select and open with Enter: the rows of a list (stringColor, as in the coloured YAML).
	actStyle = term.Style{FG: stringColor}

	// hintStyle is the keys in the header, a light gray, less bright than the text.
	hintStyle    = term.Style{FG: term.RGB(0xa8a8a8)}
	hintKeyStyle = term.Style{FG: term.RGB(0xa8a8a8), Bold: true}

	// barColor is the breadcrumb bar's background, #A863F8. Its black is RGB, not the palette's:
	// Konsole and other terminals draw bold palette black as the bright colour, near white on the bar.
	barColor    = term.RGB(0xa863f8)
	barStyle    = term.Style{FG: term.RGB(0), BG: barColor}
	barKeyStyle = term.Style{FG: term.RGB(0), BG: barColor, Bold: true}
	divStyle    = term.Style{FG: barColor} // the divider under the header

	// msgColor is the message line's: the colour of the operators in YAML and shell (hlOp).
	msgColor = term.Yellow
)

// titleGap separates the items of a screen's title line.
const titleGap = "    "

// The smallest screen the TUI draws on (plinth-tui.README.md, Runtime): at 80 x 24 the Runs list keeps 14 rows under
// the 6-row header and its divider.
const (
	minWidth  = 80
	minHeight = 24
)

// The header: headerRows rows, on the right two columns of hints, hintWidth wide each, so maxHints fit; the left
// block takes the rest (34 columns at 80), its labels labelWidth wide. A picker shows at most pickerRows entries.
const (
	headerRows = 6
	hintWidth  = 23
	labelWidth = 12
	maxHints   = 2 * headerRows
	pickerRows = 6
)

func text(s string, st term.Style) *term.Line      { return (&term.Line{}).Text(s, st) }
func untrusted(s string, st term.Style) *term.Line { return (&term.Line{}).Untrusted(s, st) }

// View draws the model on a w x h screen, k9s style: the header (Plinth, the filters, the hints), a divider, the
// picker while one is open, the screen's title and body, the message line and the breadcrumb bar. Every string from a
// file goes through term's sanitizer (Line.Untrusted); only the TUI's own text doesn't.
func (m *Model) View(w, h int) []*term.Line {
	m.w, m.h = w, h
	if w < minWidth || h < minHeight {
		note := fmt.Sprintf("terminal too small (%d x %d, needs %d x %d) - Ctrl-C quits", w, h, minWidth, minHeight)
		return []*term.Line{text(note, normal)}
	}
	// The divider sets the header apart from the rest.
	lines := append(m.header(w), text(strings.Repeat(m.glyphs.Rule, w), divStyle))
	lines = append(lines, m.pickerView(w)...)
	// room is the rows of the title and the body, above the message line and the breadcrumb; rows(n) the body's below
	// n title rows.
	room := h - len(lines) - 2
	rows := func(n int) int { return room - n }
	v := m.top()
	var title, body []*term.Line
	switch {
	case m.prompt != nil:
		title, body = m.promptView(w, rows(1))
	case m.help:
		title, body = m.helpView(rows(1))
	case v.kind == screenRuns:
		title, body = m.runsView(v, w, room)
	case v.kind == screenRun:
		title, body = m.runView(v, w, rows(1))
	case v.kind == screenPager || v.kind == screenEvents:
		title, body = m.pagerView(v, w, rows(2))
	case v.kind == screenLogs:
		title, body = m.logsView(v, w, rows(2))
	default:
		title, body = m.imageView(v, w, rows(1))
	}
	lines = append(lines, title...)
	lines = append(lines, body[:max(0, min(len(body), room-len(title)))]...)
	for len(lines) < h-2 {
		lines = append(lines, nil)
	}
	return append(lines, m.msgLine(), m.crumbs(w))
}

// header is the top headerRows rows (k9s style). On the left one item per row, the labels aligned: <work>
// (giving way from its left, …/end, when it doesn't fit), the controller, Ollama and the three filters. A
// controller that runs shows as its PID (it is the one process, its runners inside it), "running" for a PID
// that couldn't be read; what needs the user is counted after it, and the message line says where to look. On the
// right the hints, in two columns, the first filled first.
func (m *Model) header(w int) []*term.Line {
	left := w - 2*hintWidth
	ctl := &term.Line{}
	switch {
	case m.snap.controller && m.snap.pid > 0:
		ctl.Text(fmt.Sprintf("PID %d", m.snap.pid), normal)
	case m.snap.controller:
		ctl.Text("running", normal)
	default:
		ctl.Text("not running", boldStyle)
	}
	if n := len(m.snap.notices); n > 0 {
		ctl.Text(", "+noticeWord(n), boldStyle)
	}
	ollama := "not configured"
	if m.snap.g.Ollama.Host != "" {
		ollama = "configured"
	}
	work := untrusted(m.store.Work, normal)
	if room := left - labelWidth - 1; work.Width() > room {
		e := len([]rune(m.glyphs.Ellipsis))
		work = text(m.glyphs.Ellipsis, normal).Append(work.Cut(work.Width()-room+e, room-e))
	}
	released := "hidden"
	if m.showReleased {
		released = "shown"
	}
	items := []struct {
		label string
		value *term.Line
	}{
		{"Work:", work},
		{"Controller:", ctl},
		{"Ollama:", text(ollama, normal)},
		{"Namespace:", untrusted(or(m.nsFilter, "all"), boldStyle)},
		{"Agent:", untrusted(or(m.agentFilter, "all"), boldStyle)},
		{"Released:", text(released, boldStyle)},
	}
	hs := m.hints()
	cell := func(i int) *term.Line {
		if i >= len(hs) {
			return &term.Line{}
		}
		return text("<"+hs[i].key+">", hintKeyStyle).Text(" "+hs[i].label, hintStyle).Cut(0, hintWidth-1)
	}
	var out []*term.Line
	for i, it := range items {
		l := text(it.label, labelStyle).PadTo(labelWidth, normal).Append(it.value.Cut(0, left-labelWidth-1)).PadTo(left, normal)
		out = append(out, l.Append(cell(i)).PadTo(left+hintWidth, normal).Append(cell(headerRows+i)))
	}
	return out
}

// hint is a key in the header: the key as typed (<e>, <shift-r>, <ctrl-d>, <:>), then what it does.
type hint struct{ key, label string }

// hints are the screen's own keys, in the header: the actions that apply to the selection (one that can't act now
// too: it says why when pressed), then the screen's other keys. Navigation, Esc and Q aren't shown (k9s); the general keys
// (general) are in the bar, and ? lists every key. More than maxHints: the first maxHints. None while a prompt, Help, a
// picker or the filter is open: those say their keys themselves.
func (m *Model) hints() []hint {
	var hs []hint
	acts := func(as []act) {
		for _, a := range as {
			hs = append(hs, hint{a.key, hintWord(a)})
		}
	}
	v := m.top()
	switch {
	case m.prompt != nil || m.help || m.typing || m.pick != nil:
		return nil
	case v.kind == screenRuns:
		if ri := m.selected(); ri != nil {
			acts(m.actions(ri))
		}
		hs = append(hs, hint{"n", "Namespace"}, hint{"a", "Agent"}, hint{"t", "Show/hide released"}, hint{"/", "Filter"})
	case v.kind == screenRun:
		if ri := m.selected(); ri != nil {
			acts(m.actions(ri))
		}
	case v.kind == screenImage:
		acts(m.imageActs(v))
		hs = append(hs, hint{"shift-r", "Reload"})
	case v.kind == screenLogs:
		if v.logsState().files {
			hs = append(hs, hint{"/", "Filter"}, hint{"f", "Lines"}, hint{"ctrl-d", "Scrub logs"})
		} else {
			hs = append(hs, hint{"l", "Level"}, hint{"c", "Component"}, hint{"d", "Date"}, hint{"h", "Hour"}, hint{"/", "Search"},
				hint{"f", "Files"}, hint{"ctrl-d", "Scrub logs"})
		}
	case v.kind == screenEvents:
		hs = append(hs, hint{"/", "Filter"}, hint{"w", "Wrap"})
	case v.kind == screenPager:
		hs = append(hs, hint{"/", "Search"}, hint{"w", "Wrap"})
	}
	return hs[:min(len(hs), maxHints)]
}

// general are the keys that work on every screen but the confirmations, in the right end of the breadcrumb bar: the
// screen picker (not in the pager) and Help. None while a confirmation, Help, a picker or the filter is open.
func (m *Model) general() []hint {
	switch {
	case m.prompt != nil || m.help || m.typing || m.pick != nil:
		return nil
	case m.top().kind == screenPager:
		return []hint{{"?", "Help"}}
	}
	return []hint{{":", "Screens"}, {"?", "Help"}}
}

// hintWord is an action's word in the header: its label's first word, or a short one (Accept network: Network).
func hintWord(a act) string {
	if a.hint != "" {
		return a.hint
	}
	switch a.label {
	case "Accept network":
		return "Network"
	case "Log in":
		return "Login"
	case "Log out":
		return "Logout"
	}
	w, _, _ := strings.Cut(a.label, " ")
	return w
}

// pickerView is the rows under the header while the filter or a picker is open: the input line, then the
// picker's entries, at most pickerRows around the selected one; nil while neither is.
func (m *Model) pickerView(w int) []*term.Line {
	switch {
	case m.typing:
		return []*term.Line{text("/", boldStyle).Untrusted(*m.typed(), boldStyle).Text("_", normal)}
	case m.pick == nil:
		return nil
	}
	p := m.pick
	out := []*term.Line{text(p.label+" ", boldStyle).Untrusted(p.input, boldStyle).Text("_", normal)}
	items := p.items()
	if len(items) == 0 {
		return append(out, text("  nothing matches", dimStyle))
	}
	top := max(0, p.cursor-pickerRows+1)
	for i := top; i < len(items) && i < top+pickerRows; i++ {
		st := actStyle
		if i == p.cursor {
			st = selStyle
		}
		out = append(out, text("  ", normal).Append(untrusted(" "+items[i], st).PadTo(hintWidth, st).Cut(0, w-2)))
	}
	return out
}

// noticeWord is "1 notice" or "<n> notices".
func noticeWord(n int) string {
	if n == 1 {
		return "1 notice"
	}
	return fmt.Sprintf("%d notices", n)
}

// msgLine is the outcome of the last action, or an error; without one, while a notice is open, the newest one:
// its message and what to do about it.
func (m *Model) msgLine() *term.Line {
	switch {
	case m.msg != nil:
		return m.msg
	case len(m.snap.notices) > 0:
		n := m.snap.notices[0]
		s := n.Last.Msg
		if next := n.Next(); next != "" {
			s += " - " + next
		}
		st := term.Style{Bold: true, FG: msgColor}
		return text(noticeWord(len(m.snap.notices))+": ", st).Untrusted(s, st)
	}
	return &term.Line{}
}

// crumbs is the breadcrumb bar, the last row (k9s style): the namespace filter (<all> without one), then each
// screen on the stack (the Run screen as its run, the pager as its file), the last bold, and at its right end the general
// keys. Black on barColor, reverse video without colour; the breadcrumb gives way from its left (…) when it
// doesn't fit.
func (m *Model) crumbs(w int) *term.Line {
	l := text(" ", barStyle)
	add := func(s string, st term.Style) { l.Text("<", st).Untrusted(s, st).Text(">", st).Text(" ", barStyle) }
	add(or(m.nsFilter, "all"), barStyle)
	for i, v := range m.stack {
		st := barStyle
		if i == len(m.stack)-1 {
			st = barKeyStyle
		}
		add(v.crumb, st)
		if v.kind == screenLogs && !v.logsState().files { // the filters set, after the screen
			for _, c := range v.logsState().q.chips() {
				add(c, barStyle)
			}
			if v.search != "" {
				add("/"+v.search, barStyle)
			}
		}
	}
	// The general keys take the right end of the bar; the breadcrumb gives way from its left to what is left.
	right := &term.Line{}
	for i, h := range m.general() {
		if i > 0 {
			right.Text("  ", barStyle)
		}
		right.Text("<"+h.key+">", barKeyStyle).Text(" "+h.label, barStyle)
	}
	if right.Width() > 0 {
		right.Text(" ", barStyle)
	}
	room := w - right.Width()
	if l.Width() > room {
		e := len([]rune(m.glyphs.Ellipsis))
		l = text(m.glyphs.Ellipsis, barStyle).Append(l.Cut(l.Width()-room+e, room-e))
	}
	return l.PadTo(room, barStyle).Append(right)
}

// scroll moves v.top so that row cursor shows among n rows.
func scroll(v *view, cursor, n int) {
	if cursor < v.top {
		v.top = cursor
	}
	if cursor >= v.top+n {
		v.top = cursor - n + 1
	}
	v.top = max(0, v.top)
}

// columns are the widths of the Runs table's columns at width w: RUN, NAMESPACE, AGENT, ASK, ANSWER, ACTION STATE, AGE.
// At 80 columns each still shows, but "released (gate)" is cut; more width goes first to ANSWER (up to 15), then a
// little to RUN and NAMESPACE, then all to ACTION STATE.
func columns(w int) []int {
	cols := []int{15, 11, 8, 13, 11, 12, 3}
	extra := w - 79
	for _, c := range []struct{ i, most int }{{4, 4}, {0, 4}, {1, 4}, {5, 1 << 20}} {
		add := min(extra, c.most)
		cols[c.i] += add
		extra -= add
	}
	return cols
}

// row lays cells out in columns of the widths cols, cutting each, one space between them.
func row(cols []int, cells []*term.Line, st term.Style) *term.Line {
	l := &term.Line{}
	x := 0
	for i, c := range cells {
		if i > 0 {
			x++
			l.PadTo(x, st)
		}
		l.Append(c.Cut(0, cols[i]))
		x += cols[i]
		l.PadTo(x, st)
	}
	return l
}

// runsView is the Runs list under its column headings, with the run filter above them while one is set; the header
// has the namespace and agent filters. room is the rows for both.
func (m *Model) runsView(v *view, w, room int) (title, body []*term.Line) {
	if m.runFilter != "" && !m.typing {
		title = append(title, text("filter: ", normal).Untrusted(m.runFilter, boldStyle))
	}
	cols := columns(w)
	var heads []*term.Line
	for _, s := range []string{"RUN", "NAMESPACE", "AGENT", "ASK", "ANSWER", "ACTION STATE", "AGE"} {
		heads = append(heads, text(s, boldStyle))
	}
	title = append(title, row(cols, heads, normal))
	rows := room - len(title)
	for _, sp := range m.snap.spaces {
		if sp.err == nil || (m.nsFilter != "" && sp.ns.String() != m.nsFilter) {
			continue
		}
		if sp.cfg == nil {
			body = append(body, untrusted(sp.ns.String()+": not served: plinthctl config check -n "+sp.ns.String(), errStyle))
		} else {
			body = append(body, untrusted(sp.ns.String()+": "+sp.err.Error(), errStyle))
		}
	}
	rs := m.rows()
	if len(rs) == 0 {
		return title, append(body, text("No runs.", dimStyle))
	}
	v.cursor = max(0, min(v.cursor, len(rs)-1))
	n := max(1, rows-len(body))
	scroll(v, v.cursor, n)
	for i := v.top; i < len(rs) && i < v.top+n; i++ {
		st := actStyle
		if i == v.cursor {
			st = selStyle
		}
		ri := rs[i]
		r := ri.r
		ask, ans := "broken", ""
		if r.Broken == nil {
			ask, ans = r.AskStatus(), r.AnswerStatus()
			if ri.waiting.String() != "" {
				ans = runner.WaitingStatus
			}
		}
		cells := []*term.Line{untrusted(r.Name, st), untrusted(ri.sp.ns.String(), st), untrusted(r.Agent, st),
			untrusted(ask, st), untrusted(ans, st), untrusted(requests(ri), st), text(age(m.store.Now(), r.Newest), st)}
		body = append(body, row(cols, cells, st).PadTo(w, st))
	}
	return title, body
}

// requests is the ACTION STATE cell: the run's requests and their states, and "no controller" while its namespace doesn't
// beat (short, so it fits the column; the header says "not running").
func requests(ri *runInfo) string {
	var s []string
	for _, q := range ri.reqs {
		s = append(s, q.Action+" "+q.State)
	}
	if ri.sp.stale {
		s = append(s, "no controller")
	}
	return strings.Join(s, ", ")
}

// stage is the furthest stage the run reached (plinth-web-ui.README.md, What a tab shows).
func stage(r *runstate.Run) string {
	switch f := r.Facts; {
	case f.Answer == "released":
		return "released"
	case f.Answer != "":
		return "answer"
	case slices.ContainsFunc(r.Files, func(f runstate.File) bool { return f.Name == runstate.Running }):
		return "running"
	case len(r.Reviews) > 0:
		return "review"
	}
	return "ask"
}

// runView is the Run screen: the run's fields and what its files say, wrapped to the width w, then its files.
func (m *Model) runView(v *view, w, rows int) (title, body []*term.Line) {
	ri := m.find(v.run)
	if ri == nil {
		return []*term.Line{text("Run", headStyle)}, []*term.Line{text("The run is gone.", dimStyle)}
	}
	r, d := ri.r, v.det
	title = []*term.Line{text("Run ", headStyle).Untrusted(r.Name, headStyle)}
	var ls []*term.Line
	field := func(name, value string, st term.Style) {
		ls = append(ls, text(fmt.Sprintf("%-14s", name), boldStyle).Untrusted(value, st))
	}
	section := func(name string) { ls = append(ls, text(name, headStyle)) }
	field("Namespace", ri.sp.ns.String(), normal)
	field("Agent", r.Agent, normal)
	if r.Broken != nil {
		field("State", "broken: "+r.Broken.Error(), errStyle)
		return title, m.wrapAll(ls, w)
	}
	field("Stage", stage(r), normal)
	field("Ask status", r.AskStatus(), normal)
	if s := r.AnswerStatus(); s != "" {
		if ws := ri.waiting.String(); ws != "" {
			s = ws
		}
		field("Answer", s, normal)
	}
	field("Mode", d.mode.Name(), normal)
	field("Interactive", map[bool]string{true: "yes (INTERACTIVE=1)", false: "no"}[d.mode.Interactive], normal)
	if d.mode.Network {
		field("Network", map[bool]string{true: "accepted", false: "not accepted yet (w)"}[r.Facts.NetworkAccepted], normal)
	}
	switch n := names.Issue(r.Name); {
	case d.issue != "":
		field("Issue", d.issue, normal)
	case n != "":
		field("Issue", strings.TrimRight(m.snap.g.ForgejoURL, "/")+"/"+ri.sp.ns.String()+"/issues/"+n, normal)
	}
	if ri.sp.stale {
		ls = append(ls, text("controller not running: requests wait until it runs", errStyle))
	}

	section("Script header")
	if d.err != nil {
		ls = append(ls, untrusted("  "+d.err.Error(), errStyle))
	}
	for _, h := range d.header {
		ls = append(ls, untrusted("  "+h, normal))
	}
	section("Reviews")
	for _, t := range ri.sp.cfg.Automatic {
		if r.Reviews[t] == "" {
			ls = append(ls, untrusted("  "+t+": pending (automatic)", dimStyle))
		}
	}
	for _, f := range slices.Sorted(maps.Keys(d.reviews)) {
		st := normal
		if strings.HasSuffix(f, ".FAILED.md") || d.reviews[f] == "Do not run" {
			st = errStyle
		}
		ls = append(ls, untrusted("  "+f+": "+d.reviews[f], st))
	}
	if r.Facts.ReviewOverridden {
		ls = append(ls, text("  review overridden (override.review.md)", boldStyle))
	}
	if a := r.Facts.Answer; a != "" {
		section("Answer")
		switch {
		case a == "new":
			ls = append(ls, text("  being scanned", dimStyle))
		case a == "DLP-FAIL" && r.Facts.DLPOverridden:
			ls = append(ls, untrusted("  DLP scan: overridden - "+d.dlp, boldStyle))
		case a == "DLP-FAIL":
			ls = append(ls, untrusted("  "+or(d.dlp, "DLP scan: failed"), errStyle))
		default:
			ls = append(ls, untrusted("  "+or(d.dlp, "DLP scan: passed"), normal))
		}
		for _, l := range d.answer {
			ls = append(ls, text("  | ", dimStyle).Untrusted(l, normal))
		}
	}
	if len(ri.reqs) > 0 {
		section("Requests")
		for _, q := range ri.reqs {
			l := untrusted("  "+q.Action+" "+q.State, normal)
			if q.Reason != "" {
				l = untrusted("  "+q.Action+" "+q.State+": "+q.Reason, errStyle)
			}
			ls = append(ls, l)
		}
	}
	ls = m.wrapAll(ls, w)
	section("Files (Enter opens one; up/down arrow selects)")
	first := len(ls)
	items := m.runItems(v)
	v.cursor = max(0, min(v.cursor, len(items)-1))
	for i, f := range r.Files {
		st := actStyle
		if i == v.cursor {
			st = selStyle
		}
		l := untrusted("  "+f.Name, st).PadTo(40, st).Text(fmt.Sprintf("%10d  %s", f.Size, f.Mod.Format(time.RFC3339)), st)
		ls = append(ls, l)
	}
	scroll(v, first+v.cursor, rows)
	if v.cursor == 0 && first+1 < rows {
		v.top = 0 // the whole head shows while it fits
	}
	return title, ls[min(v.top, len(ls)):]
}

// pagerView is a file or the event log; the Events screen shows only the lines that hold its filter. Long lines
// wrap, or with w scroll sideways; the line count, the search, g and G go by the file's lines either way.
func (m *Model) pagerView(v *view, w, rows int) (title, body []*term.Line) {
	lines := m.textOf(v)
	n := len(lines)
	v.top = max(0, min(v.top, n-1))
	t := untrusted(v.title, headStyle).Text(fmt.Sprintf(titleGap+"scroll: at line %d of %d", min(v.top+1, n), n), normal)
	if v.nowrap {
		t.Text(titleGap+"not wrapped", normal)
	} else {
		t.Text(titleGap+"wrapped", normal)
	}
	if v.kind == screenEvents && v.search != "" && !m.typing {
		t.Text(titleGap+"filter: ", normal).Untrusted(v.search, boldStyle)
	}
	title = []*term.Line{t, text(strings.Repeat(m.glyphs.Rule, w), divStyle)} // the rule sets the text apart from the title
	if v.note != "" {
		body = append(body, untrusted(v.note, errStyle))
	}
	for i := v.top; i < n && len(body) < rows; i++ {
		l := untrusted(lines[i], normal)
		if len(v.hl) == n {
			l = styledLine(v.hl[i])
		}
		if v.nowrap {
			body = append(body, l.Cut(v.left, w))
			continue
		}
		body = append(body, m.wrapped(l, w)...)
	}
	return title, body
}

// wrapped is l in rows at most w columns wide: a continuation row starts with the wrap mark, dim, and a space,
// so it can't be taken for a line of its own.
func (m *Model) wrapped(l *term.Line, w int) []*term.Line {
	if l == nil {
		return []*term.Line{nil}
	}
	rows := l.Wrap(w, w-2)
	for i := 1; i < len(rows); i++ {
		rows[i] = text(m.glyphs.Wrap+" ", dimStyle).Append(rows[i])
	}
	return rows
}

// wrapAll is every line of ls wrapped.
func (m *Model) wrapAll(ls []*term.Line, w int) []*term.Line {
	var out []*term.Line
	for _, l := range ls {
		out = append(out, m.wrapped(l, w)...)
	}
	return out
}

// logFilesView is the general log's files, those whose name holds the filter, after f on the Logs screen.
func (m *Model) logFilesView(v *view, w, rows int) (title, body []*term.Line) {
	days := m.snap.g.Plinth.Log.MaxAgeDays
	t := text("Logs", headStyle).Text(titleGap, normal).Untrusted(m.store.Work+"/logs", normal).
		Text(fmt.Sprintf(titleGap+"old: more than %d days", days), normal)
	if v.search != "" && !m.typing {
		t.Text(titleGap+"filter: ", normal).Untrusted(v.search, boldStyle)
	}
	cols := []int{28, 12, 6, 3}
	title = []*term.Line{t, row(cols, []*term.Line{text("FILE", boldStyle), text("SIZE", boldStyle), text("AGE", boldStyle), text("OLD", boldStyle)}, normal)}
	old := slices.ContainsFunc(m.logFiles, func(f logFile) bool { return f.old })
	switch {
	case old && m.snap.controller:
		body = append(body, text("<ctrl-d> Scrub logs deletes the old files.", normal))
	case old:
		body = append(body, text("<ctrl-d> Scrub logs - controller not running", dimStyle))
	}
	files := m.logRows(v)
	if len(files) == 0 {
		return title, append(body, text("No log files.", dimStyle))
	}
	v.cursor = max(0, min(v.cursor, len(files)-1))
	n := max(1, rows-len(body))
	scroll(v, v.cursor, n)
	for i := v.top; i < len(files) && i < v.top+n; i++ {
		f := files[i]
		st := actStyle
		if i == v.cursor {
			st = selStyle
		}
		mark := ""
		if f.old {
			mark = "old"
		}
		body = append(body, row(cols, []*term.Line{untrusted(f.name, st), text(fmt.Sprint(f.size), st), text(age(m.store.Now(), f.mod), st), text(mark, st)}, st).PadTo(w, st))
	}
	return title, body
}

// promptView shows the box, its lines wrapped to the width w, then the question: a box too long for rows ends in
// "… n more lines", so the question always shows.
func (m *Model) promptView(w, rows int) (title, body []*term.Line) {
	p := m.prompt
	title = []*term.Line{text("Confirm", headStyle)}
	box := m.wrapAll(p.box, w)
	room := rows - 2 // the blank line and the question
	if p.word != "" {
		room-- // the input line
	}
	if len(box) > room {
		more := fmt.Sprintf("%s %d more lines", m.glyphs.Ellipsis, len(box)-max(0, room-1))
		box = append(slices.Clip(box[:max(0, room-1)]), text(more, dimStyle))
	}
	body = append(body, box...)
	if len(box) > 0 {
		body = append(body, nil)
	}
	body = append(body, p.ask)
	if p.word != "" {
		body = append(body, text("> ", boldStyle).Untrusted(p.input, boldStyle).Text("_", normal))
	}
	return title, body
}

// helpView lists every key; when they don't fit in rows, it scrolls (helpKey) and the title says which lines show.
func (m *Model) helpView(rows int) (title, body []*term.Line) {
	defer func() {
		m.helpTop = max(0, min(m.helpTop, len(body)-rows))
		if len(body) > rows {
			title[0].Text(fmt.Sprintf(titleGap+"lines %d-%d of %d", m.helpTop+1, min(m.helpTop+rows, len(body)), len(body)), normal)
		}
		body = body[m.helpTop:]
	}()
	title = []*term.Line{text("Keys", headStyle)}
	// The keys as typed, as the header writes them.
	for _, k := range [][2]string{
		{"General (the bottom bar, every screen)", ""},
		{"<up> <down> <k> <j>", "move (lists, pager, a picker: <up> <down>)"},
		{"<pgup> <pgdn> <g> <shift-g>", "page, start, end"},
		{"<enter>", "open; in a picker: switch"},
		{"<esc>", "back (clears a filter first); closes a picker"},
		{"<:>", "Screens: Runs, Events, Logs, Image; typing narrows the list (bottom bar; not in the pager)"},
		{"<?>", "this list (bottom bar)"},
		{"<shift-e> <shift-l> <shift-i>", "Events, Logs, Image"},
		{"<q>", "back (elsewhere)"},
		{"<shift-q>", "quit (Runs)"},
		{"<ctrl-v>", "the Plinth version, on the message line"},
		{"<ctrl-z>", "suspend"},
		{"<ctrl-c>", "quit"},
		{"The screen's own keys (the header; one that can't act now says why)", ""},
		{"</>", "filter (Runs, Logs, Events) or search (pager)"},
		{"<n> <a>", "Runs: namespace, agent (a picker for each)"},
		{"<t>", "Runs: show/hide released"},
		{"<l> <c> <d> <h>", "Logs: level (or Open notices), component, date, hour (a picker for each); Esc clears the last, then back"},
		{"<f>", "Logs: the file list, and back to the lines; </> searches the lines"},
		{"<e>", "Exec (y/N)"},
		{"<o>", "Override review, or Override DLP scan: names what failed, then type yes"},
		{"<d>", "Done (y/N)"},
		{"<w>", "Accept network: names what the run can reach, then type yes"},
		{"<s>", "Save: the run's files as a zip on its issue in plinth.repo (y/N); with plinth.autosave.runs \"(runs auto-save)\""},
		{"<r>", "Report: names plinth.repo and what is posted, masked first; then y/N; \"(auto-report)\" with plinth.autosave.failures"},
		{"<v>", "Review: which review type (a digit; none to ask with one), then y/N"},
		{"<ctrl-d>", "Scrub (type yes); Logs: Scrub logs (type yes); Image: Delete"},
		{"<l> <o> <b> <u> <p> <shift-r>", "Image: log in, log out, publish, use, pull, reload"},
		{"<w>", "pager, Events: wrap long lines, or scroll them sideways"},
		{"<left> <right>", "pager, Events: scroll sideways (not wrapped)"},
	} {
		if k[1] == "" { // a group's heading
			body = append(body, text(k[0], headStyle))
			continue
		}
		body = append(body, text("  "+k[0], boldStyle).PadTo(34, normal).Text(k[1], normal))
	}
	return title, body
}

// age is a short duration since t: 45s, 12m, 3h, 5d.
func age(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func or(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
