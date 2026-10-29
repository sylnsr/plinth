package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"plinth/internal/container"
	"plinth/internal/decide"
	"plinth/internal/doc"
	"plinth/internal/logfile"
	"plinth/internal/request"
	"plinth/internal/runner"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// runExec queues the exec of a run for the controller, or runs a privileged one in this terminal (runTerminal).
func runExec(c *command, args []string, stdout, stderr io.Writer) int {
	a, code, ok := runsFlags(c, args, stdout, stderr, true)
	if !ok {
		return code
	}
	if found, err := a.find(); err == nil {
		if mode, _, err := runstate.Script(a.work, a.ns, found.Agent, found.Name); err == nil && mode.Privileged {
			return runTerminal(c, a, found, mode, stdout, stderr)
		}
	}
	return queueRequest(c, args, stdout, stderr, decide.Exec, nil)
}

func runDone(c *command, args []string, stdout, stderr io.Writer) int {
	return queueRequest(c, args, stdout, stderr, decide.Done, nil)
}

func runSave(c *command, args []string, stdout, stderr io.Writer) int {
	return queueRequest(c, args, stdout, stderr, decide.Save, nil)
}

// runReport names plinth.repo and what Report will post there, masked first, then asks for confirmation.
func runReport(c *command, args []string, stdout, stderr io.Writer) int {
	return queueRequest(c, args, stdout, stderr, decide.Report, func(a *runsArgs, r *runstate.Run, out io.Writer) bool {
		if a.g.Ollama.Host == "" {
			fmt.Fprintln(out, "Report is not available without Ollama: everything it posts is masked first (ollama.host).")
			return false
		}
		fmt.Fprint(out, runner.ReportBox(a.g.Plinth.Repo, r))
		return confirm(out, "Report "+r.Name)
	})
}

func runScrub(c *command, args []string, stdout, stderr io.Writer) int {
	return queueRequest(c, args, stdout, stderr, decide.Scrub, func(a *runsArgs, r *runstate.Run, out io.Writer) bool {
		fmt.Fprintf(out, "Scrub %s (agent %s) deletes, for good: its run directory (%d files), the DLP scan's untouched original\n"+
			"and its request files. The event log keeps its lines (names and decisions, no content).\n", r.Name, r.Agent, len(r.Files))
		return confirm(out, "Are you sure? Scrub "+r.Name)
	})
}

func runOverrideReview(c *command, args []string, stdout, stderr io.Writer) int {
	return queueRequest(c, args, stdout, stderr, decide.OverrideReview, func(a *runsArgs, r *runstate.Run, out io.Writer) bool {
		fmt.Fprintf(out, "Failed review(s) of %s (agent %s):\n", r.Name, r.Agent)
		for _, t := range r.Facts.ReviewsFailed {
			file := "review." + t + ".FAILED.md"
			if !slices.ContainsFunc(r.Files, func(f runstate.File) bool { return f.Name == file }) {
				file = "review." + t + ".md" // a Do not run verdict
			}
			fmt.Fprintf(out, "  %-12s %s: %s\n", t, file, firstLine(a, r, file))
		}
		fmt.Fprintln(out, "Overriding lets exec run the script without a passing review. The event log records it.")
		return confirm(out, "Override the review of "+r.Name)
	})
}

func runOverrideDLP(c *command, args []string, stdout, stderr io.Writer) int {
	return queueRequest(c, args, stdout, stderr, decide.OverrideDLP, func(a *runsArgs, r *runstate.Run, out io.Writer) bool {
		why := "no reason in the answer's status.dlp"
		b, _ := readRunFile(a, r, r.AnswerFile())
		if s, ok := strings.CutPrefix(runstate.DLPLine(b), "DLP scan: FAILED - "); ok {
			why = s
		}
		fmt.Fprintf(out, "The DLP scan of %s's answer failed: %s\n", r.Name, why)
		fmt.Fprintln(out, "Overriding lets Done release the answer to the agent as it is (with the fixed-pattern masks). Check it in the TUI\nor the web UI first. The event log records it.")
		return confirm(out, "Override the DLP scan of "+r.Name)
	})
}

func runAcceptNetwork(c *command, args []string, stdout, stderr io.Writer) int {
	return queueRequest(c, args, stdout, stderr, decide.AcceptNetwork, func(a *runsArgs, r *runstate.Run, out io.Writer) bool {
		fmt.Fprint(out, container.NetworkBox(r.Name, r.Agent))
		return confirm(out, "Let "+r.Name+" run with the network")
	})
}

// queueRequest is exec, done, scrub and the overrides: it finds the run, asks decide the way the controller will (so a
// refusal comes at once, naming its rule), asks for confirmation when the action has one, and writes the request. The
// controller decides again when it carries it out.
func queueRequest(c *command, args []string, stdout, stderr io.Writer, action string, ask func(a *runsArgs, r *runstate.Run, out io.Writer) bool) int {
	a, code, ok := runsFlags(c, args, stdout, stderr, true)
	if !ok {
		return code
	}
	found, err := a.find()
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	d, r, err := request.Decide(a.work, a.ns, a.g, a.cfg, found.Agent, found.Name, action, a.live())
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	if d.Outcome != decide.Allow {
		fmt.Fprintf(stderr, "plinthctl %s: %s: %s by rule %s: %s\n", c.name, r.Name, d.Outcome, d.Rule, d.Reason)
		return exitFail
	}
	if ask != nil && !ask(a, r, stdout) {
		fmt.Fprintln(stdout, "Nothing queued.")
		return exitFail
	}
	err = request.Queue(a.work, a.ns, r.Agent, r.Name, action)
	switch {
	case errors.Is(err, request.ErrQueued):
		fmt.Fprintf(stdout, "%s of %s: already queued.\n", action, r.Name)
		return exitOK
	case errors.Is(err, request.ErrInProgress):
		fmt.Fprintf(stdout, "%s of %s: in progress.\n", action, r.Name)
		return exitOK
	case err != nil:
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	fmt.Fprintf(stdout, "Queued %s of %s.\n", action, r.Name)
	logfile.New(a.work, logfile.CLI).With("command", c.name, "ns", a.ns, "agent", r.Agent, "run", r.Name).Info("queued a request", "action", action)
	if !controllerRunning(a.work) {
		fmt.Fprintf(stderr, "plinthctl %s: the controller is not running: it carries this out once it runs (plinthctl controller start)\n", c.name)
	}
	return exitOK
}

// confirm asks question and reports whether the user typed yes: every typed confirmation is yes.
func confirm(out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s? (type yes): ", question)
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	return strings.TrimSpace(answer) == "yes"
}

// firstLine is the first line of a run's file that isn't a heading or empty, cut to 160 characters.
func firstLine(a *runsArgs, r *runstate.Run, name string) string {
	b, err := readRunFile(a, r, name)
	if err != nil {
		return err.Error()
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			if len(l) > 160 {
				l = l[:160] + "..."
			}
			return l
		}
	}
	return "(empty)"
}

func readRunFile(a *runsArgs, r *runstate.Run, name string) ([]byte, error) {
	root, err := os.OpenRoot(a.work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return safefs.ReadFile(root, filepath.Join(runstate.RunDir(a.ns, r.Agent, r.Name), name), doc.MaxSize)
}
