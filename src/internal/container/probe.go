package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProbeArg is the hidden first argument that makes plinthctl (or a test binary, from its TestMain) the probe:
// ProbeMain checks a run's container from inside.
const ProbeArg = "__container-probe"

// ProbePath is where Smoke mounts the probe binary in the container, read-only.
const ProbePath = "/aaa/probe"

// smokeWait bounds a smoke container: podman run of a static binary that checks a few files and dials a few
// addresses for a second each.
const smokeWait = time.Minute

// Smoke runs the probe binary bin (a static binary that takes ProbeArg: plinthctl itself) in a container made with
// exactly the flags of r's run, the script swapped for the probe, and returns why that container doesn't hold what the
// run's mode promises (plinthctl.README.md, Run modes, Requirements): the user's uid and gid, no capability,
// no_new_privs, a seccomp filter, a read-only root, an empty $HOME, no <work>, and loopback only in normal mode. In
// network mode the probe also tries the addresses through which pasta could reach this machine's loopback, at the
// port of a listener Smoke holds on 127.0.0.1: a connection that arrives there fails the check. r's Dir and Ask are
// made here; its Name is the probe's.
func Smoke(ctx context.Context, r Run, bin string) error {
	dir, err := os.MkdirTemp("", "plinth-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	r.Dir, r.Ask = filepath.Join(dir, "run"), filepath.Join(dir, "ask.sh")
	if err := os.Mkdir(r.Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(r.Ask, nil, 0o600); err != nil {
		return err
	}
	if !filepath.IsAbs(bin) || strings.ContainsAny(bin, ",\n\x00") {
		return fmt.Errorf("probe binary %q: not an absolute path podman can mount", bin)
	}
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer ln.Close()
	mode := "normal"
	if r.Network {
		mode = "network"
	}
	// The run's flags, with the probe mounted and run instead of bash ask.sh; podman run --rm instead of create.
	a, err := r.args([]string{"--mount=type=bind,source=" + bin + ",destination=" + ProbePath + ",ro"}, ProbePath,
		ProbeArg, "-mode", mode, "-uid", strconv.Itoa(os.Getuid()), "-gid", strconv.Itoa(os.Getgid()), "-home", r.Home,
		"-work", r.Work, "-port", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	if err != nil {
		return err
	}
	a = append([]string{"run", "--rm"}, a[1:]...)
	ctx, cancel := context.WithTimeout(ctx, smokeWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "podman", a...)
	cmd.Dir = "/"
	out, msg := &capped{max: 4096}, &capped{max: 4096}
	cmd.Stdout, cmd.Stderr = out, msg
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	// A connection the probe made has been accepted by the kernel already: it waits in the listener's backlog.
	reached := 0
	ln.SetDeadline(time.Now().Add(100 * time.Millisecond))
	for {
		c, aerr := ln.Accept()
		if aerr != nil {
			break
		}
		reached++
		c.Close()
	}
	verdict := strings.TrimSpace(out.String())
	switch {
	case err != nil && verdict == "":
		return fmt.Errorf("the %s smoke container didn't run: %v: %s", mode, err, strings.TrimSpace(msg.String()))
	case err != nil || verdict != "ok":
		return fmt.Errorf("the %s smoke container: %s", mode, strings.ReplaceAll(verdict, "\n", "; "))
	case reached > 0:
		return fmt.Errorf("the %s smoke container reached a service on this machine's loopback", mode)
	}
	return nil
}

// ProbeMain is the probe, inside the smoke container: it checks what Smoke says, prints "ok" or one line per failed
// check to stdout, and returns the exit code.
func ProbeMain(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet(ProbeArg, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mode := fs.String("mode", "", "normal or network")
	uid, gid := fs.Int("uid", -1, ""), fs.Int("gid", -1, "")
	home, work := fs.String("home", "", ""), fs.String("work", "", "")
	port := fs.Int("port", 0, "")
	if err := fs.Parse(args); err != nil || (*mode != "normal" && *mode != "network") || *home == "" || *work == "" {
		fmt.Fprintln(stdout, "the probe's arguments are wrong")
		return 2
	}
	var fails []string
	fail := func(format string, a ...any) { fails = append(fails, fmt.Sprintf(format, a...)) }
	if os.Getuid() != *uid || os.Getgid() != *gid {
		fail("uid %d, gid %d: not the user's %d, %d", os.Getuid(), os.Getgid(), *uid, *gid)
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		fail("/proc/self/status: %v", err)
	}
	for _, want := range []struct{ key, ok string }{{"CapEff", "zero"}, {"CapPrm", "zero"}, {"CapBnd", "zero"},
		{"NoNewPrivs", "1"}, {"Seccomp", "2"}} {
		v := statusField(status, want.key)
		if want.ok == "zero" {
			if b, err := hex.DecodeString(v); err != nil || len(b) == 0 || bytes.Count(b, []byte{0}) != len(b) {
				fail("%s %q: capabilities left", want.key, v)
			}
		} else if v != want.ok {
			fail("%s %q, want %s", want.key, v, want.ok)
		}
	}
	if err := os.Mkdir("/plinth-probe", 0o700); err == nil {
		os.Remove("/plinth-probe")
		fail("the root file system is writable")
	} else if !errors.Is(err, syscall.EROFS) {
		fail("the root file system: %v, not read-only", err)
	}
	if entries, err := os.ReadDir(*home); err != nil || len(entries) > 0 {
		fail("$HOME %s: not empty (%d entries, %v)", *home, len(entries), err)
	}
	if _, err := os.Stat(*work); !errors.Is(err, os.ErrNotExist) {
		fail("<work> %s: visible in the container (%v)", *work, err)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		fail("network interfaces: %v", err)
	}
	others := 0
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback == 0 && i.Flags&net.FlagUp != 0 {
			others++
		}
	}
	switch {
	case *mode == "normal" && others > 0:
		fail("normal mode: %d network interfaces besides loopback", others)
	case *mode == "network" && others == 0:
		fail("network mode: no network interface besides loopback")
	case *mode == "network":
		// The paths pasta could map to this machine's loopback: Smoke's listener there counts what arrives.
		for _, host := range hostCandidates() {
			if c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(*port)), time.Second); err == nil {
				c.Close()
			}
		}
	}
	if len(fails) == 0 {
		fmt.Fprintln(stdout, "ok")
		return 0
	}
	for _, f := range fails {
		fmt.Fprintln(stdout, f)
	}
	return 1
}

// statusField is the value of a "Key:\tvalue" line of /proc/self/status.
func statusField(status []byte, key string) string {
	for _, line := range strings.Split(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// hostCandidates are the addresses through which a container could reach the host: its default gateway, and what
// /etc/hosts names host.containers.internal or host.docker.internal (podman writes them), read from the file, so no
// DNS query leaves.
func hostCandidates() []string {
	var out []string
	if b, err := os.ReadFile("/proc/net/route"); err == nil {
		for _, line := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 3 || f[1] != "00000000" {
				continue
			}
			if g, err := hex.DecodeString(f[2]); err == nil && len(g) == 4 {
				out = append(out, net.IPv4(g[3], g[2], g[1], g[0]).String()) // little-endian
			}
		}
	}
	if f, err := os.Open("/etc/hosts"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(strings.SplitN(sc.Text(), "#", 2)[0])
			for _, name := range fields[min(1, len(fields)):] {
				if name == "host.containers.internal" || name == "host.docker.internal" {
					out = append(out, fields[0])
				}
			}
		}
	}
	return out
}
