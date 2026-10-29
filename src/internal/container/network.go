package container

import "fmt"

// NetworkBox says what a network run can reach, for the user who accepts it. The CLI and the TUI show it before
// the user types network. A run gets no ssh: nothing of ~/.ssh, no key, no agent; remote work over
// ssh belongs to Ansible, run by the user, or to a privileged run.
func NetworkBox(run, agent string) string {
	return fmt.Sprintf("%s (agent %s) is a network run (NETWORK=1). Once you accept it, it runs in its container with:\n"+
		"  - a network of its own: the internet and other machines, not this machine's local services;\n"+
		"  - no ssh: nothing of your ~/.ssh, no key and no agent of yours;\n"+
		"  - like every run, the variables of plinth.exec.env-file: a token there can be used, and sent, anywhere.\n"+
		"Read its steps for what it sends where first. The event log records it.\n", run, agent)
}
