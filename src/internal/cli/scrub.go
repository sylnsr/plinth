package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"plinth/internal/config"
	"plinth/internal/plain"
)

func runScrubber(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := fs.String("repo", ".", "the repo directory")
	work := fs.String("work", defaultWork, "Plinth's work directory, for plinth.aaa-raw-dir")
	all := fs.Bool("all", false, "runs with an issue number too")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	w, err := expandWork(*work)
	if err != nil {
		return fail(err)
	}
	raw, err := config.RawDir(w, os.LookupEnv)
	if err != nil {
		return fail(err)
	}
	dir, err := filepath.Abs(plain.WorkDir(*repo))
	if err != nil {
		return fail(err)
	}
	entries, err := plain.Scrub(dir, raw, *all, time.Now())
	if err != nil {
		return fail(err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.What]++
		if e.Why != "" {
			fmt.Fprintf(stdout, "%-6s %s (%s)\n", e.What, e.Name, e.Why)
		} else {
			fmt.Fprintf(stdout, "%-6s %s\n", e.What, e.Name)
		}
	}
	fmt.Fprintf(stdout, "Wrote %s: %d to scrub, %d kept, %d skipped. Read it, then run it.\n",
		filepath.Join(dir, plain.ScrubScript), counts["scrub"], counts["keep"], counts["skip"])
	return exitOK
}

func runHandoffScrubber(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := fs.String("repo", ".", "the repo directory")
	branch := fs.String("branch", "", "the handoff branch, handoff/...")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	if *branch == "" {
		return usageError(c, stderr, "--branch is required")
	}
	r, err := filepath.Abs(*repo)
	if err == nil {
		err = plain.HandoffScrub(plain.WorkDir(r), r, *branch, time.Now())
	}
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	fmt.Fprintf(stdout, "Wrote %s: it deletes %s here and on origin. Read it, then run it.\n",
		filepath.Join(plain.WorkDir(r), plain.HandoffScript), *branch)
	return exitOK
}
