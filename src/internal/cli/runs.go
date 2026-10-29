package cli

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"plinth/internal/asklint"
	"plinth/internal/config"
	"plinth/internal/controller"
	"plinth/internal/names"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

func runAskLint(c *command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	plinth := flags.Bool("plinth", false, "the submission check: no relative path, nothing naming the work directory")
	work := flags.String("work", defaultWork, "Plinth's work directory, which a script must not name (with --plinth)")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageError(c, stderr, "one ask script, please")
	}
	script, err := os.ReadFile(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl ask lint: %v\n", err)
		return exitFail
	}
	o := asklint.Options{Plinth: *plinth}
	if run := filepath.Base(filepath.Dir(pos[0])); names.ValidRun(run) {
		o.RunName = run
	}
	if *plinth {
		if o.Work, err = expandWork(*work); err != nil {
			fmt.Fprintf(stderr, "plinthctl ask lint: %v\n", err)
			return exitFail
		}
	}
	findings := asklint.Check(script, o)
	for _, f := range findings {
		fmt.Fprintf(stdout, "%s: %s\n", pos[0], f)
	}
	for _, w := range asklint.Warnings(script) {
		fmt.Fprintf(stdout, "%s: layout warning: %s\n", pos[0], w)
	}
	for _, w := range asklint.PlinthctlWarnings(script) {
		fmt.Fprintf(stdout, "%s: plinthctl warning: %s\n", pos[0], w)
	}
	if len(findings) > 0 {
		return exitFail
	}
	return exitOK
}

// runsArgs are the parsed arguments of a command about runs.
type runsArgs struct {
	work  string
	ns    names.Namespace
	agent string
	run   string
	g     *config.Global
	cfg   *config.Namespace
}

// runsFlags parses -n (namespaceArg), --agent and --work and at most one <RUN>, and loads the configs (for the automatic reviews).
func runsFlags(c *command, args []string, stdout, stderr io.Writer, needRun bool) (*runsArgs, int, bool) {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	n := flags.String("n", "", "the namespace <owner>/<repo> (default: the origin of the checkout you are in)")
	agent := flags.String("agent", "", "only this agent's runs")
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return nil, code, false
	}
	switch {
	case len(pos) > 1:
		return nil, usageError(c, stderr, "too many arguments"), false
	case needRun && len(pos) == 0:
		return nil, usageError(c, stderr, "<RUN> is required"), false
	case *agent != "" && !names.ValidAgent(*agent):
		return nil, usageError(c, stderr, fmt.Sprintf("%q is not an agent name", *agent)), false
	}
	a := &runsArgs{agent: *agent}
	if len(pos) == 1 {
		if a.run = pos[0]; !names.ValidRun(a.run) {
			return nil, usageError(c, stderr, fmt.Sprintf("%q is not a run name", a.run)), false
		}
	}
	var err error
	if a.work, err = expandWork(*work); err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return nil, exitFail, false
	}
	if a.ns, code, ok = namespaceArg(c, *n, a.work, stderr); !ok {
		return nil, code, false
	}
	if a.g, err = config.LoadGlobal(a.work, os.LookupEnv); err == nil {
		a.cfg, _, err = config.LoadNamespace(a.work, a.ns, a.g, os.LookupEnv)
	}
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return nil, exitFail, false
	}
	return a, exitOK, true
}

// live is controller.Live for the run's namespace.
func (a *runsArgs) live() runstate.Live { return controller.Live(a.work, a.ns) }

// find loads the run a.run of any agent (or of a.agent).
func (a *runsArgs) find() (*runstate.Run, error) {
	runs, err := runstate.List(a.work, a.ns, a.agent, a.cfg.Automatic, a.live())
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.Name == a.run {
			if r.Broken != nil {
				return nil, fmt.Errorf("run %s is broken: %w", r.Name, r.Broken)
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("no run %s in %s", a.run, a.ns)
}

func runGet(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := runsFlags(c, args, stdout, stderr, false)
	if !ok {
		return code
	}
	if a.run != "" {
		return usageError(c, stderr, "get takes no <RUN>; use get ask or get ans")
	}
	if code := listRuns(a, stdout, stderr, false); code != exitOK {
		return code
	}
	fmt.Fprintln(stdout)
	return listRuns(a, stdout, stderr, true)
}

func runGetAsk(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := runsFlags(c, args, stdout, stderr, false)
	if !ok {
		return code
	}
	if a.run == "" {
		return listRuns(a, stdout, stderr, false)
	}
	r, err := a.find()
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl get ask: %v\n", err)
		return exitFail
	}
	if !r.Approved() {
		fmt.Fprintf(stderr, "Run not approved (%s)\n", r.AskStatus())
		return exitFail
	}
	return printRunFile(a, r, runstate.AskFile, stdout, stderr)
}

func runGetAns(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := runsFlags(c, args, stdout, stderr, false)
	if !ok {
		return code
	}
	if a.run == "" {
		return listRuns(a, stdout, stderr, true)
	}
	r, err := a.find()
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl get ans: %v\n", err)
		return exitFail
	}
	switch f := r.Facts; {
	case f.Answer == "" && r.AskStatus() == "no-answer":
		fmt.Fprintf(stderr, "%s: no answer: its last exec ended without one; next: plinthctl exec %s runs it again\n", r.Name, r.Name)
		return exitFail
	case f.Answer == "":
		fmt.Fprintf(stderr, "%s: no answer yet (%s); next: get ans again once it has run (plinthctl get ask %s says where it stands)\n", r.Name, r.AskStatus(), r.Name)
		return exitFail
	case f.Answer == "new" && waiting(a.work, r).String() != "":
		fmt.Fprintf(stderr, "%s: the answer is not scanned yet, %s - not shown; next: get ans again once the model can be asked "+
			"(plinthctl ollama status, plinthctl ollama-mcp status)\n", r.Name, waiting(a.work, r))
		return exitFail
	case f.Answer == "new":
		fmt.Fprintf(stderr, "%s: the answer is being scanned; next: get ans again once the controller has scanned it\n", r.Name)
		return exitFail
	case f.Answer == "DLP-FAIL" && !f.DLPOverridden:
		fmt.Fprintf(stderr, "%s: the DLP scan failed - not shown; next: read it with its masks in the TUI or the web UI, then plinthctl override dlp %s or scrub it\n", r.Name, r.Name)
		return exitFail
	case f.Answer == "INVALID":
		fmt.Fprintf(stderr, "%s: the answer is INVALID, not a valid document (a bug, never data) - not shown; next: tell the agent, whose rerun is a new run (the dlp.scan event says why)\n", r.Name)
		return exitFail
	}
	return printRunFile(a, r, r.AnswerFile(), stdout, stderr)
}

func printRunFile(a *runsArgs, r *runstate.Run, name string, stdout, stderr io.Writer) int {
	root, err := os.OpenRoot(a.work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl: %v\n", err)
		return exitFail
	}
	defer root.Close()
	b, err := safefs.ReadFile(root, filepath.Join(runstate.RunDir(a.ns, r.Agent, r.Name), name), 16<<20)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl: %v\n", err)
		return exitFail
	}
	stdout.Write(b)
	return exitOK
}

// listRuns prints the runs (answers: only those with an answer) as a table.
func listRuns(a *runsArgs, stdout, stderr io.Writer, answers bool) int {
	runs, err := runstate.List(a.work, a.ns, a.agent, a.cfg.Automatic, a.live())
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl: %v\n", err)
		return exitFail
	}
	head := "ASK STATUS"
	if answers {
		head = "ANSWER STATUS"
	}
	fmt.Fprintf(stdout, "%-22s %-12s %-18s %s\n", "RUN", "AGENT", head, "AGE")
	for _, r := range runs {
		status := r.AskStatus()
		if answers {
			if status = r.AnswerStatus(); status == "" {
				continue
			}
			if r.Facts.Answer == "new" && waiting(a.work, r).String() != "" {
				status = runner.WaitingStatus
			}
		}
		fmt.Fprintf(stdout, "%-22s %-12s %-18s %s\n", r.Name, r.Agent, status, age(r.Newest))
	}
	return exitOK
}

func runDescribe(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := runsFlags(c, args, stdout, stderr, true)
	if !ok {
		return code
	}
	r, err := a.find()
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl describe: %v\n", err)
		return exitFail
	}
	header, mode, err := runstate.Header(a.work, a.ns, r.Agent, r.Name)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl describe: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(stdout, "Run:         %s\nNamespace:   %s\nAgent:       %s\n", r.Name, a.ns, r.Agent)
	fmt.Fprintf(stdout, "Ask status:  %s\n", r.AskStatus())
	if s := r.AnswerStatus(); s != "" {
		if w := waiting(a.work, r).String(); r.Facts.Answer == "new" && w != "" {
			s = w
		}
		fmt.Fprintf(stdout, "Answer:      %s\n", s)
	}
	fmt.Fprintf(stdout, "Mode:        %s\n", mode.Name())
	fmt.Fprintf(stdout, "Interactive: %v\n", mode.Interactive)
	if mode.Network {
		fmt.Fprintf(stdout, "Network:     %s\n", accepted(r.Facts.NetworkAccepted))
	}
	if i := strings.LastIndex(r.Name, "-"); i > 0 && names.ValidIssue(r.Name[i+1:]) {
		fmt.Fprintf(stdout, "Issue:       %s\n", r.Name[i+1:])
	}
	fmt.Fprintln(stdout, "Header:")
	for _, h := range header {
		fmt.Fprintf(stdout, "  %s\n", h)
	}
	fmt.Fprintln(stdout, "Reviews:")
	for _, t := range a.cfg.Automatic {
		v := r.Reviews[t]
		if v == "" {
			v = "pending"
		}
		fmt.Fprintf(stdout, "  %-12s %s (automatic)\n", t, v)
	}
	for _, t := range slices.Sorted(maps.Keys(r.Reviews)) {
		if !slices.Contains(a.cfg.Automatic, t) {
			fmt.Fprintf(stdout, "  %-12s %s\n", t, r.Reviews[t])
		}
	}
	if r.Facts.ReviewOverridden {
		fmt.Fprintln(stdout, "  review overridden")
	}
	if r.Facts.DLPOverridden {
		fmt.Fprintln(stdout, "  DLP scan overridden")
	}
	fmt.Fprintln(stdout, "Files:")
	for _, f := range r.Files {
		fmt.Fprintf(stdout, "  %-32s %8d  %s\n", f.Name, f.Size, f.Mod.Format(time.RFC3339))
	}
	reqs, err := request.List(a.work, a.ns, r.Agent, r.Name)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl describe: %v\n", err)
		return exitFail
	}
	if len(reqs) > 0 {
		fmt.Fprintln(stdout, "Requests:")
	}
	for _, q := range reqs {
		if q.Reason != "" {
			fmt.Fprintf(stdout, "  %-16s %s: %s\n", q.Action, q.State, q.Reason)
		} else {
			fmt.Fprintf(stdout, "  %-16s %s\n", q.Action, q.State)
		}
	}
	return exitOK
}

// age is a short duration since t: 45s, 12m, 3h, 5d.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// accepted says whether a network run is accepted (accept.network.md) or still needs it.
func accepted(yes bool) string {
	if yes {
		return "accepted"
	}
	return "not accepted yet (plinthctl accept network)"
}

// waiting is what run's unscanned answer waits for, from the event log (runner.ScanWaiting); the zero value when
// the log can't be read.
func waiting(work string, r *runstate.Run) runner.Waiting {
	w, _ := runner.ScanWaiting(work, r.NS, r.Agent, r.Name)
	return w
}
