// Package names holds the name patterns every part of Plinth validates against, and the run-namer dictionaries.
package names

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	runRe   = regexp.MustCompile(`^([a-z]+-[a-z]+)(-[0-9]+)?$`)
	agentRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	partRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	issueRe = regexp.MustCompile(`^[0-9]{1,9}$`)
)

// ValidRun reports whether s is a RUN_NAME: <adjective>-<noun>[-<issue>].
func ValidRun(s string) bool { return runRe.MatchString(s) }

// Stem returns the <adjective>-<noun> part of a RUN_NAME, or "" if s is not one.
func Stem(s string) string {
	m := runRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// Issue is the issue number a RUN_NAME ends in, "" without one (or a number of more than 9 digits).
func Issue(s string) string {
	m := runRe.FindStringSubmatch(s)
	if m == nil || !ValidIssue(strings.TrimPrefix(m[2], "-")) {
		return ""
	}
	return m[2][1:]
}

// Reserved are names that match the agent pattern but are never agent names: user and runner are the event log's
// callers next to the agents (worker in older lines), controller names the process, and messages is
// requests/messages/ next to requests/<agent>/ (Scan 3).
var Reserved = []string{"user", "worker", "runner", "controller", "messages"}

// ValidAgent reports whether s is an agent name: the pattern, and not a Reserved name.
func ValidAgent(s string) bool { return agentRe.MatchString(s) && !slices.Contains(Reserved, s) }

// ValidIssue reports whether s is an issue number (digits only).
func ValidIssue(s string) bool { return issueRe.MatchString(s) }

// Namespace is <owner>/<repo>.
type Namespace struct{ Owner, Repo string }

func (n Namespace) String() string { return n.Owner + "/" + n.Repo }

// ParseNamespace parses "<owner>/<repo>". Each part starts with a letter or digit, so "." and ".." never pass.
func ParseNamespace(s string) (Namespace, error) {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || !partRe.MatchString(owner) || !partRe.MatchString(repo) {
		return Namespace{}, fmt.Errorf("namespace %q is not <owner>/<repo>", s)
	}
	return Namespace{owner, repo}, nil
}

// Adjectives and Nouns are run-namer's dictionaries: 40 x 200 = 8000 pairs.
var Adjectives = strings.Fields(`amber bold brave brisk calm clever eager gentle happy keen
	lucky mellow merry nimble plucky quick quiet sunny swift witty
	agile ample bright cheery cosmic crisp dapper deft fair fleet
	frosty golden hardy jolly lively noble proud rapid steady vivid`)

var Nouns = strings.Fields(`acorn anchor arrow aspen badger beacon birch bison bridge brook
	cabin canyon cedar cliff clover comet coral crane creek daisy
	delta dune eagle ember falcon fern ferret finch fjord flint
	forest fox gecko glacier grove harbor hawk heron hill ibex
	island jasper kestrel lagoon lantern lark lemur lily lynx maple
	marsh meadow moon moss newt oak orchid osprey otter panda
	pebble pelican pine pond puffin quail quartz quokka raven reef
	ridge river robin sage seal shore sparrow spruce star stoat stone
	storm summit swan thistle tide trail tulip valley walrus willow wolf
	anvil bluff cove dawn kite mesa mist petal
	alder apricot atlas aurora basil bay beaver bramble breeze butte
	cactus canoe caribou cascade cherry chestnut cinder citrus cloud condor
	cougar cricket crocus cypress dahlia dingo dolphin dove elm ermine
	fawn fig firefly galaxy garnet geyser ginger glade granite gull
	hazel heather hedge holly horizon iris ivy jay juniper kelp
	koala larch laurel lichen lotus magpie mango marmot meteor mink
	nebula nectar nutmeg oasis olive onyx opal orca owl palm
	peak pepper plover poppy prairie quill rain rapids reed rowan
	ruby sable saffron sequoia shrew sierra sloth snow sorrel sprout
	squall tapir thrush timber topaz tundra vole wren yak zephyr`)

// Draw returns a free stem (<adjective>-<noun>) not in taken, using pick(n) for a random index in [0, n).
// It fails when every pair is taken.
func Draw(taken map[string]bool, pick func(n int) int) (string, error) {
	free := make([]string, 0, len(Adjectives)*len(Nouns))
	for _, a := range Adjectives {
		for _, n := range Nouns {
			if s := a + "-" + n; !taken[s] {
				free = append(free, s)
			}
		}
	}
	if len(free) == 0 {
		return "", fmt.Errorf("all %d adjective-noun pairs are in use", len(Adjectives)*len(Nouns))
	}
	return free[pick(len(free))], nil
}
