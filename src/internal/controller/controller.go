// Package controller is Plinth's one host process: it takes <work>/state/plinth.lock, attaches a runner
// (package runner) to every namespace with at least one registered agent and supervises its loop, serves its socket,
// and carries out the process-level requests in <work>/requests/. Spec: plinthctl.README.md, "Process".
package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"plinth/internal/agents"
	"plinth/internal/auth"
	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/decide"
	"plinth/internal/logfile"
	"plinth/internal/names"
	"plinth/internal/ollamamcp"
	"plinth/internal/review"
	"plinth/internal/runner"
	"plinth/internal/safefs"
	"plinth/internal/workdir"
)

// LockFile is the process's flock below <work>; it holds the PID while the process runs.
const LockFile = "state/plinth.lock"

// RequestsDir is the directory of the process-level requests below <work>.
const RequestsDir = "requests"

// LogsScrub is the process-level request to delete the old log files (see logfile.Scrub).
const LogsScrub = "logs-scrub"

// Restart backoff of a failed runner's loop: 1 s, doubling to 60 s; a loop that ran longer than maxBackoff starts
// again at minBackoff.
const (
	minBackoff = time.Second
	maxBackoff = 60 * time.Second
)

// ErrRunning: another process holds the lock.
var ErrRunning = errors.New("the controller is already running")

// Options are what Run needs besides the context.
type Options struct {
	Work   string        // absolute
	Getenv config.Getenv // os.LookupEnv
	Stderr io.Writer     // only what the user has to act on

	// Version is plinthctl's, for the Ollama gateway's serverInfo.
	Version string

	// ForgejoCAFile is a CA bundle for forgejo-url's certificate, for the drift check; "" for the system's (tests set
	// one).
	ForgejoCAFile string

	// Poll, when set, is called by a runner's loop at the end of every poll: a hook for tests. An error or a panic
	// fails the loop, which restarts with backoff.
	Poll func(ctx context.Context, ns names.Namespace, cfg *config.Namespace) error

	// ExecTimeoutUnit is the unit of plinth.runner.exec-timeout-minutes and KillGrace the time between SIGTERM and
	// SIGKILL; zero for a minute and 30 s (tests shorten both).
	ExecTimeoutUnit, KillGrace time.Duration
	// ScanRetry is the wait before an answer's DLP scan that couldn't ask the model starts again; zero for a minute.
	ScanRetry time.Duration

	// MCPListener, when set, is where the Plinth MCP server listens instead of plinth.mcp.listen:plinth.mcp.port (tests
	// give one on a free port).
	MCPListener net.Listener
}

// process is the state of one Run.
type process struct {
	Options
	g        *config.Global
	poll     time.Duration
	log      *logfile.Log
	out      *notifier
	ctx      context.Context    // Run's: cancelled when the process stops
	stop     context.CancelFunc // stops the process: controller stop over the socket
	started  time.Time
	attached map[string]*nsRunner
	mu       sync.Mutex // attached against the Plinth MCP server's reads (namespace) and status; only the main loop changes it
	mcpURL   string     // where the Plinth MCP server listens, once it does (mu)
	ready    bool       // the first poll is done (mu)
	globalAt fileSig    // config.yml when the process loaded it

	s     *runner.Services // what every runner gets from the process, the same for all of them
	termc chan termOp      // a run to start in the user's terminal, from the socket, for the main loop to route
}

// nsRunner is one attached namespace: its runner, the end of the goroutine that supervises it, and its config.yml
// when the runner's config was loaded.
type nsRunner struct {
	*runner.Runner
	ns     names.Namespace
	cfgAt  fileSig
	cancel context.CancelFunc
	done   chan struct{} // closed once supervise has returned
}

// Run is plinthctl controller run: it returns when ctx is cancelled (nil) or when the process can't start (the error says why).
func Run(ctx context.Context, o Options) error {
	if !filepath.IsAbs(o.Work) {
		return fmt.Errorf("work directory %s: not an absolute path", o.Work)
	}
	if err := hideFromSameUser(); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	p := &process{Options: o, ctx: ctx, stop: stop, started: time.Now(), attached: map[string]*nsRunner{}, termc: make(chan termOp)}
	p.log = logfile.New(o.Work, logfile.Controller)
	p.out = &notifier{w: o.Stderr, log: p.log, last: map[string]string{}, open: map[string]bool{}}
	p.log.Watch(func(err error) { // a failing write never stops the controller: a notice until a line is written again
		if err != nil {
			p.out.Once("log", "the general log can't be written", "err", err, "effect", "the controller goes on")
		} else {
			p.out.Clear("log")
		}
	})

	created, err := workdir.EnsureWork(o.Work)
	for _, c := range created {
		p.log.Info("created", "path", filepath.Join(o.Work, c))
	}
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(o.Work, config.FileName)
	for _, c := range created {
		if c == config.FileName {
			return fmt.Errorf("fill in %s before starting the controller (plinthctl config check lists what is missing)", cfgPath)
		}
	}
	p.globalAt = sig(cfgPath)
	if p.g, err = config.LoadGlobal(o.Work, o.Getenv); err != nil {
		return err
	}
	p.s = &runner.Services{Work: o.Work, Global: p.g, Log: p.log, Out: p.out, Stopping: ctx,
		Ollama: ollamamcp.NewClient(o.Work, p.g.Ollama.Host != ""), Fetcher: review.NewFetcher(p.g.ForgejoURL, p.g.ForgejoToken),
		ForgejoCAFile: o.ForgejoCAFile, Poll: o.Poll, ExecTimeoutUnit: o.ExecTimeoutUnit, KillGrace: o.KillGrace,
		ScanRetry: o.ScanRetry}
	p.poll = time.Duration(p.g.Plinth.Runner.PollSeconds) * time.Second
	if open, err := logfile.OpenNotices(o.Work, time.Now(), p.g.Plinth.Notice.Expires); err == nil { // a Clear ends those of an earlier controller
		keys := make([]string, 0, len(open))
		for _, n := range open {
			keys = append(keys, n.Key)
		}
		p.out.seed(keys)
	}

	lock, err := acquire(o.Work)
	if err != nil {
		return err
	}
	defer lock.Close() // releases the flock; the file stays, only the lock counts
	p.log.Info("started", "polling_every", p.poll.String())
	for _, w := range p.g.Warnings {
		p.log.Warn("config warning", "warning", w)
	}
	p.setUpContainers()
	ln, err := p.listen()
	if err != nil {
		return fmt.Errorf("the controller's socket: %w", err)
	}
	defer ln.Close()
	go p.serve(ln)
	stopAgents, err := p.serveAgents()
	if err != nil {
		return err
	}
	if p.s.Ollama.Configured() {
		ended := p.keepGateway(ctx)
		defer func() { <-ended }()
	}

	t := time.NewTicker(p.poll)
	defer t.Stop()
	for {
		p.requests()
		p.reconcile(ctx)
		p.mu.Lock()
		p.ready = true // controller start waits for this
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			stopAgents() // first: nothing is recorded through a runner being detached
			p.detachAll()
			p.log.Info("stopped")
			return nil
		case op := <-p.termc:
			p.route(op) // then a poll at once: harmless, every step is idempotent
		case <-t.C:
		}
	}
}

// gatewayCheck is how often the controller checks that the Ollama gateway runs; tests shorten it.
var gatewayCheck = 10 * time.Second

// keepGateway keeps the Ollama gateway's container running while ollama.host is set, until ctx ends. At once it starts
// it, or replaces one of another description or binary (ollamamcp.Gateway.Start), so the runners' first reviews find
// it; then, every gatewayCheck, it starts it again if it is gone. When the controller stops, the gateway stays: plain
// AAA shares it (plinthctl ollama-mcp stop ends it). One that can't start is said once on stderr and in the log;
// meanwhile the reviews and the DLP scans fail closed, and the controller goes on. It returns a channel closed when the
// checks have ended.
func (p *process) keepGateway(ctx context.Context) <-chan struct{} {
	bin, err := ollamamcp.Executable()
	g := ollamamcp.Gateway{Work: p.Work, Bin: bin, Ollama: p.g.Ollama, Version: p.Version}
	keep := func(first bool) {
		did, gerr := "", err
		if gerr == nil && !first {
			var st container.ServiceState
			if st, gerr = ollamamcp.State(p.Work); gerr == nil && st.Running {
				did = "running"
			}
		}
		if gerr == nil && did == "" {
			did, gerr = g.Start()
		}
		if gerr != nil {
			p.log.Component(logfile.OllamaGateway).Once("ollama-gateway", "the Ollama gateway does not run", "err", gerr)
			p.out.Once("ollama-gateway", "the Ollama gateway does not run", "err", gerr,
				"effect", "the reviews and the DLP scans fail until it runs; the controller goes on", "next", "plinthctl ollama-mcp start")
			return
		}
		p.log.Clear("ollama-gateway")
		p.out.Clear("ollama-gateway")
		if first || did != "running" {
			p.log.Component(logfile.OllamaGateway).Info("the Ollama gateway "+did, "socket", ollamamcp.SocketPath(p.Work))
		}
	}
	keep(true)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(gatewayCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				keep(false)
			}
		}
	}()
	return ended
}

// acquire takes the exclusive flock on <work>/state/plinth.lock and writes the PID into it. A process that holds it
// makes it fail with ErrRunning and that process's PID. A shared lock held for a moment (register asking whether the
// controller runs) is waited out: up to 10 tries 100 ms apart.
func acquire(work string) (*os.File, error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := safefs.OpenCreate(root, filepath.Dir(filepath.FromSlash(LockFile)), filepath.Base(LockFile), os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || try == 9 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		b := make([]byte, 32)
		n, _ := f.ReadAt(b, 0)
		f.Close()
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b[:n]))); perr == nil {
			return nil, fmt.Errorf("%w (PID %d)", ErrRunning, pid)
		}
		return nil, ErrRunning
	}
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Join(work, LockFile), err)
	}
	return f, nil
}

// reconcile attaches every namespace with a registered agent whose config loads, reloads a namespace whose config.yml
// changed, and detaches one with no agent left or whose changed config no longer loads.
func (p *process) reconcile(ctx context.Context) {
	if sig(filepath.Join(p.Work, config.FileName)) != p.globalAt {
		p.out.Once("global-config", "the global config changed", "path", filepath.Join(p.Work, config.FileName),
			"effect", "the controller still runs with the old one", "next", "plinthctl controller restart")
	}
	nss, err := workdir.Namespaces(p.Work)
	if err != nil {
		p.log.Once("namespaces", "listing the namespaces failed", "err", err)
		return // keep what is attached; a listing error says nothing about the namespaces
	}
	p.log.Clear("namespaces")
	want := map[string]names.Namespace{}
	for _, ns := range nss {
		as, err := agents.List(p.Work, ns)
		if err != nil {
			p.log.With("ns", ns.String()).Once("agents "+ns.String(), "listing its agents failed", "err", err)
			continue
		}
		p.log.Clear("agents " + ns.String())
		if len(as) > 0 {
			want[ns.String()] = ns
		}
	}
	for key, l := range p.attached {
		if _, ok := want[key]; !ok && p.idle(l, "no agent is registered any more") {
			p.detach(l, "no agent is registered any more")
		}
	}
	for key, ns := range want {
		cfgAt := sig(filepath.Join(p.Work, ns.Owner, ns.Repo, config.FileName))
		l, ok := p.attached[key]
		if ok && l.cfgAt == cfgAt {
			continue
		}
		if ok && !p.idle(l, "its config.yml changed") {
			continue
		}
		cfg, warns, err := config.LoadNamespace(p.Work, ns, p.g, p.Getenv)
		if err != nil {
			if ok {
				p.detach(l, "its config.yml no longer loads")
			}
			p.out.Once("config "+key, "the namespace is not served until its config loads", "ns", ns.String(), "err", err,
				"next", "plinthctl config check -n "+ns.String())
			p.log.With("ns", ns.String()).Once("config "+key, "the namespace is not served until its config loads", "err", err)
			continue
		}
		p.out.Clear("config " + key)
		p.log.Clear("config " + key)
		for _, w := range warns {
			p.log.With("ns", ns.String()).Warn("config warning", "warning", w)
		}
		if ok {
			p.detach(l, "its config.yml changed")
		}
		p.attach(ctx, ns, cfg, cfgAt)
	}
}

func (p *process) attach(ctx context.Context, ns names.Namespace, cfg *config.Namespace, cfgAt fileSig) {
	c, cancel := context.WithCancel(ctx)
	l := &nsRunner{Runner: runner.New(c, p.s, ns, cfg), ns: ns, cfgAt: cfgAt, cancel: cancel, done: make(chan struct{})}
	p.mu.Lock()
	p.attached[ns.String()] = l
	p.mu.Unlock()
	p.log.With("ns", ns.String()).Info("attached", "automatic_reviews", cfg.Automatic)
	go p.supervise(c, l)
}

// idle reports whether l can be detached: none of its execs is left. Otherwise it logs once that l waits for them
// (stop: SIGINT or SIGTERM stops them, see the runner's watch).
func (p *process) idle(l *nsRunner, why string) bool {
	if n := l.Busy(); n > 0 {
		p.log.With("ns", l.ns.String()).Once("busy "+l.ns.String(), "it is detached once its running scripts end", "why", why, "running", n)
		return false
	}
	p.log.Clear("busy " + l.ns.String())
	return true
}

// detach cancels the namespace's runner and waits for its loop, then for the rest of it: an exec the stop ended is
// finished like any other, or at the next start (runner.Finish).
func (p *process) detach(l *nsRunner, why string) {
	p.mu.Lock()
	delete(p.attached, l.ns.String()) // first: the Plinth MCP server no longer finds it
	p.mu.Unlock()
	l.cancel()
	<-l.done
	l.Finish()
	l.Detached() // after its own last events, before another runner of the namespace opens the event log
	p.log.With("ns", l.ns.String()).Info("detached", "why", why)
}

func (p *process) detachAll() {
	for _, l := range p.attachedRunners() {
		p.detach(l, "the controller stops")
	}
}

// attachedRunners is a copy of attached's runners: detach changes the map.
func (p *process) attachedRunners() []*nsRunner {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*nsRunner, 0, len(p.attached))
	for _, l := range p.attached {
		out = append(out, l)
	}
	return out
}

// supervise runs the namespace's runner until ctx is cancelled, restarting its loop with backoff when it fails or
// panics.
func (p *process) supervise(ctx context.Context, l *nsRunner) {
	defer close(l.done)
	backoff := minBackoff
	for {
		start := time.Now()
		err := l.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > maxBackoff {
			backoff = minBackoff
		}
		p.log.With("ns", l.ns.String()).Error("the namespace loop failed, restarting", "in", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// requests carries out the process-level requests in <work>/requests/: <name>.queued is renamed to <name>.running,
// carried out, and deleted, or renamed to <name>.FAILED with the reason as its content. A <name>.running left by a
// crash is carried out again. Files it doesn't know are logged once and left alone.
func (p *process) requests() {
	root, err := os.OpenRoot(p.Work)
	if err != nil {
		p.log.Once("requests", "reading the requests failed", "err", err)
		return
	}
	defer root.Close()
	entries, err := safefs.ReadDir(root, RequestsDir)
	if err != nil {
		p.log.Once("requests", "reading the requests failed", "err", err)
		return
	}
	p.log.Clear("requests")
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == ".gitignore", strings.HasSuffix(name, ".FAILED"):
		case name == LogsScrub+".queued", name == LogsScrub+".running":
			if !e.Type().IsRegular() {
				p.log.Once("request "+name, "request is not a regular file, left alone", "request", name)
				continue
			}
			p.carryOut(root, LogsScrub, name, p.logsScrub)
		default:
			p.log.Once("request "+name, "request is unknown, left alone", "request", name)
		}
	}
}

// carryOut runs one process-level request; name is its .queued or .running file.
func (p *process) carryOut(root *os.Root, action, name string, do func() error) {
	running := filepath.Join(RequestsDir, action+".running")
	if name != action+".running" {
		if err := safefs.Rename(root, filepath.Join(RequestsDir, name), running); err != nil {
			p.log.Once("request "+name, "request could not be taken up", "request", name, "err", err)
			return
		}
	} else {
		if _, err := safefs.Lstat(root, running); err != nil {
			return // carried out already in this pass, from its .queued
		}
		p.log.Info("request left over from an earlier run, carrying it out again", "request", name)
	}
	if err := do(); err != nil {
		p.log.Error("request failed", "action", action, "err", err)
		err = safefs.Replace(root, RequestsDir, action+".running", []byte(err.Error()+"\n"), 0o600)
		if err == nil {
			err = safefs.Rename(root, running, filepath.Join(RequestsDir, action+".FAILED"))
		}
		if err != nil {
			p.log.Error("marking the request failed did not work", "action", action, "err", err)
		}
		return
	}
	if err := safefs.Remove(root, running); err != nil {
		p.log.Warn("request done, but removing it failed", "action", action, "err", err)
	}
}

func (p *process) logsScrub() error {
	if d := decide.Decide(decide.Request{Caller: auth.HostUser(), Action: decide.LogsScrub}); d.Outcome != decide.Allow {
		return fmt.Errorf("%s by rule %s: %s", d.Outcome, d.Rule, d.Reason)
	}
	gone, err := logfile.Scrub(p.Work, p.g.Plinth.Log.MaxAgeDays, time.Now())
	if len(gone) > 0 {
		p.log.Info("logs scrub: deleted log files", "files", strings.Join(gone, " "))
	} else if err == nil {
		p.log.Info("logs scrub: no log file is older", "days", p.g.Plinth.Log.MaxAgeDays)
	}
	return err
}

// fileSig tells whether a file changed: its modification time and size; the zero value for a missing file.
type fileSig struct {
	mod  time.Time
	size int64
}

func sig(path string) fileSig {
	fi, err := os.Lstat(path)
	if err != nil {
		return fileSig{}
	}
	return fileSig{fi.ModTime(), fi.Size()}
}

// notifier writes to stderr what the user has to act on, once per distinct message for a key, and records it in the general
// log as a notice: a WARN line with the key, ended by an INFO line with cleared=true. The log is where the TUI and
// controller status read the open notices from, so they survive a restart of the controller.
type notifier struct {
	w    io.Writer
	log  *logfile.Log
	mu   sync.Mutex
	last map[string]string // key -> the sentence last written, for the keys raised by this process
	open map[string]bool   // the keys open in the log: those of an earlier controller (seed) and this one's
}

// seed makes the keys that are open in the log known, so a Clear of one ends it though this process never raised it.
func (n *notifier) seed(keys []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, k := range keys {
		n.open[k] = true
	}
}

// noticeText is a notice as stderr gets it: the message, its other fields, then why, the effect and what to do.
func noticeText(msg string, fields []any) string {
	var err, effect, next string
	var rest []string
	for i := 0; i+1 < len(fields); i += 2 {
		k, v := fmt.Sprint(fields[i]), fmt.Sprint(fields[i+1])
		switch k {
		case "err":
			err = v
		case "effect":
			effect = v
		case "next":
			next = v
		default:
			rest = append(rest, k+"="+v)
		}
	}
	s := "plinthctl controller: " + msg
	if len(rest) > 0 {
		s += " (" + strings.Join(rest, " ") + ")"
	}
	if err != "" {
		s += ": " + err
	}
	if effect != "" {
		s += " (" + effect + ")"
	}
	if next != "" {
		s += "; next: " + next
	}
	return s
}

// Once raises a notice for key unless the same one was the last for it: the sentence to stderr and a WARN line to the log.
// fields are alternating keys and values; err, effect and next say why, what it costs and what to do.
func (n *notifier) Once(key, msg string, fields ...any) {
	text := noticeText(msg, fields)
	n.mu.Lock()
	same := n.last[key] == text
	if !same {
		n.last[key], n.open[key] = text, true
	}
	n.mu.Unlock()
	if !same { // the log is written outside the lock: a failing write raises a notice itself
		fmt.Fprintln(n.w, text)
		n.log.Warn(msg, append([]any{"key", key}, fields...)...)
	}
}

// Clear ends the notice of key, when one is open: an INFO line with cleared=true.
func (n *notifier) Clear(key string) {
	n.mu.Lock()
	delete(n.last, key)
	was := n.open[key]
	delete(n.open, key)
	n.mu.Unlock()
	if was {
		n.log.Info("notice cleared", "key", key, "cleared", true)
	}
}

// hideFromSameUser makes the controller not dumpable: its /proc/<pid>/environ, mem and the like then belong to root, so no
// other process of the user (an executed script included, which is the controller's child) reads the controller's environment
// and the secrets in it (Scan 3). A child is dumpable again after its exec; it never has those secrets
// (the runner's scriptEnv). The controller refuses to start without it.
func hideFromSameUser() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0): %w", e)
	}
	return nil
}

// userHome is the user's $HOME, where a run's empty $HOME is mounted. Tests replace it: $HOME itself also locates
// podman's storage.
var userHome = os.UserHomeDir

// setUpContainers checks, once at start, that runs can start on this host: the user's $HOME is known (a run's
// empty $HOME is a tmpfs at that path) and podman answers. Then it removes every container an earlier controller of this
// <work> left (a crash, a kill: a container outlives its controller until its own timeout), before the runners
// finish their exec requests as killed (leftoverExec). If runs can't start, the controller still takes in submissions and
// carries out every other request, but no script runs. What it finds goes to the runners (Services).
func (p *process) setUpContainers() {
	home, err := userHome()
	switch {
	case err != nil:
		p.s.Podman = err
	case !filepath.IsAbs(home) || strings.ContainsAny(home, ",:\n\x00"):
		p.s.Podman = fmt.Errorf("$HOME %q: not an absolute path, or one podman can't mount at", home)
	default:
		p.s.Home = filepath.Clean(home)
		if err := container.MemoryLimits(); err != nil {
			p.s.Podman = err // a run without its memory limit isn't what plinth.exec.memory asks for
		}
		left, err := container.Leftovers(p.Work)
		if err != nil {
			p.s.Podman = err
		}
		for _, name := range left {
			if err := container.Remove(p.Work, name); err != nil {
				p.s.Podman = fmt.Errorf("the container of %s, left by an earlier controller: %w", name, err)
				continue
			}
			p.log.Info("removed a container left by an earlier controller", "container", name)
		}
	}
	if _, err := exec.LookPath("pasta"); err != nil {
		p.s.Pasta = errors.New("no pasta (package passt), so no network run starts")
		p.log.Warn("network runs can't start on this host", "err", p.s.Pasta)
	}
	if p.s.Podman != nil {
		p.log.Error("runs can't start on this host, so no script runs", "err", p.s.Podman)
		p.out.Once("podman", "runs can't start on this host, so no script runs", "err", p.s.Podman,
			"next", "check that podman works for this user (podman info)")
	}
}
