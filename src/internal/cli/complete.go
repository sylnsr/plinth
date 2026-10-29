package cli

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"plinth/internal/config"
	"plinth/internal/controller"
	"plinth/internal/decide"
	"plinth/internal/names"
	"plinth/internal/plain"
	"plinth/internal/request"
	"plinth/internal/runstate"
	"plinth/internal/term"
	"plinth/internal/workdir"
)

// CompleteArg is the hidden first argument the completion script calls plinthctl with: the words typed so far, the
// last one being completed (plinthctl.README.md, Shell completion). Not in the help.
const CompleteArg = "__complete"

// completionScript is what plinthctl completion bash prints: bash asks plinthctl itself for the candidates. A run comes
// as <name><TAB><listing line>: bash lists the lines (a second Tab, with more than one candidate: COMP_TYPE 63,
// or 33 and 64 with show-all-if-ambiguous and show-all-if-unmodified) and inserts the name only, in plinthctl's order.
const completionScript = `# bash completion for plinthctl: source <(plinthctl completion bash) in ~/.bashrc, or save it once as
# ~/.local/share/bash-completion/completions/plinthctl
_plinthctl() {
	local l list=0
	local -a out
	mapfile -t out < <("${COMP_WORDS[0]}" ` + CompleteArg + ` "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)
	case "${COMP_TYPE:-}" in
	63 | 33 | 64) ((${#out[@]} > 1)) && list=1 ;;
	esac
	COMPREPLY=()
	for l in "${out[@]}"; do
		if ((list)); then
			COMPREPLY+=("${l#*$'\t'}")
		else
			COMPREPLY+=("${l%%$'\t'*}")
		fi
	done
	compopt -o nosort 2>/dev/null
	return 0
}
complete -o default -F _plinthctl plinthctl
`

func runCompletion(c *command, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		printUsage(stdout, c)
		return exitOK
	}
	if len(args) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	io.WriteString(stdout, completionScript)
	return exitOK
}

// runCommands are the commands whose first argument is a run, with -n (review: only with -n).
var runCommands = []string{"get ask", "get ans", "describe", "exec", "done", "save", "scrub", "report", "review",
	"override review", "override dlp", "accept network", "events show"}

// hintActions are the decide actions behind the mark of a controller command (review: its action for any reviewer
// type).
var hintActions = map[string]string{"exec": decide.Exec, "done": decide.Done, "save": decide.Save, "scrub": decide.Scrub,
	"report": decide.Report, "override review": decide.OverrideReview, "override dlp": decide.OverrideDLP,
	"accept network": decide.AcceptNetwork}

// flagRe finds the flags in a usage line, with the word after one that takes a value.
var flagRe = regexp.MustCompile(`(?:^|[\s\[|])(--?[a-z][a-z-]*)(\s+[A-Z<][^\s\]|]*)?`)

// candidate is one word complete offers. A run, or a run's file, has a state, and fits when the command can act on it
// now: the listing shows the state, and the mark on those that fit, which come first.
type candidate struct {
	word, state string
	fits        bool
}

// words are candidates without a state: command words, flags, namespaces, review types.
func words(ws []string) []candidate {
	var out []candidate
	for _, w := range ws {
		out = append(out, candidate{word: w})
	}
	return out
}

// complete prints the candidates for the last of words, one per line, each starting with it: command words, flags, the
// namespaces below <work>, review types, and runs: every run, with its state, the ones the command can act on now
// first and marked. It reads names and the facts decide needs, never a file's content, and prints nothing on any
// error; nothing at all leaves bash its default (file names), which --repo, --work and --template want.
func complete(words []string, stdout io.Writer) {
	if len(words) == 0 {
		words = []string{""}
	}
	cur, prev := words[len(words)-1], words[:len(words)-1]
	var out []candidate
	cmds := commands()
	switch {
	case len(prev) == 0:
		for _, c := range cmds {
			out = append(out, candidate{word: strings.Fields(c.name)[0]})
		}
	case len(prev) == 1 && slices.ContainsFunc(cmds, func(c *command) bool { return strings.HasPrefix(c.name, prev[0]+" ") }):
		for _, c := range cmds {
			if w, ok := strings.CutPrefix(c.name, prev[0]+" "); ok {
				out = append(out, candidate{word: w})
			}
		}
	default:
		c, rest := lookup(prev)
		if c == nil {
			return
		}
		out = completeArgs(c, rest, cur)
	}
	printCandidates(stdout, cur, out, mark(os.Getenv))
}

// printCandidates prints the candidates that start with cur, each once (it fits when any of its copies does): those
// that fit first, then by word. One with a state is <word><TAB><listing line>, the line padded into columns:
// <word>  (<state>) <mark>.
func printCandidates(w io.Writer, cur string, cs []candidate, mark string) {
	seen := map[string]int{}
	var out []candidate
	for _, c := range cs {
		if !strings.HasPrefix(c.word, cur) {
			continue
		}
		if i, ok := seen[c.word]; ok {
			out[i].fits = out[i].fits || c.fits
			continue
		}
		seen[c.word] = len(out)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b candidate) int {
		if a.fits != b.fits {
			if a.fits {
				return -1
			}
			return 1
		}
		return strings.Compare(a.word, b.word)
	})
	wordW, stateW := 0, 0
	for _, c := range out {
		if c.state != "" {
			wordW, stateW = max(wordW, len(c.word)), max(stateW, len(c.state)+2)
		}
	}
	for _, c := range out {
		line := c.word
		if c.state != "" {
			m := ""
			if c.fits {
				m = mark
			}
			line = strings.TrimRight(fmt.Sprintf("%s\t%-*s  %-*s %s", c.word, wordW, c.word, stateW, "("+c.state+")", m), " ")
		}
		io.WriteString(w, line+"\n")
	}
}

// mark is the sign on a run the command can act on now: ✓ in a UTF-8 locale, + otherwise
// (term.UTF8, the one locale check).
func mark(getenv func(string) string) string { return term.GlyphsFor(term.UTF8(getenv)).Mark }

// completeArgs is complete for the words after command c: rest are the words before the one being completed, cur.
func completeArgs(c *command, rest []string, cur string) []candidate {
	usage := strings.SplitN(c.usage, "\n", 2)[0]
	// These take a value wherever they appear, named in the usage line or not (every controller command takes --work).
	valued := map[string]bool{"-n": true, "--work": true, "--repo": true, "--agent": true, "--template": true, "--branch": true}
	var flags []string
	for _, m := range flagRe.FindAllStringSubmatch(usage, -1) {
		flags = append(flags, m[1])
		valued[m[1]] = valued[m[1]] || m[2] != ""
	}
	if c.name == "help" {
		if len(rest) > 0 {
			return nil
		}
		var out []string
		for _, c := range commands() {
			out = append(out, strings.Fields(c.name)[0])
		}
		out = append(out, "aaa") // help aaa: the AAA workflow
		return words(out)
	}
	if strings.HasPrefix(cur, "-") {
		return words(flags)
	}
	work, ns, agent, repo := defaultWork, "", "", ""
	var pos []string
	for i := 0; i < len(rest); i++ {
		switch w := rest[i]; {
		case valued[w] && i+1 < len(rest):
			switch w {
			case "--work":
				work = rest[i+1]
			case "-n":
				ns = rest[i+1]
			case "--agent":
				agent = rest[i+1]
			case "--repo":
				repo = rest[i+1]
			}
			i++
		case strings.HasPrefix(w, "-"):
		default:
			pos = append(pos, w)
		}
	}
	w, err := expandWork(work)
	if err != nil {
		return nil
	}
	if len(rest) > 0 && valued[rest[len(rest)-1]] {
		if rest[len(rest)-1] == "-n" {
			return words(namespaceNames(w))
		}
		return nil // --repo, --work, ...: bash's own file names; --agent, --branch: free text
	}
	switch {
	case c.name == "review" && ns == "" && len(pos) == 0:
		return words(reviewTypes(w))
	case c.name == "review" && ns == "" && len(pos) == 1, c.name == "dlp-scan" && len(pos) == 0:
		return plainFiles(c.name)
	case c.name == "review" && ns != "" && len(pos) == 1:
		return words(reviewTypes(w))
	case (c.name == "finish" || c.name == "read-answer") && len(pos) == 0:
		return plainRuns(repo, c.name)
	case slices.Contains(runCommands, c.name) && len(pos) == 0 && (c.name != "review" || ns != ""):
		return runCandidates(w, ns, agent, c.name)
	}
	return nil
}

// namespaceNames are the namespaces set up below work.
func namespaceNames(work string) []string {
	nss, err := workdir.Namespaces(work)
	if err != nil {
		return nil
	}
	var out []string
	for _, ns := range nss {
		out = append(out, ns.String())
	}
	return out
}

// runCandidates are the runs of ns, or of every namespace for ns "" (of agent only, unless agent is ""), each with its
// state (get ans's, else get ask's) and whether cmd can act on it now (runFits). A namespace whose config doesn't load
// offers its runs without a mark.
func runCandidates(work, ns, agent, cmd string) []candidate {
	nss := []string{ns}
	if ns == "" {
		nss = namespaceNames(work)
	}
	g, err := config.LoadGlobal(work, os.LookupEnv)
	if err != nil {
		g = nil
	}
	var out []candidate
	for _, n := range nss {
		parsed, err := names.ParseNamespace(n)
		if err != nil {
			continue
		}
		var cfg *config.Namespace
		var automatic []string
		if g != nil {
			if cfg, _, err = config.LoadNamespace(work, parsed, g, os.LookupEnv); err == nil {
				automatic = cfg.Automatic
			} else {
				cfg = nil
			}
		}
		live := controller.Live(work, parsed)
		runs, err := runstate.List(work, parsed, agent, automatic, live)
		if err != nil {
			continue
		}
		for _, r := range runs {
			out = append(out, candidate{word: r.Name, state: runLabel(r), fits: runFits(cmd, work, parsed, g, cfg, r, live)})
		}
	}
	return out
}

// runLabel is a run's state in the listing: its answer status as get ans says it, else its ask status as get ask does.
func runLabel(r *runstate.Run) string {
	if r.Broken != nil {
		return "broken"
	}
	s := r.AnswerStatus()
	if s == "" {
		s = r.AskStatus()
	}
	return strings.Replace(s, " (gate)", ", gate", 1)
}

// runFits reports whether cmd would act on the run now, the mark: get ask when it is approved, get
// ans when its answer can be printed, describe and events show always; the other commands when decide allows their
// action (request.Hint: no ask lint, so the mark is a hint). exec asks as plinthctl exec does: a privileged script from
// the user's terminal. A fact that can't be read (ask.sh missing, unreadable or too large) gives no mark.
func runFits(cmd, work string, ns names.Namespace, g *config.Global, cfg *config.Namespace, r *runstate.Run, live runstate.Live) bool {
	if r.Broken != nil {
		return false
	}
	f := r.Facts
	switch cmd {
	case "describe", "events show":
		return true
	case "get ask":
		return r.Approved()
	case "get ans":
		return f.Answer == "dlp-ok" || f.Answer == "released" || (f.Answer == "DLP-FAIL" && f.DLPOverridden)
	}
	if g == nil || cfg == nil {
		return false
	}
	var actions []string
	if a, ok := hintActions[cmd]; ok {
		actions = []string{a}
	}
	if cmd == "review" {
		for _, t := range slices.Sorted(maps.Keys(g.Reviewers.Type)) {
			actions = append(actions, decide.Review+"."+t)
		}
	}
	terminal := false
	if cmd == "exec" {
		mode, _, err := runstate.Script(work, ns, r.Agent, r.Name)
		if err != nil {
			return false
		}
		terminal = mode.Privileged
	}
	for _, a := range actions {
		if d, err := request.Hint(work, ns, g, cfg, r.Agent, r.Name, a, live, terminal); err == nil && d.Outcome == decide.Allow {
			return true
		}
	}
	return false
}

// plainList is the runs of repo's AAA work directory (the current directory's for ""), none on any error.
func plainList(repo string) []plain.Run {
	if repo == "" {
		var err error
		if repo, err = os.Getwd(); err != nil {
			return nil
		}
	}
	runs, _ := plain.Runs(plain.WorkDir(repo))
	return runs
}

// plainState is a plain AAA run's state in the listing: its file names' state, with done and gate.
func plainState(r plain.Run) string {
	s := r.State
	if r.Done {
		s = "done, " + s
	}
	if r.Gate {
		s += ", gate"
	}
	return s
}

// plainRuns are the runs of repo's AAA work directory for finish and read-answer, every one with its state: finish
// fits a run that ended and isn't finished (answer[.FAIL].new.yml, not in done/), read-answer one with an answer to
// print (answer[.FAIL].dlp-ok.yml, nothing before it in read-answer's order).
func plainRuns(repo, cmd string) []candidate {
	var out []candidate
	for _, r := range plainList(repo) {
		fits := r.State == plain.StateDLPOK
		if cmd == "finish" {
			fits = !r.Done && r.State == plain.StateNew
		}
		out = append(out, candidate{word: r.Name, state: plainState(r), fits: fits})
	}
	return out
}

// plainFiles are the files of the current directory's AAA work directory that review and dlp-scan take, as paths from
// it (ai/work/[done/]<RUN>/<file>): every run's ask.sh and answer document, with the run's state. review fits a file
// its own check passes (plain.CheckReviewFile), dlp-scan an answer whose scan failed (.DLP-FAIL.yml: it scans it again).
func plainFiles(cmd string) []candidate {
	var out []candidate
	for _, r := range plainList("") {
		dir := path.Join("ai", "work", r.Name)
		if r.Done {
			dir = path.Join("ai", "work", "done", r.Name)
		}
		for _, f := range r.Files {
			p := path.Join(dir, f)
			var fits bool
			switch cmd {
			case "review":
				_, err := plain.CheckReviewFile(p)
				fits = err == nil
			case "dlp-scan":
				if f == runstate.AskFile {
					continue
				}
				fits = strings.HasSuffix(f, "."+plain.StateDLPFail+".yml")
			}
			out = append(out, candidate{word: p, state: plainState(r), fits: fits})
		}
	}
	return out
}

// reviewTypes are the reviewer types of the global config, none when it can't be loaded.
func reviewTypes(work string) []string {
	g, err := config.LoadGlobal(work, os.LookupEnv)
	if err != nil {
		return nil
	}
	return slices.Collect(maps.Keys(g.Reviewers.Type))
}
