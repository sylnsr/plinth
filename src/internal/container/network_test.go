package container

import (
	"strings"
	"testing"
)

func TestNetworkBox(t *testing.T) {
	s := NetworkBox("keen-tapir", "main")
	for _, want := range []string{"keen-tapir (agent main) is a network run (NETWORK=1)", "a network of its own", "no ssh: nothing of your ~/.ssh",
		"plinth.exec.env-file"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in the box:\n%s", want, s)
		}
	}
}
