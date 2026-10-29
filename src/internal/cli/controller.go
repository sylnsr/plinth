package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"plinth/internal/config"
	"plinth/internal/controller"
	"plinth/internal/logfile"
	"plinth/internal/ollamamcp"
	"plinth/internal/safefs"
	"plinth/internal/workdir"
)

// stdin is where commands read a confirmation from; tests replace it.
var stdin io.Reader = os.Stdin

func runController(c *command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl controller: %v\n", err)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := controller.Run(ctx, controller.Options{Work: w, Getenv: os.LookupEnv, Stderr: stderr, Version: Version}); err != nil {
		fmt.Fprintf(stderr, "plinthctl controller: %v\n", err)
		return exitFail
	}
	return exitOK
}

// exitNotRunning is controller status's exit code when no controller runs, as systemctl status has it.
const exitNotRunning = 3

// startWait is how long controller start waits for the controller to serve, stopWait how long controller stop waits
// for it to end; tests shorten them.
var (
	startWait = 30 * time.Second
	stopWait  = 30 * time.Second
	killWait  = time.Minute // more for stop --force: a run's SIGTERM, 30 s later its SIGKILL, podman's cleanup
)

// controllerOut is the file below <work> that a controller started in the background writes its stdout and stderr to.
var controllerOut = filepath.Join(logfile.Dir, "controller.out")

// runControllerHelp is plinthctl controller without a subcommand: it lists them.
func runControllerHelp(c *command, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printUsage(stdout, c)
		return exitOK
	}
	what := "name what to do"
	if len(args) > 0 {
		what = fmt.Sprintf("%q is not a subcommand", args[0])
	}
	fmt.Fprintf(stderr, "plinthctl controller: %s:\n", what)
	for _, sub := range commands() {
		if name, ok := strings.CutPrefix(sub.name, "controller "); ok {
			fmt.Fprintf(stderr, "  %-8s %s\n", name, sub.summary)
		}
	}
	return exitUsage
}

// controllerWork parses the --work flag of a controller subcommand, which takes no arguments.
func controllerWork(c *command, args []string, stdout, stderr io.Writer) (string, int, bool) {
	w, _, code, ok := controllerFlags(c, args, stdout, stderr, false)
	return w, code, ok
}

// controllerFlags parses --work, and --force where withForce (stop, restart).
func controllerFlags(c *command, args []string, stdout, stderr io.Writer, withForce bool) (string, bool, int, bool) {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	work := flags.String("work", defaultWork, "Plinth's work directory")
	force := new(bool)
	if withForce {
		force = flags.Bool("force", false, "stop it even while runs run: their scripts are stopped and count as killed")
	}
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return "", false, code, false
	}
	if len(pos) > 0 {
		return "", false, usageError(c, stderr, "too many arguments"), false
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return "", false, exitFail, false
	}
	return w, *force, exitOK, true
}

func runControllerStart(c *command, args []string, stdout, stderr io.Writer) int {
	w, code, ok := controllerWork(c, args, stdout, stderr)
	if !ok {
		return code
	}
	if running, pid := controller.Running(w); running {
		fmt.Fprintf(stdout, "plinthctl controller: already running (PID %d) - next: plinthctl controller status\n", pid)
		return exitOK
	}
	return startController(w, stdout, stderr)
}

// startController starts plinthctl controller run for w in a session of its own, so it outlives this command and the
// terminal, and waits until it serves (Status.Ready) or has ended.
func startController(w string, stdout, stderr io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "plinthctl controller: "+format+"\n", args...)
		return exitFail
	}
	bin, err := os.Executable()
	if err != nil {
		return fail("can't start: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(w, logfile.Dir), 0o700); err != nil {
		return fail("can't start: %v", err)
	}
	outPath := filepath.Join(w, controllerOut)
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fail("can't start: %v", err)
	}
	cmd := exec.Command(bin, "controller", "run", "--work", w)
	cmd.Stdout, cmd.Stderr = out, out // stdin is /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	out.Close()
	if err != nil {
		return fail("can't start: %v", err)
	}
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	deadline := time.After(startWait)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-ended:
			why := "it ended"
			if err != nil {
				why = err.Error()
			}
			b, _ := os.ReadFile(outPath)
			return fail("didn't start (%s):\n%s", why, strings.TrimRight(string(b), "\n"))
		case <-deadline:
			return fail("started (PID %d), but it doesn't serve after %s - next: plinthctl controller status (its last lines say "+
				"what it does)", cmd.Process.Pid, startWait)
		case <-tick.C:
		}
		st, err := controller.Query(w)
		if err != nil || !st.Ready {
			continue
		}
		fmt.Fprintf(stdout, "plinthctl controller: started (PID %d); the Plinth MCP server listens on %s\n", st.PID, st.MCP)
		printNotices(stderr, openNotices(w), "")
		fmt.Fprintln(stdout, "next: plinthctl controller <status|stop>")
		return exitOK
	}
}

func runControllerStop(c *command, args []string, stdout, stderr io.Writer) int {
	w, force, code, ok := controllerFlags(c, args, stdout, stderr, true)
	if !ok {
		return code
	}
	return stopController(w, force, stdout, stderr)
}

// stopController asks the controller of w to stop and waits until it no longer holds the lock: over the socket, or
// with SIGTERM when the socket doesn't answer and that PID still holds the lock. Without force it stops nothing while
// runs run (the controller refuses); with force it waits killGrace longer, for their scripts to be stopped.
func stopController(w string, force bool, stdout, stderr io.Writer) int {
	running, pid := controller.Running(w)
	if !running {
		fmt.Fprintln(stdout, "plinthctl controller: not running")
		return exitOK
	}
	wait := stopWait
	if force {
		wait += killWait
	}
	var refused *controller.StopRefused
	if err := controller.RequestStop(w, force); errors.As(err, &refused) {
		fmt.Fprintf(stderr, "plinthctl controller: not stopped: %s; a stop would kill them - next: wait until they have ended "+
			"(plinthctl controller status lists what runs), or plinthctl controller stop --force\n", refused.Reason)
		return exitFail
	} else if err != nil {
		if again, p := controller.Running(w); again && p == pid && pid > 0 {
			if kerr := syscall.Kill(pid, syscall.SIGTERM); kerr != nil {
				fmt.Fprintf(stderr, "plinthctl controller: its socket doesn't answer (%v), and SIGTERM to PID %d failed: %v\n", err, pid, kerr)
				return exitFail
			}
			fmt.Fprintf(stdout, "plinthctl controller: its socket doesn't answer (%v): sent SIGTERM to PID %d\n", err, pid)
		}
	}
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if running, _ := controller.Running(w); !running {
			fmt.Fprintf(stdout, "plinthctl controller: stopped (PID %d)\n", pid)
			return exitOK
		}
	}
	fmt.Fprintf(stderr, "plinthctl controller: still running after %s (PID %d) - next: plinthctl controller status; if it hangs, kill %d\n",
		wait, pid, pid)
	return exitFail
}

func runControllerRestart(c *command, args []string, stdout, stderr io.Writer) int {
	w, force, code, ok := controllerFlags(c, args, stdout, stderr, true)
	if !ok {
		return code
	}
	if code := stopController(w, force, stdout, stderr); code != exitOK {
		return code
	}
	return startController(w, stdout, stderr)
}

func runControllerStatus(c *command, args []string, stdout, stderr io.Writer) int {
	w, code, ok := controllerWork(c, args, stdout, stderr)
	if !ok {
		return code
	}
	code = exitOK
	running, pid := controller.Running(w)
	st, err := controller.Query(w)
	switch {
	case !running:
		fmt.Fprintln(stdout, "plinthctl controller: not running - next: plinthctl controller start")
		code = exitNotRunning
	case err != nil:
		fmt.Fprintf(stdout, "plinthctl controller: running (PID %d), but its socket doesn't answer (%v) - next: ask again in a "+
			"moment; plinthctl controller restart if it lasts\n", pid, err)
		code = exitFail
	default:
		state := "running"
		if !st.Ready {
			state = "starting"
		}
		fmt.Fprintf(stdout, "plinthctl controller: %s (PID %d) since %s (%s)\n", state, st.PID, st.Started.UTC().Format(time.RFC3339),
			time.Since(st.Started).Round(time.Second))
		fmt.Fprintf(stdout, "  work:           %s\n", st.Work)
		fmt.Fprintf(stdout, "  MCP server:     %s\n", cmp.Or(st.MCP, "not listening yet"))
		fmt.Fprintf(stdout, "  socket:         %s\n", filepath.Join(w, filepath.FromSlash(controller.Socket)))
		if len(st.Namespaces) == 0 {
			fmt.Fprintln(stdout, "  namespaces:     none yet (one is served once an agent is registered and its config loads)")
		}
		for i, n := range st.Namespaces {
			label := ""
			if i == 0 {
				label = "namespaces:"
			}
			beat := "no heartbeat yet"
			if !n.Heartbeat.IsZero() {
				beat = "heartbeat " + time.Since(n.Heartbeat).Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(stdout, "  %-15s %s (%s)\n", label, n.Name, beat)
		}
		fmt.Fprintf(stdout, "  Ollama gateway: %s\n", gatewayLine(w, st.Ollama))
		if len(st.Running) > 0 {
			fmt.Fprintf(stdout, "  running now:    %s (controller stop refuses while they run; --force stops them)\n", strings.Join(st.Running, ", "))
		}
	}
	if ns := openNotices(w); len(ns) > 0 {
		fmt.Fprintf(stdout, "notices (%d), open in the log:\n", len(ns))
		printNotices(stdout, ns, "  ")
	}
	if lines, err := logfile.Tail(w, 10, time.Now(), logfile.ControllerComponents...); err == nil && len(lines) > 0 {
		fmt.Fprintf(stdout, "its last lines:\n  %s\n", strings.Join(lines, "\n  "))
	}
	return code
}

// openNotices are the notices open now, read from the log. The age limit is plinth.notice.expires, 14 days when the
// config doesn't load.
func openNotices(w string) []logfile.Notice {
	days := 14
	if g, err := config.LoadGlobal(w, os.LookupEnv); err == nil {
		days = g.Plinth.Notice.Expires
	}
	n, _ := logfile.OpenNotices(w, time.Now(), days)
	return n
}

// printNotices writes each notice: its level, message and key, then where, why, what it costs and what to do.
func printNotices(out io.Writer, notices []logfile.Notice, indent string) {
	for _, n := range notices {
		e := n.Last
		fmt.Fprintf(out, "%s%s %s [%s, %s ago]\n", indent, e.Level, e.Msg, n.Key, time.Since(e.Time).Round(time.Second))
		for _, k := range []string{"ns", "agent", "run", "err", "effect", "next"} {
			if v := e.Get(k); v != "" {
				fmt.Fprintf(out, "%s    %s: %s\n", indent, k, strings.ReplaceAll(v, "\n", "\n"+indent+"      "))
			}
		}
	}
}

// gatewayLine is the Ollama gateway's state, for controller status.
func gatewayLine(w string, configured bool) string {
	if !configured {
		return "not used (ollama.host is not set)"
	}
	st, err := ollamamcp.State(w)
	switch {
	case err != nil:
		return fmt.Sprintf("unknown (%v)", err)
	case st.Running:
		return "running since " + st.Started.UTC().Format(time.RFC3339)
	}
	return "not running; the controller starts it again (plinthctl ollama-mcp status says why)"
}

// runLogsScrub lists the old log files, asks for confirmation and queues the request; the controller deletes them.
func runLogsScrub(c *command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl logs scrub: %v\n", err)
		return exitFail
	}
	g, err := config.LoadGlobal(w, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl logs scrub: the max age comes from the global config, which doesn't load:\n%v\n", err)
		return exitFail
	}
	days := g.Plinth.Log.MaxAgeDays
	old, err := logfile.Old(w, days, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl logs scrub: %v\n", err)
		return exitFail
	}
	if len(old) == 0 {
		fmt.Fprintf(stdout, "No log file in %s is older than %d days.\n", filepath.Join(w, logfile.Dir), days)
		return exitOK
	}
	fmt.Fprintf(stdout, "Log files in %s older than %d days:\n", filepath.Join(w, logfile.Dir), days)
	for _, n := range old {
		fmt.Fprintf(stdout, "  %s\n", n)
	}
	fmt.Fprintf(stdout, "Are you sure? Delete these %d file(s) (type yes): ", len(old))
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	if strings.TrimSpace(answer) != "yes" {
		fmt.Fprintln(stdout, "Nothing queued.")
		return exitFail
	}
	if _, err := workdir.EnsureWork(w); err != nil { // requests/ in a <work> set up before it existed
		fmt.Fprintf(stderr, "plinthctl logs scrub: %v\n", err)
		return exitFail
	}
	root, err := os.OpenRoot(w)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl logs scrub: %v\n", err)
		return exitFail
	}
	defer root.Close()
	err = safefs.WriteNew(root, controller.RequestsDir, controller.LogsScrub+".queued", nil, 0o600)
	switch {
	case errors.Is(err, fs.ErrExist):
		fmt.Fprintln(stdout, "Already queued.")
	case err != nil:
		fmt.Fprintf(stderr, "plinthctl logs scrub: %v\n", err)
		return exitFail
	default:
		fmt.Fprintln(stdout, "Queued.")
	}
	logfile.New(w, logfile.CLI).With("command", "logs scrub").Info("queued a request", "action", controller.LogsScrub, "files", len(old), "older_than_days", days)
	if !controllerRunning(w) {
		fmt.Fprintln(stderr, "plinthctl logs scrub: the controller is not running: it deletes them once it runs (plinthctl controller start)")
	}
	return exitOK
}
