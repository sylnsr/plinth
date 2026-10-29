// Package container runs an executed ask script in a rootless podman container of its own: the image the
// namespace's repo defines in .plinth/Containerfile, only the mounts the run needs (its run directory read-write; the
// checked ask.sh, the AAA library and the controller's binary, its text filter, read-only; an empty $HOME), the
// variables of one env file, no capability, a read-only root, no network in normal mode and a network of its own
// (pasta) in network mode. No mode gets ssh: nothing of ~/.ssh is mounted.
// Plinth calls the podman command; no Go dependency. Spec: plinthctl.README.md, "Run modes".
package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Label marks every container of a run; its value is the run's Name, <owner>/<repo>/<agent>/<RUN>.
	Label = "plinth.run"
	// WorkLabel marks it with the <work> of the controller that started it: a controller ends only its own containers, never
	// those of another <work> (another controller, a test).
	WorkLabel = "plinth.work"
	// RunDir is where the run directory is mounted in the container, as its working directory.
	RunDir = "/aaa/run"
	// LibPath is where the AAA library is mounted in the container; $AAA_LIB names it.
	LibPath = "/aaa/lib.bash"
	// BinPath is where the controller's own binary is mounted in the container (read-only): the AAA library's text
	// filter.
	BinPath = "/aaa/plinthctl"
)

// Run is one run's container.
type Run struct {
	Work    string        // the controller's <work>, absolute: WorkLabel's value
	Name    string        // <owner>/<repo>/<agent>/<RUN>: Label's value; the container's name is derived from both
	Image   string        // the image (its ID), built and present already: the container never pulls
	Dir     string        // the run directory, absolute: mounted read-write at RunDir
	Ask     string        // the checked copy of ask.sh, absolute: mounted read-only over RunDir/ask.sh
	Lib     string        // the AAA library, absolute, mounted read-only at LibPath; "" for none
	Bin     string        // the controller's binary, absolute, mounted read-only at BinPath (the library's text filter); "" for none
	EnvFile string        // the namespace's env file, absolute; "" for none
	Home    string        // the user's $HOME, absolute: an empty tmpfs at that path in the container
	Network bool          // network mode: a network of its own (pasta)
	Timeout time.Duration // podman ends the container after it (at least a second)
	Memory  string        // the memory limit, swap included: plinth.exec.memory, checked by config
}

// memoryRe is a memory limit as plinth.exec.memory takes it (config checks the range).
var memoryRe = regexp.MustCompile(`^[1-9][0-9]{0,8}[kmg]$`)

func (r Run) check() error {
	if r.Name == "" || strings.ContainsAny(r.Name, "\n\x00=") {
		return fmt.Errorf("container name %q: empty or not a label value", r.Name)
	}
	if !filepath.IsAbs(r.Work) || filepath.Clean(r.Work) != r.Work || strings.ContainsAny(r.Work, "\n\x00") {
		return fmt.Errorf("work %q: not an absolute, clean path", r.Work)
	}
	if r.Image == "" {
		return errors.New("no image")
	}
	// A comma in a --mount path would start another option, a colon in the --tmpfs path the tmpfs options.
	for what, p := range map[string]string{"run directory": r.Dir, "ask.sh copy": r.Ask, "home": r.Home} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, ",\n\x00") {
			return fmt.Errorf("%s %q: not an absolute, clean path podman can mount", what, p)
		}
	}
	if strings.Contains(r.Home, ":") {
		return fmt.Errorf("home %q: a colon, which podman's --tmpfs can't take", r.Home)
	}
	for what, p := range map[string]string{"AAA library": r.Lib, "binary": r.Bin, "env file": r.EnvFile} {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, ",\n\x00")) {
			return fmt.Errorf("%s %q: not an absolute, clean path podman can mount", what, p)
		}
	}
	if r.Timeout < time.Second {
		return fmt.Errorf("timeout %s: under a second", r.Timeout)
	}
	if !memoryRe.MatchString(r.Memory) {
		return fmt.Errorf("memory limit %q: not <n>k, <n>m or <n>g", r.Memory)
	}
	return nil
}

// ContainerName is the container's name for a run Name of a <work>: fixed, so a second container for the same run
// can't start (podman refuses a name in use), and the controller can end it by name.
func ContainerName(work, name string) string {
	s := sha256.Sum256([]byte(work + "\n" + name))
	return "plinth-" + hex.EncodeToString(s[:8])
}

// Args are podman create's arguments for the run's container: bash runs ask.sh, whatever ENTRYPOINT the image has.
func (r Run) Args() ([]string, error) {
	return r.args(nil, "bash", RunDir+"/ask.sh")
}

// args are podman create's arguments for the run's container, with the flags extra added and entry as the entrypoint,
// given cmd: an image's own ENTRYPOINT would get cmd as its arguments otherwise.
func (r Run) args(extra []string, entry string, cmd ...string) ([]string, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	network := "none"
	if r.Network {
		network = "pasta" // its own network namespace: the internet, not this machine's loopback services
	}
	// No --rm: once the client ends, Inspect reads how the container ended, then Remove takes it away.
	a := []string{"create", "--pull=never",
		"--name", ContainerName(r.Work, r.Name), "--label", Label + "=" + r.Name, "--label", WorkLabel + "=" + r.Work,
		"--init",      // PID 1 passes a stop's SIGTERM on to bash (which, as PID 1, would ignore it), and reaps
		"--read-only", // the image's file system; podman adds a private /tmp, /var/tmp and /run (--read-only-tmpfs)
		"--cap-drop=all", "--security-opt=no-new-privileges",
		"--userns=keep-id", // the user's uid and gid inside: what the script writes in the run directory stays the user's
		"--network=" + network,
		"--timeout=" + strconv.Itoa(int(r.Timeout/time.Second)),
		"--memory=" + r.Memory, "--memory-swap=" + r.Memory, // the same: no swap on top
		// The user's $HOME, empty. Mode 1777: the tmpfs belongs to the container's root, and the user is the only other
		// uid in the container.
		"--tmpfs=" + r.Home + ":rw,nosuid,nodev,size=64m,mode=1777",
		"--mount=type=bind,source=" + r.Dir + ",destination=" + RunDir + ",rw",
		"--mount=type=bind,source=" + r.Ask + ",destination=" + RunDir + "/ask.sh,ro",
		"--workdir=" + RunDir,
		"--env=HOME=" + r.Home, "--env=AAA_QUIET=1",
	}
	if r.Lib != "" {
		a = append(a, "--mount=type=bind,source="+r.Lib+",destination="+LibPath+",ro", "--env=AAA_LIB="+LibPath)
	}
	if r.Bin != "" {
		a = append(a, "--mount=type=bind,source="+r.Bin+",destination="+BinPath+",ro")
	}
	if r.EnvFile != "" {
		a = append(a, "--env-file="+r.EnvFile)
	}
	a = append(append(a, extra...), "--entrypoint="+entry, r.Image)
	return append(a, cmd...), nil
}

// createWait is the longest podman create may take.
const createWait = 2 * time.Minute

// Cmd is a run's podman client: podman start --attach, once Start has created the container.
// Stdin and stdout are /dev/null; stderr goes to a small buffer, which holds podman's own message when it couldn't
// start the container (and whatever the script wrote to stderr, which is never shown otherwise).
type Cmd struct {
	*exec.Cmd
	podman string   // podman's path, for create
	create []string // podman create's arguments
	work   string   // the run's Work and Name: whose container a failed start removes
	name   string
	stderr *capped
}

// CreateError is a podman create that failed: no script ran, so its stderr is podman's alone and can be shown.
type CreateError struct {
	Err  error  // how podman create ended
	Msg  string // the start of its stderr
	Left error  // a create cut short whose container couldn't be removed: it stays until the controller's next start
}

func (e *CreateError) Error() string {
	s := fmt.Sprintf("podman create: %v: %s", e.Err, e.Msg)
	if e.Left != nil {
		s += "; " + e.Left.Error()
	}
	return s
}

// Command returns the podman client for the run, not started.
func Command(r Run) (*Cmd, error) {
	args, err := r.Args()
	if err != nil {
		return nil, err
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		return nil, err
	}
	c := &Cmd{Cmd: exec.Command(podman, "start", "--attach", ContainerName(r.Work, r.Name)), podman: podman, create: args,
		work: r.Work, name: r.Name, stderr: &capped{max: 4096}}
	c.Dir = "/"
	c.Stderr = c.stderr
	return c, nil
}

// Start creates the container (podman create, with the client's Env and SysProcAttr: --env-file is read now) and
// starts the client that runs it. A create that fails returns a *CreateError. One podman refused made nothing, and
// leaves a container of that name alone: podman refuses a name in use, whoever uses it. A create cut short at
// createWait may have made the container already, and a client that doesn't start leaves it created: both remove it,
// since its config holds the env file's values and it is this run's.
func (c *Cmd) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), createWait)
	defer cancel()
	msg := &capped{max: 4096}
	create := exec.CommandContext(ctx, c.podman, c.create...)
	create.Dir, create.Env, create.SysProcAttr, create.Stderr = c.Dir, c.Env, c.SysProcAttr, msg
	create.WaitDelay = time.Second // a child of a killed podman may hold stderr
	if err := create.Run(); err != nil {
		ce := &CreateError{Err: err, Msg: strings.TrimSpace(msg.String())}
		if ctx.Err() != nil {
			ce.Left = removeLeft(c.work, c.name)
		}
		return ce
	}
	if err := c.Cmd.Start(); err != nil {
		if rerr := removeLeft(c.work, c.name); rerr != nil {
			return fmt.Errorf("%w; %v", err, rerr)
		}
		return err
	}
	return nil
}

// removeLeft removes a run's container a failed start left, and says so when it can't: the container's config holds
// the env file's values, and it stays until the controller's next start.
func removeLeft(work, name string) error {
	if err := Remove(work, name); err != nil {
		return fmt.Errorf("its container %s couldn't be removed and stays until the controller restarts: %v", ContainerName(work, name), err)
	}
	return nil
}

// Run is Start, then Wait.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Message is the start of what podman start wrote to stderr: its own message when it couldn't start the container,
// and whatever the script wrote there, so it is shown only for a container that didn't start.
func (c *Cmd) Message() string {
	return strings.TrimSpace(c.stderr.String())
}

// State is how a run's container ended, as podman inspect tells it.
type State struct {
	Started   bool   // the container started
	ExitCode  int    // the script's exit code, once Started
	OOMKilled bool   // the kernel ended it over its memory limit
	Error     string // podman's own error, when it couldn't start the container
}

// Inspect reads how the run's container ended. The container runs without --rm, so it is still there once its client
// has ended; Remove takes it away afterwards.
func Inspect(work, name string) (State, error) {
	// The client can end before podman has recorded the exit of a container it stopped (its exit code is -1 until then):
	// podman wait returns once it has. An error (no such container) shows in inspect.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wait := exec.CommandContext(ctx, "podman", "wait", ContainerName(work, name))
	wait.Run()
	out, err := podmanOut("inspect", "--type=container", "--format={{json .State}}", ContainerName(work, name))
	if err != nil {
		return State{}, err
	}
	var s struct {
		ExitCode  int
		OOMKilled bool
		Error     string
		StartedAt time.Time
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return State{}, fmt.Errorf("podman inspect: %v", err)
	}
	return State{Started: !s.StartedAt.IsZero(), ExitCode: s.ExitCode, OOMKilled: s.OOMKilled, Error: strings.TrimSpace(s.Error)}, nil
}

// MemoryLimits says whether podman can limit a container's memory: rootless podman needs cgroups v2 with the memory
// controller delegated to the user, which systemd does by default on most distributions.
func MemoryLimits() error {
	out, err := podmanOut("info", "--format={{.Host.CgroupsVersion}} {{.Host.CgroupControllers}}")
	if err != nil {
		return err
	}
	f := strings.Fields(strings.NewReplacer("[", " ", "]", " ").Replace(out))
	if len(f) == 0 || f[0] != "v2" || !slices.Contains(f[1:], "memory") {
		return fmt.Errorf("podman can't limit a container's memory (cgroups %q): it needs cgroups v2 with the memory controller delegated to this user", strings.TrimSpace(out))
	}
	return nil
}

// Stop ends the run's container: SIGTERM to the script, SIGKILL to all of it grace later (whole seconds, rounded up);
// one that doesn't exist (any more) is no error.
func Stop(work, name string, grace time.Duration) error {
	return podman("stop", "--ignore", "--time="+strconv.Itoa(int((grace+time.Second-1)/time.Second)), ContainerName(work, name))
}

// Remove ends and removes the run's container at once; one that doesn't exist (any more) is no error.
func Remove(work, name string) error {
	return podman("rm", "--force", "--ignore", "--time=0", ContainerName(work, name))
}

// Leftovers lists the runs (their Names) of work that have a container, running or not: at its start the controller
// removes them. A service's container (Service) has no Label, so it isn't one of them. podman's label filters must all
// match.
func Leftovers(work string) ([]string, error) {
	out, err := podmanOut("ps", "--all", "--filter", "label="+WorkLabel+"="+work, "--filter", "label="+Label, "--format", `{{index .Labels "`+Label+`"}}`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func podman(args ...string) error {
	_, err := podmanOut(args...)
	return err
}

func podmanOut(args ...string) (string, error) {
	var out bytes.Buffer
	msg := &capped{max: 4096}
	cmd := exec.Command("podman", args...)
	cmd.Stdout, cmd.Stderr = &out, msg
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podman %s: %v: %s", args[0], err, strings.TrimSpace(msg.String()))
	}
	return out.String(), nil
}

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	mu  sync.Mutex
	max int
	b   bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}
