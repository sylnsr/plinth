package agentmcp

import (
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"

	"plinth/internal/config"
	"plinth/internal/names"
)

// guideText is the guide's fixed part: what Plinth is, the flow, the tools, how to write an ask script, the states.
//
//go:embed guide.md
var guideText string

// Guide is what the guide tool gives an agent of namespace ns: the fixed part, then the namespace's own: the run modes
// it allows, its automatic reviews, and every review type's policy URLs from the global config, for the agent to read
// with its own tools (a type defined in the config itself is named, not quoted). plinthctl agent guide
// prints the same.
func Guide(g *config.Global, ns names.Namespace, cfg *config.Namespace) string {
	var b strings.Builder
	b.WriteString(guideText)
	fmt.Fprintf(&b, "\n## This namespace: %s\n\n", ns)
	allowed := func(ok bool) string {
		if ok {
			return "allowed"
		}
		return "not allowed here"
	}
	fmt.Fprintf(&b, "- Run modes of Plinth runs: normal (in a container of the namespace's image, without network, with an empty home "+
		"directory), always; network (`NETWORK=1`: the network, never ssh; the user accepts each run): %s; privileged "+
		"(`PRIVILEGED=1`: runs as the user in their own terminal, outside any container): %s.\n",
		allowed(cfg.Network), allowed(cfg.Privileged))
	fmt.Fprintf(&b, "- A run is stopped after %d minutes.\n", g.Plinth.Runner.ExecTimeoutMinutes)
	if len(cfg.Automatic) == 0 {
		b.WriteString("- Automatic reviews: none; the user reads every script themselves.\n")
	} else {
		fmt.Fprintf(&b, "- Automatic reviews, which every Plinth run's script gets before the user decides: %s. A failed one holds the run "+
			"back until the user overrides it.\n", "`"+strings.Join(cfg.Automatic, "`, `")+"`")
	}
	types := slices.Sorted(maps.Keys(g.Reviewers.Type))
	if len(types) == 0 {
		return b.String()
	}
	b.WriteString("\n### Review policies\n\nWhat the reviews check a script against. Read the ones of the automatic reviews " +
		"before you write a script; a script that follows them passes.\n\n")
	for _, t := range types {
		auto := ""
		if slices.Contains(cfg.Automatic, t) {
			auto = " (automatic)"
		}
		rt := g.Reviewers.Type[t]
		switch {
		case len(rt.URLs) > 0:
			fmt.Fprintf(&b, "- `%s`%s:\n", t, auto)
			for _, u := range rt.URLs {
				fmt.Fprintf(&b, "  - %s\n", u)
			}
		default:
			fmt.Fprintf(&b, "- `%s`%s: defined in Plinth's config, not published; ask the user if you need it.\n", t, auto)
		}
	}
	return b.String()
}
