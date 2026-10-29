// Package auth makes callers. Whether a caller is the user or an agent follows from how it authenticated, never from a
// name: each constructor here is one channel, and a channel can only make its own kind (Scan 3). Nothing outside
// this package can make a Caller of either kind; the zero Caller is neither, and decide refuses it.
//
//   - HostUser: the CLI and the TUI, which run as the user on the host, and the controller carrying out request files from
//     <work>, which only the user can write (mode 0700; no agent reaches it; an executed script doesn't through the
//     file system, but a process it starts outside its namespaces does, until normal mode). Works while Forgejo is
//     down.
//   - Submission: the controller taking in submissions/<agent>/, which only the MCP server writes, after the agent's token
//     matched <agent>.sha256.
//   - The MCP server's token check and the web UI's Forgejo login come with those slices, as their own constructors.
//
// Spec: plinthctl.README.md, "Decision function".
package auth

// kind is who a caller is; the zero kind is neither.
type kind int

const (
	user kind = iota + 1
	agent
)

// Caller is the caller of a request: the user, or an agent by name.
type Caller struct {
	kind  kind
	agent string
}

// HostUser is the user at the host: the CLI, the TUI, and request files in <work>.
func HostUser() Caller { return Caller{kind: user} }

// Submission is the agent whose submissions/<agent>/ directory the MCP server wrote after its token check.
func Submission(name string) Caller { return Caller{kind: agent, agent: name} }

// IsUser reports whether c is the user.
func (c Caller) IsUser() bool { return c.kind == user }

// IsAgent reports whether c is an agent; Agent is its name.
func (c Caller) IsAgent() bool { return c.kind == agent }

// Agent is the agent's name, "" for the user or the zero Caller.
func (c Caller) Agent() string { return c.agent }

// String is the caller in the event log: "user", or the agent's name ("runner" is a namespace's runner's own steps,
// "worker" in older lines; the names package reserves them, so no agent shares one). The zero
// Caller is "?".
func (c Caller) String() string {
	switch c.kind {
	case user:
		return "user"
	case agent:
		return c.agent
	}
	return "?"
}
