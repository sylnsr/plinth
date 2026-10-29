package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"plinth/internal/agents"
	"plinth/internal/config"
	"plinth/internal/controller"
	"plinth/internal/gitrepo"
	"plinth/internal/logfile"
	"plinth/internal/names"
)

// defaultWork is <work> when --work is not given.
const defaultWork = "~/.plinth"

// expandWork turns --work into an absolute path, expanding a leading ~.
func expandWork(w string) (string, error) {
	if w == "~" || strings.HasPrefix(w, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		w = filepath.Join(home, strings.TrimPrefix(w, "~"))
	}
	return filepath.Abs(w)
}

// controllerRunning reports whether a process holds the controller's lock in work. Missing or unreadable lock: not running.
func controllerRunning(work string) bool {
	running, _ := controller.Running(work)
	return running
}

// namespaceOf is -n when given, else the namespace the origin remote of the repo at top names.
func namespaceOf(n, top string) (names.Namespace, error) {
	if n != "" {
		return names.ParseNamespace(n)
	}
	ns, err := gitrepo.OriginNamespace(top)
	if err != nil {
		return names.Namespace{}, fmt.Errorf("%v; give -n <owner>/<repo>", err)
	}
	return ns, nil
}

func runRegister(c *command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := flags.String("repo", "", "a directory in the repo")
	agent := flags.String("agent", "", "the agent's name")
	n := flags.String("n", "", "the namespace <owner>/<repo> (default from the origin remote)")
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return code
	}
	switch {
	case len(pos) > 0:
		return usageError(c, stderr, "too many arguments")
	case *repo == "":
		return usageError(c, stderr, "--repo is required")
	case *agent == "":
		return usageError(c, stderr, "--agent is required")
	case !names.ValidAgent(*agent):
		return usageError(c, stderr, fmt.Sprintf("agent %q: not a valid agent name ([a-z][a-z0-9-]{0,31}, not %s)", *agent, strings.Join(names.Reserved, ", ")))
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl register: %v\n", err)
		return exitFail
	}
	top, err := gitrepo.TopLevel(*repo)
	if err != nil {
		return fail(err)
	}
	ns, err := namespaceOf(*n, top)
	if err != nil {
		return fail(err)
	}
	w, err := expandWork(*work)
	if err != nil {
		return fail(err)
	}
	configNote, err := checkOrigin(*n, top, w)
	if err != nil {
		return fail(err)
	}
	// The terminal gets only what the user has to act on (a new global config to fill in, a controller that isn't running,
	// errors); everything else goes to the log. The token goes to stdout and nowhere else.
	token, created, err := agents.Register(w, ns, *agent, top)
	log := logfile.New(w, logfile.CLI).With("command", "register")
	for _, p := range created {
		log.Info("created", "path", filepath.Join(w, p))
		if p == "config.yml" {
			fmt.Fprintf(stderr, "fill in %s before starting the controller\n", filepath.Join(w, p))
		}
	}
	if err != nil {
		log.Error("register failed", "agent", *agent, "ns", ns, "repo", top, "err", err)
		return fail(err)
	}
	// Registering is the first of two steps: the agent acts only once the user has enabled it for this repo.
	enabled := false
	if reg, err := agents.Get(w, ns, *agent); err == nil && reg.Pending == top {
		log.Info("registered agent, pending beside its registration, which keeps working", "agent", *agent, "ns", ns, "repo", top, "kept_repo", reg.Repo)
		fmt.Fprintf(stderr, "plinthctl register: agent %s in %s is registered from %s; this registration from %s waits beside it until you run: plinthctl agent enable -n %s %s\n",
			*agent, ns, reg.Repo, top, ns, *agent)
	} else {
		log.Info("registered agent; its previous token, if any, no longer works", "agent", *agent, "ns", ns, "repo", top)
		if enabled = enabledFor(w, ns, *agent, top); !enabled {
			fmt.Fprintf(stderr, "plinthctl register: agent %s is registered but not enabled in %s; it can act once you run: plinthctl agent enable -n %s %s\n",
				*agent, ns, ns, *agent)
		}
	}
	if configNote != "" {
		log.Warn("config note", "note", configNote)
		fmt.Fprintf(stderr, "plinthctl register: %s\n", configNote)
	}
	running := controllerRunning(w)
	switch {
	case running && enabled:
		log.Info("the controller is running and serves the new token")
	case running:
		log.Info("the controller is running and serves the new token once the agent is enabled (plinthctl agent enable)", "repo", top)
	default:
		log.Warn("the controller is not running")
		fmt.Fprintln(stderr, "plinthctl register: the controller is not running: the token works once it runs (plinthctl controller start)")
	}
	if err := log.Err(); err != nil {
		fmt.Fprintf(stderr, "plinthctl register: %v (registered all the same)\n", err)
	}
	fmt.Fprintln(stdout, token)
	return exitOK
}

// runAgentTokenRotate prints a new token for an agent that is registered already: register's token part, without its
// setup. Like register, stdout carries the token and nothing else.
func runAgentTokenRotate(c *command, args []string, stdout, stderr io.Writer) int {
	w, ns, pos, code, ok := eventsFlags(c, args, stdout, stderr, 1)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageError(c, stderr, "one <agent> is required")
	}
	agent := pos[0]
	if !names.ValidAgent(agent) {
		return usageError(c, stderr, fmt.Sprintf("%q is not an agent name", agent))
	}
	token, err := agents.Rotate(w, ns, agent)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	log := logfile.New(w, logfile.CLI).With("command", c.name)
	log.Info("rotated the token; the previous token no longer works", "agent", agent, "ns", ns)
	if reg, err := agents.Get(w, ns, agent); err == nil && !enabledFor(w, ns, agent, reg.Repo) {
		fmt.Fprintf(stderr, "plinthctl %s: agent %s is not enabled in %s; it can act once you run: plinthctl agent enable -n %s %s\n",
			c.name, agent, ns, ns, agent)
	}
	if !controllerRunning(w) {
		fmt.Fprintf(stderr, "plinthctl %s: the controller is not running: the token works once it runs (plinthctl controller start)\n", c.name)
	}
	if err := log.Err(); err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v (rotated all the same)\n", c.name, err)
	}
	fmt.Fprintln(stdout, token)
	return exitOK
}

func runAgentTokenRemove(c *command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := flags.String("repo", ".", "a directory in the repo")
	n := flags.String("n", "", "the namespace <owner>/<repo> (default from the origin remote of --repo)")
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageError(c, stderr, "one <agent> is required")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	top := ""
	if *n == "" {
		var err error
		if top, err = gitrepo.TopLevel(*repo); err != nil {
			return fail(err)
		}
	}
	ns, err := namespaceOf(*n, top)
	if err != nil {
		return fail(err)
	}
	w, err := expandWork(*work)
	if err != nil {
		return fail(err)
	}
	if err := agents.Remove(w, ns, pos[0]); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("agent %s is not registered in %s", pos[0], ns)
		}
		return fail(err)
	}
	log := logfile.New(w, logfile.CLI).With("command", c.name)
	log.Info("removed agent; its token no longer works", "agent", pos[0], "ns", ns)
	if err := log.Err(); err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v (removed all the same)\n", c.name, err)
	}
	return exitOK
}

// checkOrigin checks, once the global config loads, that the origin remote of the repo at top is on forgejo-url's host
// (-n skips the check: the user names the namespace). A config that doesn't load yet (a new one still to fill in, or one
// with problems) skips the check and returns a note for the user instead. The error never quotes the remote URL, which
// may hold user:token.
func checkOrigin(n, top, work string) (string, error) {
	if n != "" || !config.Exists(work) {
		return "", nil
	}
	g, err := config.LoadGlobal(work, os.LookupEnv)
	if err != nil {
		return fmt.Sprintf("%s has problems, so origin was not checked against forgejo-url (see plinthctl config check)",
			filepath.Join(work, config.FileName)), nil
	}
	remote, err := gitrepo.Origin(top)
	if err != nil {
		return "", fmt.Errorf("%v; give -n <owner>/<repo>", err)
	}
	want := ""
	if u, err := url.Parse(g.ForgejoURL); err == nil {
		want = strings.ToLower(u.Hostname())
	}
	if got := gitrepo.RemoteHost(remote); got != want {
		if got == "" {
			got = "no host (a local path)"
		}
		return "", fmt.Errorf("origin of %s is on %s, not on forgejo-url's host %s; give -n <owner>/<repo> to register it all the same", top, got, want)
	}
	return "", nil
}

// enabledFor reports whether the namespace config enables agent for repo. A config that doesn't load counts as yes, so
// register adds no note of its own (the controller and config check name the problem).
func enabledFor(work string, ns names.Namespace, agent, repo string) bool {
	g, err := config.LoadGlobal(work, os.LookupEnv)
	if err != nil {
		return true
	}
	cfg, _, err := config.LoadNamespace(work, ns, g, os.LookupEnv)
	return err != nil || cfg.Enabled[agent] == repo
}
