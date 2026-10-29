package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"plinth/internal/agentmcp"
	"plinth/internal/agents"
	"plinth/internal/config"
	"plinth/internal/logfile"
	"plinth/internal/names"
)

// agentArgs is -n, --work and one <agent>, with the global config loaded: enable and disable edit the namespace config
// and check that it still loads.
func agentArgs(c *command, args []string, stdout, stderr io.Writer) (string, names.Namespace, string, *config.Global, int, bool) {
	w, ns, pos, code, ok := eventsFlags(c, args, stdout, stderr, 1)
	if !ok {
		return "", ns, "", nil, code, false
	}
	if len(pos) != 1 {
		return "", ns, "", nil, usageError(c, stderr, "one <agent> is required"), false
	}
	if !names.ValidAgent(pos[0]) {
		return "", ns, "", nil, usageError(c, stderr, fmt.Sprintf("%q is not an agent name", pos[0])), false
	}
	g, err := config.LoadGlobal(w, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return "", ns, "", nil, exitFail, false
	}
	return w, ns, pos[0], g, exitOK, true
}

// runAgentEnable is the second step of registering: it shows where the agent's registration comes from, asks,
// promotes a pending registration, and records the agent with that repo in plinth.agents.enabled.
func runAgentEnable(c *command, args []string, stdout, stderr io.Writer) int {
	w, ns, agent, g, code, ok := agentArgs(c, args, stdout, stderr)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl agent enable: %v\n", err)
		return exitFail
	}
	reg, err := agents.Get(w, ns, agent)
	if err != nil {
		return fail(err)
	}
	repo := reg.Repo
	switch {
	case reg.Pending != "":
		repo = reg.Pending
		fmt.Fprintf(stdout, "Agent %s in %s has a pending registration from\n  %s\n", agent, ns, reg.Pending)
		fmt.Fprintf(stdout, "beside its current one from\n  %s\nEnabling it makes the pending one current: the current one's token stops working.\n", reg.Repo)
	case reg.Repo != "":
		fmt.Fprintf(stdout, "Agent %s in %s is registered from\n  %s\n", agent, ns, reg.Repo)
	default:
		return fail(fmt.Errorf("agent %s is not registered in %s", agent, ns))
	}
	fmt.Fprintln(stdout, "Check that this is the repo you launched it from: once enabled, the agent submits scripts in this namespace.")
	if !confirm(stdout, "Enable "+agent+" in "+ns.String()) {
		fmt.Fprintln(stdout, "Nothing changed.")
		return exitFail
	}
	if reg.Pending != "" {
		if err := agents.Promote(w, ns, agent); err != nil {
			return fail(err)
		}
	}
	if err := config.EnableAgent(w, ns, g, os.LookupEnv, agent, repo); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Enabled %s in %s for %s.\n", agent, ns, repo)
	logfile.New(w, logfile.CLI).With("command", c.name).Info("enabled agent", "agent", agent, "ns", ns, "repo", repo)
	return exitOK
}

func runAgentDisable(c *command, args []string, stdout, stderr io.Writer) int {
	w, ns, agent, g, code, ok := agentArgs(c, args, stdout, stderr)
	if !ok {
		return code
	}
	changed, err := config.DisableAgent(w, ns, g, os.LookupEnv, agent)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl agent disable: %v\n", err)
		return exitFail
	}
	if !changed {
		fmt.Fprintf(stdout, "Agent %s is not enabled in %s.\n", agent, ns)
		return exitOK
	}
	fmt.Fprintf(stdout, "Disabled %s in %s: it stays registered, but can't act until you enable it again.\n", agent, ns)
	logfile.New(w, logfile.CLI).With("command", c.name).Info("disabled agent", "agent", agent, "ns", ns)
	return exitOK
}

func runAgentList(c *command, args []string, stdout, stderr io.Writer) int {
	w, ns, _, code, ok := eventsFlags(c, args, stdout, stderr, 0)
	if !ok {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl agent list: %v\n", err)
		return exitFail
	}
	g, err := config.LoadGlobal(w, os.LookupEnv)
	if err != nil {
		return fail(err)
	}
	cfg, _, err := config.LoadNamespace(w, ns, g, os.LookupEnv)
	if err != nil {
		return fail(err)
	}
	registered, err := agents.List(w, ns)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "%-12s %-9s %s\n", "AGENT", "STATE", "REPO")
	for _, a := range registered {
		reg, err := agents.Get(w, ns, a)
		if err != nil {
			return fail(err)
		}
		state := "pending"
		if cfg.Enabled[a] == reg.Repo {
			state = "enabled"
		}
		fmt.Fprintf(stdout, "%-12s %-9s %s\n", a, state, reg.Repo)
		if reg.Pending != "" {
			fmt.Fprintf(stdout, "%-12s %-9s %s\n", "", "pending", reg.Pending)
		}
	}
	return exitOK
}

// runAgentGuide prints the Plinth MCP server's guide for the namespace, as an agent gets it.
func runAgentGuide(c *command, args []string, stdout, stderr io.Writer) int {
	w, ns, _, code, ok := eventsFlags(c, args, stdout, stderr, 0)
	if !ok {
		return code
	}
	if !config.Exists(filepath.Join(w, ns.Owner, ns.Repo)) {
		fmt.Fprintf(stderr, "plinthctl agent guide: %s is not set up (plinthctl register sets it up)\n", ns)
		return exitFail
	}
	g, err := config.LoadGlobal(w, os.LookupEnv)
	if err == nil {
		var cfg *config.Namespace
		if cfg, _, err = config.LoadNamespace(w, ns, g, os.LookupEnv); err == nil {
			io.WriteString(stdout, agentmcp.Guide(g, ns, cfg))
			return exitOK
		}
	}
	fmt.Fprintf(stderr, "plinthctl agent guide: %v\n", err)
	return exitFail
}
