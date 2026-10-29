package container

import (
	"strings"
	"testing"
)

func testService() Service {
	return Service{Work: "/home/u/.plinth", Name: "ollama-gateway", Root: "/home/u/.plinth/state/ollama-root", Bin: "/usr/local/bin/plinthctl",
		Args: []string{"__ollama-gateway", "-host", "http://ollama:11434"}, Mounts: []Mount{{Source: "/home/u/.plinth/state/ollama", Dest: "/sock"},
			{Source: "/etc/ssl/certs/ca-certificates.crt", Dest: "/etc/plinth-ca.pem", ReadOnly: true}},
		Env: []string{"SSL_CERT_FILE=/etc/plinth-ca.pem"}, Network: "host", Memory: "256m"}
}

// TestServiceArgs: podman run's arguments for a service: detached, labelled, read-only, no capabilities, the user's
// uid, the binary and the mounts, then the empty root and the binary's arguments. A bad description is refused.
func TestServiceArgs(t *testing.T) {
	s := testService()
	a, err := s.args("abc")
	if err != nil {
		t.Fatal(err)
	}
	want := "run --detach --name " + ServiceName(s.Work, s.Name) + " --label plinth.service=ollama-gateway --label " + WorkLabel + "=/home/u/.plinth" +
		" --label plinth.spec=abc --read-only --cap-drop=all --security-opt=no-new-privileges --userns=keep-id --network=host" +
		" --memory=256m --memory-swap=256m --mount=type=bind,source=/usr/local/bin/plinthctl,destination=/plinthctl,ro" +
		" --mount=type=bind,source=/home/u/.plinth/state/ollama,destination=/sock,rw" +
		" --mount=type=bind,source=/etc/ssl/certs/ca-certificates.crt,destination=/etc/plinth-ca.pem,ro" +
		" --env=SSL_CERT_FILE=/etc/plinth-ca.pem --rootfs /home/u/.plinth/state/ollama-root /plinthctl __ollama-gateway -host http://ollama:11434"
	if got := strings.Join(a, " "); got != want {
		t.Errorf("args:\n%s\nwant\n%s", got, want)
	}
	if ServiceName(s.Work, s.Name) == ContainerName(s.Work, s.Name) || !strings.HasPrefix(ServiceName(s.Work, s.Name), "plinth-") {
		t.Errorf("name %s", ServiceName(s.Work, s.Name))
	}

	spec, err := s.Spec("sum")
	if err != nil || len(spec) != 32 {
		t.Fatalf("Spec: %q %v", spec, err)
	}
	other := testService()
	other.Args = append(other.Args, "-max-parallel", "2")
	for what, got := range map[string]func() (string, error){
		"the same":       func() (string, error) { return testService().Spec("sum") },
		"another binary": func() (string, error) { return s.Spec("other") },
		"another config": func() (string, error) { return other.Spec("sum") },
	} {
		g, err := got()
		if err != nil || (g == spec) != (what == "the same") {
			t.Errorf("%s: %q %v (first %q)", what, g, err, spec)
		}
	}

	for _, c := range []struct {
		change func(*Service)
		want   string
	}{
		{func(s *Service) { s.Name = "a/b" }, "service name"},
		{func(s *Service) { s.Work = "rel" }, "not an absolute, clean path"},
		{func(s *Service) { s.Root = "/x/../y" }, "root"},
		{func(s *Service) { s.Bin = "/a,b" }, "binary"},
		{func(s *Service) { s.Mounts[0].Dest = "sock" }, "mount 0 destination"},
		{func(s *Service) { s.Network = "bridge" }, "network"},
		{func(s *Service) { s.Memory = "1t" }, "memory limit"},
	} {
		bad := testService()
		c.change(&bad)
		if _, err := bad.args(""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", bad, err, c.want)
		}
	}
}
