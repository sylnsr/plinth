package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"plinth/internal/names"
	"plinth/internal/tui"
)

func runTUI(c *command, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(c.name, flag.ContinueOnError)
	n := flags.String("n", "", "start with the runs of this namespace <owner>/<repo>")
	work := flags.String("work", defaultWork, "Plinth's work directory")
	pos, code, ok := parse(c, flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	if *n != "" {
		if _, err := names.ParseNamespace(*n); err != nil {
			return usageError(c, stderr, err.Error())
		}
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl tui: %v\n", err)
		return exitFail
	}
	// The screen is the terminal itself, so the TUI takes os.Stdin and os.Stdout, not the writers.
	err = tui.Run(tui.Options{Work: w, Namespace: *n, Version: Version, Getenv: os.LookupEnv, In: os.Stdin, Out: os.Stdout,
		Color: os.Getenv("NO_COLOR") == ""})
	switch {
	case errors.Is(err, tui.ErrNoTerminal):
		fmt.Fprintln(stderr, "plinthctl tui: needs a terminal")
		return exitFail
	case err != nil:
		fmt.Fprintf(stderr, "plinthctl tui: %v\n", err)
		return exitFail
	}
	return exitOK
}
