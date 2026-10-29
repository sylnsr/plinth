package runner

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/decide"
	"plinth/internal/dlp"
	"plinth/internal/doc"
	"plinth/internal/events"
	"plinth/internal/forgejo"
	"plinth/internal/image"
	"plinth/internal/logfile"
	"plinth/internal/names"
	"plinth/internal/ollama"
	"plinth/internal/plain"
	"plinth/internal/request"
	"plinth/internal/runstate"
	"plinth/internal/safefs"
)

// Raw is the directory below <ns-dir> that keeps the DLP scan's untouched originals: <agent>/<RUN>/. It also keeps,
// in <agent>/<RUN>/unexpected/ (unexpected.<n>/ for attempt n > 1), any state file a script wrote into its own run
// directory, in <agent>/<RUN>/running.<n>.yml the running.yml attempt n left without an answer, in
// <agent>/<RUN>/ask.sh the copy of the checked ask.sh that its container ran, and in <agent>/<RUN>/lib.bash the
// AAA library the run sourced ($AAA_LIB).
const Raw = "raw"

// LibFile is the AAA library of a run below raw/<agent>/<RUN>/: written right before the start, for the run alone.
const LibFile = "lib.bash"

// maxAnswer is the largest answer the DLP scan reads; a larger one is kept in raw/ and is INVALID (a document can't be
// larger).
const maxAnswer = doc.MaxSize

// startFiles separates, in the reason of an exec.start event, the ask.sh's sha256 from the names in the run directory
// at the start.
const startFiles = "; run directory: "

// child is one exec: from its start until the loop has finished it (exec.end in the event log, the DLP scan, the
// request finished). Its watcher sets result, then closes ended.
type child struct {
	agent, run string
	before     map[string]bool // the run directory's names before the start
	attempt    int             // 1 for the run's first exec; more after execs that ended without an answer
	ended      chan struct{}
	result     string   // "exit 0", "exit 3", "exit 143 (signal 15, terminated)", with why it was stopped, if it was
	logged     bool     // exec.end is in the event log
	scan       *scanJob // the answer's DLP scan, from its start until finishExec applied it
	// unavailable: a scan of the answer couldn't ask the model (dlp.scan unavailable is in the event log); the next
	// starts once retryAt has passed. waiting: that is all the exec waits for, no scan in flight,
	// so it holds back no detach (Busy).
	unavailable bool
	retryAt     time.Time
	waiting     atomic.Bool
	timeouts    int // attempts of the scan that timed out (dlp.scan timeout in the event log)
}

func (l *Runner) child(key string) *child {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.execs[key]
}

// Running lists the namespace's runs whose script still runs (agent/RUN), in their container or in the user's
// terminal, sorted: what controller stop refuses to kill.
func (l *Runner) Running() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for key, c := range l.execs {
		select {
		case <-c.ended:
		default:
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// Busy is the number of the namespace's execs not finished yet, but for those that only wait for the model: the
// controller detaches a runner only without any. One whose answer waits for the model (its script ended, no scan in
// flight) mustn't hold back the config change that may be its fix: its exec request stays
// .running, and the next runner scans the answer (leftoverExec).
func (l *Runner) Busy() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.execs {
		if !c.waiting.Load() {
			n++
		}
	}
	return n
}

// live is the runstate.Live of the namespace: a run is running while its exec isn't finished.
func (l *Runner) live(agent, run string) bool {
	return l.child(agent+"/"+run) != nil
}

// execTimeout is plinth.runner.exec-timeout-minutes, in minutes unless a test shortens the unit (Services).
func (l *Runner) execTimeout() time.Duration {
	return time.Duration(l.Global.Plinth.Runner.ExecTimeoutMinutes) * cmp.Or(l.ExecTimeoutUnit, time.Minute)
}

// killGrace is the time between SIGTERM and SIGKILL: 30 s unless a test shortens it (Services).
func (l *Runner) killGrace() time.Duration { return cmp.Or(l.KillGrace, 30*time.Second) }

func (l *Runner) scanRetry() time.Duration { return cmp.Or(l.ScanRetry, time.Minute) }

// ScanAttempts is how often an answer's DLP scan is tried when the model doesn't reply in time.
const ScanAttempts = 3

// startExec starts the run's script in a rootless podman container of its own (package container): the
// namespace's pinned image (plinth.exec.image, present here with its labels; a drift only warns), the run directory
// mounted read-write, a copy of the checked ask.sh, the AAA library (LibFile, $AAA_LIB) and this controller's binary
// (the library's text filter) read-only, an empty $HOME, no network (in network mode a network of its own; no ssh in
// either mode), and the variables of plinth.exec.env-file only. The podman client is the
// controller's child, in its own session (no controlling terminal), with scriptEnv; the script's stdin and stdout are
// /dev/null (it writes its answer itself). On a host where runs can't start (the check at start) nothing starts. The
// controller writes the empty running.yml first (the library writes the answer into it), so a script that dies before
// it sourced the library still shows it ran, without an answer (no-answer). A run whose earlier execs ended without an
// answer runs again (again). The exec request is .starting until then; right before the start it becomes
// .running, so a request a stopped controller left says whether the script may have run. An error before that
// leaves the request .starting, for the caller to fail; a start that fails fails the .running one here.
func (l *Runner) startExec(root *os.Root, r *runstate.Run) error {
	if l.Podman != nil {
		return fmt.Errorf("runs can't start on this host, so no script runs: %w", l.Podman)
	}
	dir := runstate.RunDir(l.ns, r.Agent, r.Name)
	// decide checked ask.sh against its sum when the request was decided; check it again right before the start, and
	// record what runs. The container mounts a copy of exactly the bytes checked, kept in raw/<agent>/<RUN>/, out
	// of the script's reach. The event also names the files in the run directory, out of the script's reach as well: a
	// controller that stopped while the script ran moves out at its next start what the script wrote itself (leftoverExec).
	script, sum, err := runstate.CheckScript(l.Work, l.ns, r.Agent, r.Name)
	if err != nil {
		return err
	}
	// decide checked the mode too (exec.mode); the bytes that run say it once more.
	mode := runstate.ModeOf(script)
	switch {
	case mode.Privileged || mode.Interactive:
		return fmt.Errorf("a %s script never runs as the controller's child", mode.Name())
	case mode.Network && (!l.cfg.Network || !r.Facts.NetworkAccepted):
		return errors.New("a network run needs plinth.exec.network and accept.network")
	case mode.Network && l.Pasta != nil:
		return l.Pasta
	}
	if l.cfg.Enabled[r.Agent] == "" {
		return fmt.Errorf("agent %s is not enabled in %s", r.Agent, l.ns)
	}
	// The pin, on this machine with its labels: the controller builds nothing, pulls nothing and runs no git.
	pin := l.cfg.Image
	if err := image.CheckLocal(pin, l.ns); err != nil {
		return err
	}
	if err := l.smoke(pin.Ref, mode); err != nil {
		return err
	}
	drift := l.drift(pin)
	rawDir := filepath.Join(l.ns.Owner, l.ns.Repo, Raw, r.Agent, r.Name)
	if err := ensureDirs(root, rawDir); err != nil {
		return err
	}
	if err := safefs.Replace(root, rawDir, runstate.AskFile, script, 0o600); err != nil {
		return err
	}
	// The AAA library, its text filter this controller's own binary, mounted read-only like the library.
	bin, err := selfBin()
	if err != nil {
		return err
	}
	if err := safefs.Replace(root, rawDir, LibFile, plain.ControllerLib(container.BinPath), 0o600); err != nil {
		return err
	}
	key, reqDir := r.Agent+"/"+r.Name, request.RunDir(l.ns, r.Agent, r.Name)
	// The container's own timeout comes 30 s after the controller's SIGKILL: it ends the run of a controller that died, and
	// never races the controller's stop (the two at once lost the exit code).
	run := container.Run{
		Work:    l.Work,
		Name:    l.ns.String() + "/" + key,
		Image:   pin.Ref,
		Dir:     filepath.Join(l.Work, dir),
		Ask:     filepath.Join(l.Work, rawDir, runstate.AskFile),
		Lib:     filepath.Join(l.Work, rawDir, LibFile),
		Bin:     bin,
		Home:    l.Home,
		Timeout: l.execTimeout() + l.killGrace() + 30*time.Second,
		Network: mode.Network,
		Memory:  l.cfg.Memory,
	}
	if l.cfg.EnvFile != "" {
		name := filepath.Join(l.ns.Owner, l.ns.Repo, l.cfg.EnvFile)
		if err := safefs.Regular(root, name); err != nil {
			return fmt.Errorf("plinth.exec.env-file %s: %w", l.cfg.EnvFile, err)
		}
		run.EnvFile = filepath.Join(l.Work, name)
	}
	cmd, err := container.Command(run)
	if err != nil {
		return err
	}
	cmd.Env = scriptEnv(os.Environ(), l.Global.EnvRefs)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	attempt, err := l.again(root, r)
	if err != nil {
		return err
	}
	before, names := startNames(r)
	if _, err := l.event(Caller, "exec.start", key, "ok", "allow", "ask.sh sha256 "+sum+"; mode "+mode.Name()+attemptNote(attempt)+"; image "+pin.Ref+"; commit "+pin.Commit+"; "+drift+
		startFiles+strings.Join(names, " ")); err != nil {
		return err
	}
	if err := safefs.WriteNew(root, dir, runstate.Running, nil, 0o600); err != nil {
		return err
	}
	// undo takes back running.yml, for a script that didn't start.
	undo := func(err error) error {
		if rerr := safefs.Remove(root, filepath.Join(dir, runstate.Running)); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return err
	}
	if err := safefs.Rename(root, filepath.Join(reqDir, decide.Exec+"."+request.Starting), filepath.Join(reqDir, decide.Exec+"."+request.Running)); err != nil {
		return undo(err) // still .starting: the caller fails it
	}
	// watch starts the client: podman create first, which may take a moment, so not in the loop.
	c := &child{agent: r.Agent, run: r.Name, before: before, attempt: attempt, ended: make(chan struct{})}
	l.mu.Lock()
	l.execs[key] = c
	l.mu.Unlock()
	l.wg.Add(1)
	go l.watch(c, cmd, run)
	l.runLog(c.agent, c.run).Info("exec starting", "container", container.ContainerName(l.Work, run.Name), "image", pin.Ref)
	return nil
}

// smoke runs the probe in a container of image with the flags of a run in mode, the first time a run of that image
// and mode starts in this controller, and returns what it found then and for every later run of the two (plinthctl
// README, Run modes, Requirements): a mode whose container doesn't hold what it promises takes no exec. The probe is
// this controller's own binary (container.ProbeArg).
func (l *Runner) smoke(image string, mode runstate.Mode) error {
	key := image + " " + mode.Name()
	l.smokeMu.Lock()
	defer l.smokeMu.Unlock()
	if err, ok := l.smoked[key]; ok {
		return err
	}
	bin, err := selfBin()
	if err == nil {
		err = container.Smoke(l.Stopping, container.Run{Work: l.Work, Name: l.ns.String() + "/smoke/" + mode.Name(), Image: image,
			Home: l.Home, Timeout: time.Minute, Network: mode.Network, Memory: l.cfg.Memory}, bin)
	}
	if l.Stopping.Err() != nil {
		return err // the controller stops: the next one checks again
	}
	if l.smoked == nil {
		l.smoked = map[string]error{}
	}
	l.smoked[key] = err
	if err != nil {
		l.Out.Once("smoke "+key, "runs of the image can't start", "ns", l.ns.String(), "mode", mode.Name(), "image", image, "err", err,
			"next", "check the image and podman on this host (plinthctl image list)")
	} else {
		l.nsLog().Info("the smoke container holds what the mode promises", "mode", mode.Name(), "image", image)
	}
	return err
}

// PinCheck is what the reason of an exec.start says of its drift check for the image ref: "no drift", "drift: ..." or
// "drift not checked". ok is false for a run of another image, or from before the pin.
func PinCheck(reason, ref string) (check string, ok bool) {
	_, rest, ok := strings.Cut(reason, "; image "+ref+"; commit ")
	if !ok {
		return "", false
	}
	if _, rest, ok = strings.Cut(rest, "; "); !ok {
		return "", false
	}
	check, _, _ = strings.Cut(rest, startFiles)
	return check, true
}

// driftTimeout bounds the drift check: Forgejo that doesn't answer delays a run by that much at most.
var driftTimeout = 5 * time.Second

// drift compares the .plinth/ tree of the default branch's head on Forgejo with the pin's: "no drift", "drift:
// ..." or "drift not checked", for exec.start. A drift goes into the event log (image.drift, target the namespace)
// and to stderr once per pair of trees. It only warns: the run gets the pin either way.
func (l *Runner) drift(pin config.ImagePin) string {
	ctx, cancel := context.WithTimeout(l.Stopping, driftTimeout)
	defer cancel()
	branch, tree, err := l.branchTree(ctx, l.ns)
	if err != nil {
		l.nsLog().Warn("drift not checked", "err", err)
		return "drift not checked"
	}
	if tree == pin.Tree {
		l.mu.Lock()
		l.lastDrift = ""
		l.mu.Unlock()
		l.Out.Clear("drift " + l.ns.String())
		return "no drift"
	}
	if tree == "" {
		tree = "none"
	}
	reason := fmt.Sprintf("%s at %s has .plinth tree %s, the pin has %s", branch.Name, branch.Commit, tree, pin.Tree)
	l.mu.Lock()
	seen := l.lastDrift == pin.Tree+" "+tree
	l.lastDrift = pin.Tree + " " + tree
	l.mu.Unlock()
	if !seen {
		if _, err := l.event(Caller, "image.drift", l.ns.String(), "drift", "allow", reason); err != nil {
			l.nsLog().Error("the image.drift event could not be written", "err", err)
		}
	}
	l.Out.Once("drift "+l.ns.String(), ".plinth/ changed since the image was published", "ns", l.ns.String(), "reason", reason,
		"effect", "runs still use the pinned image", "next", "to run the change, review it and publish again: plinthctl image publish -n "+l.ns.String())
	return "drift: " + reason
}

// branchTree is the default branch of ns on Forgejo and the tree of .plinth/ at its head ("" for none).
func (l *Runner) branchTree(ctx context.Context, ns names.Namespace) (forgejo.Branch, string, error) {
	fj, err := forgejo.New(l.Global.ForgejoURL, l.Global.ForgejoToken, l.ForgejoCAFile)
	if err != nil {
		return forgejo.Branch{}, "", err
	}
	defer fj.Close()
	name, err := fj.DefaultBranch(ctx, ns)
	if err != nil {
		return forgejo.Branch{}, "", err
	}
	branch, err := fj.Branch(ctx, ns, name)
	if err != nil {
		return branch, "", err
	}
	branch.Name = name // checked; Forgejo's text goes no further than that, and Tree checks the commit
	tree, err := fj.Tree(ctx, ns, branch.Commit, container.RepoDir)
	return branch, tree, err
}

// scriptEnv is the environment of the podman client that runs a script (the script itself gets only the env file's
// variables): the controller's, without any variable the config refers to (a ${VAR} or a -env key) or whose name
// starts with PLINTH_ (an agent's token included), without AAA_LIB and AAA_LOADED (the AAA library is the controller's), and with AAA_QUIET=1: none of Plinth's secrets reach a run
// through its environment.
func scriptEnv(environ, refs []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(refs, k) || strings.HasPrefix(k, "PLINTH_") || k == "AAA_QUIET" || k == "AAA_LIB" || k == "AAA_LOADED" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "AAA_QUIET=1")
}

// watch starts the run's podman client and waits for it. A container podman couldn't create ends the run at once, with
// podman's message: no script ran. After plinth.runner.exec-timeout-minutes, or when the controller stops, the
// container is stopped: SIGTERM to the script, then killGrace later SIGKILL to everything in it. Once the script has
// ended, podman inspect says how (the script's exit code, a kill over the memory limit, or podman's own failure), and
// the container goes, with whatever the script left running in it: a run ends with its script.
func (l *Runner) watch(c *child, cmd *container.Cmd, run container.Run) {
	defer l.wg.Done()
	name := run.Name
	if err := cmd.Start(); err != nil {
		if ce := (*container.CreateError)(nil); errors.As(err, &ce) {
			c.result = "the container wasn't created: " + orElse(ce.Msg, ce.Err.Error())
			if ce.Left != nil {
				c.result += "; " + ce.Left.Error()
			}
		} else {
			c.result = "the podman client didn't start: " + err.Error()
		}
		close(c.ended)
		return
	}
	waitc := make(chan error, 1)
	go func() { waitc <- cmd.Wait() }()
	timeout := l.execTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	why := ""
	select {
	case err = <-waitc:
	case <-timer.C:
		why = fmt.Sprintf("stopped after %s (plinth.runner.exec-timeout-minutes)", timeout)
	case <-l.Stopping.Done():
		why = "stopped with the controller"
	}
	if why != "" {
		if serr := container.Stop(l.Work, name, l.killGrace()); serr != nil {
			l.runLog(c.agent, c.run).Warn("stopping the container failed", "err", serr)
		}
		select {
		case err = <-waitc:
		case <-time.After(l.killGrace() + 30*time.Second):
			container.Remove(l.Work, name)
			err = <-waitc
		}
	}
	st, ierr := container.Inspect(l.Work, name)
	if rerr := container.Remove(l.Work, name); rerr != nil {
		l.runLog(c.agent, c.run).Warn("removing the container failed", "err", rerr)
	}
	c.result = runResult(err, st, ierr, cmd.Message(), run.Memory, why != "")
	if why != "" {
		c.result = why + ": " + c.result
	}
	close(c.ended)
	if l.Stopping.Err() != nil {
		l.Out.Once("stopped "+l.ns.String()+"/"+c.agent+"/"+c.run, "a run was still running and was stopped; it counts as killed",
			"ns", l.ns.String(), "agent", c.agent, "run", c.run)
	}
}

// runResult says how a run ended, from its container's state: the script's exit code, a kill over the memory limit, or
// a container that never started, with podman's message. Without a state (no container: podman failed before it made
// one), from the podman client's exit (125: podman's own error), never with its stderr, which may be the script's.
// podman doesn't reliably set OOMKilled (it didn't on the test host, cgroups v2), so a SIGKILL the controller didn't send
// (stopped: a timeout or the controller's stop) counts as the memory limit: in its container only the kernel sends one,
// unless the script sent it itself.
func runResult(err error, st container.State, ierr error, msg, memory string, stopped bool) string {
	switch {
	case ierr != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 125 {
			// Not msg: without a container to say the script never ran, the client's stderr may hold the script's. podman's
			// failure or a script's exit 125 whose container went: the words fit both.
			return "podman exit 125, and no container left to inspect: " + ierr.Error()
		}
		return exitText(err) + "; podman can't say how the container ended: " + ierr.Error()
	case !st.Started:
		return "the container didn't run: " + orElse(st.Error, orElse(msg, exitText(err)))
	}
	code := st.ExitCode
	// podman has no exit code: the client's, which passes on the script's, unless it is podman's -1 as well (255).
	if ee := (*exec.ExitError)(nil); code < 0 && errors.As(err, &ee) && ee.ExitCode() >= 0 && ee.ExitCode() != 255 {
		code = ee.ExitCode()
	}
	switch {
	case code < 0:
		return "exit code lost (podman recorded none)"
	case st.OOMKilled:
		return codeText(code) + "; killed over its memory limit (plinth.exec.memory " + memory + ")"
	case code == 128+9 && !stopped:
		return codeText(code) + "; over its memory limit (plinth.exec.memory " + memory + "), unless the script sent SIGKILL itself"
	}
	return codeText(code)
}

func orElse(s, other string) string {
	if s != "" {
		return s
	}
	return other
}

// exitText says how the podman client ended: an exit code (codeText) or a signal.
func exitText(err error) string {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return "exit 0"
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fmt.Sprintf("signal %d (%s)", int(ws.Signal()), ws.Signal())
		}
		return codeText(ee.ExitCode())
	}
	return err.Error()
}

// codeText is an exit code, and for 129 to 192 the signal it means (128+n: killed by signal n).
func codeText(code int) string {
	if code > 128 && code <= 128+64 {
		s := syscall.Signal(code - 128)
		return fmt.Sprintf("exit %d (signal %d, %s)", code, int(s), s)
	}
	return fmt.Sprintf("exit %d", code)
}

// finishExecs finishes every exec whose child has ended, in a fixed order.
func (l *Runner) finishExecs(root *os.Root) error {
	l.mu.Lock()
	var ended []*child
	for _, c := range l.execs {
		select {
		case <-c.ended:
			ended = append(ended, c)
		default:
		}
	}
	l.mu.Unlock()
	slices.SortFunc(ended, func(a, b *child) int { return strings.Compare(a.agent+"/"+a.run, b.agent+"/"+b.run) })
	for _, c := range ended {
		if err := l.perRun("exec of "+c.agent+"/"+c.run, l.finishExec(root, c)); err != nil {
			return err
		}
	}
	return nil
}

// finishExec is the rest of an exec once its child ended: state files the script wrote itself are moved out, exec.end
// goes into the event log, the answer gets its DLP scan, and the exec request is finished. The scan's model step runs
// in the background (scanStep): until it is done the child stays, so the run still counts as running. An error leaves
// the child in place, so the next poll tries again.
func (l *Runner) finishExec(root *os.Root, c *child) error {
	key := c.agent + "/" + c.run
	if !c.logged {
		moved, err := l.moveUnexpected(root, c.agent, c.run, c.before, c.attempt)
		if err != nil {
			return err
		}
		r, err := runstate.Load(l.Work, l.ns, c.agent, c.run, l.cfg.Automatic, nil)
		if err != nil {
			return err
		}
		kept, err := keepNoAnswer(root, r)
		if err != nil {
			return err
		}
		outcome, reason := endOutcome(r, moved), c.result+movedNote(key, c.attempt, moved)+kept
		if _, err := l.event(Caller, "exec.end", key, outcome, "allow", reason); err != nil {
			return err
		}
		c.logged = true
		lg, fields := l.runLog(c.agent, c.run), []any{"outcome", outcome, "reason", reason}
		if outcome == "answered" || outcome == "answered at a gate" {
			lg.Info("exec ended", fields...)
		} else {
			lg.Warn("exec ended", fields...)
		}
	}
	if pending, err := l.scanStep(root, c); err != nil || pending {
		return err
	}
	if err := l.requestDone(root, request.RunDir(l.ns, c.agent, c.run), decide.Exec); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	l.mu.Lock()
	delete(l.execs, key)
	l.mu.Unlock()
	return nil
}

// endOutcome is the outcome of exec.end, once the state files the script wrote itself (moved) are out.
func endOutcome(r *runstate.Run, moved []string) string {
	switch {
	case r.Facts.Answer == "" && slices.ContainsFunc(moved, newAnswer):
		return "answer refused" // moved aside: not a regular file with one hard link
	case r.Facts.Answer != "" && r.Gate:
		return "answered at a gate"
	case r.Facts.Answer != "":
		return "answered"
	}
	return "no answer" // not done: the user may run it again
}

// movedNote is what exec.end says about the state files the script wrote itself, "" when there were none.
func movedNote(key string, attempt int, moved []string) string {
	if len(moved) == 0 {
		return ""
	}
	return "; it wrote " + strings.Join(moved, ", ") + " itself, moved to " + Raw + "/" + key + "/" + unexpectedDir(attempt) + "/"
}

// unexpectedDir is the directory below raw/<agent>/<RUN>/ for the state files attempt wrote itself: unexpected for
// the run's first exec, unexpected.<n> for attempt n of a run that ran again, so none overwrites another's.
func unexpectedDir(attempt int) string {
	if attempt <= 1 {
		return "unexpected"
	}
	return fmt.Sprintf("unexpected.%d", attempt)
}

// attemptNote is what exec.start says of an exec that isn't the run's first, "" for the first.
func attemptNote(attempt int) string {
	if attempt <= 1 {
		return ""
	}
	return fmt.Sprintf("; attempt %d, after %d without an answer", attempt, attempt-1)
}

// again readies a run for its next exec and returns that exec's attempt: 1 for the run's first, n+1 after n execs that
// ended without an answer (only an answer says a run is done, and decide refuses an exec of a run that has one).
// The count is the event log's exec.end events since the run was stored, out of the script's reach. A left-over
// running.yml (what the last attempt wrote of its answer) goes to raw/<agent>/<RUN>/running.<n>.yml, n the attempt
// that left it: the controller writes a fresh one for the start.
func (l *Runner) again(root *os.Root, r *runstate.Run) (int, error) {
	_, err := safefs.Lstat(root, filepath.Join(request.RunDir(l.ns, r.Agent, r.Name), decide.Exec+"."+request.Finished))
	if errors.Is(err, fs.ErrNotExist) && !r.Has(runstate.Running) {
		return 1, nil // no exec of the run ended yet
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	ex, err := l.lastExec(r.Agent + "/" + r.Name)
	if err != nil {
		return 0, err
	}
	if r.Has(runstate.Running) {
		rawDir := filepath.Join(l.ns.Owner, l.ns.Repo, Raw, r.Agent, r.Name)
		if err := ensureDirs(root, rawDir); err != nil {
			return 0, err
		}
		left := filepath.Join(rawDir, fmt.Sprintf("running.%d.yml", ex.ends))
		if err := safefs.Rename(root, filepath.Join(runstate.RunDir(l.ns, r.Agent, r.Name), runstate.Running), left); err != nil {
			return 0, err
		}
		l.runLog(r.Agent, r.Name).Info("exec running again; the running.yml the earlier attempt left is renamed", "attempt", ex.ends, "now", left)
	}
	return ex.ends + 1, nil
}

// startNames are the run directory's names at an exec's start, as before and as the list exec.start records:
// running.yml, the controller's own, isn't one of them (again moved a left-over one out).
func startNames(r *runstate.Run) (map[string]bool, []string) {
	before := map[string]bool{}
	var names []string
	for _, f := range r.Files {
		if f.Name == runstate.Running {
			continue
		}
		before[f.Name] = true
		names = append(names, f.Name)
	}
	return before, names
}

// leftoverExec finishes an exec request that an earlier controller left .running: that controller started the script, whose
// container this controller removed at its start (setUpContainers), so the exec is finished, never carried out again.
// Unless the event log has the exec's exec.end (finishExec got that far), the state files the script wrote
// itself are moved out first, as finishExec does, with the names its exec.start recorded, and exec.end is recorded:
// no answer, unless the script wrote one. Then an answer gets its DLP scan, as an exec of this controller does (finishExec,
// with a child that has ended), and the request is finished.
func (l *Runner) leftoverExec(root *os.Root, agent, run string) error {
	key := agent + "/" + run
	ex, err := l.lastExec(key)
	if err != nil {
		return err
	}
	var moved []string
	dir := runstate.RunDir(l.ns, agent, run)
	if fi, err := safefs.Lstat(root, dir); !ex.ended && err == nil && fi.IsDir() { // a run that's gone has nothing to move
		before := ex.before
		if before == nil { // no exec.start that names the files (an older one, or none)
			if before, err = beforeStart(root, dir); err != nil {
				return err
			}
		}
		if moved, err = l.moveUnexpected(root, agent, run, before, ex.ends+1); err != nil { // the exec without its exec.end
			return err
		}
	}
	r, err := runstate.Load(l.Work, l.ns, agent, run, l.cfg.Automatic, nil)
	if err != nil {
		return err
	}
	kept, err := keepNoAnswer(root, r)
	if err != nil {
		return err
	}
	if !ex.ended {
		why := "the controller stopped while the script ran"
		if r.Facts.Answer != "" {
			why = "the controller stopped before it scanned the answer"
		}
		if _, err := l.event(Caller, "exec.end", key, endOutcome(r, moved), "allow", why+movedNote(key, ex.ends+1, moved)+kept); err != nil {
			return err
		}
	}
	if r.Facts.Answer == "new" { // scanned as for an exec of this controller: by finishExec, with a child that has ended
		ended := make(chan struct{})
		close(ended)
		l.mu.Lock()
		l.execs[key] = &child{agent: agent, run: run, ended: ended, logged: true, unavailable: ex.unavailable, timeouts: ex.timeouts}
		l.mu.Unlock()
		return nil
	}
	return l.requestDone(root, request.RunDir(l.ns, agent, run), decide.Exec)
}

// keepNoAnswer writes running.yml back for a run that ended with neither an answer nor running.yml: its script removed
// it, and the run's names must still say it ran, without an answer. It returns what exec.end says about
// that, "" when it didn't.
func keepNoAnswer(root *os.Root, r *runstate.Run) (string, error) {
	if !r.Facts.Exists || r.Facts.Answer != "" || r.Has(runstate.Running) {
		return "", nil
	}
	if err := safefs.WriteNew(root, runstate.RunDir(r.NS, r.Agent, r.Name), runstate.Running, nil, 0o600); err != nil {
		return "", err
	}
	return "; it removed running.yml itself, written back", nil
}

// restartExec readies an exec request that an earlier controller left .starting to be carried out again. Its script
// never started (the request is .running from right before the start), so an empty running.yml in the run directory
// is the one that controller wrote for the start and goes: no attempt left it (again would keep it in raw/). Any other
// running.yml stays.
func (l *Runner) restartExec(root *os.Root, agent, run string) error {
	name := filepath.Join(runstate.RunDir(l.ns, agent, run), runstate.Running)
	fi, err := safefs.Lstat(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() != 0 {
		return nil
	}
	return safefs.Remove(root, name)
}

// execRecord is what the event log says about a run's last exec: the names in its run directory at its exec.start (nil
// without one, or when the event names none), and whether it ended (exec.end); and how many of the run's execs ended.
type execRecord struct {
	ended  bool
	before map[string]bool
	ends   int // exec.end events since the run was stored: attempts that ended without an answer, or the one with it
	// unavailable: the last scan of its answer couldn't ask the model (dlp.scan unavailable);
	// timeouts: its scans that timed out (dlp.scan timeout).
	unavailable bool
	timeouts    int
}

// lastExec reads the run's last exec from the namespace's event log, since the run was stored: the name of a scrubbed
// run can be drawn again.
func (l *Runner) lastExec(key string) (execRecord, error) {
	var ex execRecord
	err := events.Walk(l.Work, filepath.Join(l.ns.Owner, l.ns.Repo), func(e events.Event, _ []byte) error {
		if e.Target != key {
			return nil
		}
		switch e.Action {
		case "ask.stored":
			ex = execRecord{}
		case "exec.start":
			ex = execRecord{ends: ex.ends}
			if _, files, ok := strings.Cut(e.Reason, startFiles); ok {
				ex.before = map[string]bool{}
				for _, n := range strings.Fields(files) {
					ex.before[n] = true
				}
			}
		case "exec.end":
			ex.ended = true
			ex.ends++
		case "dlp.scan":
			ex.unavailable = e.Outcome == "unavailable"
			if e.Outcome == "timeout" {
				ex.timeouts++
			}
		}
		return nil
	})
	return ex, err
}

// beforeStart lists the names in the run directory that a run can hold before its script starts (runstate.BeforeExec):
// what a left-over exec without a recorded list counts as there before the start.
func beforeStart(root *os.Root, dir string) (map[string]bool, error) {
	entries, err := safefs.ReadDir(root, dir)
	if err != nil {
		return nil, err
	}
	before := map[string]bool{}
	for _, e := range entries {
		if runstate.BeforeExec(e.Name()) {
			before[e.Name()] = true
		}
	}
	return before, nil
}

// moveUnexpected moves every state file the script created in its own run directory (a review, an override,
// report.md, an answer in any state but new), and an answer[.FAIL].new.yml that isn't a regular file with one hard
// link (refused: the DLP scan reads only such a file), to raw/<agent>/<RUN>/unexpected/ (unexpectedDir: one per attempt): a run's state is the controller's to write. before
// are the names that were there at the start. Of answer.new.yml and answer.FAIL.new.yml, written both, the gate's counts.
func (l *Runner) moveUnexpected(root *os.Root, agent, run string, before map[string]bool, attempt int) ([]string, error) {
	dir := runstate.RunDir(l.ns, agent, run)
	entries, err := safefs.ReadDir(root, dir)
	if err != nil {
		return nil, err
	}
	var moved, answers []string
	for _, e := range entries {
		n := e.Name()
		switch {
		case before[n] || n == runstate.Running || !runstate.StateFile(n):
		case newAnswer(n) && safefs.Regular(root, filepath.Join(dir, n)) == nil: // not a hard link, symlink, FIFO or directory
			answers = append(answers, n)
		default:
			moved = append(moved, n)
		}
	}
	if len(answers) == 2 {
		moved = append(moved, "answer.new.yml")
	}
	if len(moved) == 0 {
		return nil, nil
	}
	out := filepath.Join(l.ns.Owner, l.ns.Repo, Raw, agent, run, unexpectedDir(attempt))
	if err := ensureDirs(root, out); err != nil {
		return nil, err
	}
	for _, n := range moved {
		if err := root.Rename(filepath.Join(dir, n), filepath.Join(out, n)); err != nil { // the name itself, never what it points to
			return nil, err
		}
	}
	slices.Sort(moved)
	return moved, nil
}

// scanJob is the DLP scan of one answer document: what the loop read and kept before the model step and, once
// done is closed, the scan's result.
type scanJob struct {
	name    string // answer[.FAIL].new.yml
	gate    bool
	sum     string // the untouched original's sha256
	done    chan struct{}
	answer  *doc.Answer // masked once done: with the model's masks when err is nil, with the fixed-pattern ones alone otherwise
	invalid error       // not a valid document (doc.ParseEnded, or too large): never scanned, it becomes INVALID
	res     dlp.Result
	err     error
}

// scanStep is the DLP scan of the exec's answer, if it has a new one: startScan starts it, a later poll applies its
// result (applyScan). It reports whether the scan is still going. A scan the controller's stop cut short is never applied:
// the exec request stays .running, and the next start scans the answer again (leftoverExec). A scan that couldn't ask
// the model (Ollama not configured or unreachable, the gateway not running) isn't applied either: the answer stays
// answer[.FAIL].new.yml, the first such scan of the exec records dlp.scan unavailable, and the scan starts again after
// scanRetry, as plinthctl finish leaves it for the user to run again. Until then the run is running,
// though it holds back no detach (Busy); a runner that takes such an exec over records no second dlp.scan unavailable.
// A scan that timed out in between ends the outage: the next one is recorded again.
func (l *Runner) scanStep(root *os.Root, c *child) (bool, error) {
	if c.scan == nil && time.Now().Before(c.retryAt) {
		return true, nil
	}
	if c.scan == nil {
		r, err := runstate.Load(l.Work, l.ns, c.agent, c.run, l.cfg.Automatic, nil)
		if err != nil {
			return false, err
		}
		if r.Facts.Answer != "new" {
			return false, nil
		}
		if l.ctx.Err() != nil {
			return true, nil // a stopped runner starts no scan: the next one does (leftoverExec)
		}
		if c.scan, err = l.startScan(root, r); err != nil {
			return false, err
		}
		c.waiting.Store(false)
	}
	select {
	case <-c.scan.done:
	default:
		return true, nil
	}
	if c.scan.err != nil && l.ctx.Err() != nil {
		return true, nil // cut short by the stop
	}
	var re *dlp.ReplyError
	if j := c.scan; j.err != nil && !errors.As(j.err, &re) {
		key := c.agent + "/" + c.run
		switch ollama.KindOf(j.err) {
		case ollama.CantAsk:
			c.scan, c.retryAt = nil, time.Now().Add(l.scanRetry())
			c.waiting.Store(true)
			if c.unavailable {
				return true, nil
			}
			reason := fmt.Sprintf("%s; the answer stays %s, scanned again once the model can be asked; original sha256 %s", j.err, j.name, j.sum)
			if _, err := l.event(Caller, "dlp.scan", key, "unavailable", "dlp.unavailable", reason); err != nil {
				return false, err
			}
			c.unavailable = true
			l.runLog(c.agent, c.run).Component(logfile.DLP).Warn("DLP scan unavailable", "reason", reason)
			return true, nil
		case ollama.Timeout:
			// Asked again a scanRetry apart, ScanAttempts in all, each attempt that times out in the event log; the
			// last one fails the scan.
			if c.timeouts+1 < ScanAttempts {
				c.scan, c.retryAt = nil, time.Now().Add(l.scanRetry())
				c.waiting.Store(true)
				c.unavailable = false // the model was asked: an outage after this is a new one, recorded, as lastExec reads it
				c.timeouts++
				reason := fmt.Sprintf("%s; attempt %d of %d, the scan starts again; original sha256 %s", j.err, c.timeouts, ScanAttempts, j.sum)
				if _, err := l.event(Caller, "dlp.scan", key, "timeout", "dlp.timeout", reason); err != nil {
					return false, err
				}
				l.runLog(c.agent, c.run).Component(logfile.DLP).Warn("DLP scan timed out", "reason", reason)
				return true, nil
			}
			j.err = fmt.Errorf("%w, %d attempts", j.err, ScanAttempts)
			if j.answer.Status != nil && j.answer.Status.DLP != nil {
				j.answer.Status.DLP.Reason = j.err.Error()
			}
		}
		// Any other error: Ollama answered, and asking again gives the same: the scan ran and failed.
	}
	if err := l.applyScan(root, c.agent, c.run, c.scan); err != nil {
		return false, err
	}
	c.scan = nil
	return false, nil
}

// startScan starts the DLP scan of a finished exec's answer[.FAIL].new.yml: the untouched original goes to
// raw/<agent>/<RUN>/, then the document is decoded and checked (doc.ParseEnded: from the script, of this run, its
// result matching its name) and dlp.ScanAnswer (the fixed patterns, then the model step, on its texts) runs in the
// background, behind the process's Ollama semaphore, so a model step of minutes holds up no other run. An answer
// that isn't a valid document, or is over maxAnswer, isn't scanned: it is INVALID at once (applyScan). While
// ollama.host is not set no scan can ask the model (scanStep).
func (l *Runner) startScan(root *os.Root, r *runstate.Run) (*scanJob, error) {
	dir := runstate.RunDir(l.ns, r.Agent, r.Name)
	name := r.AnswerFile()
	src := filepath.Join(dir, name)
	rawDir := filepath.Join(l.ns.Owner, l.ns.Repo, Raw, r.Agent, r.Name)
	if err := ensureDirs(root, rawDir); err != nil {
		return nil, err
	}
	fi, err := safefs.Lstat(root, src)
	if err != nil {
		return nil, err
	}
	j := &scanJob{name: name, gate: r.Gate, done: make(chan struct{})}
	if fi.Size() > maxAnswer {
		if j.sum, err = copyFile(root, src, rawDir, name); err != nil {
			return nil, err
		}
		j.invalid = fmt.Errorf("%d bytes, more than the %d a document may have", fi.Size(), maxAnswer)
		close(j.done)
		return j, nil
	}
	b, err := safefs.ReadFile(root, src, maxAnswer)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	j.sum = hex.EncodeToString(h[:])
	if err := safefs.WriteNew(root, rawDir, name, b, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err // ErrExist: kept by a scan a stop or a crash cut short, and the original
	}
	if j.answer, j.invalid = doc.ParseEnded(b, r.Name, r.Gate); j.invalid != nil {
		close(j.done)
		return j, nil
	}
	ctx, model := l.ctx, l.Global.Ollama.Models.DLP
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer close(j.done)
		j.res, j.err = dlp.ScanAnswer(ctx, l.Ollama, model, j.answer)
	}()
	return j, nil
}

// applyScan writes a finished scan's result: the masked document, with status.dlp, the event dlp.scan (with the
// original's sha256), and the rename to answer[.FAIL].dlp-ok.yml, or to answer[.FAIL].DLP-FAIL.yml when the scan
// failed (the model's replies didn't fit): the fixed-pattern masks stay, and status.dlp says why; the user overrides or
// scrubs it. An answer that isn't a valid document is renamed as it is to answer[.FAIL].INVALID.yml: a bug, never
// data, kept for debugging, never released, reviewed or shown; the event says why.
func (l *Runner) applyScan(root *os.Root, agent, run string, j *scanJob) error {
	key, dir := agent+"/"+run, runstate.RunDir(l.ns, agent, run)
	var state, outcome, rule, reason string
	switch {
	case j.invalid != nil:
		state, outcome, rule = "INVALID", "invalid", "answer.invalid"
		reason = fmt.Sprintf("not a valid answer document (a bug, never data): %s; original sha256 %s", j.invalid, j.sum)
	case j.err != nil:
		state, outcome, rule = "DLP-FAIL", "failed", "dlp.failed"
		reason = fmt.Sprintf("%s; %d fixed-pattern mask(s); original sha256 %s", j.err, j.res.Fixed, j.sum)
	default:
		state, outcome, rule = "dlp-ok", "ok", "allow"
		reason = fmt.Sprintf("model %s, %d chunk(s); %d fixed-pattern mask(s), %d line(s) masked; original sha256 %s",
			j.res.Model, j.res.Chunks, j.res.Fixed, j.res.Lines, j.sum)
	}
	if j.invalid == nil {
		if err := safefs.Replace(root, dir, j.name, j.answer.Encode(), 0o600); err != nil {
			return err
		}
	}
	if _, err := l.event(Caller, "dlp.scan", key, outcome, rule, reason); err != nil {
		return err
	}
	lg, fields := l.runLog(agent, run).Component(logfile.DLP), []any{"outcome", outcome, "reason", reason}
	switch outcome {
	case "ok":
		lg.Info("DLP scan", fields...)
	case "invalid":
		lg.Error("DLP scan", fields...)
	default:
		lg.Warn("DLP scan", fields...)
	}
	final := "answer." + state + ".yml"
	if j.gate {
		final = "answer.FAIL." + state + ".yml"
	}
	if err := safefs.Rename(root, filepath.Join(dir, j.name), filepath.Join(dir, final)); err != nil {
		return err
	}
	if outcome == "failed" { // plinth.autosave.failures
		return l.autosave(root, agent, run, decide.Report)
	}
	return nil
}

// copyFile copies src to dir/name below root (created, or overwritten from the start) and returns its sha256.
func copyFile(root *os.Root, src, dir, name string) (string, error) {
	in, err := safefs.OpenFile(root, src, os.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := safefs.OpenCreate(root, dir, name, os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Truncate(n)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return hex.EncodeToString(h.Sum(nil)), err
}

// ensureDirs creates dir below root and each of its missing parents below <ns-dir>/<top>, e.g. raw/<agent>/<RUN>.
func ensureDirs(root *os.Root, dir string) error {
	parts := strings.Split(dir, string(filepath.Separator))
	for i := 3; i <= len(parts); i++ { // <owner>/<repo>/<top> exist (workdir set-up)
		if _, err := safefs.EnsureDir(root, filepath.Join(parts[:i]...)); err != nil {
			return err
		}
	}
	return nil
}

// newAnswer reports whether n is the name the AAA library gives a script's answer when it ends.
func newAnswer(n string) bool { return n == "answer.new.yml" || n == "answer.FAIL.new.yml" }

// selfBin is this controller's own binary, symlinks resolved: the smoke container's probe, and the AAA library's text
// filter in a run.
func selfBin() (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(bin)
}
