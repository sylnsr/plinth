package tui

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"plinth/internal/agents"
	"plinth/internal/auth"
	"plinth/internal/container"
	"plinth/internal/controller"
	"plinth/internal/decide"
	"plinth/internal/events"
	"plinth/internal/names"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
	"plinth/internal/term"
)

type screen int

const (
	screenRuns screen = iota
	screenRun
	screenPager
	screenEvents
	screenLogs
	screenImage
)

// view is one screen on the stack: Esc goes back to the one below.
type view struct {
	kind   screen
	cursor int    // the selected row (Runs, Logs) or item (Run)
	top    int    // the first row shown
	left   int    // the first column shown (Pager, Events)
	run    string // runInfo.key of the run (Run, and the run whose file or events are shown)
	crumb  string // its name in the breadcrumb bar
	title  string
	text   []string // the lines of a file or of the event log (Pager, Events)
	note   string   // shown above the text: a file cut at maxFile, the chain's problem
	nowrap bool     // Pager, Events: long lines scroll sideways instead of wrapping (w)
	search string   // the pager's search; the filter of Logs and Events
	det    detail   // Run
	// logs is the Logs screen's state: the filters, the lines.
	logs *logState
	// hl is the colours of a shell script (the run's ask.sh) in the pager: one slice of spans per line of text.
	hl [][]hlSpan
}

// picker is the list under the header that : opens (the screens), or n and a (the namespaces, the agents): typing
// narrows it to the entries that hold the input, whatever the case; up and down select, Enter picks, Esc closes it.
type picker struct {
	label  string // before the input: ":", "namespace:", "agent:"
	all    []string
	input  string
	cursor int
	pick   func(string)
}

// items are the entries that hold the input.
func (p *picker) items() []string {
	var out []string
	for _, s := range p.all {
		if strings.Contains(strings.ToLower(s), strings.ToLower(p.input)) {
			out = append(out, s)
		}
	}
	return out
}

// screens are what : lists (Conversations is hidden until messages are built).
var screens = []string{"Runs", "Events", "Logs", "Image"}

// prompt is a question that must be answered before anything is written: y/N, a word to type, or a menu.
type prompt struct {
	box     []*term.Line
	ask     *term.Line
	word    string // "" for y/N
	input   string
	yes     func()
	choices map[rune]func() // a menu: the key picks; any other key cancels
}

// Model is the TUI's state. Key updates it, calling the store for the I/O; View draws it.
type Model struct {
	store   *Store
	version string
	snap    *snapshot
	stack   []*view
	msg     *term.Line
	prompt  *prompt
	pick    *picker
	glyphs  term.Glyphs // the locale's: Unicode or ASCII
	help    bool
	helpTop int // the first line of Help that shows, when it doesn't fit
	quit    bool
	suspend bool
	w, h    int // the size of the last frame

	nsFilter     string // a namespace, or "" for all
	agentFilter  string
	runFilter    string
	typing       bool // keys go to the filter or the search
	showReleased bool
	logFiles     []logFile

	img   *imageScreen // while the Image screen is open
	async chan func()  // what background work (image list) hands back, for the loop to apply (Async)
}

// New loads the first snapshot; an error when the global config doesn't load (as every command).
func New(store *Store, version, ns string) (*Model, error) {
	if ns != "" {
		if _, err := names.ParseNamespace(ns); err != nil {
			return nil, err
		}
	}
	snap, err := store.load()
	if err != nil {
		return nil, err
	}
	m := &Model{store: store, version: version, snap: snap, stack: []*view{{kind: screenRuns, crumb: "runs"}}, nsFilter: ns, w: 80, h: 24,
		async: make(chan func(), 8)}
	m.glyphs = term.GlyphsFor(term.UTF8(func(k string) string {
		if store.Getenv == nil {
			return ""
		}
		v, _ := store.Getenv(k)
		return v
	}))
	return m, nil
}

// Async delivers the results of background work: the loop calls each one, which updates the model.
func (m *Model) Async() <-chan func() { return m.async }

// Quit reports whether the user quit (Q on Runs, Ctrl-C).
func (m *Model) Quit() bool { return m.quit }

// Suspended reports a Ctrl-Z and clears it.
func (m *Model) Suspended() bool {
	s := m.suspend
	m.suspend = false
	return s
}

// RefreshSeconds is plinth.tui.refresh-seconds.
func (m *Model) RefreshSeconds() int { return m.snap.g.Plinth.TUI.RefreshSeconds }

// Reload reads everything again: at every refresh and after every action. A global config that no longer loads keeps
// the last snapshot and says so.
func (m *Model) Reload() {
	snap, err := m.store.load()
	if err != nil {
		m.say(errStyle, "reload: "+err.Error())
		return
	}
	m.snap = snap
	for _, v := range m.stack {
		if v.kind == screenRun {
			if ri := m.find(v.run); ri != nil {
				v.det = m.store.detail(ri)
			}
		}
	}
	if m.top().kind == screenRun && m.find(m.top().run) == nil {
		m.pop()
		m.say(normal, "the run is gone")
	}
	if m.top().kind == screenLogs {
		m.loadLogs()
	}
}

func (m *Model) top() *view { return m.stack[len(m.stack)-1] }

func (m *Model) push(v *view) { m.stack = append(m.stack, v) }

// pop goes back to the screen below; leaving the Image screen drops its state.
func (m *Model) pop() {
	if len(m.stack) > 1 {
		if m.top().kind == screenImage {
			m.img = nil
		}
		m.stack = m.stack[:len(m.stack)-1]
	}
}

// say sets the message line. Everything but an error (errStyle, red) shows in msgColor, so the line stands out.
func (m *Model) say(st term.Style, s string) {
	if st != errStyle {
		st.FG = msgColor
	}
	m.msg = (&term.Line{}).Untrusted(s, st)
}

// find is the run with key k in the snapshot; nil when it is gone.
func (m *Model) find(k string) *runInfo {
	for _, sp := range m.snap.spaces {
		for _, ri := range sp.runs {
			if ri.key() == k {
				return ri
			}
		}
	}
	return nil
}

// rows are the runs the Runs screen lists: filtered, not released first, newest first.
func (m *Model) rows() []*runInfo {
	var out []*runInfo
	for _, sp := range m.snap.spaces {
		if m.nsFilter != "" && sp.ns.String() != m.nsFilter {
			continue
		}
		for _, ri := range sp.runs {
			r := ri.r
			switch {
			case m.agentFilter != "" && r.Agent != m.agentFilter,
				m.runFilter != "" && !strings.Contains(r.Name, m.runFilter),
				!m.showReleased && r.Facts.Answer == "released":
				continue
			}
			out = append(out, ri)
		}
	}
	slices.SortStableFunc(out, func(a, b *runInfo) int {
		ar, br := a.r.Facts.Answer == "released", b.r.Facts.Answer == "released"
		if ar != br {
			if ar {
				return 1
			}
			return -1
		}
		return b.r.Newest.Compare(a.r.Newest)
	})
	return out
}

// selected is the run the Runs screen's cursor is on, or the Run screen's run.
func (m *Model) selected() *runInfo {
	v := m.top()
	switch v.kind {
	case screenRun:
		return m.find(v.run)
	case screenRuns:
		rows := m.rows()
		if v.cursor >= 0 && v.cursor < len(rows) {
			return rows[v.cursor]
		}
	}
	return nil
}

// Key handles one key press.
func (m *Model) Key(k term.Key) {
	switch {
	case k.Code == term.KeyCtrl && k.Rune == 'c':
		m.quit = true
		return
	case m.w < minWidth || m.h < minHeight: // the screen shows only the size note: no key acts unseen
		if k.Code == term.KeyCtrl && k.Rune == 'z' {
			m.suspend = true
		}
		return
	case m.prompt != nil:
		m.promptKey(k)
		return
	case m.help:
		if !m.helpKey(k) {
			m.help = false
		}
		return
	case m.typing:
		m.typeKey(k)
		return
	case m.pick != nil:
		m.pickKey(k)
		return
	case k.Code == term.KeyCtrl && k.Rune == 'z':
		m.suspend = true
		return
	case k.Code == term.KeyRune && k.Rune == '?':
		m.help, m.helpTop = true, 0
		return
	case k.Code == term.KeyCtrl && k.Rune == 'v':
		m.say(boldStyle, "Plinth "+m.version)
		return
	}
	m.msg = nil
	v := m.top()
	if m.moveKey(v, k) {
		return
	}
	switch v.kind {
	case screenRuns:
		m.runsKey(v, k)
	case screenRun:
		m.runKey(v, k)
	case screenPager, screenEvents:
		m.pagerKey(v, k)
	case screenLogs:
		m.logsKey(v, k)
	case screenImage:
		m.imageKey(v, k)
	default:
		m.backKey(k)
	}
}

// rowsOf is the number of rows the cursor moves over on v.
func (m *Model) rowsOf(v *view) int {
	switch v.kind {
	case screenRuns:
		return len(m.rows())
	case screenRun:
		return len(m.runItems(v))
	case screenLogs:
		if st := v.logsState(); !st.files {
			return len(st.lines)
		}
		return len(m.logRows(v))
	case screenImage:
		return len(m.imageRows())
	case screenPager, screenEvents:
		return len(m.textOf(v))
	}
	return 0
}

// textOf is the lines the pager shows: the file's, or the events that hold the Events screen's filter.
func (m *Model) textOf(v *view) []string {
	if v.kind != screenEvents || v.search == "" {
		return v.text
	}
	var out []string
	for _, l := range v.text {
		if strings.Contains(l, v.search) {
			out = append(out, l)
		}
	}
	return out
}

// logRows are the log files the Logs screen lists: those whose name holds its filter.
func (m *Model) logRows(v *view) []logFile {
	var out []logFile
	for _, f := range m.logFiles {
		if strings.Contains(f.name, v.search) {
			out = append(out, f)
		}
	}
	return out
}

// moveKey moves the cursor (or, in the pager, the view) and reports whether k was a move.
func (m *Model) moveKey(v *view, k term.Key) bool {
	n := m.rowsOf(v)
	page := max(1, m.h-headerRows-5)
	pos := &v.cursor
	if v.kind == screenPager || v.kind == screenEvents {
		pos = &v.top
		n = max(0, n-page)
	}
	switch {
	case k.Code == term.KeyUp || (k.Code == term.KeyRune && k.Rune == 'k'):
		*pos--
	case k.Code == term.KeyDown || (k.Code == term.KeyRune && k.Rune == 'j'):
		*pos++
	case k.Code == term.KeyPgUp:
		*pos -= page
	case k.Code == term.KeyPgDn:
		*pos += page
	case k.Code == term.KeyHome || (k.Code == term.KeyRune && k.Rune == 'g'):
		*pos = 0
	case k.Code == term.KeyEnd || (k.Code == term.KeyRune && k.Rune == 'G'):
		*pos = n
	case (v.kind == screenPager || v.kind == screenEvents) && k.Code == term.KeyLeft:
		v.left = max(0, v.left-8)
		return true
	case (v.kind == screenPager || v.kind == screenEvents) && k.Code == term.KeyRight:
		v.left += 8
		return true
	default:
		return false
	}
	if v.kind != screenPager && v.kind != screenEvents {
		n--
	}
	*pos = max(0, min(*pos, n))
	return true
}

func (m *Model) backKey(k term.Key) {
	if k.Code == term.KeyEsc || (k.Code == term.KeyRune && k.Rune == 'q') {
		m.pop()
	}
}

// screenKey is how every list screen reaches the others: : opens the screen picker, E L I are its shortcuts.
func (m *Model) screenKey(k term.Key) bool {
	if k.Code != term.KeyRune {
		return false
	}
	switch k.Rune {
	case ':':
		m.pick = &picker{label: ":", all: screens, pick: m.switchTo}
	case 'E':
		m.switchTo("Events")
	case 'L':
		m.switchTo("Logs")
	case 'I':
		m.switchTo("Image")
	default:
		return false
	}
	return true
}

// switchTo opens one of the screens: Runs goes back to the start screen; another opens over Runs, or over the Run
// screen while one is open, for its run (Events: the run's events; Image: its namespace's). The screens above them
// close first, so going from screen to screen doesn't pile them up.
func (m *Model) switchTo(name string) {
	for len(m.stack) > 1 && m.top().kind != screenRun {
		m.pop()
	}
	if name == "Runs" {
		m.stack = m.stack[:1]
		return
	}
	var ri *runInfo
	if m.top().kind == screenRun {
		ri = m.find(m.top().run)
	}
	switch name {
	case "Events":
		m.openEvents(ri)
	case "Logs":
		v := &view{kind: screenLogs, crumb: "logs"}
		if ri != nil { // the Run screen's L: the lines of that run
			q := &v.logsState().q
			q.ns, q.agent, q.run = ri.sp.ns.String(), ri.r.Agent, ri.r.Name
			q.set("run")
		}
		m.push(v)
		m.loadLogs()
	case "Image":
		m.openImage(ri)
	}
}

// pickKey is a key while a picker is open.
func (m *Model) pickKey(k term.Key) {
	p := m.pick
	items := p.items()
	switch k.Code {
	case term.KeyEsc:
		m.pick = nil
	case term.KeyEnter:
		if p.cursor < len(items) {
			m.pick = nil
			p.pick(items[p.cursor])
		}
	case term.KeyUp:
		p.cursor = max(0, p.cursor-1)
	case term.KeyDown:
		p.cursor = max(0, min(p.cursor+1, len(items)-1))
	case term.KeyBackspace:
		if r := []rune(p.input); len(r) > 0 {
			p.input, p.cursor = string(r[:len(r)-1]), 0
		}
	case term.KeyRune:
		if len(p.input) < 32 {
			p.input, p.cursor = p.input+string(k.Rune), 0
		}
	}
}

// filterPicker opens the picker of n or a: all, then each value; picking one sets the filter.
func (m *Model) filterPicker(label string, values []string, set func(string)) {
	m.pick = &picker{label: label, all: append([]string{"all"}, values...), pick: func(s string) {
		if s == "all" {
			s = ""
		}
		set(s)
		m.stack[0].cursor = 0
	}}
}

func (m *Model) runsKey(v *view, k term.Key) {
	if m.screenKey(k) {
		return
	}
	ri := m.selected()
	switch {
	case k.Code == term.KeyEnter && ri != nil:
		m.push(&view{kind: screenRun, crumb: ri.r.Name, run: ri.key(), det: m.store.detail(ri)})
	case k.Code == term.KeyEsc:
		m.runFilter = ""
	// Only Q quits, like the bar's Quit: q goes back on the other screens, so a q pressed once too often on the way
	// back must not end the TUI.
	case k.Code == term.KeyRune && k.Rune == 'Q':
		m.quit = true
	case k.Code == term.KeyRune && k.Rune == '/':
		m.typing, m.runFilter = true, ""
	case k.Code == term.KeyRune && k.Rune == 'n':
		m.filterPicker("namespace:", m.namespaces(), func(s string) { m.nsFilter, m.agentFilter = s, "" })
	case k.Code == term.KeyRune && k.Rune == 'a':
		m.filterPicker("agent:", m.agents(), func(s string) { m.agentFilter = s })
	case k.Code == term.KeyRune && k.Rune == 't':
		m.showReleased = !m.showReleased
		v.cursor = 0
	default:
		m.actionKey(ri, k)
	}
}

func (m *Model) runKey(v *view, k term.Key) {
	ri := m.find(v.run)
	if ri == nil {
		m.pop()
		return
	}
	if m.screenKey(k) {
		return
	}
	switch {
	case k.Code == term.KeyEsc || (k.Code == term.KeyRune && k.Rune == 'q'):
		m.pop()
	case k.Code == term.KeyEnter:
		items := m.runItems(v)
		if v.cursor < len(items) {
			m.openFile(ri, items[v.cursor])
		}
	default:
		m.actionKey(ri, k)
	}
}

// pagerKey is a key in the pager (/ searches) or on the Events screen (/ filters; the screens' keys); w switches between
// wrapping long lines and scrolling them sideways.
func (m *Model) pagerKey(v *view, k term.Key) {
	if v.kind == screenEvents && m.screenKey(k) {
		return
	}
	switch {
	case k.Code == term.KeyEsc && v.kind == screenEvents && v.search != "":
		v.search = ""
	case k.Code == term.KeyEsc || (k.Code == term.KeyRune && k.Rune == 'q'):
		m.pop()
	case k.Code == term.KeyRune && k.Rune == '/':
		m.typing = true
		v.search = ""
	case k.Code == term.KeyRune && k.Rune == 'w':
		v.nowrap, v.left = !v.nowrap, 0
	}
}

// typed is what the filter or the search being typed edits: the run filter on Runs, the view's search (the pager's
// search, the filter of Logs and Events) elsewhere.
func (m *Model) typed() *string {
	if v := m.top(); v.kind != screenRuns {
		return &v.search
	}
	return &m.runFilter
}

// typeKey edits the run filter (Runs), the filter (Logs, Events) or the search (Pager): Enter ends it, Esc clears it.
func (m *Model) typeKey(k term.Key) {
	v := m.top()
	s := m.typed()
	switch k.Code {
	case term.KeyEnter:
		m.typing = false
		if v.kind == screenPager {
			m.searchNext(v)
		}
	case term.KeyEsc:
		m.typing, *s = false, ""
	case term.KeyBackspace:
		if r := []rune(*s); len(r) > 0 {
			*s = string(r[:len(r)-1])
		}
	case term.KeyRune:
		if len(*s) < 64 {
			*s += string(k.Rune)
		}
	}
	switch v.kind {
	case screenRuns, screenLogs:
		v.cursor = 0
	case screenEvents:
		v.top = 0
	}
}

// searchNext moves the pager to the next line, after the first one shown, that holds the search; it wraps around.
func (m *Model) searchNext(v *view) {
	if v.search == "" {
		return
	}
	for i := 1; i <= len(v.text); i++ {
		j := (v.top + i) % len(v.text)
		if strings.Contains(v.text[j], v.search) {
			v.top = j
			return
		}
	}
	m.say(normal, "not found")
}

func (m *Model) openEvents(ri *runInfo) {
	sp := m.space()
	run, title := "", ""
	if ri != nil {
		sp, run, title = ri.sp, ri.r.Name, "Events of "+ri.r.Agent+"/"+ri.r.Name+" in "
	}
	if sp == nil {
		m.say(normal, "choose a namespace first (n)")
		return
	}
	lines, err := m.store.events(sp.ns, run)
	v := &view{kind: screenEvents, crumb: "events", title: title + sp.ns.String(), text: lines}
	var p *events.Problem
	switch {
	case errors.As(err, &p):
		v.note = "the chain is broken, nothing after it is shown: " + err.Error()
	case err != nil:
		v.note = err.Error()
	}
	m.push(v)
}

// space is the namespace the filter names; with no filter, the only one there is.
func (m *Model) space() *space {
	for _, sp := range m.snap.spaces {
		if sp.ns.String() == m.nsFilter || (m.nsFilter == "" && len(m.snap.spaces) == 1) {
			return sp
		}
	}
	return nil
}

// item is a selectable line of the Run screen: a file the pager opens.
type item struct {
	file string
}

func (m *Model) runItems(v *view) []item {
	ri := m.find(v.run)
	if ri == nil {
		return nil
	}
	var out []item
	for _, f := range ri.r.Files {
		out = append(out, item{file: f.Name})
	}
	return out
}

func (m *Model) openFile(ri *runInfo, it item) {
	b, cut, err := m.store.read(ri, it.file, maxFile)
	if err != nil {
		m.say(errStyle, it.file+": "+err.Error())
		return
	}
	m.openText(it.file+" of "+ri.r.Agent+"/"+ri.r.Name+" in "+ri.sp.ns.String(), it.file, b, cut)
	v := m.top()
	v.hl = highlight(it.file, v.text)
}

// openText opens b in the pager; crumb is the file's name in the breadcrumb bar.
func (m *Model) openText(title, crumb string, b []byte, cut int64) {
	v := &view{kind: screenPager, crumb: crumb, title: title, text: strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")}
	if cut > 0 {
		v.note = fmt.Sprintf("The file is %d bytes; this is its first %d MiB.", cut, maxFile>>20)
	}
	m.push(v)
}

func (m *Model) loadLogs() {
	files, err := m.store.logs(m.snap.g.Plinth.Log.MaxAgeDays)
	if err != nil {
		m.say(errStyle, err.Error())
	}
	m.logFiles = files
	if v := m.top(); v.kind == screenLogs {
		v.logsState().sig = "" // the lines are read again
	}
}

func (m *Model) namespaces() []string {
	var out []string
	for _, sp := range m.snap.spaces {
		out = append(out, sp.ns.String())
	}
	return out
}

// agents are the agent filter's choices in the filtered namespaces: the registered agents (agents.List), whether or not
// they have runs yet, and every agent a run names (one removed since keeps its runs). A list that can't be read gives
// the agents of the runs only.
func (m *Model) agents() []string {
	var out []string
	for _, sp := range m.snap.spaces {
		if m.nsFilter != "" && sp.ns.String() != m.nsFilter {
			continue
		}
		registered, _ := agents.List(m.store.Work, sp.ns)
		for _, a := range registered {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
		for _, ri := range sp.runs {
			if !slices.Contains(out, ri.r.Agent) {
				out = append(out, ri.r.Agent)
			}
		}
	}
	slices.Sort(out)
	return out
}

// keyName is k as the header writes it: "e", "shift-r", "ctrl-d"; "" for a key that is neither a rune nor Ctrl.
func keyName(k term.Key) string {
	switch {
	case k.Code == term.KeyRune && unicode.IsUpper(k.Rune):
		return "shift-" + string(unicode.ToLower(k.Rune))
	case k.Code == term.KeyRune:
		return string(k.Rune)
	case k.Code == term.KeyCtrl:
		return "ctrl-" + string(unicode.ToLower(k.Rune))
	}
	return ""
}

// act is one action key on a run: the web UI's button.
type act struct {
	key    string // as the header shows it, keyName's notation
	label  string
	action string // the request
	note   string // why it can't run now: said when pressed, writes nothing
	hint   string // the header's word instead of the label's first one (plinth.autosave says so there)
}

// actions are the run's action keys that apply, with the web UI's rules for showing and disabling its buttons
// (plinth-web-ui.README.md, Buttons; plinth-tui.README.md, Actions).
func (m *Model) actions(ri *runInfo) []act {
	r := ri.r
	if r.Broken != nil || !r.Facts.Exists {
		return nil
	}
	f := r.Facts
	queued := !f.Ran && !f.Running && f.Answer == ""
	var out []act
	if !f.Running && f.Answer == "" { // only an answer says a run is done: one without runs again
		a := act{key: "e", label: "Exec", action: decide.Exec}
		switch {
		case len(f.ReviewsPending) > 0:
			a.note = "Waiting for reviews"
		case len(f.ReviewsFailed) > 0 && !f.ReviewOverridden:
			a.note = "Review failed"
		case ri.mode.Privileged && m.store.Images == nil:
			a.note = "privileged: runs in your terminal (plinthctl exec), which this TUI can't hand over"
		case ri.mode.Network && !f.NetworkAccepted:
			a.note = "Network run: accept it first (w)"
		}
		out = append(out, a)
	}
	if queued && ri.mode.Network && !f.NetworkAccepted && !ri.mode.Privileged {
		out = append(out, act{key: "w", label: "Accept network", action: decide.AcceptNetwork})
	}
	switch {
	case queued && len(f.ReviewsFailed) > 0 && !f.ReviewOverridden:
		out = append(out, act{key: "o", label: "Override review", action: decide.OverrideReview})
	case f.Answer == "DLP-FAIL" && !f.DLPOverridden:
		out = append(out, act{key: "o", label: "Override DLP scan", action: decide.OverrideDLP})
	}
	if f.Answer != "" && f.Answer != "released" {
		a := act{key: "d", label: "Done", action: decide.Done}
		switch {
		case f.Answer == "new" && ri.waiting.Since != "": // the last event says which
			a.note = "Waiting for the model (unavailable since " + ri.waiting.Since + ")"
		case f.Answer == "new" && ri.waiting.Timeouts > 0:
			a.note = "Waiting for the model: it didn't reply in time"
		case f.Answer == "new":
			a.note = "Waiting for the DLP scan"
		case f.Answer == "DLP-FAIL" && !f.DLPOverridden:
			a.note = "Successful DLP scan needed"
		}
		out = append(out, a)
	}
	if f.Answer != "" {
		a := act{key: "s", label: "Save", action: decide.Save}
		auto, st := m.snap.g.Plinth.Autosave.Runs, ri.reqState[decide.Save]
		switch {
		case auto && st == "failed": // the controller's own save failed: the key retries it
			a.hint = "Save (auto-fail)"
		case auto && (st == "pending" || st == "finished"): // Plinth saves this run by itself
			a.hint, a.note = "(runs auto-save)", "runs auto-save: Plinth saves a run by itself right after Done"
		case m.snap.g.Plinth.Repo == "":
			a.note = "No repo configured (plinth.repo)"
		case f.Answer == "new":
			a.note = "Waiting for the DLP scan"
		case f.Answer == "DLP-FAIL" && !f.DLPOverridden:
			a.note = "Successful DLP scan needed (or Override DLP scan first)"
		}
		out = append(out, a)
	}
	if (len(f.ReviewsFailed) > 0 || f.Answer == "DLP-FAIL") && !f.Reported {
		a := act{key: "r", label: "Report", action: decide.Report}
		auto, st := m.snap.g.Plinth.Autosave.Failures, ri.reqState[decide.Report]
		switch {
		case auto && st == "failed": // the controller's own report failed: the key retries it
			a.hint = "Report (auto-fail)"
		case auto && st == "pending":
			a.hint, a.note = "(auto-report)", "failures auto-report: Plinth reports a run's first failed check by itself"
		case m.snap.g.Plinth.Repo == "":
			a.note = "No repo configured (plinth.repo)"
		case m.snap.g.Ollama.Host == "":
			a.note = "Not available without Ollama: everything Report posts is masked first"
		}
		out = append(out, a)
	}
	if (queued || f.Answer != "") && len(m.snap.g.Reviewers.Type) > 0 {
		out = append(out, act{key: "v", label: "Review", action: decide.Review})
	}
	a := act{key: "ctrl-d", label: "Scrub", action: decide.Scrub}
	if f.Running {
		a.note = "Running"
	}
	return append(out, a)
}

// actionKey carries out an action key on ri: one that can't act now says why; an enabled one asks decide (a refusal
// says its rule and writes nothing), then for confirmation, then queues the request.
func (m *Model) actionKey(ri *runInfo, k term.Key) {
	name := keyName(k)
	if !slices.Contains([]string{"e", "o", "w", "d", "s", "r", "v", "ctrl-d"}, name) {
		return
	}
	if ri == nil {
		m.say(normal, "no run selected")
		return
	}
	i := slices.IndexFunc(m.actions(ri), func(a act) bool { return a.key == name })
	if i < 0 {
		m.say(normal, name+": does not apply to "+ri.r.Name)
		return
	}
	a := m.actions(ri)[i]
	if a.note != "" {
		m.say(dimStyle, a.label+": "+a.note)
		return
	}
	if a.action == decide.Review {
		m.reviewKey(ri) // which type first: the request is review.<type>
		return
	}
	decideRun := request.Decide
	if a.action == decide.Exec && ri.mode.Privileged {
		decideRun = request.DecideTerminal // it runs in this terminal
	}
	d, r, err := decideRun(m.store.Work, ri.sp.ns, m.snap.g, ri.sp.cfg, ri.r.Agent, ri.r.Name, a.action, liveOf(m.store, ri))
	if err != nil {
		m.say(errStyle, err.Error())
		return
	}
	if d.Outcome != decide.Allow {
		m.say(errStyle, fmt.Sprintf("%s of %s: %s by rule %s: %s", a.action, ri.r.Name, d.Outcome, d.Rule, d.Reason))
		return
	}
	queue := func() { m.queue(ri, a.action) }
	run := (&term.Line{}).Untrusted(r.Name, boldStyle)
	if a.action == decide.Exec && ri.mode.Privileged {
		m.terminalExec(ri) // plinthctl exec asks its own question
		return
	}
	switch a.action {
	case decide.Exec:
		q := "?"
		if r.Facts.Ran {
			q = " again? Its last exec ended without an answer."
		}
		m.ask(nil, (&term.Line{}).Text("Exec ", normal).Append(run).Untrusted(" in "+ri.sp.ns.String()+q+" (y/N)", normal), "", queue)
	case decide.Done:
		m.ask(nil, (&term.Line{}).Text("Release the answer of ", normal).Append(run).Untrusted(" to "+r.Agent+"? (y/N)", normal), "", queue)
	case decide.Save:
		q := fmt.Sprintf(" (%d files, as %s.zip) on the run's issue in %s? (y/N)", len(r.SaveFiles()), r.Name, m.snap.g.Plinth.Repo)
		m.ask(nil, (&term.Line{}).Text("Save ", normal).Append(run).Untrusted(q, normal), "", queue)
	case decide.Report:
		var box []*term.Line
		for _, l := range strings.Split(strings.TrimSuffix(runner.ReportBox(m.snap.g.Plinth.Repo, r), "\n"), "\n") {
			box = append(box, (&term.Line{}).Untrusted(l, normal))
		}
		m.ask(box, (&term.Line{}).Text("Report ", normal).Append(run).Text("? (y/N)", normal), "", queue)
	case decide.OverrideReview:
		box := []*term.Line{(&term.Line{}).Text("Failed review(s) of ", normal).Append(run).Untrusted(" (agent "+r.Agent+"):", normal)}
		for _, t := range r.Facts.ReviewsFailed {
			file := "review." + t + ".FAILED.md"
			if !slices.ContainsFunc(r.Files, func(f runstate.File) bool { return f.Name == file }) {
				file = "review." + t + ".md" // a Do not run verdict
			}
			b, _, err := m.store.read(ri, file, 1<<20)
			why := summary(b)
			if err != nil {
				why = err.Error()
			}
			box = append(box, (&term.Line{}).Untrusted("  "+t+"  "+file+": "+why, normal))
		}
		box = append(box, (&term.Line{}).Text("Overriding lets Exec run the script without a passing review. The event log records it.", normal))
		m.ask(box, (&term.Line{}).Text("Override the review of ", normal).Append(run).Text("? Type yes:", normal), "yes", queue)
	case decide.OverrideDLP:
		why := strings.TrimPrefix(m.store.dlp(ri), "DLP scan: FAILED - ")
		if why == "" {
			why = "no reason in the answer's status.dlp"
		}
		box := []*term.Line{
			(&term.Line{}).Text("The DLP scan of ", normal).Append(run).Untrusted("'s answer failed: "+why, normal),
			(&term.Line{}).Text("Overriding lets Done release the answer to the agent as it is (with the fixed-pattern masks).", normal),
			(&term.Line{}).Text("Read it first: Enter on the answer opens it. The event log records the override.", normal),
		}
		m.ask(box, (&term.Line{}).Text("Override the DLP scan of ", normal).Append(run).Text("? Type yes:", normal), "yes", queue)
	case decide.AcceptNetwork:
		var box []*term.Line
		for _, l := range strings.Split(strings.TrimSuffix(container.NetworkBox(r.Name, r.Agent), "\n"), "\n") {
			box = append(box, (&term.Line{}).Untrusted(l, normal))
		}
		m.ask(box, (&term.Line{}).Text("Let ", normal).Append(run).Text(" run with the network? Type yes:", normal), "yes", queue)
	case decide.Scrub:
		box := []*term.Line{
			(&term.Line{}).Text("Scrub ", normal).Append(run).Untrusted(fmt.Sprintf(" (agent %s) deletes, for good: its run directory (%d files),", r.Agent, len(r.Files)), normal),
			(&term.Line{}).Text("the DLP scan's untouched original and its request files. The event log keeps its lines.", normal),
		}
		m.ask(box, (&term.Line{}).Text("Are you sure? Scrub ", normal).Append(run).Text("? Type yes:", normal), "yes", queue)
	default:
		m.say(errStyle, a.action+": no confirmation defined")
	}
}

// reviewKey is v: it asks which review type (one digit key per type, sorted, at most nine; none to ask with one
// type), then y/N, then asks decide about review.<type> (a refusal says its rule and writes nothing) and queues it, as
// plinthctl review -n does. The controller reviews the script, or the answer once there is one.
func (m *Model) reviewKey(ri *runInfo) {
	types := slices.Sorted(maps.Keys(m.snap.g.Reviewers.Type))
	run := (&term.Line{}).Untrusted(ri.r.Name, boldStyle)
	what := "script"
	if ri.r.Facts.Answer != "" {
		what = "answer"
	}
	review := func(t string) {
		action := decide.Review + "." + t
		d, _, err := request.Decide(m.store.Work, ri.sp.ns, m.snap.g, ri.sp.cfg, ri.r.Agent, ri.r.Name, action, liveOf(m.store, ri))
		switch {
		case err != nil:
			m.say(errStyle, err.Error())
		case d.Outcome != decide.Allow:
			m.say(errStyle, fmt.Sprintf("%s of %s: %s by rule %s: %s", action, ri.r.Name, d.Outcome, d.Rule, d.Reason))
		default:
			m.queue(ri, action)
		}
	}
	q := func() *term.Line { return (&term.Line{}).Text("Review the "+what+" of ", normal).Append(run) }
	confirm := func(t string) { m.ask(nil, q().Untrusted(" with "+t+"? (y/N)", normal), "", func() { review(t) }) }
	if len(types) == 1 {
		confirm(types[0])
		return
	}
	types = types[:min(len(types), 9)]
	var box []*term.Line
	choices := map[rune]func(){}
	for i, t := range types {
		box = append(box, (&term.Line{}).Untrusted(fmt.Sprintf("  %d  %s", i+1, t), normal))
		choices[rune('1'+i)] = func() { confirm(t) }
	}
	m.choose(box, q().Text(fmt.Sprintf(" with which type? (1-%d; any other key: nothing)", len(types)), normal), choices)
}

// liveOf is controller.Live for the run's namespace.
func liveOf(s *Store, ri *runInfo) runstate.Live { return controller.Live(s.Work, ri.sp.ns) }

func (m *Model) ask(box []*term.Line, q *term.Line, word string, yes func()) {
	m.prompt = &prompt{box: box, ask: q, word: word, yes: yes}
}

// choose asks for one key of choices.
func (m *Model) choose(box []*term.Line, q *term.Line, choices map[rune]func()) {
	m.prompt = &prompt{box: box, ask: q, choices: choices}
}

func (m *Model) promptKey(k term.Key) {
	p := m.prompt
	if p.choices != nil {
		m.prompt = nil
		if f := p.choices[k.Rune]; k.Code == term.KeyRune && f != nil {
			f()
			return
		}
		m.say(normal, "Nothing done.")
		return
	}
	if p.word == "" { // y/N: anything but y is no
		m.prompt = nil
		if k.Code == term.KeyRune && (k.Rune == 'y' || k.Rune == 'Y') {
			p.yes()
			return
		}
		m.say(normal, "Nothing queued.")
		return
	}
	switch k.Code {
	case term.KeyEsc:
		m.prompt = nil
		m.say(normal, "Nothing queued.")
	case term.KeyEnter:
		m.prompt = nil
		if p.input == p.word {
			p.yes()
			return
		}
		m.say(normal, "Nothing queued.")
	case term.KeyBackspace:
		if r := []rune(p.input); len(r) > 0 {
			p.input = string(r[:len(r)-1])
		}
	case term.KeyRune:
		if len(p.input) < 32 {
			p.input += string(k.Rune)
		}
	}
}

// terminalExec runs plinthctl exec of a privileged run in the terminal, handed over: it says what the script can
// do and asks for privileged itself, the controller records the start over its socket, and the script runs as the user.
// Then the screen reads everything again, and the message line shows the exit status.
func (m *Model) terminalExec(ri *runInfo) {
	what := "plinthctl exec " + ri.r.Name
	code, err := m.store.Images.Run([]string{"exec", "-n", ri.sp.ns.String(), "--agent", ri.r.Agent, "--work", m.store.Work, ri.r.Name})
	lerr := m.store.log("ran a command", "command", what, "ns", ri.sp.ns, "exit", code)
	m.Reload()
	switch {
	case err != nil && code < 0:
		m.say(errStyle, what+": "+err.Error())
	case code != 0:
		m.say(errStyle, fmt.Sprintf("%s: exit %d", what, code))
	case lerr != nil:
		m.say(errStyle, what+": done. Not in the general log: "+lerr.Error())
	default:
		m.say(normal, what+": done.")
	}
}

// queue writes the request and says what came of it; the controller decides again when it carries it out.
func (m *Model) queue(ri *runInfo, action string) {
	r := ri.r
	err := request.Queue(m.store.Work, ri.sp.ns, r.Agent, r.Name, action)
	switch {
	case errors.Is(err, request.ErrQueued):
		m.say(normal, action+" of "+r.Name+": already queued.")
	case errors.Is(err, request.ErrInProgress):
		m.say(normal, action+" of "+r.Name+": in progress.")
	case err != nil:
		m.say(errStyle, err.Error())
	default:
		lerr := m.store.log("queued a request", "action", action, "ns", ri.sp.ns, "agent", r.Agent, "run", r.Name)
		s := "Queued " + action + " of " + r.Name + "."
		if action == decide.Done {
			s += ` Tell the agent "` + r.Name + ` done".`
		}
		if !m.snap.controller {
			s += " The controller is not running: it carries this out once it runs (plinthctl controller start)."
		}
		if lerr != nil {
			m.say(errStyle, s+" Not in the general log: "+lerr.Error())
			break
		}
		m.say(normal, s)
	}
	m.Reload()
}

// scrubLogs is Scrub logs: it names the old files and asks for yes, then writes <work>/requests/logs-scrub.queued.
func (m *Model) scrubLogs() {
	var old []string
	for _, f := range m.logFiles {
		if f.old {
			old = append(old, f.name)
		}
	}
	switch {
	case len(old) == 0:
		m.say(normal, fmt.Sprintf("No log file is older than %d days.", m.snap.g.Plinth.Log.MaxAgeDays))
		return
	case !m.snap.controller:
		m.say(dimStyle, "Scrub logs: controller not running")
		return
	}
	if d := decide.Decide(decide.Request{Caller: auth.HostUser(), Action: decide.LogsScrub}); d.Outcome != decide.Allow {
		m.say(errStyle, fmt.Sprintf("logs-scrub: %s by rule %s: %s", d.Outcome, d.Rule, d.Reason))
		return
	}
	box := []*term.Line{(&term.Line{}).Text(fmt.Sprintf("Log files older than %d days:", m.snap.g.Plinth.Log.MaxAgeDays), normal)}
	for _, n := range old {
		box = append(box, (&term.Line{}).Text("  ", normal).Untrusted(n, normal))
	}
	m.ask(box, (&term.Line{}).Text(fmt.Sprintf("Are you sure? Delete these %d file(s)? Type yes:", len(old)), normal), "yes", func() {
		already, err := m.store.queueLogsScrub()
		switch {
		case err != nil:
			m.say(errStyle, err.Error())
		case already:
			m.say(normal, "Scrub logs: already queued.")
		default:
			if err := m.store.log("queued a request", "action", "logs-scrub", "files", len(old),
				"older_than_days", m.snap.g.Plinth.Log.MaxAgeDays); err != nil {
				m.say(errStyle, "Queued Scrub logs. Not in the general log: "+err.Error())
				break
			}
			m.say(normal, "Queued Scrub logs.")
		}
		m.Reload()
	})
}

// helpKey scrolls Help and reports whether k was a move; helpView keeps helpTop in range.
func (m *Model) helpKey(k term.Key) bool {
	page := max(1, m.h-headerRows-4)
	switch {
	case k.Code == term.KeyUp || (k.Code == term.KeyRune && k.Rune == 'k'):
		m.helpTop--
	case k.Code == term.KeyDown || (k.Code == term.KeyRune && k.Rune == 'j'):
		m.helpTop++
	case k.Code == term.KeyPgUp:
		m.helpTop -= page
	case k.Code == term.KeyPgDn:
		m.helpTop += page
	case k.Code == term.KeyHome || (k.Code == term.KeyRune && k.Rune == 'g'):
		m.helpTop = 0
	case k.Code == term.KeyEnd || (k.Code == term.KeyRune && k.Rune == 'G'):
		m.helpTop = 1 << 20
	default:
		return false
	}
	m.helpTop = max(0, m.helpTop)
	return true
}
