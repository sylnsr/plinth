package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"plinth/internal/config"
	"plinth/internal/decide"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
)

// privilegedCfg is globalCfg with privileged mode allowed.
var privilegedCfg = strings.Replace(globalCfg, "privileged: false", "privileged: true", 1)

// terminalRun takes in a privileged script whose body is body, with its failed review overridden: ready to run.
func terminalRun(t *testing.T, work, body string) string {
	t.Helper()
	run := submittedAs(t, work, "PRIVILEGED=1", body).Name
	queue(t, work, run, decide.OverrideReview)
	settled(t, work, run)
	return run
}

// runHere runs the granted script in its run directory, as plinthctl exec does, with the run's AAA library (its text
// filter this test binary, TestMain), and reports how it ended.
func runHere(t *testing.T, tr *TerminalRun) {
	t.Helper()
	t.Setenv("AAA_QUIET", "0") // plinthctl exec takes it out (TerminalEnv)
	cmd := exec.Command("bash", tr.Script)
	cmd.Dir = tr.Dir
	cmd.Env = append(os.Environ(), "AAA_LIB="+tr.Lib)
	if err := tr.End(cmd.Run()); err != nil {
		t.Errorf("end: %v", err)
	}
}

// TestTerminalExec: a privileged run runs only from the user's terminal, which holds its lock; the controller records the
// start, and once the terminal reports the end it finishes the exec as for its own child: exec.end, the DLP scan, the
// request.
func TestTerminalExec(t *testing.T) {
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(privilegedCfg), 0o600)
	enabled(t, work, "main")
	start(t, Options{Work: work})
	run := terminalRun(t, work, `say "uid: $(id -u)"`+"\n")
	queue(t, work, run, decide.Exec)
	if f := settled(t, work, run); !strings.Contains(f[decide.Exec], "Rule: exec.mode") || !strings.Contains(f[decide.Exec], "only in your own terminal") {
		t.Fatalf("exec as the controller's child: %v", f)
	}
	ctx := context.Background()
	if _, err := StartTerminal(ctx, work, ns, "main", run); err == nil || !strings.Contains(err.Error(), "doesn't hold the run's lock") {
		t.Errorf("without the lock: %v", err)
	}
	lock, err := runner.LockRun(work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.LockRun(work, ns, "main", run); err == nil || !strings.Contains(err.Error(), "runs in another terminal") {
		t.Errorf("a second lock: %v", err)
	}
	tr, err := StartTerminal(ctx, work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(work, request.RunDir(ns, "main", run), decide.Exec+"."+request.Running)) {
		t.Error("no exec.running while the script runs")
	}
	if tr.Dir != filepath.Join(work, runstate.RunDir(ns, "main", run)) || filepath.Base(tr.Script) != "ask.sh" || len(tr.Sum) != 64 {
		t.Errorf("granted %+v", tr)
	}
	runHere(t, tr)
	lock.Close()
	// Without Ollama the DLP scan can't ask the model: the answer stays unscanned, the exec unfinished.
	eventually(t, "dlp.scan unavailable", func() bool { return slices.Contains(eventList(t, work), "runner dlp.scan unavailable dlp.unavailable") })
	r := load(t, work, run)
	if r.Facts.Answer != "new" || finished(work, run, decide.Exec) {
		t.Errorf("answer %q, exec finished %v: want it unscanned", r.Facts.Answer, finished(work, run, decide.Exec))
	} else if out := outputOf(t, filepath.Join(tr.Dir, r.AnswerFile())); !strings.Contains(out, "uid: "+strconv.Itoa(os.Getuid())+"\n") {
		t.Errorf("answer: %q", out)
	}
	if filepath.Base(tr.Lib) != runner.LibFile || !strings.Contains(tr.Lib, filepath.Join(runner.Raw, "main", run)) {
		t.Errorf("the run's AAA library %q", tr.Lib)
	}
	if s := lastEvent(t, work, "exec.start"); !strings.Contains(s, "; mode privileged; requested over the socket from the user's terminal (plinthctl exec asks the user to type yes); run directory: ") {
		t.Errorf("exec.start = %q", s)
	}
	if s := lastEvent(t, work, "exec.end"); !strings.Contains(s, "in the user's terminal: exit 0") {
		t.Errorf("exec.end = %q", s)
	}
	if got := eventList(t, work); !slices.Contains(got, "user exec deny exec.mode") || !slices.Contains(got, "user exec allow allow") {
		t.Errorf("events:\n%s", strings.Join(got, "\n"))
	}
	lock, err = runner.LockRun(work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := StartTerminal(ctx, work, ns, "main", run); err == nil || !strings.Contains(err.Error(), "state.valid") {
		t.Errorf("a second run: %v", err)
	}
}

// TestTerminalWait: an exec the namespace's runner takes up only after the socket stopped waiting for it never starts,
// so the terminal's "nothing started" holds; one the runner took up in time gets the runner's answer, however late.
func TestTerminalWait(t *testing.T) {
	defer func(w time.Duration) { termWait = w }(termWait)
	termWait = 100 * time.Millisecond
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &process{Options: Options{Work: work}, ctx: ctx, termc: make(chan termOp)}
	ln, err := p.listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go p.serve(ln)
	for _, c := range []struct {
		name    string
		inTime  bool // the runner takes the op up before the wait ends
		claimed bool
		want    string
	}{
		{"late", false, false, "the controller didn't answer within a minute; nothing started"},
		{"in time, answered late", true, true, "refused by the runner"},
	} {
		got := make(chan error, 1)
		go func() {
			_, err := StartTerminal(ctx, work, ns, "main", "keen-tapir")
			got <- err
		}()
		op := <-p.termc
		if !c.inTime {
			time.Sleep(3 * termWait)
		}
		if claimed := op.Claim(); claimed != c.claimed {
			t.Errorf("%s: the runner's claim = %v, want %v", c.name, claimed, c.claimed)
		}
		if c.claimed {
			time.Sleep(3 * termWait)
			op.Reply <- runner.TerminalReply{Reason: "refused by the runner"}
		}
		if err := <-got; err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the terminal got %v, want %q", c.name, err, c.want)
		}
	}
}

// TestTerminalNormalRun: a run that isn't privileged doesn't run in the terminal; nor does a privileged one where the
// namespace doesn't allow privileged mode.
func TestTerminalNormalRun(t *testing.T) {
	work := setup(t)
	enabled(t, work, "main")
	start(t, Options{Work: work})
	run := submitted(t, work, "say hi\n").Name
	lock, err := runner.LockRun(work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	queue(t, work, run, decide.OverrideReview)
	settled(t, work, run)
	if _, err := StartTerminal(context.Background(), work, ns, "main", run); err == nil || !strings.Contains(err.Error(), "isn't privileged") {
		t.Errorf("a normal run: %v", err)
	}
	if exists(filepath.Join(work, request.RunDir(ns, "main", run), decide.Exec+"."+request.Running)) {
		t.Error("a refused run is running")
	}
}

// TestTerminalGone: a terminal that goes without saying how the script ended leaves the run running while its lock is
// held (the script holds it too); once the lock is free, the controller finishes the exec.
func TestTerminalGone(t *testing.T) {
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(privilegedCfg), 0o600)
	enabled(t, work, "main")
	start(t, Options{Work: work})
	run := terminalRun(t, work, "")
	lock, err := runner.LockRun(work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := StartTerminal(context.Background(), work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()
	time.Sleep(2 * time.Second) // two polls: the run must stay running
	if finished(work, run, decide.Exec) {
		t.Fatal("finished while the lock is held")
	}
	lock.Close()
	eventually(t, "the exec finished", func() bool { return finished(work, run, decide.Exec) })
	if s := lastEvent(t, work, "exec.end"); !strings.Contains(s, "ended without saying how the script ended") {
		t.Errorf("exec.end = %q", s)
	}
}

// TestTerminalControllerRestart: a controller that stops while a script runs in the terminal leaves its exec .running; the
// next controller finishes it only once the run's lock is free.
func TestTerminalControllerRestart(t *testing.T) {
	work := setup(t)
	writeFile(t, filepath.Join(work, config.FileName), []byte(privilegedCfg), 0o600)
	enabled(t, work, "main")
	_, stop := start(t, Options{Work: work})
	run := terminalRun(t, work, "")
	lock, err := runner.LockRun(work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := StartTerminal(context.Background(), work, ns, "main", run)
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	tr.End(nil) // the controller is gone: no one reads it
	start(t, Options{Work: work})
	eventually(t, "the leftover seen", func() bool {
		return strings.Contains(logText(work), "still running in the user's terminal")
	})
	if finished(work, run, decide.Exec) {
		t.Fatal("finished while the lock is held")
	}
	lock.Close()
	eventually(t, "the exec finished", func() bool { return finished(work, run, decide.Exec) })
	if s := lastEvent(t, work, "exec.end"); !strings.Contains(s, "the controller stopped") {
		t.Errorf("exec.end = %q", s)
	}
	if _, err := os.Stat(filepath.Join(work, filepath.FromSlash(Socket))); err != nil {
		t.Errorf("the socket: %v", err)
	}
}
