package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"plinth/internal/events"
	"plinth/internal/names"
)

// eventsFlags parses -n (namespaceArg) and --work and at most maxPos positional arguments.
func eventsFlags(c *command, args []string, stdout, stderr io.Writer, maxPos int) (string, names.Namespace, []string, int, bool) {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	n := flags.String("n", "", "the namespace <owner>/<repo> (default: the origin of the checkout you are in)")
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return "", names.Namespace{}, nil, code, false
	}
	if len(pos) > maxPos {
		return "", names.Namespace{}, nil, usageError(c, stderr, "too many arguments"), false
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return "", names.Namespace{}, nil, exitFail, false
	}
	ns, code, ok := namespaceArg(c, *n, w, stderr)
	if !ok {
		return "", ns, nil, code, false
	}
	return w, ns, pos, exitOK, true
}

func runEventsVerify(c *command, args []string, stdout, stderr io.Writer) int {
	w, ns, _, code, ok := eventsFlags(c, args, stdout, stderr, 0)
	if !ok {
		return code
	}
	n, err := events.Verify(w, filepath.Join(ns.Owner, ns.Repo))
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl events verify: %s: %v\n", ns, err)
		return exitFail
	}
	fmt.Fprintf(stdout, "ok: %s: %d event(s), the chain is intact\n", ns, n)
	return exitOK
}

func runEventsShow(c *command, args []string, stdout, stderr io.Writer) int {
	w, ns, pos, code, ok := eventsFlags(c, args, stdout, stderr, 1)
	if !ok {
		return code
	}
	run := ""
	if len(pos) == 1 {
		if run = pos[0]; !names.ValidRun(run) {
			return usageError(c, stderr, fmt.Sprintf("%q is not a run name", run))
		}
	}
	err := events.Walk(w, filepath.Join(ns.Owner, ns.Repo), func(e events.Event, _ []byte) error {
		if run != "" && e.Target != run && !strings.HasSuffix(e.Target, "/"+run) {
			return nil
		}
		fmt.Fprintf(stdout, "%d %s %s %s %s %s %s: %s\n", e.Seq, e.Time, e.Caller, e.Action, e.Target, e.Outcome, e.Rule, e.Reason)
		return nil
	})
	var p *events.Problem
	if errors.As(err, &p) {
		fmt.Fprintf(stderr, "plinthctl events show: %s: the chain is broken, nothing after it is shown: %v\n", ns, err)
		return exitFail
	}
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl events show: %s: %v\n", ns, err)
		return exitFail
	}
	return exitOK
}
