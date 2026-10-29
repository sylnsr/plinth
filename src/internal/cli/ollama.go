package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"plinth/internal/config"
	"plinth/internal/controller"
	"plinth/internal/decide"
	"plinth/internal/ollama"
	"plinth/internal/ollamamcp"
	"plinth/internal/plain"
	"plinth/internal/review"
)

// plainConfig reads the plain config's ollama: keys (and keys) from --work: the config and <work>, absolute.
func plainConfig(work string, keys ...string) (*config.Plain, string, error) {
	w, err := expandWork(work)
	if err != nil {
		return nil, "", err
	}
	pl, err := config.LoadPlain(w, os.LookupEnv, append(keys, config.OllamaKey)...)
	if err != nil {
		return nil, "", err
	}
	return pl, w, nil
}

// plainOllama is plainConfig and the client of <work>'s Ollama gateway.
func plainOllama(work string, keys ...string) (*config.Plain, *ollamamcp.Client, error) {
	pl, w, err := plainConfig(work, keys...)
	if err != nil {
		return nil, nil, err
	}
	return pl, ollamamcp.NewClient(w, pl.Ollama.Host != ""), nil
}

func runDLPScan(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	work := fs.String("work", defaultWork, "Plinth's work directory, for its config.yml")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageError(c, stderr, "one answer file")
	}
	file := pos[0]
	fail := func(err error) int {
		fmt.Fprintf(stderr, "plinthctl dlp-scan: %s: FAILED (the file is untouched): %v\n", file, err)
		return exitFail
	}
	pl, cl, err := plainOllama(*work)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := plain.DLPScan(ctx, cl, pl.Ollama.Models.DLP, pl.AAARawDir, file)
	if errors.Is(err, plain.ErrScanned) {
		fmt.Fprintf(stdout, "dlp-scan: %s: already scanned, left alone\n", file)
		return exitOK
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "dlp-scan: %s: %d chunk(s), %d fixed-pattern mask(s), %d line(s) masked, model %s; the original is in %s\n",
		file, res.Chunks, res.Fixed, res.Lines, res.Model, pl.AAARawDir)
	return exitOK
}

func runOllamaStatus(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	work := fs.String("work", defaultWork, "Plinth's work directory, for its config.yml")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	pl, cl, err := plainOllama(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl ollama status: %v\n", err)
		return exitFail
	}
	if !cl.Configured() {
		fmt.Fprintln(stdout, "Ollama: not configured (ollama.host is not set)")
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	v, err := cl.Version(ctx)
	if err != nil {
		fmt.Fprintf(stdout, "Ollama %s: unreachable: %v\n", pl.Ollama.Host, err)
		return exitFail
	}
	fmt.Fprintf(stdout, "Ollama %s: reachable, version %s\n", pl.Ollama.Host, v)
	tags, err := cl.Tags(ctx)
	if err != nil {
		fmt.Fprintf(stdout, "models: can't be listed: %v\n", err)
		return exitFail
	}
	loaded, err := cl.PS(ctx)
	if err != nil {
		fmt.Fprintf(stdout, "loaded models: can't be listed: %v\n", err)
		return exitFail
	}
	has := func(ms []ollama.Model, name string) bool {
		return slices.ContainsFunc(ms, func(m ollama.Model) bool { return m.Name == name })
	}
	code = exitOK
	for _, m := range []struct{ use, name string }{{"review", pl.Ollama.Models.Review}, {"dlp", pl.Ollama.Models.DLP}} {
		switch {
		case has(loaded, m.name):
			fmt.Fprintf(stdout, "%s model %s: present, loaded\n", m.use, m.name)
		case has(tags, m.name):
			fmt.Fprintf(stdout, "%s model %s: present\n", m.use, m.name)
		default:
			fmt.Fprintf(stdout, "%s model %s: missing (ollama pull it on the Ollama host)\n", m.use, m.name)
			code = exitFail
		}
	}
	return code
}

// gatewayArgs parses ollama-mcp start|stop|status [--work W]: the plain config and <work>.
func gatewayArgs(c *command, args []string, stdout, stderr io.Writer) (*config.Plain, string, int, bool) {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	work := fs.String("work", defaultWork, "Plinth's work directory, for its config.yml")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return nil, "", code, false
	}
	if len(pos) > 0 {
		return nil, "", usageError(c, stderr, "too many arguments"), false
	}
	pl, w, err := plainConfig(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return nil, "", exitFail, false
	}
	return pl, w, exitOK, true
}

// gateway is <work>'s Ollama gateway as this plinthctl describes it.
func gateway(pl *config.Plain, work string) (ollamamcp.Gateway, error) {
	bin, err := ollamamcp.Executable()
	return ollamamcp.Gateway{Work: work, Bin: bin, Ollama: pl.Ollama, Version: Version}, err
}

// runGatewayStart is ollama-mcp start: the gateway's container, unless the same runs already.
func runGatewayStart(c *command, args []string, stdout, stderr io.Writer) int {
	pl, w, code, ok := gatewayArgs(c, args, stdout, stderr)
	if !ok {
		return code
	}
	g, err := gateway(pl, w)
	did := ""
	if err == nil {
		did, err = g.Start()
	}
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	fmt.Fprintf(stdout, "ollama-mcp: %s, for Ollama %s on %s\n", did, pl.Ollama.Host, ollamamcp.SocketPath(w))
	return exitOK
}

// runGatewayStop is ollama-mcp stop. A running controller starts it again, so it says so.
func runGatewayStop(c *command, args []string, stdout, stderr io.Writer) int {
	_, w, code, ok := gatewayArgs(c, args, stdout, stderr)
	if !ok {
		return code
	}
	if err := ollamamcp.Stop(w); err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	fmt.Fprintln(stdout, "ollama-mcp: stopped")
	if running, pid := controller.Running(w); running {
		fmt.Fprintf(stdout, "the controller (PID %d) starts it again at its next check while ollama.host is set\n", pid)
	}
	return exitOK
}

// runGatewayStatus is ollama-mcp status: whether the gateway runs, whether it is the one this plinthctl and config.yml
// describe, and its last lines. Exit 1 when it doesn't run.
func runGatewayStatus(c *command, args []string, stdout, stderr io.Writer) int {
	pl, w, code, ok := gatewayArgs(c, args, stdout, stderr)
	if !ok {
		return code
	}
	st, err := ollamamcp.State(w)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		return exitFail
	}
	switch {
	case !st.Exists:
		fmt.Fprintln(stdout, "the Ollama gateway: not running (plinthctl ollama-mcp start)")
		return exitFail
	case !st.Running:
		why := ""
		if st.Error != "" {
			why = " (" + st.Error + ")"
		}
		fmt.Fprintf(stdout, "the Ollama gateway: stopped%s; plinthctl ollama-mcp start starts it again\n", why)
		code = exitFail
	default:
		fmt.Fprintf(stdout, "the Ollama gateway: running since %s, on %s\n", st.Started.Format(time.RFC3339), ollamamcp.SocketPath(w))
	}
	if g, err := gateway(pl, w); err == nil {
		if spec, err := g.Spec(); err == nil && spec != st.Spec {
			fmt.Fprintln(stdout, "it is not the one this plinthctl and config.yml describe: plinthctl ollama-mcp start replaces it")
		}
	}
	if out, err := ollamamcp.Logs(w, 10); err == nil && strings.TrimSpace(out) != "" {
		fmt.Fprintf(stdout, "its last lines:\n%s\n", strings.TrimRight(out, "\n"))
	}
	return code
}

// runReview is review <type> <file> (plain) or, with -n, review -n <ns> <RUN> <type>: the request review.<type> the
// controller carries out.
func runReview(c *command, args []string, stdout, stderr io.Writer) int {
	if slices.ContainsFunc(args, func(a string) bool { return a == "-n" || strings.HasPrefix(a, "-n=") }) {
		if len(args) == 0 || strings.HasPrefix(args[len(args)-1], "-") {
			return usageError(c, stderr, "review -n <ns> <RUN> <type>: the type is required")
		}
		typ := args[len(args)-1]
		return queueRequest(c, args[:len(args)-1], stdout, stderr, decide.Review+"."+typ, nil)
	}
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	work := fs.String("work", defaultWork, "Plinth's work directory, for its config.yml")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		return usageError(c, stderr, "a review type and a file")
	}
	pl, cl, err := plainOllama(*work, config.ReviewersKey)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl review: %v\n", err)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	f := review.NewFetcher(pl.ForgejoURL, pl.ForgejoToken)
	out, err := plain.Review(ctx, cl, pl.Ollama.Models.Review, f, pl.Reviewers.Type, pos[0], pos[1], time.Now)
	switch {
	case errors.Is(err, plain.ErrRefused):
		fmt.Fprintf(stderr, "plinthctl review: %v; nothing written\n", err)
		return exitUsage
	case err != nil && out != "":
		fmt.Fprintf(stderr, "plinthctl review: FAILED (%v): see %s\n", err, out)
		return exitFail
	case err != nil:
		fmt.Fprintf(stderr, "plinthctl review: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(stdout, "review: wrote %s\n", out)
	return exitOK
}
