package container

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ServiceLabel marks the container of a service, a long-running container Plinth starts besides the runs:
// its value is the service's Name. SpecLabel holds the digest of the description it was started from.
const (
	ServiceLabel = "plinth.service"
	SpecLabel    = "plinth.spec"
)

// ServiceBin is where a service's program, plinthctl itself, is mounted read-only.
const ServiceBin = "/plinthctl"

// Service is one service's container, described in Go and started by the podman backend below (the shape of a
// Kubernetes pod, no Kubernetes code). Its root is an empty directory, its program the host's own plinthctl, mounted
// read-only; the root is read-only, the container has no capability and runs as the user.
type Service struct {
	Work    string   // the <work> it belongs to, absolute: WorkLabel's value
	Name    string   // e.g. ollama-gateway: ServiceLabel's value; the container's name is derived from both
	Root    string   // an empty directory, absolute: the container's root file system
	Bin     string   // plinthctl's binary, absolute and static: mounted read-only at ServiceBin, which runs with Args
	Args    []string // ServiceBin's arguments
	Mounts  []Mount  // more bind mounts
	Env     []string // NAME=value, for ServiceBin
	Network string   // podman's --network: host, or none
	Memory  string   // the memory limit, swap included
}

// Mount is a bind mount of a service.
type Mount struct {
	Source, Dest string // absolute
	ReadOnly     bool
}

// ServiceName is the container's name of service name of a <work>.
func ServiceName(work, name string) string {
	return ContainerName(work, "service/"+name)
}

func (s Service) check() error {
	if s.Name == "" || strings.ContainsAny(s.Name, "\n\x00=/") {
		return fmt.Errorf("service name %q: empty or not a label value", s.Name)
	}
	if !filepath.IsAbs(s.Work) || filepath.Clean(s.Work) != s.Work || strings.ContainsAny(s.Work, "\n\x00") {
		return fmt.Errorf("work %q: not an absolute, clean path", s.Work)
	}
	paths := map[string]string{"root": s.Root, "binary": s.Bin}
	for i, m := range s.Mounts {
		paths[fmt.Sprintf("mount %d source", i)], paths[fmt.Sprintf("mount %d destination", i)] = m.Source, m.Dest
	}
	for what, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, ",\n\x00") {
			return fmt.Errorf("%s %q: not an absolute, clean path podman can mount", what, p)
		}
	}
	if s.Network != "host" && s.Network != "none" {
		return fmt.Errorf("network %q: host or none", s.Network)
	}
	if !memoryRe.MatchString(s.Memory) {
		return fmt.Errorf("memory limit %q: not <n>k, <n>m or <n>g", s.Memory)
	}
	return nil
}

// Args are podman run's arguments for the service, detached, labelled with spec.
func (s Service) args(spec string) ([]string, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	a := []string{"run", "--detach",
		"--name", ServiceName(s.Work, s.Name), "--label", ServiceLabel + "=" + s.Name, "--label", WorkLabel + "=" + s.Work,
		"--label", SpecLabel + "=" + spec,
		"--read-only", // podman adds a private /tmp, /var/tmp and /run (--read-only-tmpfs)
		"--cap-drop=all", "--security-opt=no-new-privileges",
		"--userns=keep-id",
		"--network=" + s.Network,
		"--memory=" + s.Memory, "--memory-swap=" + s.Memory,
		"--mount=type=bind,source=" + s.Bin + ",destination=" + ServiceBin + ",ro",
	}
	for _, m := range s.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		a = append(a, "--mount=type=bind,source="+m.Source+",destination="+m.Dest+","+mode)
	}
	for _, e := range s.Env {
		a = append(a, "--env="+e)
	}
	a = append(a, "--rootfs", s.Root, ServiceBin)
	return append(a, s.Args...), nil
}

// Spec is the digest of the description and of the binary's content: a container started from another one is
// replaced (StartService).
func (s Service) Spec(binSum string) (string, error) {
	a, err := s.args("")
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(binSum + "\n" + strings.Join(a, "\n")))
	return hex.EncodeToString(h[:16]), nil
}

// ServiceState is what podman says of a service's container.
type ServiceState struct {
	Exists  bool
	Running bool
	Spec    string    // SpecLabel's value
	Started time.Time // when it last started
	Error   string    // podman's own error, when it couldn't start it
}

// StateOf is the state of service name's container of work; no container is no error (Exists false).
func StateOf(work, name string) (ServiceState, error) {
	cname := ServiceName(work, name)
	if err := exec.Command("podman", "container", "exists", cname).Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return ServiceState{}, nil
		}
		return ServiceState{}, fmt.Errorf("podman container exists: %v", err)
	}
	out, err := podmanOut("inspect", "--type=container", `--format={{json .State}} {{index .Config.Labels "`+SpecLabel+`"}}`, cname)
	if err != nil {
		return ServiceState{}, err
	}
	js, spec, _ := strings.Cut(strings.TrimSpace(out), " ")
	var st struct {
		Running   bool
		StartedAt time.Time
		Error     string
	}
	if err := json.Unmarshal([]byte(js), &st); err != nil {
		return ServiceState{}, fmt.Errorf("podman inspect: %v", err)
	}
	return ServiceState{Exists: true, Running: st.Running, Spec: spec, Started: st.StartedAt, Error: strings.TrimSpace(st.Error)}, nil
}

// StartService starts the service's container unless one from the same spec runs already, and says which it did:
// "running" (left as it was), "started" (there was none, or one that had stopped) or "replaced" (one from another
// spec, which is removed first). podman's message comes with a failed start.
func StartService(s Service, spec string) (string, error) {
	a, err := s.args(spec)
	if err != nil {
		return "", err
	}
	st, err := StateOf(s.Work, s.Name)
	if err != nil {
		return "", err
	}
	did := "started"
	switch {
	case st.Running && st.Spec == spec:
		return "running", nil
	case st.Exists && st.Running:
		did = "replaced"
		fallthrough
	case st.Exists:
		if err := StopService(s.Work, s.Name, 5*time.Second); err != nil {
			return "", err
		}
	}
	if _, err := podmanOut(a...); err != nil {
		return "", err
	}
	return did, nil
}

// StopService ends the service's container (SIGTERM, SIGKILL grace later, whole seconds) and removes it; none is no
// error.
func StopService(work, name string, grace time.Duration) error {
	return podman("rm", "--force", "--ignore", "--time="+strconv.Itoa(int((grace+time.Second-1)/time.Second)), ServiceName(work, name))
}

// ServiceLogs is the end of what the service's program wrote: its last n lines, stdout and stderr together.
func ServiceLogs(work, name string, n int) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("podman", "logs", "--tail", strconv.Itoa(n), ServiceName(work, name))
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podman logs: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}
