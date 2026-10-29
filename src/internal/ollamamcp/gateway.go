package ollamamcp

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"plinth/internal/config"
	"plinth/internal/container"
	"plinth/internal/logfile"
	"plinth/internal/ollama"
	"plinth/internal/safefs"
)

// MainArg is the hidden first argument that makes plinthctl (or a test binary, from its TestMain) the gateway itself, in
// its container: Main.
const MainArg = "__ollama-gateway"

// ServiceName is the gateway's name as a service (container.Service).
const ServiceName = "ollama-gateway"

// RootDir is the gateway's empty root directory below <work>.
const RootDir = "state/ollama-root"

// sockMount is where SocketDir is mounted in the container; caMount is where the host's CA bundle is.
const (
	sockMount = "/sock"
	caMount   = "/etc/plinth-ca.pem"
)

// memory is the gateway's memory limit: a static Go binary that passes JSON on.
const memory = "256m"

// caFiles are where the host's CA bundle may be, as Go's crypto/x509 looks for it: the first that exists is mounted, so
// an https ollama.host can be checked.
var caFiles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "/etc/ssl/cert.pem"}

// Main is the gateway in its container: it serves the socket in -socket-dir for the Ollama of -host until SIGTERM or
// SIGINT, with a line per tool call (and the start and the stop) on stdout, which podman keeps (ollama-mcp status shows
// the last ones).
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(MainArg, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("socket-dir", sockMount, "the directory of the socket")
	host := fs.String("host", "", "ollama.host")
	timeout := fs.Int("timeout-seconds", 300, "ollama.timeout-seconds")
	retry := fs.Int("retry-seconds", 10, "ollama.retry-seconds")
	parallel := fs.Int("max-parallel", 1, "ollama.max-parallel")
	version := fs.String("version", "", "plinthctl's version, for serverInfo")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "ollama gateway: bad arguments")
		return 2
	}
	lg := logfile.NewTo(stdout, logfile.OllamaGateway) // the container's output, which the controller reads with podman logs
	c := ollama.New(config.Ollama{Host: *host, TimeoutSeconds: *timeout, RetrySeconds: *retry, MaxParallel: *parallel})
	if !c.Configured() {
		fmt.Fprintf(stderr, "ollama gateway: %v\n", ollama.ErrNotConfigured)
		return 1
	}
	ln, err := Listen(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "ollama gateway: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lg.Info("serving", "socket", filepath.Join(*dir, SocketName), "ollama", c.Host, "max_parallel", *parallel)
	if err := Serve(ctx, ln, New(Options{Client: c, Version: *version, Log: lg})); err != nil {
		fmt.Fprintf(stderr, "ollama gateway: %v\n", err)
		return 1
	}
	lg.Info("stopped")
	return 0
}

// Gateway is the gateway's container for a <work>: what it needs to describe it (Service).
type Gateway struct {
	Work    string        // absolute
	Bin     string        // plinthctl's binary, static (CGO_ENABLED=0): what the container runs
	Ollama  config.Ollama // its host, timeouts, retry and max-parallel
	Version string
}

// Executable is the running plinthctl's binary, its symlinks resolved: Gateway's Bin.
func Executable() (string, error) {
	bin, err := os.Executable()
	if err == nil {
		bin, err = filepath.EvalSymlinks(bin)
	}
	return bin, err
}

// Service is the gateway's description: the empty root, the binary, the socket's directory read-write and the host's
// CA bundle read-only (when there is one), the host's network (Ollama may be anywhere on the LAN or a VPN; hardening
// comes after the MVP), and the ollama: settings as arguments, so it reads nothing of <work>.
func (g Gateway) Service() container.Service {
	o := g.Ollama
	s := container.Service{
		Work: g.Work, Name: ServiceName, Root: filepath.Join(g.Work, filepath.FromSlash(RootDir)), Bin: g.Bin,
		Args: []string{MainArg, "-socket-dir", sockMount, "-host", o.Host, "-timeout-seconds", strconv.Itoa(o.TimeoutSeconds),
			"-retry-seconds", strconv.Itoa(o.RetrySeconds), "-max-parallel", strconv.Itoa(o.MaxParallel), "-version", g.Version},
		Mounts:  []container.Mount{{Source: filepath.Join(g.Work, filepath.FromSlash(SocketDir)), Dest: sockMount}},
		Network: "host",
		Memory:  memory,
	}
	for _, f := range caFiles {
		if p, err := filepath.EvalSymlinks(f); err == nil {
			s.Mounts = append(s.Mounts, container.Mount{Source: p, Dest: caMount, ReadOnly: true})
			s.Env = append(s.Env, "SSL_CERT_FILE="+caMount)
			break
		}
	}
	return s
}

// Start starts the gateway's container unless one of the same description and binary runs already, and says which it
// did (container.StartService: running, started, replaced). Without ollama.host it starts nothing (ErrNotConfigured).
// The binary must be static: the root is empty, so there is no C library to load.
func (g Gateway) Start() (string, error) {
	if g.Ollama.Host == "" {
		return "", ollama.ErrNotConfigured
	}
	spec, err := g.Spec()
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(g.Work)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, d := range []string{"state", SocketDir, RootDir} {
		if _, err := safefs.EnsureDir(root, filepath.FromSlash(d)); err != nil {
			return "", err
		}
	}
	if err := mountpoints(root); err != nil {
		return "", fmt.Errorf("the gateway's root: %w", err)
	}
	did, err := container.StartService(g.Service(), spec)
	if err == nil && did != "running" {
		err = waitSocket(SocketPath(g.Work), 10*time.Second)
	}
	if err != nil {
		if out, lerr := Logs(g.Work, 5); lerr == nil && strings.TrimSpace(out) != "" {
			err = fmt.Errorf("%w; its last lines: %s", err, strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
		}
		return "", fmt.Errorf("the Ollama gateway didn't start: %w", err)
	}
	return did, nil
}

// Spec is the digest of the gateway's description and binary, which its container carries (container.SpecLabel).
func (g Gateway) Spec() (string, error) {
	sum, err := staticSum(g.Bin)
	if err != nil {
		return "", err
	}
	return g.Service().Spec(sum)
}

// The directories and files of the empty root that podman mounts over. Start makes them, owned by the user: made by
// podman they could belong to another uid of its user namespace, which the user couldn't remove with <work>.
var (
	rootDirs  = []string{"dev", "proc", "sys", "etc", "run", "tmp", "var", "var/tmp", sockMount[1:]}
	rootFiles = []string{container.ServiceBin[1:], "etc/hosts", "etc/hostname", "etc/resolv.conf", caMount[1:]}
)

// mountpoints makes rootDirs and rootFiles below RootDir of the <work> at root, unless they exist.
func mountpoints(root *os.Root) error {
	for _, d := range rootDirs {
		if _, err := safefs.EnsureDir(root, filepath.Join(filepath.FromSlash(RootDir), filepath.FromSlash(d))); err != nil {
			return err
		}
	}
	for _, f := range rootFiles {
		p := filepath.Join(filepath.FromSlash(RootDir), filepath.FromSlash(f))
		fh, err := safefs.OpenCreate(root, filepath.Dir(p), filepath.Base(p), os.O_RDONLY, 0o600)
		if err != nil {
			return err
		}
		fh.Close()
	}
	return nil
}

// staticSum is the sha256 of the binary at bin, which must be a static ELF file (no interpreter).
func staticSum(bin string) (string, error) {
	f, err := elf.Open(bin)
	if err != nil {
		return "", fmt.Errorf("the gateway's binary %s: %w", bin, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return "", fmt.Errorf("the gateway's binary %s is not static (build it with CGO_ENABLED=0): its root has no C library", bin)
		}
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Stop ends the gateway's container of work; none is no error.
func Stop(work string) error {
	return container.StopService(work, ServiceName, 5*time.Second)
}

// State is what podman says of the gateway's container of work.
func State(work string) (container.ServiceState, error) {
	return container.StateOf(work, ServiceName)
}

// Logs is the end of the gateway's output: its last n lines.
func Logs(work string, n int) (string, error) {
	return container.ServiceLogs(work, ServiceName, n)
}

// errNotRunning is a call to a gateway whose socket nobody serves.
var errNotRunning = errors.New("the Ollama gateway isn't running (plinthctl ollama-mcp start)")

// SocketPath is the gateway's socket of work.
func SocketPath(work string) string {
	return filepath.Join(work, filepath.FromSlash(SocketDir), SocketName)
}

// waitSocket waits up to d until the socket at path takes connections: the container is up before the gateway in it
// listens.
func waitSocket(path string, d time.Duration) error {
	for end := time.Now().Add(d); ; time.Sleep(100 * time.Millisecond) {
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("its socket %s takes no connections after %s", path, d)
		}
	}
}
