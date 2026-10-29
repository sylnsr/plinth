package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"plinth/internal/agents"
	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/events"
	"plinth/internal/gitrepo"
	"plinth/internal/image"
	"plinth/internal/logfile"
	"plinth/internal/names"
	"plinth/internal/ollamamcp"
	"plinth/internal/plain"
	"plinth/internal/runner"
	"plinth/internal/runstate"
	"plinth/internal/workdir"
)

var ns = names.Namespace{Owner: "michael", Repo: "devops"}

// globalCfg: nothing listens at its forgejo-url, so the drift check fails at once ("drift not checked").
const globalCfg = `forgejo-url: https://127.0.0.1:9
reviewers:
  type:
    security:
      definition: "Review the script."
  automatic: [security]
plinth:
  exec:
    privileged: false   # the default; tests add their keys under exec:
`

var env = config.Getenv(func(k string) (string, bool) {
	if k == "HOME" {
		return "/home/u", true
	}
	return "", false
})

// syncBuffer is stderr for a controller: several goroutines write to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// setup returns a <work> with a filled-in global config.
func setup(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), ".plinth")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, config.FileName), []byte(globalCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return work
}

// enabled registers agent from /r and enables it there, as plinthctl register and agent enable do.
func enabled(t *testing.T, work, agent string) {
	t.Helper()
	enabledFrom(t, work, agent, "/r")
}

func enabledFrom(t *testing.T, work, agent, repo string) {
	t.Helper()
	if _, _, err := agents.Register(work, ns, agent, repo); err != nil {
		t.Fatal(err)
	}
	g, err := config.LoadGlobal(work, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.EnableAgent(work, ns, g, env, agent, repo); err != nil {
		t.Fatal(err)
	}
}

// defaultImage is the build's own image (Containerfile): present wherever plinthctl was built. It has bash.
const defaultImage = "docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89"

// needPodman returns the image a test's runs are built on (PLINTH_TEST_IMAGE, else defaultImage). Without podman or
// the image the test skips, unless PLINTH_TEST_PODMAN=required, which makes that a failure.
func needPodman(t *testing.T) string {
	t.Helper()
	required := os.Getenv("PLINTH_TEST_PODMAN") == "required"
	if _, err := exec.LookPath("podman"); err != nil {
		if required {
			t.Fatal("PLINTH_TEST_PODMAN=required, but there is no podman")
		}
		t.Skip("no podman")
	}
	img := os.Getenv("PLINTH_TEST_IMAGE")
	if img == "" {
		img = defaultImage
	}
	if err := exec.Command("podman", "image", "exists", img).Run(); err != nil {
		if required {
			t.Fatalf("PLINTH_TEST_PODMAN=required, but image %s is not present", img)
		}
		t.Skipf("image %s is not present", img)
	}
	return img
}

// execEnabled enables agent main, pins the tests' image (pinned) and gives the controller a model (fakeModel): what a
// test that runs scripts needs.
func execEnabled(t *testing.T, work string) (dlpDown *atomic.Bool) {
	t.Helper()
	pin := pinned(t, strings.Repeat("c", 40), strings.Repeat("e", 40))
	enabled(t, work, "main")
	setPin(t, work, pin)
	return fakeModel(t, work)
}

// setPin writes pin into the namespace's config.yml, as image publish and image use do.
func setPin(t *testing.T, work string, pin config.ImagePin) {
	t.Helper()
	g, err := config.LoadGlobal(work, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SetImage(work, ns, g, env, pin); err != nil {
		t.Fatal(err)
	}
}

// testImages names the images pinned builds: localhost/plinth-controller-test-<pid>, removed by TestMain.
var testImages = "localhost/plinth-controller-test-" + strconv.Itoa(os.Getpid())

var pins = struct {
	sync.Mutex
	m map[string]config.ImagePin
}{m: map[string]config.ImagePin{}}

// pinned returns the pin of an image as image publish leaves it, built here on the test image (needPodman) with the
// labels of a published image of ns for commit and tree, once per test process: present on this machine, and named by
// its digest.
func pinned(t *testing.T, commit, tree string) config.ImagePin {
	t.Helper()
	img := needPodman(t)
	pins.Lock()
	defer pins.Unlock()
	if pin, ok := pins.m[commit+tree]; ok {
		return pin
	}
	files := []gitrepo.File{{Path: container.Containerfile, Data: []byte("FROM " + img + "\n")}}
	id, _, err := container.Build(context.Background(), files, nil, "--label", container.ImageLabel+"="+ns.String(),
		"--label", image.RevisionLabel+"="+commit, "--label", image.TreeLabel+"="+tree, "--tag", testImages+":"+tree[:12])
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("podman", "image", "inspect", "--format", "{{.Digest}}", id).Output()
	if err != nil {
		t.Fatalf("the digest of %s: %v", id, err)
	}
	pin := config.ImagePin{Ref: testImages + "@" + strings.TrimSpace(string(out)), Commit: commit, Tree: tree}
	if err := pin.Check(); err != nil {
		t.Fatal(err)
	}
	pins.m[commit+tree] = pin
	return pin
}

// TestMain removes the images pinned built. Run with container.ProbeArg, the test binary is the smoke container's probe:
// the controller mounts its own binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == container.ProbeArg {
		os.Exit(container.ProbeMain(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == ollamamcp.MainArg {
		os.Exit(ollamamcp.Main(os.Args[2:], os.Stdout, os.Stderr))
	}
	// The AAA library's text filter: the controller mounts its own binary in a run's container.
	if len(os.Args) > 1 && os.Args[1] == plain.TextArg {
		os.Exit(plain.TextMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	code := m.Run()
	if _, err := exec.LookPath("podman"); err == nil {
		out, _ := exec.Command("podman", "images", "--quiet", "--filter", "reference="+testImages).Output()
		for _, id := range strings.Fields(string(out)) {
			exec.Command("podman", "rmi", "--force", id).Run()
		}
	}
	os.Exit(code)
}

// start runs a controller until the test ends; stop stops it and returns Run's error.
func start(t *testing.T, o Options) (stderr *syncBuffer, stop func() error) {
	t.Helper()
	stderr = &syncBuffer{}
	o.Stderr, o.Getenv = stderr, env
	if o.ScanRetry == 0 {
		o.ScanRetry = time.Second // a scan that ran before the gateway was up starts again soon
	}
	if o.MCPListener == nil {
		o.MCPListener = mcpListener(t) // never plinth.mcp.port: a controller of the user's may hold it
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(45 * time.Second):
				err = errors.New("the controller did not stop within 45 s (a running script gets killGrace)")
			}
		})
		return err
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("controller: %v", err) // e.g. it could not start, which a timeout alone would not say
		}
	})
	return stderr, stop
}

// mcpListener is a listener on a free loopback port for the Plinth MCP server; the controller closes it.
func mcpListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// eventually waits up to 30 s for cond: a run may build its image and start a container first.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// logText is everything in <work>/logs/.
func logText(work string) string {
	var all strings.Builder
	files, _ := filepath.Glob(filepath.Join(work, "logs", "plinth-*.log"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all.Write(b)
	}
	return all.String()
}

// logLines is true once the log holds at least n lines that contain every one of subs (a line is key=value pairs).
func logLines(work string, n int, subs ...string) func() bool {
	return func() bool {
		count := 0
		for _, l := range strings.Split(logText(work), "\n") {
			if l != "" && !slices.ContainsFunc(subs, func(s string) bool { return !strings.Contains(l, s) }) {
				count++
			}
		}
		return count >= n
	}
}

// told is true once stderr holds s.
func told(stderr *syncBuffer, s string) func() bool {
	return func() bool {
		return strings.Contains(stderr.String(), s)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestRefusesToStart(t *testing.T) {
	ctx := context.Background()
	if err := Run(ctx, Options{Work: "rel", Getenv: env, Stderr: &syncBuffer{}}); err == nil {
		t.Error("a relative work directory was taken")
	}
	// No config yet: it is created from the template, and the user has to fill it in.
	work := filepath.Join(t.TempDir(), ".plinth")
	err := Run(ctx, Options{Work: work, Getenv: env, Stderr: &syncBuffer{}})
	if err == nil || !strings.Contains(err.Error(), "fill in "+filepath.Join(work, "config.yml")) {
		t.Fatalf("first start: %v", err)
	}
	// The template as it is: every problem at once, and no lock taken.
	err = Run(ctx, Options{Work: work, Getenv: env, Stderr: &syncBuffer{}})
	var ce *config.Error
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "still holds the template placeholder") {
		t.Errorf("start with the template: %v", err)
	}
	if exists(filepath.Join(work, LockFile)) {
		t.Error("the lock was taken although the config doesn't load")
	}
}

func TestLock(t *testing.T) {
	work := setup(t)
	_, stop := start(t, Options{Work: work})
	eventually(t, "the lock with the PID", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, LockFile))
		return string(b) == strconv.Itoa(os.Getpid())+"\n"
	})
	err := Run(context.Background(), Options{Work: work, Getenv: env, Stderr: &syncBuffer{}})
	if !errors.Is(err, ErrRunning) || !strings.Contains(err.Error(), "PID "+strconv.Itoa(os.Getpid())) {
		t.Errorf("second controller: %v, want ErrRunning naming the PID", err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// Once stopped, the lock is free again (the file stays).
	_, stop = start(t, Options{Work: work})
	eventually(t, "a new start", logLines(work, 2, "msg=started", "component=controller"))
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !logLines(work, 1, "msg=stopped", "component=controller")() {
		t.Errorf("no stop in the log:\n%s", logText(work))
	}
}

// TestStatusAndStop: over the socket the controller says how it is (Query: ready after its first poll, the MCP
// server's URL, the attached namespaces with their heartbeats, the notices until they are cleared) and stops when
// asked (RequestStop), as on Ctrl-C: Run returns nil and the lock is free again.
func TestStatusAndStop(t *testing.T) {
	work := setup(t)
	_, stop := start(t, Options{Work: work})
	if _, _, err := agents.Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	var st Status
	eventually(t, "ready, michael/devops attached with a heartbeat", func() bool {
		var err error
		st, err = Query(work)
		return err == nil && st.Ready && len(st.Namespaces) == 1 && !st.Namespaces[0].Heartbeat.IsZero()
	})
	if st.PID != os.Getpid() || st.Work != work || !strings.HasPrefix(st.MCP, "http://127.0.0.1:") || !strings.HasSuffix(st.MCP, "/mcp") ||
		st.Namespaces[0].Name != "michael/devops" || time.Since(st.Started) > time.Minute || notServed(work) != 0 {
		t.Errorf("status: %+v", st)
	}
	cfg := filepath.Join(work, "michael", "devops", config.FileName)
	if err := os.WriteFile(cfg, []byte("reviewers:\n  automatic: [nope]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the notice in the status", func() bool {
		st, err := Query(work)
		return err == nil && notServed(work) == 1 && len(st.Namespaces) == 0
	})
	if err := os.WriteFile(cfg, []byte("reviewers:\n  automatic: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the notice cleared", func() bool {
		st, err := Query(work)
		return err == nil && notServed(work) == 0 && len(st.Namespaces) == 1
	})

	if err := RequestStop(work, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the lock free", func() bool { running, _ := Running(work); return !running })
	if err := stop(); err != nil {
		t.Errorf("Run after a stop over the socket: %v", err)
	}
	if !logLines(work, 1, `msg="stopping: controller stop asked over the socket"`)() || !logLines(work, 1, "msg=stopped", "component=controller")() {
		t.Errorf("the log:\n%s", logText(work))
	}
	if _, err := Query(work); err == nil {
		t.Error("a stopped controller answers")
	}
}

// notServed counts the open notices (read from the log) that michael/devops isn't served (others, such as podman's,
// depend on the host).
func notServed(work string) int {
	open, _ := logfile.OpenNotices(work, time.Now(), 14)
	n := 0
	for _, o := range open {
		if strings.HasPrefix(o.Key, "config ") && o.Last.Get("ns") == "michael/devops" {
			n++
		}
	}
	return n
}

func TestNamespaces(t *testing.T) {
	work := setup(t)
	stderr, _ := start(t, Options{Work: work})
	beat := filepath.Join(work, "michael", "devops", filepath.FromSlash(runner.Heartbeat))

	// Registering an agent attaches its namespace: the heartbeat appears and keeps beating.
	if _, _, err := agents.Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the heartbeat", func() bool { return exists(beat) })
	fi, _ := os.Stat(beat)
	eventually(t, "a second beat", func() bool {
		now, err := os.Stat(beat)
		return err == nil && now.ModTime().After(fi.ModTime())
	})
	if b, _ := os.ReadFile(beat); string(b) != strconv.Itoa(os.Getpid())+"\n" {
		t.Errorf("heartbeat = %q, want the PID", b)
	}

	// A namespace config that doesn't load: detached, and the user is told once.
	cfg := filepath.Join(work, "michael", "devops", config.FileName)
	if err := os.WriteFile(cfg, []byte("reviewers:\n  automatic: [nope]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the namespace detached", logLines(work, 1, "msg=detached", `why="its config.yml no longer loads"`))
	eventually(t, "the user told", told(stderr, "the namespace is not served until its config loads (ns=michael/devops)"))
	time.Sleep(2500 * time.Millisecond) // two more polls
	if n := strings.Count(stderr.String(), "is not served"); n != 1 {
		t.Errorf("told %d times, want once:\n%s", n, stderr.String())
	}
	// Fixed, with automatic: [] - attached again, the empty list warned about in the log.
	if err := os.WriteFile(cfg, []byte("reviewers:\n  automatic: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "attached again", logLines(work, 2, "msg=attached", "ns=michael/devops"))
	if !logLines(work, 1, `msg="config warning"`, "ns=michael/devops", `warning="reviewers.automatic is empty`)() {
		t.Errorf("no warning about the empty automatic list:\n%s", logText(work))
	}

	// The last agent removed: detached.
	if err := agents.Remove(work, ns, "main"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "detached", logLines(work, 1, "msg=detached", `why="no agent is registered any more"`))

	// The global config changed: the user is told to restart.
	if err := os.WriteFile(filepath.Join(work, config.FileName), []byte(globalCfg+"# changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "restart notice", told(stderr, "next: plinthctl controller restart"))
}

func TestPanicRestarts(t *testing.T) {
	work := setup(t)
	var calls atomic.Int32
	poll := func(ctx context.Context, n names.Namespace, cfg *config.Namespace) error {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return nil
	}
	if _, _, err := agents.Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	start(t, Options{Work: work, Poll: poll})
	eventually(t, "the loop restarted after the panic", func() bool { return calls.Load() >= 3 })
	log := logText(work)
	if !strings.Contains(log, `msg="the namespace loop failed, restarting"`) || !strings.Contains(log, "ns=michael/devops") || !strings.Contains(log, `err="panic: boom`) {
		t.Errorf("panic not logged:\n%s", log)
	}
	for _, line := range strings.Split(strings.TrimSuffix(log, "\n"), "\n") {
		if !strings.HasPrefix(line, "time=20") {
			t.Errorf("a log entry spans lines (the stack trace must be flattened): %q", line)
		}
	}
}

func TestLogsScrubRequest(t *testing.T) {
	work := setup(t)
	logs, reqs := filepath.Join(work, "logs"), filepath.Join(work, "requests")
	for _, d := range []string{logs, reqs} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(logs, "plinth-2020-01-01.log")
	for _, f := range []string{old, filepath.Join(reqs, LogsScrub+".queued"), filepath.Join(reqs, "mystery.queued")} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, stop := start(t, Options{Work: work})
	eventually(t, "the request carried out", func() bool {
		return !exists(old) && !exists(filepath.Join(reqs, LogsScrub+".queued"))
	})
	eventually(t, "the request gone", func() bool {
		return !exists(filepath.Join(reqs, LogsScrub+".running"))
	})
	if !strings.Contains(logText(work), `msg="logs scrub: deleted log files"`) || !strings.Contains(logText(work), "files=plinth-2020-01-01.log") ||
		!strings.Contains(logText(work), `msg="request is unknown, left alone"`) || !strings.Contains(logText(work), "request=mystery.queued") {
		t.Errorf("log:\n%s", logText(work))
	}
	if !exists(filepath.Join(reqs, "mystery.queued")) {
		t.Error("an unknown request was touched")
	}
	stop()

	// A scrub that fails (a hard-linked log file is refused): .FAILED, with the reason.
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(old, filepath.Join(logs, "plinth-2020-01-02.log")); err != nil {
		t.Fatal(err)
	}
	// Left over from a crash: carried out again.
	if err := os.WriteFile(filepath.Join(reqs, LogsScrub+".running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, Options{Work: work})
	failed := filepath.Join(reqs, LogsScrub+".FAILED")
	eventually(t, "the request failed", func() bool { return exists(failed) })
	if b, _ := os.ReadFile(failed); !strings.Contains(string(b), "more than one hard link") {
		t.Errorf("%s = %q, want the reason", failed, b)
	}
	if !strings.Contains(logText(work), "left over from an earlier run") {
		t.Errorf("log:\n%s", logText(work))
	}
}

// A namespace whose event log doesn't continue cleanly doesn't act: its loop fails (and retries with backoff), and
// writes no heartbeat.
func TestDamagedEventLog(t *testing.T) {
	work := setup(t)
	if _, _, err := agents.Register(work, ns, "main", "/r"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "michael", "devops", "events", "1.jsonl"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, Options{Work: work})
	eventually(t, "the failure logged", logLines(work, 1, `msg="the namespace loop failed, restarting"`, "in=1s", `err="event log:`))
	if exists(filepath.Join(work, "michael", "devops", filepath.FromSlash(runner.Heartbeat))) {
		t.Error("a heartbeat although the event log is damaged")
	}
}

// filledTemplate is the namespace template with its header filled in, as an agent submits it.
func filledTemplate(t *testing.T) []byte {
	t.Helper()
	s := strings.Replace(string(workdir.AskTemplate), "# AAA script: <one line: what this round is for>", "# AAA script: check the disks", 1)
	s = strings.Replace(s, "# READ-ONLY diagnostics - nothing is started, stopped or changed.   (or: CHANGES: <exact list>)", "# READ-ONLY diagnostics - nothing is started, stopped or changed.", 1)
	s = strings.Replace(s, "#   1 <section>  2 <section>  3 <section>", "#   1 disks  2 smart", 1)
	if s == string(workdir.AskTemplate) {
		t.Fatal("the template's header line changed; update this test")
	}
	return []byte(s)
}

func writeFile(t *testing.T, p string, body []byte, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, perm); err != nil {
		t.Fatal(err)
	}
}

func TestSubmissions(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	work := setup(t)
	enabled(t, work, "main")
	// Registered but not enabled, and enabled for another repo than its registration's: both refused.
	for _, a := range []string{"al", "bo"} {
		if _, _, err := agents.Register(work, ns, a, "/r"); err != nil {
			t.Fatal(err)
		}
	}
	g, err := config.LoadGlobal(work, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.EnableAgent(work, ns, g, env, "bo", "/elsewhere"); err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(work, "michael", "devops")
	subs := filepath.Join(nsDir, runner.Submissions)
	good := filledTemplate(t)
	writeFile(t, filepath.Join(subs, "main", "0123456789abcdef-73.sh"), good, 0o600)
	writeFile(t, filepath.Join(subs, "main", "fedcba9876543210.sh"), append(slices.Clone(good), "cd ..\n"...), 0o600)
	writeFile(t, filepath.Join(subs, "ghost", "00000000000000aa.sh"), good, 0o600)
	writeFile(t, filepath.Join(subs, "al", "00000000000000bb.sh"), good, 0o600)
	writeFile(t, filepath.Join(subs, "bo", "00000000000000cc.sh"), good, 0o600)
	writeFile(t, filepath.Join(subs, "main", "notes.txt"), nil, 0o600)
	// A crash left one run complete but not in place (its submission still there) and one incomplete.
	sum := sha256.Sum256(good)
	writeFile(t, filepath.Join(nsDir, "runs", "main", ".new.calm-otter.1111111111111111", "ask.sh"), good, 0o700)
	writeFile(t, filepath.Join(nsDir, "runs", "main", ".new.calm-otter.1111111111111111", "ask.sh.sha256"), []byte(hex.EncodeToString(sum[:])+"  ask.sh\n"), 0o600)
	writeFile(t, filepath.Join(subs, "main", "1111111111111111.sh"), good, 0o600)
	writeFile(t, filepath.Join(nsDir, "runs", "main", ".new.eager-lynx.2222222222222222", "ask.sh"), good[:10], 0o700)
	writeFile(t, filepath.Join(subs, "main", "2222222222222222.sh"), good, 0o600)

	start(t, Options{Work: work})
	auto := []string{"security"}
	eventually(t, "three runs, each reviewed", func() bool {
		runs, err := runstate.List(work, ns, "main", auto, nil)
		if err != nil || len(runs) != 3 {
			return false
		}
		for _, r := range runs {
			if len(r.Facts.ReviewsPending) > 0 {
				return false
			}
		}
		return true
	})
	runs, _ := runstate.List(work, ns, "main", auto, nil)
	var issueRun *runstate.Run
	for _, r := range runs {
		if strings.HasSuffix(r.Name, "-73") {
			issueRun = r
		}
		// Without Ollama every automatic review fails closed, so exec needs override.review.
		if r.AskStatus() != "review-failed" {
			t.Errorf("%s: %s, want review-failed", r.Name, r.AskStatus())
		}
		b, _ := os.ReadFile(filepath.Join(work, runstate.RunDir(ns, "main", r.Name), "review.security.FAILED.md"))
		if !strings.Contains(string(b), "not available without Ollama") {
			t.Errorf("%s: review = %q", r.Name, b)
		}
	}
	if issueRun == nil {
		t.Fatalf("no run for issue 73 in %v", runs)
	}
	dir := filepath.Join(work, runstate.RunDir(ns, "main", issueRun.Name))
	script, _ := os.ReadFile(filepath.Join(dir, "ask.sh"))
	if !strings.Contains(string(script), "\nRUN_NAME=\""+issueRun.Name+"\"") || !strings.Contains(string(script), "\nAGENT_NAME=\"devops/main\"") {
		t.Errorf("header not filled in:\n%s", script)
	}
	fsum := sha256.Sum256(script)
	if b, _ := os.ReadFile(filepath.Join(dir, "ask.sh.sha256")); string(b) != hex.EncodeToString(fsum[:])+"  ask.sh\n" {
		t.Errorf("ask.sh.sha256 = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dir, "ask.sh")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("ask.sh mode: %v %v", fi, err)
	}
	// The complete leftover is in place under its name; the incomplete one is gone and its submission taken in anew.
	if _, err := os.Stat(filepath.Join(nsDir, "runs", "main", "calm-otter", "ask.sh")); err != nil {
		t.Errorf("the complete leftover was not put in place: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(nsDir, "runs", "main", ".new.*"))
	remaining, _ := filepath.Glob(filepath.Join(subs, "main", "*.sh"))
	if len(left) != 0 || len(remaining) != 0 {
		t.Errorf("left over: %v, submissions: %v", left, remaining)
	}
	// The refused ones are kept with their reason.
	for _, c := range []struct{ dir, want string }{
		{filepath.Join(nsDir, runner.Rejected, "main", "fedcba9876543210"), "Rule: ask.lint"},
		{filepath.Join(nsDir, runner.Rejected, "ghost", "00000000000000aa"), "agent ghost is not registered"},
		{filepath.Join(nsDir, runner.Rejected, "al", "00000000000000bb"), "agent al is registered but not enabled"},
		{filepath.Join(nsDir, runner.Rejected, "bo", "00000000000000cc"), "agent bo is registered but not enabled"},
	} {
		b, _ := os.ReadFile(filepath.Join(c.dir, "reason.md"))
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%s/reason.md = %q, want %q", c.dir, b, c.want)
		}
		if _, err := os.Stat(filepath.Join(c.dir, "ask.sh")); err != nil {
			t.Errorf("%s: the script isn't kept: %v", c.dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(subs, "main", "notes.txt")); err != nil {
		t.Error("a file that isn't a submission was touched")
	}
	// Every step is in the event log, and the chain holds.
	var actions []string
	if err := events.Walk(work, filepath.Join("michael", "devops"), func(e events.Event, _ []byte) error {
		actions = append(actions, e.Action+" "+e.Outcome+" "+e.Rule)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ask.submit allow allow", "ask.stored ok allow", "ask.submit deny ask.lint", "ask.submit deny target.valid", "review.security failed review.failed"} {
		if !slices.Contains(actions, want) {
			t.Errorf("no %q event in %v", want, actions)
		}
	}
}

// TestHideFromSameUser: once the controller started, it is not dumpable, so its /proc/<pid>/environ isn't readable by the
// user's other processes.
func TestHideFromSameUser(t *testing.T) {
	if err := hideFromSameUser(); err != nil {
		t.Fatal(err)
	}
	if d, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0); e != 0 || d != 0 {
		t.Errorf("PR_GET_DUMPABLE = %d (errno %v), want 0", d, e)
	}
}
