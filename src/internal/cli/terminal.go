package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"plinth/internal/controller"
	"plinth/internal/decide"
	"plinth/internal/logfile"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
)

// runTerminal runs a privileged run in this terminal (plinthctl.README.md, Run modes, Privileged mode): decide
// first, the way the controller will; then what the script can do, and the user types privileged; the run's lock; the
// controller's go-ahead over its socket (it records the start); the script, as the user, in its run directory; and how it
// ended, back to the controller, which finishes the exec and scans the answer.
func runTerminal(c *command, a *runsArgs, r *runstate.Run, mode runstate.Mode, stdout, stderr io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "plinthctl %s: "+format+"\n", append([]any{c.name}, args...)...)
		return exitFail
	}
	d, _, err := request.DecideTerminal(a.work, a.ns, a.g, a.cfg, r.Agent, r.Name, decide.Exec, a.live())
	if err != nil {
		return fail("%v", err)
	}
	if d.Outcome != decide.Allow {
		return fail("%s: %s by rule %s: %s", r.Name, d.Outcome, d.Rule, d.Reason)
	}
	if !controllerRunning(a.work) {
		return fail("the controller is not running: a privileged run needs it, to record the run and scan its answer (plinthctl controller start)")
	}
	fmt.Fprint(stdout, privilegedBox(r.Name, r.Agent, mode.Interactive))
	if !confirm(stdout, "Run "+r.Name+" as you, in this terminal") {
		fmt.Fprintln(stdout, "Nothing run.")
		return exitFail
	}
	lock, err := runner.LockRun(a.work, a.ns, r.Agent, r.Name)
	if err != nil {
		return fail("%v", err)
	}
	defer lock.Close()
	t, err := controller.StartTerminal(context.Background(), a.work, a.ns, r.Agent, r.Name)
	if err != nil {
		return fail("%s: %v", r.Name, err)
	}
	// What runs is the run directory's ask.sh (the ask template finds its run directory from its own path), checked
	// here once more against the sum the controller checked right before the start.
	if b, err := os.ReadFile(t.Script); err != nil || sha256Hex(b) != t.Sum {
		t.End(errors.New("not run: ask.sh changed after the controller checked it"))
		return fail("%s: ask.sh changed after the controller checked it; not run", r.Name)
	}
	cmd := exec.Command("bash", t.Script)
	cmd.Dir = t.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The run's AAA library, which the controller wrote for it.
	cmd.Env = append(runner.TerminalEnv(os.Environ(), a.g.EnvRefs), "AAA_LIB="+t.Lib)
	extra := make([]*os.File, runner.LockFD-2) // the lock on fd LockFD: held as long as the script, or a process it left, runs
	extra[len(extra)-1] = lock
	cmd.ExtraFiles = extra
	// Ctrl-C and Ctrl-\ are the script's: caught here (an ignored signal would stay ignored in the script).
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	werr := cmd.Run()
	ended := runner.ExitText(werr)
	if err := t.End(werr); err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: the controller didn't get how %s ended (%v): its next start finishes the run, once this terminal lets go of it\n", c.name, r.Name, err)
	}
	logfile.New(a.work, logfile.CLI).With("command", c.name, "ns", a.ns, "agent", r.Agent, "run", r.Name).Info("ran in this terminal", "ended", ended)
	fmt.Fprintf(stdout, "%s ended in this terminal: %s. The controller scans its answer; Done releases it to the agent.\n", r.Name, ended)
	if werr != nil {
		return exitFail
	}
	return exitOK
}

// privilegedBox says what a privileged run can do, for the user who runs it (docs/privileged-mode.md).
func privilegedBox(run, agent string, interactive bool) string {
	asks := ""
	if interactive {
		asks = "; INTERACTIVE=1: it will ask you things"
	}
	return fmt.Sprintf("%s (agent %s) is a privileged run (PRIVILEGED=1%s). It runs here, as you, with nothing in the way:\n"+
		"  - it can read and change any of your files, your keys and passwords included;\n"+
		"  - it can use the network and your SSH agent, and log in to anything you can;\n"+
		"  - it can change Plinth itself: its settings (network or privileged mode for later runs), its event log and its\n"+
		"    own state, such as marking itself done. Every event records the settings' fingerprint, so a change shows.\n"+
		"Read the whole script first (plinthctl get ask). The event log records the run.\n", run, agent, asks)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
