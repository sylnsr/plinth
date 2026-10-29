package runner

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"plinth/internal/container"
)

// TestRunResult: how exec.end tells a run's end, from podman inspect's state and whether the controller stopped it.
func TestRunResult(t *testing.T) {
	gone := errors.New("no such container")
	client := exec.Command("sh", "-c", "exit 137").Run() // the podman client's exit, which passes on the script's
	for _, c := range []struct {
		st      container.State
		ierr    error
		stopped bool
		want    string
	}{
		{container.State{Started: true, ExitCode: 3}, nil, false, "exit 3"},
		{container.State{Started: true, ExitCode: 127}, nil, false, "exit 127"}, // the script's, not podman's
		{container.State{Started: true, ExitCode: 137, OOMKilled: true}, nil, false, "exit 137 (signal 9, killed); killed over its memory limit (plinth.exec.memory 512m)"},
		{container.State{Started: true, ExitCode: 137}, nil, false, "exit 137 (signal 9, killed); over its memory limit (plinth.exec.memory 512m), unless the script sent SIGKILL itself"},
		{container.State{Started: true, ExitCode: 137}, nil, true, "exit 137 (signal 9, killed)"},
		{container.State{Started: true, ExitCode: 143}, nil, true, "exit 143 (signal 15, terminated)"},
		{container.State{Error: "mount: no such file"}, nil, false, "the container didn't run: mount: no such file"},
		{container.State{}, gone, false, "exit 137 (signal 9, killed); podman can't say how the container ended: no such container"},
		{container.State{Started: true, ExitCode: -1}, nil, true, "exit 137 (signal 9, killed)"}, // the client's
		{container.State{Started: true, ExitCode: -1}, nil, false, "exit 137 (signal 9, killed); over its memory limit (plinth.exec.memory 512m), unless the script sent SIGKILL itself"},
	} {
		if got := runResult(client, c.st, c.ierr, "podman says", "512m", c.stopped); got != c.want {
			t.Errorf("%+v, %v, stopped %v: %q, want %q", c.st, c.ierr, c.stopped, got, c.want)
		}
	}
	// No container to inspect and a client exit of 125: podman's failure, or a script's exit 125 whose container went;
	// either way the client's stderr (which may be the script's) stays out.
	exit125 := exec.Command("sh", "-c", "exit 125").Run()
	if got := runResult(exit125, container.State{}, gone, "token=s3cret", "512m", false); strings.Contains(got, "s3cret") ||
		got != "podman exit 125, and no container left to inspect: no such container" {
		t.Errorf("exit 125: %q", got)
	}
	// podman lost the exit code, and its client says -1 (255): nothing made up.
	lost := exec.Command("sh", "-c", "exit 255").Run()
	if got := runResult(lost, container.State{Started: true, ExitCode: -1}, nil, "", "512m", true); got != "exit code lost (podman recorded none)" {
		t.Errorf("lost: %q", got)
	}
}
