package container

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// defaultImage is the build's own image (Containerfile): present wherever plinthctl was built. It has bash.
const defaultImage = "docker.io/library/golang:1.26@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89"

// needPodman returns the image the tests run in. Without podman or the image they skip, unless
// PLINTH_TEST_PODMAN=required, which makes that a failure.
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

// newRun makes a run directory with script as its ask.sh (and as the checked copy), a home with .ssh files on this
// machine (none of which may reach the run), and an env file.
func newRun(t *testing.T, img, script string) Run {
	t.Helper()
	base := t.TempDir()
	r := Run{
		Work:    base,
		Name:    "michael/devops/main/" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())) + "-" + strconv.Itoa(os.Getpid()),
		Image:   img,
		Dir:     filepath.Join(base, "run"),
		Ask:     filepath.Join(base, "ask.copy"),
		Home:    filepath.Join(base, "home"),
		EnvFile: filepath.Join(base, "env"),
		Timeout: time.Minute,
		Memory:  "512m",
	}
	write(t, filepath.Join(r.Dir, "ask.sh"), script)
	write(t, r.Ask, script)
	write(t, filepath.Join(r.Home, ".ssh", "known_hosts"), "host-key\n")
	write(t, filepath.Join(r.Home, ".ssh", "id_ed25519"), "PERSONAL KEY\n")
	write(t, filepath.Join(r.Home, ".ssh", "config"), "Host *\n  StrictHostKeyChecking no\n")
	write(t, r.EnvFile, "FROM_ENV_FILE=yes\n")
	return r
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run runs r, then reads how its container ended and removes it: the state, podman's message, and Inspect's error.
func run(t *testing.T, r Run) (State, string, error) {
	t.Helper()
	c, err := Command(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	return ended(t, r, c)
}

// ended reads how the container of r, whose client c has ended, ended, and removes it.
func ended(t *testing.T, r Run, c *Cmd) (State, string, error) {
	t.Helper()
	st, err := Inspect(r.Work, r.Name)
	if rerr := Remove(r.Work, r.Name); rerr != nil {
		t.Error(rerr)
	}
	return st, c.Message(), err
}

func TestArgs(t *testing.T) {
	r := Run{Work: "/w", Name: "michael/devops/main/keen-tapir", Image: "sha256:abc", Dir: "/w/run", Ask: "/w/ask", Home: "/home/u",
		EnvFile: "/w/env", Lib: "/w/lib.bash", Bin: "/usr/bin/plinthctl", Timeout: 2 * time.Minute, Memory: "512m"}
	a, err := r.Args()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--memory=512m", "--memory-swap=512m", "--pull=never", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges",
		"--userns=keep-id", "--network=none", "--timeout=120", "--label", Label + "=michael/devops/main/keen-tapir",
		"--mount=type=bind,source=/w/run,destination=/aaa/run,rw", "--mount=type=bind,source=/w/ask,destination=/aaa/run/ask.sh,ro",
		"--env-file=/w/env", "--env=HOME=/home/u", "--init", WorkLabel + "=/w",
		"--mount=type=bind,source=/w/lib.bash,destination=/aaa/lib.bash,ro", "--env=AAA_LIB=/aaa/lib.bash",
		"--mount=type=bind,source=/usr/bin/plinthctl,destination=/aaa/plinthctl,ro"} {
		if !slices.Contains(a, want) {
			t.Errorf("no %q in %q", want, a)
		}
	}
	if a[0] != "create" {
		t.Errorf("podman %s, want create: Start creates the container, the client only starts it", a[0])
	}
	if got := a[len(a)-3:]; !slices.Equal(got, []string{"--entrypoint=bash", "sha256:abc", "/aaa/run/ask.sh"}) {
		t.Errorf("command %q", got)
	}
	if slices.Contains(a, "--rm") {
		t.Error("--rm: Inspect needs the container once its client ended")
	}
	r.Network = true
	n, err := r.Args()
	if err != nil || !slices.Contains(n, "--network=pasta") || len(n) != len(a) {
		t.Errorf("network mode: %q, %v", n, err)
	}
	for _, s := range append(a, n...) {
		if strings.Contains(s, "ssh") || strings.HasPrefix(s, "--passwd-entry") {
			t.Errorf("ssh in a run: %q", s)
		}
	}
	if ContainerName(r.Work, r.Name) != ContainerName(r.Work, r.Name) || !strings.HasPrefix(ContainerName(r.Work, r.Name), "plinth-") ||
		ContainerName(r.Work, r.Name) == ContainerName(r.Work, r.Name+"x") || ContainerName(r.Work, r.Name) == ContainerName("/v", r.Name) {
		t.Error("container names")
	}
	for what, bad := range map[string]Run{
		"relative dir":  {Work: "/w", Name: "n", Image: "i", Dir: "w/run", Ask: "/a", Home: "/h", Timeout: time.Minute, Memory: "1g"},
		"no image":      {Work: "/w", Name: "n", Dir: "/w", Ask: "/a", Home: "/h", Timeout: time.Minute, Memory: "1g"},
		"no timeout":    {Work: "/w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Home: "/h", Memory: "1g"},
		"name with =":   {Work: "/w", Name: "n=x", Image: "i", Dir: "/w", Ask: "/a", Home: "/h", Timeout: time.Minute, Memory: "1g"},
		"work not abs":  {Work: "w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Home: "/h", Timeout: time.Minute, Memory: "1g"},
		"no memory":     {Work: "/w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Home: "/h", Timeout: time.Minute},
		"memory 1 gig":  {Work: "/w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Home: "/h", Timeout: time.Minute, Memory: "1 g"},
		"comma in dir":  {Work: "/w", Name: "n", Image: "i", Dir: "/w/a,destination=/etc", Ask: "/a", Home: "/h", Timeout: time.Minute, Memory: "1g"},
		"comma in lib":  {Work: "/w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Lib: "/l,ro=false", Home: "/h", Timeout: time.Minute, Memory: "1g"},
		"comma in bin":  {Work: "/w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Bin: "/b,ro=false", Home: "/h", Timeout: time.Minute, Memory: "1g"},
		"colon in home": {Work: "/w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Home: "/h:exec", Timeout: time.Minute, Memory: "1g"},
		"comma in home": {Work: "/w", Name: "n", Image: "i", Dir: "/w", Ask: "/a", Home: "/h,x", Timeout: time.Minute, Memory: "1g"},
	} {
		if _, err := bad.Args(); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
}

// facts is a script that writes what it sees to out.txt in its run directory and exits 3.
const facts = `{
  echo "uid: $(id -u)"
  echo "home: $(ls -A "$HOME" | tr '\n' ' ')"
  echo "ssh: $(ls -A "$HOME/.ssh" 2> /dev/null | tr '\n' ' ')"
  echo "usr write: $( (: > /usr/x) 2> /dev/null && echo yes || echo refused)"
  echo "ask write: $( (echo x >> ask.sh) 2> /dev/null && echo yes || echo refused)"
  echo "tmp write: $( (: > /tmp/x) 2> /dev/null && echo yes || echo refused)"
  echo "net: $(ls /sys/class/net | tr '\n' ' ')"
  echo "from env file: ${FROM_ENV_FILE-unset}"
  echo "host secret: ${HOST_SECRET-unset}"
  echo "quiet: ${AAA_QUIET-unset}"
  echo "lib: $(cat "${AAA_LIB-/nonexistent}" 2> /dev/null || echo none)"
  echo "pwd: $PWD"
} > out.txt
exit 3
`

func readOut(t *testing.T, r Run) string {
	t.Helper()
	out, err := os.ReadFile(filepath.Join(r.Dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(r.Dir, "out.txt"))
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		t.Errorf("out.txt does not belong to the user: %+v", fi.Sys())
	}
	return string(out)
}

// TestNormal: the run directory and a private /tmp are writable, nothing else; $HOME is empty; no network; only the env
// file's variables; ask.sh can't be changed.
func TestNormal(t *testing.T) {
	img := needPodman(t)
	t.Setenv("HOST_SECRET", "from-the-host")
	r := newRun(t, img, facts)
	if st, msg, err := run(t, r); err != nil || !st.Started || st.ExitCode != 3 || st.OOMKilled {
		t.Fatalf("%+v, %q, %v", st, msg, err)
	}
	out := readOut(t, r)
	for _, want := range []string{"uid: " + strconv.Itoa(os.Getuid()) + "\n", "home: \n", "ssh: \n", "usr write: refused",
		"ask write: refused", "tmp write: yes", "net: lo \n", "from env file: yes", "host secret: unset", "quiet: 1",
		"lib: none", "pwd: " + RunDir + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, "ask.sh")); string(b) != facts {
		t.Error("ask.sh changed")
	}
}

// netFacts is a script that writes what a network run sees to out.txt in its run directory and exits 3.
const netFacts = `{
  echo "home: $(ls -A "$HOME" | tr '\n' ' ')"
  echo "ssh: $(ls -A "$HOME/.ssh" 2> /dev/null | tr '\n' ' ')"
  echo "aaa: $(ls -A /aaa | tr '\n' ' ')"
  echo "net: $(ls /sys/class/net | tr '\n' ' ')"
  echo "lib: $(cat "${AAA_LIB-/nonexistent}" 2> /dev/null || echo none)"
  echo "usr write: $( (: > /usr/x) 2> /dev/null && echo yes || echo refused)"
} > out.txt
exit 3
`

// TestNetwork: a network of its own besides lo, and the AAA library and the binary; no ssh: $HOME is empty, and nothing of the user's
// ~/.ssh is in the container.
func TestNetwork(t *testing.T) {
	img := needPodman(t)
	if _, err := exec.LookPath("pasta"); err != nil {
		if os.Getenv("PLINTH_TEST_PODMAN") == "required" {
			t.Fatal("PLINTH_TEST_PODMAN=required, but there is no pasta (package passt)")
		}
		t.Skip("no pasta")
	}
	r := newRun(t, img, netFacts)
	r.Network = true
	r.Lib = filepath.Join(filepath.Dir(r.Dir), "lib.bash")
	write(t, r.Lib, "LIB\n")
	r.Bin = r.Lib // any file: what counts is the mount
	if st, msg, err := run(t, r); err != nil || !st.Started || st.ExitCode != 3 {
		t.Fatalf("%+v, %q, %v", st, msg, err)
	}
	out := readOut(t, r)
	for _, want := range []string{"home: \n", "ssh: \n", "aaa: lib.bash plinthctl run \n", "lib: LIB\n", "usr write: refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "net: lo \n") {
		t.Errorf("network mode has no network:\n%s", out)
	}
}

// TestTimeout: podman ends a container that outlives its timeout.
func TestTimeout(t *testing.T) {
	img := needPodman(t)
	r := newRun(t, img, "sleep 60\n")
	r.Timeout = 2 * time.Second
	start := time.Now()
	st, msg, err := run(t, r)
	if err != nil || !st.Started || st.ExitCode == 0 || time.Since(start) > 30*time.Second {
		t.Errorf("%+v, %q, %v after %s", st, msg, err, time.Since(start))
	}
}

// TestRemove: the controller ends a run's container by name, and Leftovers lists it until then.
func TestRemove(t *testing.T) {
	img := needPodman(t)
	r := newRun(t, img, "sleep 60\n")
	c, err := Command(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if left, err := Leftovers(r.Work); err == nil && slices.Contains(left, r.Name) {
			break
		}
		if time.Now().After(end) {
			Remove(r.Work, r.Name)
			t.Fatal("the container never showed in Leftovers")
		}
	}
	if err := Remove(r.Work, r.Name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the client did not end after Remove")
	}
	if left, err := Leftovers(r.Work); err != nil || slices.Contains(left, r.Name) {
		t.Errorf("after Remove: %v, %v", left, err)
	}
	if err := Remove(r.Work, r.Name); err != nil {
		t.Errorf("Remove of a container that is gone: %v", err)
	}
}

// TestStop: Stop's SIGTERM reaches the script (--init: bash as PID 1 would ignore it), so the container ends well before
// the grace; a script that ignores SIGTERM ends at the grace (SIGKILL).
func TestStop(t *testing.T) {
	img := needPodman(t)
	for _, c := range []struct {
		script      string
		grace       time.Duration
		least, most time.Duration
	}{
		{"sleep 60\n", 30 * time.Second, 0, 20 * time.Second},
		{"trap '' TERM\nsleep 60\n", 2 * time.Second, 2 * time.Second, 25 * time.Second},
	} {
		r := newRun(t, img, c.script)
		cmd, err := Command(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		for end := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if left, err := Leftovers(r.Work); err == nil && slices.Contains(left, r.Name) {
				break
			}
			if time.Now().After(end) {
				Remove(r.Work, r.Name)
				t.Fatal("the container never showed in Leftovers")
			}
		}
		time.Sleep(time.Second) // bash has started sleep
		start := time.Now()
		if err := Stop(r.Work, r.Name, c.grace); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			Remove(r.Work, r.Name)
			t.Fatal("the client did not end after Stop")
		}
		if took := time.Since(start); took < c.least || took > c.most {
			t.Errorf("%q: ended %s after Stop, want %s..%s", c.script, took, c.least, c.most)
		}
		if st, msg, err := ended(t, r, cmd); err != nil || !st.Started || st.ExitCode == 0 {
			t.Errorf("%q: %+v, %q, %v", c.script, st, msg, err)
		}
	}
}

// TestStartFails: a client that doesn't start once its container was created removes the container, whose config
// holds the env file's values.
func TestStartFails(t *testing.T) {
	img := needPodman(t)
	r := newRun(t, img, "exit 0\n")
	c, err := Command(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Remove(r.Work, r.Name) })
	c.Cmd.Path = filepath.Join(t.TempDir(), "no-podman") // the client only: the create still runs podman
	if err := c.Start(); err == nil || errors.As(err, new(*CreateError)) {
		if err == nil {
			c.Wait() // never leave a started client behind (jolly-elm-19)
		}
		t.Fatalf("Start: %v, want the client's own error", err)
	}
	if left, err := Leftovers(r.Work); err != nil || slices.Contains(left, r.Name) {
		t.Errorf("after the failed start: %v, %v", left, err)
	}
}

// TestFailure: an image that isn't there is podman create's failure, with podman's message, before any container
// exists, and the client never starts; nothing is pulled.
func TestFailure(t *testing.T) {
	needPodman(t)
	r := newRun(t, "localhost/plinth-no-such-image:none", "exit 0\n")
	c, err := Command(r)
	if err != nil {
		t.Fatal(err)
	}
	var ce *CreateError
	if err := c.Run(); !errors.As(err, &ce) || !strings.Contains(ce.Msg, "plinth-no-such-image") || c.ProcessState != nil {
		t.Errorf("%v, %+v", err, c.ProcessState)
	} else if ee := (*exec.ExitError)(nil); !errors.As(ce.Err, &ee) || ee.ExitCode() != 125 {
		t.Errorf("podman create: %v, want exit 125", ce.Err)
	}
	if st, _, err := ended(t, r, c); err == nil {
		t.Errorf("a container exists: %+v", st)
	}
}

// TestMemory: a script over its memory limit is killed (SIGKILL), with no swap on top.
func TestMemory(t *testing.T) {
	img := needPodman(t)
	if err := MemoryLimits(); err != nil {
		t.Fatal(err)
	}
	r := newRun(t, img, "x=$(head -c 200000000 /dev/zero | tr '\\0' a)\necho survived > out.txt\n")
	r.Memory = "64m"
	// The kernel's SIGKILL; podman doesn't reliably set OOMKilled (it didn't on the test host), so it isn't required.
	st, msg, err := run(t, r)
	if err != nil || !st.Started || st.ExitCode != 128+9 {
		t.Errorf("%+v, %q, %v", st, msg, err)
	}
	t.Logf("OOMKilled: %v", st.OOMKilled)
	if _, err := os.Stat(filepath.Join(r.Dir, "out.txt")); err == nil {
		t.Error("the script went on over its memory limit")
	}
	// The same script under a limit it fits in: the limit, nothing else, killed it.
	r = newRun(t, img, "x=$(head -c 200000000 /dev/zero | tr '\\0' a)\necho survived > out.txt\n")
	r.Memory = "1g"
	if st, msg, err := run(t, r); err != nil || st.ExitCode != 0 {
		t.Errorf("under 1g: %+v, %q, %v", st, msg, err)
	}
}

// TestCreateErrorLeft: a container a cut-short create left behind, which couldn't be removed, is in the error's text.
func TestCreateErrorLeft(t *testing.T) {
	ce := &CreateError{Err: errors.New("signal: killed"), Msg: ""}
	if s := ce.Error(); strings.Contains(s, ";") {
		t.Errorf("no removal failure, yet %q", s)
	}
	ce.Left = errors.New("its container plinth-x couldn't be removed and stays until the controller restarts: exit status 125")
	if s := ce.Error(); !strings.HasSuffix(s, "; "+ce.Left.Error()) {
		t.Errorf("got %q", s)
	}
}

// TestMain: run with ProbeArg, the test binary is the smoke container's probe (Smoke mounts it).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ProbeArg {
		os.Exit(ProbeMain(os.Args[2:], os.Stdout))
	}
	os.Exit(m.Run())
}

// TestSmoke: the smoke container of each mode holds what the mode promises, and one whose probe can't run fails
// closed.
func TestSmoke(t *testing.T) {
	img := needPodman(t)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	r := Run{Work: t.TempDir(), Name: "michael/devops/smoke/normal", Image: img, Home: home, Timeout: time.Minute, Memory: "512m"}
	ctx := context.Background()
	if err := Smoke(ctx, r, bin); err != nil {
		t.Errorf("normal: %v", err)
	}
	if _, err := exec.LookPath("pasta"); err == nil {
		n := r
		n.Name, n.Network = "michael/devops/smoke/network", true
		if err := Smoke(ctx, n, bin); err != nil {
			t.Errorf("network: %v", err)
		}
	} else if os.Getenv("PLINTH_TEST_PODMAN") == "required" {
		t.Error("PLINTH_TEST_PODMAN=required, but there is no pasta (package passt)")
	}
	if err := Smoke(ctx, r, filepath.Join(t.TempDir(), "no-probe")); err == nil || !strings.Contains(err.Error(), "the normal smoke container") {
		t.Errorf("no probe binary: %v", err)
	}
	if left, err := Leftovers(r.Work); err != nil || len(left) > 0 {
		t.Errorf("left over: %v, %v", left, err)
	}
}

// TestProbeArgs: the probe refuses arguments it doesn't know, with nothing checked.
func TestProbeArgs(t *testing.T) {
	var out bytes.Buffer
	if code := ProbeMain([]string{"-mode", "privileged", "-home", "/h", "-work", "/w"}, &out); code != 2 || !strings.Contains(out.String(), "arguments") {
		t.Errorf("exit %d, %q", code, out.String())
	}
}

// TestNetworkHostAddresses measures which of this machine's own addresses (not loopback: Smoke checks that) a network
// run reaches, at a port the test listens on. It logs, never fails: the spec says this machine stays
// reachable at its other addresses.
func TestNetworkHostAddresses(t *testing.T) {
	img := needPodman(t)
	if _, err := exec.LookPath("pasta"); err != nil {
		t.Skip("no pasta")
	}
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	addrs, _ := net.InterfaceAddrs()
	var hosts []string
	for _, a := range addrs {
		if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil && !ip.IsLoopback() {
			hosts = append(hosts, ip.String())
		}
	}
	if len(hosts) == 0 {
		t.Skip("no IPv4 address but loopback")
	}
	script := "for h in " + strings.Join(hosts, " ") + "; do\n" +
		"  if timeout 3 bash -c \"exec 3<>/dev/tcp/$h/" + port + "\" 2> /dev/null; then echo \"$h reached\"; else echo \"$h not reached\"; fi\n" +
		"done > out.txt\n"
	r := newRun(t, img, script)
	r.Network = true
	if st, msg, err := run(t, r); err != nil || !st.Started || st.ExitCode != 0 {
		t.Fatalf("%+v, %q, %v", st, msg, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(readOut(t, r)), "\n") {
		t.Logf("a network run, at this machine's %s", line)
	}
}
