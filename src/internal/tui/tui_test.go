package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"plinth/internal/agents"
	"plinth/internal/asklint"
	"plinth/internal/config"
	"plinth/internal/doc"
	"plinth/internal/logfile"
	"plinth/internal/names"
	"plinth/internal/term"
)

const globalCfg = `forgejo-url: https://forgejo.example.org
reviewers:
  type:
    security:
      definition: "Review the script."
  automatic: [security]
`

func env(k string) (string, bool) {
	if k == "HOME" {
		return "/home/u", true
	}
	return "", false
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hostile is answer text full of escape sequences: the frames must hold none of them.
const hostile = "\x1b]0;forged title\x07 \x1b]52;c;cm0gLXJmIH4=\x07 clipboard \x1b[2J\x1b[H forged \u202eevil\u202c \u009b31m"

func script(interactive string) (string, string) {
	s := "#!/bin/bash\n# AAA script: check the disks\n# READ-ONLY diagnostics - nothing is started, stopped or changed.\n" +
		"AGENT_NAME=\"devops/main\"\nRUN_NAME=\"x\"\nINTERACTIVE=" + interactive + "\nNETWORK=0\nPRIVILEGED=0\n" + libLine
	sum := sha256.Sum256([]byte(s))
	return s, hex.EncodeToString(sum[:]) + "  ask.sh\n"
}

// libLine sources the AAA library: a script without it fails ask lint --plinth, and exec refuses it.
const libLine = asklint.PlinthLibLine + "\n"

// answerDoc is an answer document of bold-*, one section of output, its DLP scan's result in status.dlp.
func answerDoc(output string, d *doc.DLP) string {
	a := doc.Answer{APIVersion: doc.APIVersion, Kind: doc.KindAnswer, Metadata: doc.AnswerMeta{Run: "bold-bison"},
		Sections: []doc.Section{{Title: "1. disks\n", Output: output}}, Result: doc.ResultOK, Status: &doc.AnswerStatus{DLP: d}}
	return string(a.Encode())
}

// setup is a <work> with one namespace and a run in each state the actions care about.
func setup(t *testing.T) (*Model, string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), ".plinth")
	write(t, filepath.Join(work, "config.yml"), globalCfg+"plinth:\n  repo: michael/runs\n")
	if err := os.MkdirAll(filepath.Join(work, "michael", "devops", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, sum := script("0")
	si, sumi := script("1")
	ok := "# Review security\n\n## Verdict\nOK to run\n"
	failed := answerDoc("x\n", &doc.DLP{Result: doc.ResultFail, Reason: "Ollama (model dlp): the model's answer is not the expected JSON\n"})
	passed := answerDoc("first line\n"+hostile+"\n", &doc.DLP{Result: doc.ResultOK, Model: "m", Chunks: 1})
	runs := map[string]map[string]string{
		"keen-tapir-73": {"ask.sh": s, "ask.sh.sha256": sum, "review.security.FAILED.md": "# Review security: FAILED\n\nnot available without Ollama.\n"},
		"calm-otter":    {"ask.sh": s, "ask.sh.sha256": sum, "review.security.md": ok, "notes\x1b]0;pwned\x07.txt": "x"},
		"eager-lynx":    {"ask.sh": si, "ask.sh.sha256": sumi, "review.security.md": ok},
		"brisk-fig":     {"ask.sh": s, "ask.sh.sha256": "0000  ask.sh\n", "review.security.md": ok},
		"bold-birch-9":  {"ask.sh": s, "ask.sh.sha256": sum, "review.security.md": ok, "answer.DLP-FAIL.yml": failed},
		"bold-bison":    {"ask.sh": s, "ask.sh.sha256": sum, "review.security.md": ok, "answer.dlp-ok.yml": passed},
		"swift-wren":    {"ask.sh": s, "ask.sh.sha256": sum, "review.security.md": ok, "answer.new.yml": "unscanned secret\n"},
	}
	for run, files := range runs {
		for n, body := range files {
			write(t, filepath.Join(work, "michael", "devops", "runs", "main", run, n), body)
		}
	}
	m, err := New(&Store{Work: work, Getenv: env, Now: time.Now}, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	return m, work
}

func runes(m *Model, s string) {
	for _, r := range s {
		m.Key(term.Key{Code: term.KeyRune, Rune: r})
	}
}

func key(m *Model, c term.KeyCode) { m.Key(term.Key{Code: c}) }

func ctrl(m *Model, r rune) { m.Key(term.Key{Code: term.KeyCtrl, Rune: r}) }

// pick filters the Runs screen to run and selects it.
func pick(m *Model, run string) {
	key(m, term.KeyEsc)
	runes(m, "/"+run)
	key(m, term.KeyEnter)
}

// frame draws the model and returns its text; it fails on any control or hidden rune in it.
func frame(t *testing.T, m *Model) string {
	t.Helper()
	var b strings.Builder
	for _, l := range m.View(120, 40) {
		if l == nil {
			b.WriteString("\n")
			continue
		}
		s := l.String()
		for _, r := range s {
			if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
				(r >= 0x2060 && r <= 0x2069) || r == 0xfeff {
				t.Fatalf("rune %U on the screen: %q", r, s)
			}
		}
		b.WriteString(s + "\n")
	}
	return b.String()
}

// allowed are the sequences term.Screen writes: what reaches the terminal holds no other.
var allowed = regexp.MustCompile(`^\x1b(\[[0-9]+;1H|\[0(;[0-9]+)*m|\[K|\[2J|\[\?(1049|25|7)[hl])`)

// terminal draws the model through term.Screen and fails on any escape sequence the screen doesn't write itself.
func terminal(t *testing.T, m *Model) {
	t.Helper()
	var out strings.Builder
	scr := term.NewScreen(&out, true)
	scr.Resize(100, 30)
	if err := scr.Draw(m.View(100, 30)); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for i := strings.IndexByte(s, 0x1b); i >= 0; {
		seq := allowed.FindString(s[i:])
		if seq == "" {
			t.Fatalf("a sequence the screen doesn't write: %q", s[i:min(len(s), i+24)])
		}
		j := strings.IndexByte(s[i+len(seq):], 0x1b)
		if j < 0 {
			break
		}
		i += len(seq) + j
	}
	for _, r := range s {
		if r == 0x07 || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("rune %U reached the terminal", r)
		}
	}
}

func queued(work, run, action string) bool {
	_, err := os.Stat(filepath.Join(work, "michael", "devops", "requests", "main", run, action+".queued"))
	return err == nil
}

func requestsOf(t *testing.T, work string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(filepath.Join(work, "michael", "devops", "requests"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(work, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func TestRunsScreen(t *testing.T) {
	m, _ := setup(t)
	f := frame(t, m)
	for _, want := range []string{"Controller: not running", "Ollama:     not configured", "Namespace:  all",
		"Agent:      all", "Released:   hidden", "RUN", "ASK", "keen-tapir-73", "review-failed", "calm-otter", "approved",
		"bold-birch", "dlp-failed", "swift-wren", "scanning", "no controller"} {
		if !strings.Contains(f, want) {
			t.Errorf("no %q in the Runs screen:\n%s", want, f)
		}
	}
	terminal(t, m)

	// A running controller shows as its PID; the notices open in the log are counted after it, with or without a
	// controller, and the message line says the newest one and what to do.
	m.snap.controller, m.snap.pid = true, 4242
	if h := m.header(120)[1].String(); !strings.HasPrefix(h, "Controller: PID 4242 ") || strings.Contains(h, "notice") {
		t.Errorf("header with a controller: %q", h)
	}
	notice := func(msg, next string) logfile.Notice {
		return logfile.Notice{Key: "k", Last: logfile.Entry{Level: "WARN", Msg: msg, Fields: []logfile.Field{{Key: "next", Value: next}}}}
	}
	m.snap.notices = []logfile.Notice{notice("the Ollama gateway does not run", "plinthctl ollama-mcp start"), notice("another", "")}
	if h := m.header(120)[1].String(); !strings.HasPrefix(h, "Controller: PID 4242, 2 notices ") ||
		m.msgLine().String() != "2 notices: the Ollama gateway does not run - plinthctl ollama-mcp start" {
		t.Errorf("header with notices: %q, message line %q", h, m.msgLine().String())
	}
	m.snap.notices = m.snap.notices[1:]
	m.snap.controller, m.snap.pid = false, 0
	if h := m.header(120)[1].String(); !strings.HasPrefix(h, "Controller: not running, 1 notice ") || m.msgLine().String() != "1 notice: another" {
		t.Errorf("header with a notice and no controller: %q, message line %q", h, m.msgLine().String())
	}
	m.snap.notices = nil
	// A long <work> gives way from its left inside the left block; the hints keep their columns.
	if h := []rune(m.header(80)[0].String()); len(m.store.Work) > 21 &&
		(string(h[:15]) != "Work:       ..." || !strings.HasSuffix(string(h[:33]), ".plinth") || h[33] != ' ') {
		t.Errorf("Work at 80 columns: %q", string(h))
	}

	// The filter narrows the list and shows above the headings; Esc clears it.
	pick(m, "bold")
	if f := frame(t, m); strings.Contains(f, "calm-otter") || !strings.Contains(f, "bold-birch") || !strings.Contains(f, "filter: bold") {
		t.Errorf("filtered to bold:\n%s", f)
	}
	key(m, term.KeyEsc)
	if f := frame(t, m); !strings.Contains(f, "calm-otter") || strings.Contains(f, "filter:") {
		t.Errorf("Esc didn't clear the filter:\n%s", f)
	}

	// Too small a terminal shows only the size (80 x 24).
	if l := m.View(79, 24); len(l) != 1 || !strings.Contains(l[0].String(), "terminal too small (79 x 24, needs 80 x 24)") {
		t.Errorf("79 x 24: %v", l)
	}
	if l := m.View(80, 23); len(l) != 1 {
		t.Errorf("80 x 23: %d lines", len(l))
	}
	// Keys do nothing unseen there: only Ctrl-Z and Ctrl-C.
	runes(m, "q?e:")
	if m.Quit() || m.help || m.prompt != nil || m.pick != nil {
		t.Errorf("a key acted on a screen too small: quit %v, help %v, prompt %v, picker %v", m.Quit(), m.help, m.prompt != nil, m.pick != nil)
	}
	if ctrl(m, 'z'); !m.Suspended() {
		t.Error("Ctrl-Z on a screen too small")
	}
	if l := m.View(80, 24); len(l) != 24 {
		t.Errorf("80 x 24: %d lines", len(l))
	}

	runes(m, "?")
	if f := frame(t, m); !strings.Contains(f, "Keys") || !strings.Contains(f, "Override review") {
		t.Errorf("help:\n%s", f)
	}
	// At 80 x 24 Help doesn't fit: it scrolls, the title says which lines show, and any other key goes back.
	text24 := func() string {
		var b strings.Builder
		for _, l := range m.View(80, 24) {
			if l != nil {
				b.WriteString(l.String() + "\n")
			}
		}
		return b.String()
	}
	if f := text24(); !strings.Contains(f, "Keys    lines 1-14 of 30") || strings.Contains(f, "<left> <right>") || !strings.Contains(f, "<up> <down> <k> <j>") || !strings.Contains(f, "General") {
		t.Errorf("help at 80 x 24:\n%s", f)
	}
	runes(m, "G")
	if f := text24(); !m.help || !strings.Contains(f, "lines 17-30 of 30") || !strings.Contains(f, "<left> <right>") {
		t.Errorf("help at its end:\n%s", f)
	}
	runes(m, "x")
	ctrl(m, 'v')
	if m.msg == nil || m.msg.String() != "Plinth test" {
		t.Errorf("ctrl-v: %v", m.msg)
	}
	ctrl(m, 'c')
	if !m.Quit() {
		t.Error("Ctrl-C didn't quit")
	}
	// Only Q quits on Runs: q goes back elsewhere, so q pressed once too often on the way back leaves the TUI open.
	m, _ = setup(t)
	key(m, term.KeyEnter)
	runes(m, "E")
	if m.top().kind != screenEvents {
		t.Fatalf("E on the Run screen: screen %v", m.top().kind)
	}
	runes(m, "qqq")
	if m.Quit() || m.top().kind != screenRuns {
		t.Errorf("q q q from the Events screen: quit %v, screen %v", m.Quit(), m.top().kind)
	}
	if runes(m, "Q"); !m.Quit() {
		t.Error("Q didn't quit")
	}
}

// layout checks a w x h frame: h rows; the header's left block, its hints in two columns, from w-46 and w-23;
// the hints, joined by two spaces; the breadcrumb bar, w wide, with the general keys at its right end.
func layout(t *testing.T, m *Model, w, h int, hints, general, crumb string) {
	t.Helper()
	ls := m.View(w, h)
	if len(ls) != h {
		t.Fatalf("%d x %d: %d lines", w, h, len(ls))
	}
	var hs []string
	for _, x := range m.hints() {
		hs = append(hs, "<"+x.key+"> "+x.label)
	}
	if got := strings.Join(hs, "  "); got != hints {
		t.Errorf("hints at %d x %d: %q, want %q", w, h, got, hints)
	}
	top := ls[0].String()
	if !strings.HasPrefix(top, "Work:       ") || (len(hs) > 0 && strings.Index(top, hs[0]) != w-2*hintWidth) ||
		(len(hs) > headerRows && strings.Index(top, hs[headerRows]) != w-hintWidth) {
		t.Errorf("first row at %d x %d: %q", w, h, top)
	}
	// The divider under the header, ASCII without a UTF-8 locale.
	if d := ls[headerRows]; d == nil || d.String() != strings.Repeat("-", w) {
		t.Errorf("divider at %d x %d: %v", w, h, d)
	}
	want := " " + crumb
	if general != "" {
		want += strings.Repeat(" ", w-len([]rune(want))-len([]rune(general))-1) + general
	}
	if b := ls[h-1]; b.Width() != w || strings.TrimRight(b.String(), " ") != want {
		t.Errorf("breadcrumb at %d x %d: %q, want %q", w, h, b.String(), want)
	}
}

// TestLayout: the k9s layout at 80 x 24 and at 120 x 40 on Runs, Run, the pager, Events and Logs: the header's
// two blocks, the hints of what is selected (a key that doesn't apply is absent), the message line and the breadcrumb.
func TestLayout(t *testing.T) {
	m, work := setup(t)
	s, sum := script("0")
	for i := range 10 {
		dir := filepath.Join(work, "michael", "devops", "runs", "main", fmt.Sprintf("quiet-fox-%d", i+1))
		write(t, filepath.Join(dir, "ask.sh"), s)
		write(t, filepath.Join(dir, "ask.sh.sha256"), sum)
	}
	m.Reload()
	const screens = "<:> Screens  <?> Help"
	both := func(hints, general, crumb string) {
		t.Helper()
		layout(t, m, 80, 24, hints, general, crumb)
		layout(t, m, 120, 40, hints, general, crumb)
	}
	// At 80 x 24 the Runs list keeps 14 rows: 6 header rows, the divider, the headings, the message line and the breadcrumb.
	ls := m.View(80, 24)
	if !strings.HasPrefix(ls[7].String(), "RUN ") {
		t.Errorf("row 8: %q", ls[7].String())
	}
	for i := 8; i < 22; i++ {
		if ls[i] == nil || !strings.Contains(ls[i].String(), "-") {
			t.Errorf("row %d isn't a run: %v", i+1, ls[i])
		}
	}

	runsKeys := "  <n> Namespace  <a> Agent  <t> Show/hide released  </> Filter"
	pick(m, "keen-tapir-73")
	both("<e> Exec  <o> Override  <r> Report  <v> Review  <ctrl-d> Scrub"+runsKeys, screens, "<all> <runs>")
	pick(m, "calm-otter")
	both("<e> Exec  <v> Review  <ctrl-d> Scrub"+runsKeys, screens, "<all> <runs>")
	ls = m.View(80, 24)
	for i, want := range map[int]string{
		1: fmt.Sprintf("%-34s%s", "Controller: not running", "<v> Review"),
		5: fmt.Sprintf("%-34s%s", "Released:   hidden", "<t> Show/hide released"),
	} {
		if got := strings.TrimRight(ls[i].String(), " "); got != want {
			t.Errorf("row %d at 80 x 24:\n%q\nwant\n%q", i+1, got, want)
		}
	}
	if r := []rune(strings.TrimRight(ls[1].String(), " ")); string(r[34:]) != "<v> Review" {
		t.Errorf("row 2 at 80 x 24: %q", string(r))
	}
	// The message line is the row above the breadcrumb.
	runes(m, "d")
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		if ls := m.View(size[0], size[1]); ls[size[1]-2].String() != "d: does not apply to calm-otter" {
			t.Errorf("message line at %v: %q", size, ls[size[1]-2].String())
		}
	}

	key(m, term.KeyEnter)
	both("<e> Exec  <v> Review  <ctrl-d> Scrub", screens, "<all> <runs> <calm-otter>")
	key(m, term.KeyEnter) // ask.sh, the first file
	both("</> Search  <w> Wrap", "<?> Help", "<all> <runs> <calm-otter> <ask.sh>")
	key(m, term.KeyEsc)
	runes(m, "E")
	both("</> Filter  <w> Wrap", screens, "<all> <runs> <calm-otter> <events>")
	runes(m, "L") // from Events to Logs: Events closes, the Run screen stays
	both("<l> Level  <c> Component  <d> Date  <h> Hour  </> Search  <f> Files  <ctrl-d> Scrub logs", screens, "<all> <runs> <calm-otter> <logs> <run:calm-otter>")
	if len(m.stack) != 3 {
		t.Errorf("stack after E, L: %d screens", len(m.stack))
	}
	key(m, term.KeyEsc) // clears the run filter of the Logs screen
	key(m, term.KeyEsc)
	// A hostile file name reaches the breadcrumb escaped.
	runes(m, "jj")
	key(m, term.KeyEnter)
	if f := frame(t, m); !strings.Contains(f, `<notes\x1b]0;pwned\x07.txt>`) {
		t.Errorf("hostile file name in the breadcrumb:\n%s", f)
	}
	terminal(t, m)
}

// TestPicker: : opens the screen picker under the header; up and down move, typing narrows it, Enter switches, Esc
// closes it and leaves the screen as it was; n and a pick the filters the same way.
func TestPicker(t *testing.T) {
	m, work := setup(t)
	runes(m, ":")
	ls := m.View(80, 24)
	got := []string{ls[7].String()}
	for _, l := range ls[8:12] {
		got = append(got, strings.TrimSpace(l.String()))
	}
	if !slices.Equal(got, []string{": _", "Runs", "Events", "Logs", "Image"}) || !strings.HasPrefix(ls[12].String(), "RUN ") ||
		len(m.hints()) != 0 {
		t.Errorf("the picker: %q, then %q; hints %v", got, ls[12].String(), m.hints())
	}
	runes(m, "lo")
	if items := m.pick.items(); !slices.Equal(items, []string{"Logs"}) {
		t.Errorf("lo leaves %v", items)
	}
	key(m, term.KeyEnter)
	if m.pick != nil || m.top().kind != screenLogs {
		t.Fatalf("Enter on Logs: picker %v, screen %v", m.pick != nil, m.top().kind)
	}
	// From Logs to Events: Logs closes first.
	runes(m, ":")
	key(m, term.KeyDown)
	key(m, term.KeyEnter)
	if m.top().kind != screenEvents || len(m.stack) != 2 {
		t.Errorf("Down, Enter: screen %v, %d screens", m.top().kind, len(m.stack))
	}
	// Esc closes the picker; the screen stays.
	runes(m, ":ru")
	key(m, term.KeyEsc)
	if m.pick != nil || m.top().kind != screenEvents {
		t.Errorf("Esc: picker %v, screen %v", m.pick != nil, m.top().kind)
	}
	// Nothing matches: Enter does nothing.
	runes(m, ":zzz")
	key(m, term.KeyEnter)
	if f := frame(t, m); m.pick == nil || !strings.Contains(f, "nothing matches") {
		t.Errorf("nothing matches:\n%s", f)
	}
	key(m, term.KeyEsc)
	runes(m, ":runs")
	key(m, term.KeyEnter)
	if m.top().kind != screenRuns || len(m.stack) != 1 {
		t.Errorf("Runs: screen %v, %d screens", m.top().kind, len(m.stack))
	}

	// n: all, then each namespace; a: all, then each agent.
	runes(m, "n")
	if items := m.pick.items(); !slices.Equal(items, []string{"all", "michael/devops"}) {
		t.Errorf("n lists %v", items)
	}
	key(m, term.KeyDown)
	key(m, term.KeyEnter)
	runes(m, "ama")
	key(m, term.KeyEnter)
	if f := frame(t, m); m.nsFilter != "michael/devops" || m.agentFilter != "main" || !strings.Contains(f, "Namespace:  michael/devops") ||
		!strings.Contains(f, "Agent:      main") || !strings.HasPrefix(m.crumbs(120).String(), " <michael/devops> <runs> ") {
		t.Errorf("filters %q %q:\n%s", m.nsFilter, m.agentFilter, f)
	}
	runes(m, "n")
	key(m, term.KeyEnter)
	if m.nsFilter != "" || m.agentFilter != "" {
		t.Errorf("all: %q %q", m.nsFilter, m.agentFilter)
	}
	// a offers the registered agents too, with runs or without.
	ns, _ := names.ParseNamespace("michael/devops")
	if _, _, err := agents.Register(work, ns, "plinth-coder", "/r"); err != nil {
		t.Fatal(err)
	}
	runes(m, "a")
	if items := m.pick.items(); !slices.Equal(items, []string{"all", "main", "plinth-coder"}) {
		t.Errorf("a lists %v", items)
	}
	key(m, term.KeyEsc)
}

func TestActions(t *testing.T) {
	m, work := setup(t)

	// Exec: y/N; anything but y writes nothing.
	pick(m, "calm-otter")
	runes(m, "e")
	if f := frame(t, m); !strings.Contains(f, "Exec calm-otter in michael/devops? (y/N)") {
		t.Fatalf("exec prompt:\n%s", f)
	}
	runes(m, "n")
	if r := requestsOf(t, work); len(r) != 0 || !strings.Contains(frame(t, m), "Nothing queued.") {
		t.Fatalf("answered n: %v", r)
	}
	runes(m, "ey")
	if !queued(work, "calm-otter", "exec") || !strings.Contains(frame(t, m), "Queued exec of calm-otter. The controller is not running") {
		t.Errorf("exec not queued: %v\n%s", requestsOf(t, work), frame(t, m))
	}
	// The request shows in the run's row.
	if f := frame(t, m); !strings.Contains(f, "exec queued") {
		t.Errorf("no exec queued in the row:\n%s", f)
	}

	// A dimmed action says why and writes nothing; so does one that doesn't apply.
	for _, c := range []struct{ run, keys, want string }{
		{"keen-tapir-73", "e", "Exec: Review failed"},
		{"eager-lynx", "e", "exec of eager-lynx: deny by rule state.valid"}, // INTERACTIVE=1 without PRIVILEGED=1: lint refuses that
		{"bold-birch", "d", "Done: Successful DLP scan needed"},
		{"bold-birch", "s", "Save: Successful DLP scan needed (or Override DLP scan first)"},
		{"swift-wren", "d", "Done: Waiting for the DLP scan"},
		{"keen-tapir-73", "r", "Report: Not available without Ollama: everything Report posts is masked first"},
		{"calm-otter", "s", "s: does not apply to calm-otter"}, // no answer yet

		{"calm-otter", "d", "d: does not apply to calm-otter"},
	} {
		pick(m, c.run)
		runes(m, c.keys)
		if f := frame(t, m); !strings.Contains(f, c.want) {
			t.Errorf("%s %s: no %q in\n%s", c.run, c.keys, c.want, f)
		}
	}
	// decide refuses what the controller would refuse, naming the rule, and nothing is written: brisk-fig's ask.sh no
	// longer matches its sum.
	pick(m, "brisk-fig")
	runes(m, "e")
	if f := frame(t, m); !strings.Contains(f, "exec of brisk-fig: deny by rule state.valid") {
		t.Errorf("no refusal:\n%s", f)
	}
	if r := requestsOf(t, work); len(r) != 1 {
		t.Errorf("requests after the refusals: %v", r)
	}

	// Review: with one review type, y/N for it; the request is review.<type>, as plinthctl review -n writes it.
	pick(m, "calm-otter")
	runes(m, "v")
	if f := frame(t, m); !strings.Contains(f, "Review the script of calm-otter with security? (y/N)") {
		t.Errorf("review prompt:\n%s", f)
	}
	runes(m, "n")
	if queued(work, "calm-otter", "review.security") {
		t.Error("review queued after n")
	}
	runes(m, "vy")
	if !queued(work, "calm-otter", "review.security") || !strings.Contains(frame(t, m), "Queued review.security of calm-otter.") {
		t.Errorf("review not queued: %v\n%s", requestsOf(t, work), frame(t, m))
	}
	// With more types, a digit per type, sorted, then y/N; another key queues nothing.
	m.snap.g.Reviewers.Type["dlp"] = config.ReviewType{Definition: "Find secrets."}
	pick(m, "swift-wren")
	runes(m, "v")
	if f := frame(t, m); !strings.Contains(f, "Review the answer of swift-wren with which type? (1-2; any other key: nothing)") ||
		!strings.Contains(f, "  1  dlp") || !strings.Contains(f, "  2  security") {
		t.Errorf("review choice:\n%s", f)
	}
	runes(m, "x")
	if queued(work, "swift-wren", "review.dlp") || queued(work, "swift-wren", "review.security") || !strings.Contains(frame(t, m), "Nothing done.") {
		t.Error("a review queued after x")
	}
	m.snap.g.Reviewers.Type["dlp"] = config.ReviewType{Definition: "Find secrets."} // the queue reloads the snapshot
	runes(m, "v1")
	if f := frame(t, m); !strings.Contains(f, "Review the answer of swift-wren with dlp? (y/N)") {
		t.Errorf("review confirmation after the choice:\n%s", f)
	}
	runes(m, "y")
	if !queued(work, "swift-wren", "review.dlp") {
		t.Errorf("review.dlp not queued: %v\n%s", requestsOf(t, work), frame(t, m))
	}

	// Override review: a box names the failed review; only a typed yes writes the request.
	pick(m, "keen-tapir-73")
	runes(m, "o")
	f := frame(t, m)
	if !strings.Contains(f, "Failed review(s) of keen-tapir-73") || !strings.Contains(f, "review.security.FAILED.md: not available without Ollama.") {
		t.Errorf("override box:\n%s", f)
	}
	runes(m, "ye")
	key(m, term.KeyEnter)
	if queued(work, "keen-tapir-73", "override.review") {
		t.Error("queued without the whole yes")
	}
	runes(m, "o")
	key(m, term.KeyEsc)
	runes(m, "o")
	runes(m, "yes")
	key(m, term.KeyEnter)
	if !queued(work, "keen-tapir-73", "override.review") {
		t.Errorf("override.review not queued: %v", requestsOf(t, work))
	}

	// Override DLP scan names the reason in status.dlp.
	pick(m, "bold-birch")
	runes(m, "o")
	if f := frame(t, m); !strings.Contains(f, "answer failed: Ollama (model dlp): the model's answer is not the expected JSON") {
		t.Errorf("override DLP box:\n%s", f)
	}
	key(m, term.KeyEsc)

	// Save: once the DLP scan is overridden, y/N names plinth.repo (the run needs no issue in its name).
	write(t, filepath.Join(work, "michael", "devops", "runs", "main", "bold-birch-9", "override.dlp.md"), "overridden\n")
	m.Reload()
	pick(m, "bold-birch")
	runes(m, "s")
	if f := frame(t, m); !strings.Contains(f, "Save bold-birch-9 (5 files, as bold-birch-9.zip) on the run's issue in michael/runs? (y/N)") {
		t.Errorf("save prompt:\n%s", f)
	}
	runes(m, "y")
	if !queued(work, "bold-birch-9", "save") {
		t.Errorf("save not queued: %v\n%s", requestsOf(t, work), frame(t, m))
	}
	// Report: without Ollama it can't mask, so it says so; with it, a box names the repo and what is posted, then y/N.
	write(t, filepath.Join(work, "config.yml"), globalCfg+"plinth:\n  repo: michael/runs\n")
	m.Reload()
	pick(m, "keen-tapir")
	runes(m, "r")
	if f := frame(t, m); !strings.Contains(f, "Report: Not available without Ollama") {
		t.Errorf("report without Ollama:\n%s", f)
	}
	write(t, filepath.Join(work, "config.yml"), globalCfg+"ollama:\n  host: http://127.0.0.1:9\n  models:\n    review: rev\n    dlp: dlp\n"+
		"plinth:\n  repo: michael/runs\n")
	m.Reload()
	pick(m, "keen-tapir")
	runes(m, "r")
	f = frame(t, m)
	for _, want := range []string{"Report posts on the run's issue in michael/runs:", `"michael/devops keen-tapir: <what the script says>", opened if the run has none,`,
		`a comment "Report of failed scans: review security" with the events that decided it,`, "attaches keen-tapir-73.report.zip:", "  ask.sh, review.security.FAILED.md", "Everything is masked first", "Report keen-tapir-73? (y/N)"} {
		if !strings.Contains(f, want) {
			t.Errorf("no %q in the report box:\n%s", want, f)
		}
	}
	runes(m, "y")
	if !queued(work, "keen-tapir-73", "report") {
		t.Errorf("report not queued: %v\n%s", requestsOf(t, work), frame(t, m))
	}

	// Done: y/N, then the reminder.
	pick(m, "bold-bison")
	runes(m, "dy")
	if !queued(work, "bold-bison", "done") || !strings.Contains(frame(t, m), `Tell the agent "bold-bison done"`) {
		t.Errorf("done: %v\n%s", requestsOf(t, work), frame(t, m))
	}

	// Scrub: Ctrl-D, then yes; S does nothing now.
	pick(m, "swift-wren")
	runes(m, "S")
	if m.prompt != nil {
		t.Error("S opened a prompt")
	}
	ctrl(m, 'd')
	if f := frame(t, m); !strings.Contains(f, "deletes, for good") {
		t.Errorf("scrub box:\n%s", f)
	}
	runes(m, "yes")
	key(m, term.KeyEnter)
	if !queued(work, "swift-wren", "scrub") {
		t.Errorf("scrub not queued: %v", requestsOf(t, work))
	}
	want := []string{"exec.queued", "review.security.queued", "review.dlp.queued", "override.review.queued", "save.queued", "report.queued",
		"done.queued", "scrub.queued"}
	if r := requestsOf(t, work); len(r) != len(want) {
		t.Errorf("requests: %v, want %v", r, want)
	}
	// Every queued request is in the general log.
	b, _ := os.ReadFile(filepath.Join(work, "logs", "plinth-"+time.Now().Format("2006-01-02")+".log"))
	if n := strings.Count(string(b), `msg="queued a request" component=tui `); n != len(want) {
		t.Errorf("%d queued lines in the log:\n%s", n, b)
	}
}

func TestRunAndPager(t *testing.T) {
	m, _ := setup(t)
	pick(m, "bold-bison")
	key(m, term.KeyEnter)
	f := frame(t, m)
	for _, want := range []string{"Run bold-bison", "Stage", "answer", "Interactive", "no", "Done",
		"Script header", "AAA script: check the disks", "Reviews", "review.security.md: OK to run", "Answer",
		"DLP scan: ok", "## 1. disks", "first line", `\x1b]52;c;cm0gLXJmIH4=\x07`, `\u202eevil`, "Files", "answer.dlp-ok.yml"} {
		if !strings.Contains(f, want) {
			t.Errorf("no %q on the Run screen:\n%s", want, f)
		}
	}
	// The pane has no Actions section: the header has them; Enter on a file is said above the list, not in the header.
	want := []hint{{"d", "Done"}, {"s", "Save"}, {"v", "Review"}, {"ctrl-d", "Scrub"}} // Save: any run with an answer
	if hs := m.hints(); strings.Contains(f, "Actions") || !slices.Equal(hs, want) {
		t.Errorf("Actions in the pane, or hints %v:\n%s", hs, f)
	}
	terminal(t, m)
	// The files are sorted: answer.dlp-ok.yml, ask.sh, ...; Enter on the first opens it in full: the document.
	key(m, term.KeyEnter)
	f = frame(t, m)
	if !strings.Contains(f, "answer.dlp-ok.yml of main/bold-bison") || !strings.Contains(f, "clipboard") || !strings.Contains(f, `result: "ok"`) {
		t.Errorf("pager:\n%s", f)
	}
	terminal(t, m)
	runes(m, "/clip")
	key(m, term.KeyEnter)
	if f := frame(t, m); !strings.Contains(f, "at line 10 of") {
		t.Errorf("search didn't move to line 10:\n%s", f)
	}
	key(m, term.KeyEsc)
	key(m, term.KeyEsc)

	// A file name with escape sequences is shown escaped.
	pick(m, "calm-otter")
	key(m, term.KeyEnter)
	if f := frame(t, m); !strings.Contains(f, `notes\x1b]0;pwned\x07.txt`) {
		t.Errorf("hostile file name:\n%s", f)
	}
	terminal(t, m)
	key(m, term.KeyEsc)

	// The pager never opens an answer that hasn't been scanned.
	pick(m, "swift-wren")
	key(m, term.KeyEnter)
	key(m, term.KeyEnter) // answer.new.yml, the first file
	if f := frame(t, m); !strings.Contains(f, "answer.new.yml: not scanned yet") || strings.Contains(f, "unscanned secret") {
		t.Errorf("unscanned answer:\n%s", f)
	}

	// The run's events: none yet, and no problem.
	runes(m, "E")
	if f := frame(t, m); !strings.Contains(f, "Events of main/swift-wren in michael/devops") {
		t.Errorf("events:\n%s", f)
	}
}

func TestLogs(t *testing.T) {
	m, work := setup(t)
	write(t, filepath.Join(work, "logs", "plinth-2020-01-01.log"), "old\x1b[2J line\n")
	write(t, filepath.Join(work, "logs", "plinth-"+time.Now().Format("2006-01-02")+".log"), "today\n")
	runes(m, "L")
	runes(m, "f") // the file list (the lines are TestLogLines')
	f := frame(t, m)
	if !strings.Contains(f, "plinth-2020-01-01.log") || !strings.Contains(f, "old") || !strings.Contains(f, "controller not running") {
		t.Errorf("logs:\n%s", f)
	}
	// Scrub logs (Ctrl-D) needs the controller; nothing is written without it.
	ctrl(m, 'd')
	if f := frame(t, m); !strings.Contains(f, "Scrub logs: controller not running") {
		t.Errorf("scrub logs without the controller:\n%s", f)
	}
	if _, err := os.Stat(filepath.Join(work, "requests", "logs-scrub.queued")); err == nil {
		t.Error("logs-scrub queued without the controller")
	}
	// A log file opens in the pager, escaped.
	key(m, term.KeyDown)
	key(m, term.KeyEnter)
	if f := frame(t, m); !strings.Contains(f, `old\x1b[2J line`) {
		t.Errorf("log in the pager:\n%s", f)
	}
	terminal(t, m)
	// / filters the files by name; Esc clears the filter, then goes back.
	key(m, term.KeyEsc)
	runes(m, "/2020")
	key(m, term.KeyEnter)
	today := "plinth-" + time.Now().Format("2006-01-02")
	if f := frame(t, m); !strings.Contains(f, "filter: 2020") || strings.Contains(f, today) || !strings.Contains(f, "plinth-2020-01-01.log") {
		t.Errorf("logs filtered to 2020:\n%s", f)
	}
	key(m, term.KeyEsc)
	if f := frame(t, m); m.top().kind != screenLogs || !strings.Contains(f, today) {
		t.Errorf("Esc didn't clear the filter:\n%s", f)
	}
	// The Events screen shows the lines that hold its filter.
	v := &view{kind: screenEvents, text: []string{"a exec.start", "b done", "c exec.end"}, search: "exec"}
	if got := m.textOf(v); !slices.Equal(got, []string{"a exec.start", "c exec.end"}) {
		t.Errorf("events filtered to exec: %q", got)
	}
}

// TestAcceptNetwork: Exec of a network run says to accept it first; w shows what the run can reach, and only the typed
// yes queues accept.network.
func TestAcceptNetwork(t *testing.T) {
	work := filepath.Join(t.TempDir(), ".plinth")
	write(t, filepath.Join(work, "config.yml"), globalCfg)
	write(t, filepath.Join(work, "michael", "devops", "config.yml"), "plinth:\n  exec:\n    network: true\n")
	s := "#!/bin/bash\n# AAA script: reach the host\n# READ-ONLY diagnostics - nothing is started, stopped or changed.\n" +
		"AGENT_NAME=\"devops/main\"\nRUN_NAME=\"x\"\nINTERACTIVE=0\nNETWORK=1\nPRIVILEGED=0\n" + libLine
	sum := sha256.Sum256([]byte(s))
	dir := filepath.Join(work, "michael", "devops", "runs", "main", "keen-hawk")
	write(t, filepath.Join(dir, "ask.sh"), s)
	write(t, filepath.Join(dir, "ask.sh.sha256"), hex.EncodeToString(sum[:])+"  ask.sh\n")
	write(t, filepath.Join(dir, "review.security.md"), "# Review security\n\n## Verdict\nOK to run\n")
	m, err := New(&Store{Work: work, Getenv: env, Now: time.Now}, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	pick(m, "keen-hawk")
	runes(m, "e")
	if f := frame(t, m); !strings.Contains(f, "Exec: Network run: accept it first (w)") {
		t.Errorf("exec of a network run:\n%s", f)
	}
	runes(m, "w")
	if f := frame(t, m); !strings.Contains(f, "is a network run (NETWORK=1)") || !strings.Contains(f, "Type yes:") {
		t.Errorf("accept box:\n%s", f)
	}
	runes(m, "ye")
	key(m, term.KeyEnter)
	if queued(work, "keen-hawk", "accept.network") {
		t.Error("queued without the whole yes")
	}
	runes(m, "w")
	runes(m, "yes")
	key(m, term.KeyEnter)
	if !queued(work, "keen-hawk", "accept.network") {
		t.Errorf("accept.network not queued: %v\n%s", requestsOf(t, work), frame(t, m))
	}
}

// TestPrivilegedExec: e on a privileged run runs plinthctl exec in the terminal, handed over, which asks its own
// question; no request is written. Without a way to hand the terminal over, e says so.
func TestPrivilegedExec(t *testing.T) {
	_, work := setup(t)
	write(t, filepath.Join(work, "config.yml"), globalCfg+"plinth:\n  exec:\n    privileged: true\n")
	s := "#!/bin/bash\n# AAA script: check the disks\n# READ-ONLY diagnostics - nothing is started, stopped or changed.\n" +
		"AGENT_NAME=\"devops/main\"\nRUN_NAME=\"x\"\nINTERACTIVE=0\nNETWORK=0\nPRIVILEGED=1\n" + libLine
	sum := sha256.Sum256([]byte(s))
	dir := filepath.Join(work, "michael", "devops", "runs", "main", "brave-heron")
	write(t, filepath.Join(dir, "ask.sh"), s)
	write(t, filepath.Join(dir, "ask.sh.sha256"), hex.EncodeToString(sum[:])+"  ask.sh\n")
	write(t, filepath.Join(dir, "review.security.md"), "# Review security\n\n## Verdict\nOK to run\n")
	m, err := New(&Store{Work: work, Getenv: env, Now: time.Now}, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	pick(m, "brave-heron")
	runes(m, "e")
	if f := frame(t, m); !strings.Contains(f, "Exec: privileged: runs in your terminal (plinthctl exec), which this TUI can't hand over") {
		t.Errorf("no handover:\n%s", f)
	}
	f := &fakeImages{}
	m.store.Images = f
	runes(m, "e")
	want := []string{"exec", "-n", "michael/devops", "--agent", "main", "--work", work, "brave-heron"}
	if len(f.ran) != 1 || !slices.Equal(f.ran[0], want) {
		t.Errorf("ran %q, want %q", f.ran, want)
	}
	if fr := frame(t, m); !strings.Contains(fr, "plinthctl exec brave-heron: done.") {
		t.Errorf("after the run:\n%s", fr)
	}
	if r := requestsOf(t, work); len(r) != 0 {
		t.Errorf("requests: %v", r)
	}
}

// TestPromptFits: a confirmation box too long for the screen ends in "… n more lines", so the question and its input
// line always show (80 x 19 is enough).
func TestPromptFits(t *testing.T) {
	m, _ := setup(t)
	var box []*term.Line
	for i := range 20 {
		box = append(box, text(fmt.Sprintf("box line %d", i+1), normal))
	}
	m.ask(box, text("Are you sure? Type yes:", normal), "yes", func() {})
	_, body := m.promptView(80, 10)
	var got []string
	for _, l := range body {
		if l == nil {
			got = append(got, "")
			continue
		}
		got = append(got, l.String())
	}
	if len(got) != 10 || got[5] != "box line 6" || got[6] != "... 14 more lines" || got[8] != "Are you sure? Type yes:" || !strings.HasPrefix(got[9], "> ") {
		t.Errorf("prompt in 10 rows: %q", got)
	}
	if _, body := m.promptView(80, 30); len(body) != 20+3 {
		t.Errorf("prompt in 30 rows: %d lines, want the whole box", len(body))
	}
}

// TestWrap: the pager wraps long lines: at a space, at the column in a line without one, a wide character whole, the
// continuation row starting with the mark (> without a UTF-8 locale); the line count and j go by file lines; w scrolls
// sideways instead; a resize wraps again; a confirmation box wraps too.
func TestWrap(t *testing.T) {
	m, _ := setup(t)
	long, nospace, wide := strings.Repeat("word ", 30), strings.Repeat("x", 100), strings.Repeat("日本", 30)
	filler := strings.Repeat("filler\n", 40) // longer than a page, so j moves
	m.openText("wrap test", "wrap.txt", []byte("first\n"+long+"\n"+nospace+"\n"+wide+"\nlast\n"+filler), 0)
	shown := func(w int) (title string, body []string) {
		ls := m.View(w, 40)
		for _, l := range ls[9:37] { // under the header, the divider, the title and its rule, above the message line and the breadcrumb
			if l != nil {
				body = append(body, l.String())
			}
		}
		if r := ls[8].String(); r != strings.Repeat("-", w) && r != strings.Repeat("─", w) {
			t.Errorf("no rule under the title: %q", r)
		}
		return ls[7].String(), body
	}
	title, body := shown(80)
	want := []string{"first", strings.Repeat("word ", 16), "> " + strings.Repeat("word ", 14), strings.Repeat("x", 80),
		"> " + strings.Repeat("x", 20), strings.Repeat("日本", 20), "> " + strings.Repeat("日本", 10), "last"}
	if !strings.Contains(title, "scroll: at line 1 of 45    wrapped") || len(body) < 8 || !slices.Equal(body[:8], want) {
		t.Errorf("wrapped at 80:\n%s\n%q\nwant\n%q", title, body, want)
	}
	// A resize wraps again.
	if _, body := shown(120); body[5] != "last" || body[1] != strings.Repeat("word ", 24) || body[2] != "> "+strings.Repeat("word ", 6) {
		t.Errorf("wrapped at 120: %q", body)
	}
	// j moves by the file's lines.
	runes(m, "j")
	if title, body := shown(80); !strings.Contains(title, "scroll: at line 2 of 45") || body[0] != strings.Repeat("word ", 16) {
		t.Errorf("after j: %s %q", title, body)
	}
	// w: not wrapped, cut at the edge, scrolled sideways.
	runes(m, "w")
	if title, body := shown(80); !strings.Contains(title, "not wrapped") || !strings.HasSuffix(body[0], "word>") {
		t.Errorf("not wrapped: %s %q", title, body)
	}
	key(m, term.KeyRight)
	if _, body := shown(80); !strings.HasPrefix(body[0], "d word") {
		t.Errorf("scrolled sideways: %q", body)
	}
	runes(m, "w")
	if title, _ := shown(80); !strings.Contains(title, "    wrapped") {
		t.Errorf("w again: %s", title)
	}
	// A confirmation box's long line wraps.
	m.ask([]*term.Line{text(long, normal)}, text("Sure? (y/N)", normal), "", func() {})
	if _, body := m.promptView(80, 20); len(body) != 4 || !strings.HasPrefix(body[1].String(), "> word") {
		t.Errorf("box: %d rows", len(body))
	}
}

// TestAutosaveHints: with plinth.autosave.runs, the header's s says (runs auto-save) while the controller saves
// the run by itself, and the key only says why; a failed save brings the key back as Save (auto-fail). The same for r with
// plinth.autosave.failures.
func TestAutosaveHints(t *testing.T) {
	m, work := setup(t)
	write(t, filepath.Join(work, "config.yml"), globalCfg+"plinth:\n  repo: michael/runs\n  autosave:\n    runs: true\n    failures: true\n")
	req := func(run, name, body string) string {
		p := filepath.Join(work, "michael", "devops", "requests", "main", run, name)
		write(t, p, body)
		return p
	}

	// Save: finished, or on its way, the controller does it.
	saved := req("bold-bison", "save.finished", "")
	m.Reload()
	pick(m, "bold-bison")
	if f := frame(t, m); !strings.Contains(f, "<s> (runs auto-save)") {
		t.Errorf("no (runs auto-save) hint:\n%s", f)
	}
	runes(m, "s")
	if f := frame(t, m); !strings.Contains(f, "Save: runs auto-save: Plinth saves a run by itself right after Done") || m.prompt != nil {
		t.Errorf("s under auto-save:\n%s", f)
	}
	// Its own save failed (and nothing succeeded after it): the key works again.
	if err := os.Remove(saved); err != nil {
		t.Fatal(err)
	}
	req("bold-bison", "save.FAILED", "Error: 403\n")
	m.Reload()
	pick(m, "bold-bison")
	if f := frame(t, m); !strings.Contains(f, "<s> Save (auto-fail)") {
		t.Errorf("no Save (auto-fail) hint:\n%s", f)
	}
	runes(m, "s")
	if f := frame(t, m); !strings.Contains(f, "Save bold-bison (") || m.prompt == nil {
		t.Errorf("s after a failed auto-save:\n%s", f)
	}
	key(m, term.KeyEsc)

	// Report: queued, the controller does it; failed, the key is back.
	queuedReport := req("keen-tapir-73", "report.queued", "")
	m.Reload()
	pick(m, "keen-tapir-73")
	if f := frame(t, m); !strings.Contains(f, "<r> (auto-report)") {
		t.Errorf("no (auto-report) hint:\n%s", f)
	}
	runes(m, "r")
	if f := frame(t, m); !strings.Contains(f, "Report: failures auto-report: Plinth reports a run's first failed check by itself") {
		t.Errorf("r under auto-report:\n%s", f)
	}
	if err := os.Remove(queuedReport); err != nil {
		t.Fatal(err)
	}
	req("keen-tapir-73", "report.FAILED", "Error: not available without Ollama\n")
	m.Reload()
	pick(m, "keen-tapir-73")
	if f := frame(t, m); !strings.Contains(f, "<r> Report (auto-fail)") {
		t.Errorf("no Report (auto-fail) hint:\n%s", f)
	}
}

// TestGeneralKeys: the screen picker and Help sit at the right end of the breadcrumb bar; a long breadcrumb gives
// way to them from its left; none shows while Help is open, and the header's hints hold only the screen's own keys.
func TestGeneralKeys(t *testing.T) {
	m, _ := setup(t)
	bar := func() string { return m.View(80, 24)[23].String() }
	if b := bar(); !strings.HasSuffix(b, "<:> Screens  <?> Help ") || len([]rune(b)) != 80 || !strings.HasPrefix(b, " <all> <runs> ") {
		t.Errorf("the bar on Runs: %q", b)
	}
	for _, h := range m.hints() {
		if h.key == ":" || h.key == "?" {
			t.Errorf("a general key in the header: %v", h)
		}
	}
	m.stack[len(m.stack)-1].crumb = strings.Repeat("x", 100)
	if b := bar(); !strings.HasSuffix(b, "<:> Screens  <?> Help ") || len([]rune(b)) != 80 || !strings.HasPrefix(b, m.glyphs.Ellipsis) {
		t.Errorf("a long breadcrumb: %q", b)
	}
	m.stack[len(m.stack)-1].crumb = "runs"
	runes(m, "?")
	if b := bar(); strings.Contains(b, "Screens") || strings.Contains(b, "Help") {
		t.Errorf("the bar while Help is open: %q", b)
	}
}

// TestLogLines: the Logs screen shows the log's lines, newest first, with the fields in dim; Level, Component and
// Search narrow them and show in the breadcrumb bar, Esc clears the last; Open notices shows the lines of the keys still
// open; Enter shows a line in full; a hostile message is escaped; f is the file list.
func TestLogLines(t *testing.T) {
	m, work := setup(t)
	if err := os.MkdirAll(filepath.Join(work, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	lg := logfile.New(work, logfile.Runner).With("ns", "michael/devops")
	lg.Info("submitted", "agent", "main", "run", "calm-otter")
	lg.Warn("request refused", "agent", "main", "run", "keen-owl", "rule", "repo.set")
	logfile.New(work, logfile.Controller).Warn("the Ollama gateway does not run", "key", "ollama-gateway", "next", "plinthctl ollama-mcp start")
	logfile.New(work, logfile.DLP).Error("scan \x1b[2J failed", "reason", "x")
	runes(m, "L")
	f := frame(t, m)
	for _, want := range []string{"Logs", "4 lines", "INFO", "WARN", "ERROR", "runner", "dlp", "submitted", "request refused", "rule=repo.set",
		`scan \x1b[2J failed`} {
		if !strings.Contains(f, want) {
			t.Errorf("no %q on the Logs screen:\n%s", want, f)
		}
	}
	if strings.Index(f, "scan") > strings.Index(f, "submitted") || strings.Index(f, "the Ollama gateway") > strings.Index(f, "request refused") {
		t.Errorf("the newest line is not first:\n%s", f)
	}
	// Level: warn and above; the breadcrumb says so; Esc clears it.
	runes(m, "l")
	runes(m, "warn")
	key(m, term.KeyEnter)
	if f := frame(t, m); strings.Contains(f, "submitted") || !strings.Contains(f, "request refused") || !strings.Contains(f, "<>=warn>") || !strings.Contains(f, "3 lines") {
		t.Errorf("level warn:\n%s", f)
	}
	key(m, term.KeyEsc)
	if f := frame(t, m); !strings.Contains(f, "submitted") || strings.Contains(f, "<>=warn>") || m.top().kind != screenLogs {
		t.Errorf("Esc after a level:\n%s", f)
	}
	// Open notices: the lines of the keys still open; Enter shows one in full.
	runes(m, "l")
	key(m, term.KeyEnter) // the first entry
	f = frame(t, m)
	if strings.Contains(f, "submitted") || !strings.Contains(f, "the Ollama gateway does not run") || !strings.Contains(f, "<notices>") || !strings.Contains(f, "1 line") {
		t.Errorf("open notices:\n%s", f)
	}
	key(m, term.KeyEnter)
	if f := frame(t, m); m.top().kind != screenPager || !strings.Contains(f, "next:") || !strings.Contains(f, "plinthctl ollama-mcp start") || !strings.Contains(f, "component:  controller") {
		t.Errorf("a line in full:\n%s", f)
	}
	key(m, term.KeyEsc)
	key(m, term.KeyEsc) // the filter
	// Component, then the search.
	runes(m, "c")
	runes(m, "dlp")
	key(m, term.KeyEnter)
	if f := frame(t, m); strings.Contains(f, "submitted") || !strings.Contains(f, "scan") || !strings.Contains(f, "<dlp>") {
		t.Errorf("component dlp:\n%s", f)
	}
	key(m, term.KeyEsc)
	runes(m, "/refused")
	key(m, term.KeyEnter)
	if f := frame(t, m); strings.Contains(f, "submitted") || !strings.Contains(f, "request refused") || !strings.Contains(f, "</refused>") {
		t.Errorf("search refused:\n%s", f)
	}
	key(m, term.KeyEsc)
	// f is the file list, and back.
	runes(m, "f")
	if f := frame(t, m); !strings.Contains(f, "FILE") || !strings.Contains(f, "plinth-"+time.Now().Format("2006-01-02")+".log") {
		t.Errorf("the file list:\n%s", f)
	}
	runes(m, "f")
	if f := frame(t, m); !strings.Contains(f, "request refused") {
		t.Errorf("back to the lines:\n%s", f)
	}
	terminal(t, m)
}
