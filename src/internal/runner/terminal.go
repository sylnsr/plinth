package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"plinth/internal/auth"
	"plinth/internal/decide"
	"plinth/internal/names"
	"plinth/internal/plain"
	"plinth/internal/request"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// Privileged runs run in the user's own terminal, never as the controller's child (plinthctl.README.md, Run modes,
// Privileged mode). The terminal's plinthctl exec takes the run's lock (LockRun), then asks the controller over its
// socket (controller.StartTerminal), which passes the exec to the namespace's runner; the runner decides as for any
// exec, records the start and hands back what to run; the terminal runs it and reports how it ended over the same
// connection, and the runner finishes the exec like any other (exec.end, the DLP scan, the request). The lock goes to
// the script too: while it is held, the run is running, whatever happened to the terminal's plinthctl or the controller.

// TerminalLock is the lock file of a run that runs in the user's terminal, below <work>.
func TerminalLock(ns names.Namespace, agent, run string) string {
	return filepath.Join(ns.Owner, ns.Repo, "state", "terminal", agent, run+".lock")
}

// LockFD is the descriptor the script gets the run's lock on: not 3 or 4, which the ask template takes for itself.
const LockFD = 9

// TerminalOp is one "exec" of a run in the user's terminal, from the controller's socket, for the namespace's runner:
// its loop takes it up (terminalExec), since it owns the run's state.
type TerminalOp struct {
	Agent, Run string

	// Reply takes the runner's answer. Once it is OK, the runner owns the connection: End waits for the terminal's
	// report of how the script ended (ok is false when the connection ended without one), Close drops it.
	Reply chan<- TerminalReply
	End   func() (result string, ok bool)
	Close func() error

	// Turn holds one token. The runner takes it when it takes the op up, the socket when it stops waiting; whoever has
	// it decides, so a run whose terminal was told it didn't start never starts.
	Turn chan struct{}
}

// Claim takes the op's token: false when the other side has it.
func (op TerminalOp) Claim() bool {
	select {
	case <-op.Turn:
		return true
	default:
		return false
	}
}

// TerminalReply is the runner's answer to a TerminalOp: OK and what to run, or the rule (when decide refused) and the
// reason.
type TerminalReply struct {
	OK     bool
	Rule   string
	Reason string
	Script string // the run directory's ask.sh, absolute: what runs
	Sum    string // its sha256, as checked right before the start
	Dir    string // the run directory, absolute
	Lib    string // the run's AAA library, absolute: $AAA_LIB
}

// Terminal passes op to the runner's loop: false while the loop has another one it hasn't taken up yet.
func (l *Runner) Terminal(op TerminalOp) bool {
	select {
	case l.term <- op:
		return true
	default:
		return false
	}
}

// terminalExec starts a run in the user's terminal, in the runner's loop: the terminal must hold the run's lock;
// decide is asked as for any exec, from the terminal (decide.Request.Terminal); then exec.start goes into the event log,
// running.yml into the run directory and exec.running into its requests, ask.sh is checked once more, and the terminal
// gets what to run, with its AAA library (raw/<agent>/<RUN>/lib.bash, $AAA_LIB). watchTerminal waits for its end.
// An op whose connection stopped waiting is dropped untouched.
func (l *Runner) terminalExec(root *os.Root, op TerminalOp) {
	refuse := func(rule, format string, a ...any) {
		op.Reply <- TerminalReply{Rule: rule, Reason: fmt.Sprintf(format, a...)}
	}
	agent, run := op.Agent, op.Run
	key := agent + "/" + run
	if !op.Claim() {
		l.runLog(agent, run).Warn("exec in the user's terminal not started: the terminal was told the controller didn't answer in time")
		return
	}
	if l.child(key) != nil {
		refuse("state.valid", "run %s is running", run)
		return
	}
	if !lockHeld(root, TerminalLock(l.ns, agent, run)) {
		refuse("", "the terminal doesn't hold the run's lock (%s)", TerminalLock(l.ns, agent, run))
		return
	}
	d, r, err := request.DecideTerminal(l.Work, l.ns, l.Global, l.cfg, agent, run, decide.Exec, l.live)
	if err != nil {
		refuse("", "%v", err)
		return
	}
	if _, err := l.event(auth.HostUser().String(), decide.Exec, key, string(d.Outcome), d.Rule, d.Reason+"; from the user's terminal"); err != nil {
		refuse("", "%v", err)
		return
	}
	if d.Outcome != decide.Allow {
		l.runLog(agent, run).Warn("exec in the user's terminal refused", "outcome", d.Outcome, "rule", d.Rule, "reason", d.Reason)
		refuse(d.Rule, "%s", d.Reason)
		return
	}
	if l.cfg.Enabled[agent] == "" {
		refuse("", "agent %s is not enabled in %s", agent, l.ns)
		return
	}
	dir := runstate.RunDir(l.ns, agent, run)
	_, sum, err := runstate.CheckScript(l.Work, l.ns, agent, run)
	if err != nil {
		refuse("", "%v", err)
		return
	}
	reqDir := request.RunDir(l.ns, agent, run)
	if err := ensureDirs(root, reqDir); err != nil {
		refuse("", "%v", err)
		return
	}
	// The AAA library, its text filter this controller's own binary, as the user's terminal finds it.
	rawDir := filepath.Join(l.ns.Owner, l.ns.Repo, Raw, agent, run)
	bin, err := selfBin()
	if err == nil {
		err = ensureDirs(root, rawDir)
	}
	if err == nil {
		err = safefs.Replace(root, rawDir, LibFile, plain.ControllerLib(bin), 0o600)
	}
	if err != nil {
		refuse("", "%v", err)
		return
	}
	attempt, err := l.again(root, r)
	if err != nil {
		refuse("", "%v", err)
		return
	}
	before, files := startNames(r)
	// Only what the controller knows: the confirmation is plinthctl exec's, which the controller can't see; any process of
	// the user's that holds the run's lock can ask over the socket.
	if _, err := l.event(Caller, "exec.start", key, "ok", "allow", "ask.sh sha256 "+sum+"; mode privileged"+attemptNote(attempt)+
		"; requested over the socket from the user's terminal (plinthctl exec asks the user to type yes)"+startFiles+strings.Join(files, " ")); err != nil {
		refuse("", "%v", err)
		return
	}
	if err := safefs.WriteNew(root, dir, runstate.Running, nil, 0o600); err != nil {
		refuse("", "%v", err)
		return
	}
	if err := safefs.WriteNew(root, reqDir, decide.Exec+"."+request.Running, nil, 0o600); err != nil {
		safefs.Remove(root, filepath.Join(dir, runstate.Running))
		refuse("", "%v", err)
		return
	}
	c := &child{agent: agent, run: run, before: before, attempt: attempt, ended: make(chan struct{})}
	l.mu.Lock()
	l.execs[key] = c
	l.mu.Unlock()
	l.wg.Add(1)
	go l.watchTerminal(c, op)
	l.runLog(agent, run).Info("exec starting in the user's terminal")
	op.Reply <- TerminalReply{OK: true, Script: filepath.Join(l.Work, dir, runstate.AskFile), Sum: sum, Dir: filepath.Join(l.Work, dir),
		Lib: filepath.Join(l.Work, rawDir, LibFile)}
}

// watchTerminal waits for the terminal's report of how the script ended. A terminal gone without one leaves the run
// running until its lock is free: the script holds it too. When the controller stops first, the exec stays .running, and
// the next controller finishes it once the lock is free (act).
func (l *Runner) watchTerminal(c *child, op TerminalOp) {
	defer l.wg.Done()
	defer op.Close()
	endc := make(chan string, 1)
	go func() {
		if result, ok := op.End(); ok {
			endc <- result
		}
		close(endc)
	}()
	select {
	case result, ok := <-endc:
		if ok {
			c.result = resultText(result)
			break
		}
		lock := TerminalLock(l.ns, c.agent, c.run)
		for lockHeldAt(l.Work, lock) {
			select {
			case <-l.Stopping.Done():
				return
			case <-time.After(time.Second):
			}
		}
		c.result = "the terminal's plinthctl ended without saying how the script ended"
	case <-l.Stopping.Done():
		return
	}
	close(c.ended)
}

// resultText is how the terminal says the script ended, kept to one short printable line: it is the user's report,
// and goes into exec.end.
func resultText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120]
	}
	return "in the user's terminal: " + s
}

// lockHeld reports whether some process holds the lock file name below root (a shared flock can't be had): a missing
// file is no lock.
func lockHeld(root *os.Root, name string) bool {
	f, err := safefs.OpenFile(root, name, os.O_RDONLY)
	if err != nil {
		return false
	}
	defer f.Close() // releases a lock taken below
	return errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

func lockHeldAt(work, name string) bool {
	root, err := os.OpenRoot(work)
	if err != nil {
		return false
	}
	defer root.Close()
	return lockHeld(root, name)
}

// LockRun takes the run's lock for the terminal, without waiting: another terminal running it holds it.
func LockRun(work string, ns names.Namespace, agent, run string) (*os.File, error) {
	if !names.ValidAgent(agent) || !names.ValidRun(run) {
		return nil, fmt.Errorf("%s/%s: not an agent and a run", agent, run)
	}
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := TerminalLock(ns, agent, run)
	if err := ensureDirs(root, filepath.Dir(name)); err != nil {
		return nil, err
	}
	f, err := safefs.OpenCreate(root, filepath.Dir(name), filepath.Base(name), os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("run %s runs in another terminal", run)
		}
		return nil, err
	}
	return f, nil
}

// TerminalEnv is the environment of a script run in the user's terminal: the user's own, without Plinth's variables
// and secrets (as for the controller's child) and without AAA_QUIET, since the user is there.
func TerminalEnv(environ, refs []string) []string {
	env := scriptEnv(environ, refs)
	return env[:len(env)-1] // scriptEnv's AAA_QUIET=1
}

// ExitText is how a script's Wait error reads in exec.end: "exit 0", "exit 3", "signal 2 (interrupt)".
func ExitText(err error) string { return exitText(err) }
