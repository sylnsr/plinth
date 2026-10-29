package cli

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"plinth/internal/config"
	"plinth/internal/names"
	"plinth/internal/workdir"
)

// runConfigCheck loads the global config and every namespace's (or the one -n names), prints each file's problems,
// and for a namespace the effective value of every namespace key and where it comes from. It never prints a value
// that may be secret: no token, no key.
func runConfigCheck(c *command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	n := flags.String("n", "", "only this namespace <owner>/<repo>")
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl config check: %v\n", err)
		return exitFail
	}
	var namespaces []names.Namespace
	if *n != "" {
		ns, err := names.ParseNamespace(*n)
		if err != nil {
			return usageError(c, stderr, err.Error())
		}
		namespaces = []names.Namespace{ns}
	} else if namespaces, err = workdir.Namespaces(w); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "plinthctl config check: %v\n", err)
		return exitFail
	}

	g, err := config.LoadGlobal(w, os.LookupEnv)
	if err != nil {
		fmt.Fprintln(stdout, err)
		if len(namespaces) > 0 {
			fmt.Fprintf(stdout, "%d namespace config(s) not checked: they need a global config that loads\n", len(namespaces))
		}
		return exitFail
	}
	fmt.Fprintf(stdout, "ok: %s\n", filepath.Join(w, config.FileName))
	for _, warn := range g.Warnings {
		fmt.Fprintf(stdout, "  warning: %s\n", warn)
	}
	result := exitOK
	for _, ns := range namespaces {
		file := filepath.Join(w, ns.Owner, ns.Repo, config.FileName)
		eff, warns, err := config.LoadNamespace(w, ns, g, os.LookupEnv)
		if err != nil {
			fmt.Fprintln(stdout, err)
			result = exitFail
			continue
		}
		fmt.Fprintf(stdout, "ok: %s (%s)\n", file, ns)
		for _, warn := range warns {
			fmt.Fprintf(stdout, "  warning: %s\n", warn)
		}
		for _, kv := range []struct {
			key string
			val string
		}{
			{"reviewers.automatic", list(eff.Automatic)},
			{"plinth.messages.expiration-minutes", fmt.Sprint(eff.ExpirationMinutes)},
			{"plinth.messages.ungated.agents", list(eff.UngatedAgents)},
			{"plinth.messages.ungated.messages", list(eff.UngatedMessages)},
			{"plinth.agents.enabled", enabledList(eff.Enabled)},
			{"plinth.exec.image", pinText(eff.Image)},
			{"plinth.exec.env-file", orNone(eff.EnvFile, "none")},
			{"plinth.exec.network", fmt.Sprint(eff.Network)},
			{"plinth.exec.privileged", fmt.Sprint(eff.Privileged)},
			{"plinth.exec.memory", eff.Memory},
		} {
			fmt.Fprintf(stdout, "  %s: %s (%s)\n", kv.key, kv.val, eff.From[kv.key])
		}
	}
	return result
}

func list(s []string) string { return "[" + strings.Join(s, ", ") + "]" }

// orNone is s, or what an empty s means.
func orNone(s, empty string) string {
	if s == "" {
		return "\"\" (" + empty + ")"
	}
	return s
}

// enabledList shows plinth.agents.enabled as agent=repo pairs, sorted.
func enabledList(m map[string]string) string {
	var s []string
	for _, a := range slices.Sorted(maps.Keys(m)) {
		s = append(s, a+"="+m[a])
	}
	return list(s)
}

// pinText shows plinth.exec.image: the ref, with the commit it was built from.
func pinText(p config.ImagePin) string {
	if p == (config.ImagePin{}) {
		return "none (plinthctl image publish sets it)"
	}
	return p.Ref + " (commit " + p.Commit + ", tree " + p.Tree + ")"
}
