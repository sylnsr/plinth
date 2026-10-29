package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"plinth/internal/doc"
	"plinth/internal/plain"
	"plinth/internal/workdir"
)

// runReadAnswer is read-answer: the answer on stdout, or one fixed status line and its exit code.
func runReadAnswer(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := fs.String("repo", ".", "the repo directory")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageError(c, stderr, "one RUN_NAME")
	}
	a, err := plain.ReadAnswer(plain.WorkDir(*repo), pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl read-answer: %v\n", err)
		return exitFail
	}
	if a.Code != 0 {
		fmt.Fprintln(stdout, a.Status)
		return a.Code
	}
	if a.Failed {
		fmt.Fprintf(stderr, "%s: the run FAILED at a gate; its answer follows\n", pos[0])
	}
	stdout.Write(a.Body)
	return exitOK
}

// runAskTemplate is ask template: the base AskTemplate manifest of mode aaa, or with --plinth of mode plinth.
func runAskTemplate(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	plinth := fs.Bool("plinth", false, "the base of mode plinth, the controller's, not plain AAA's")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	mode := doc.ModeAAA
	if *plinth {
		mode = doc.ModePlinth
	}
	stdout.Write(workdir.BaseTemplate(mode))
	return exitOK
}

// runAskLib is ask lib: the AAA library, with this binary as the text filter it calls.
func runAskLib(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(c, stderr, "too many arguments")
	}
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl ask lib: %v\n", err)
		return exitFail
	}
	stdout.Write(plain.Lib(bin))
	return exitOK
}

// runFinish is finish: the end of a plain AAA run, one line saying how it ended. INVALID and DLP-FAIL exit 1, as does
// a scan that couldn't run (nothing changed).
func runFinish(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := fs.String("repo", ".", "the repo directory")
	work := fs.String("work", defaultWork, "Plinth's work directory, for its config.yml")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageError(c, stderr, "one RUN_NAME")
	}
	run := pos[0]
	pl, cl, err := plainOllama(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl finish: %s: %v (nothing was changed)\n", run, err)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	f, err := plain.Finish(ctx, cl, pl.Ollama.Models.DLP, pl.AAARawDir, plain.WorkDir(*repo), run)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl finish: %v\n", err)
		return exitFail
	}
	switch f.State {
	case plain.StateInvalid:
		fmt.Fprintf(stdout, "finish: %s: INVALID, not a valid answer document (%s) - saved as %s; a bug, never data: the agent won't read it\n",
			run, f.Reason, f.File)
		return exitFail
	case plain.StateDLPFail:
		fmt.Fprintf(stdout, "finish: %s: DLP scan FAILED (%s) - saved as %s; the agent won't read it: review it yourself and tell the agent what to do\n",
			run, f.Reason, f.File)
		return exitFail
	}
	fmt.Fprintf(stdout, "finish: %s: dlp-ok (%d chunk(s), %d fixed-pattern mask(s), %d line(s) masked, model %s) - %s; the original is in %s\n",
		run, f.DLP.Chunks, f.DLP.Fixed, f.DLP.Lines, f.DLP.Model, f.File, f.Raw)
	return exitOK
}

// aaaHelp is help aaa: the AAA workflow, a nano pipeline.
const aaaHelp = `AAA WORKFLOW - a nano pipeline: stages, the files passed between them, gates, and stages the agent has no part in.

  Stage        Needed       Who / command                                Produces                          Gate
  1 Author     required     agent: plinthctl new-ask [<issue>]           ai/work/<RUN>/ask.sh              ask lint (optional)
  2 Validate   required     the AAA library, before anything runs        -                                 bash -n, RUN_NAME = its directory
  3 Review     optional     user: plinthctl review <type> <ask.sh>       review.<type>.md                  the verdict is not "Do not run"
  4 Approve    if changes   user, a comment on the issue                 issue comment                     scripts that change things need it
  5 Execute    required     user's terminal, or "exec <RUN>"             running.yml, then answer.new.yml  INTERACTIVE=1 refuses AAA_QUIET=1
  6 Sanitise   required     user: plinthctl finish <RUN> (no agent)      answer.dlp-ok.yml (status.dlp)    else .DLP-FAIL.yml or .INVALID.yml
  7 Consume    required     agent: plinthctl read-answer <RUN>           what the agent sees               exit 0/10/11/12/13/14
  8 Publish    optional     "done, save": the result to the issue        issue record                      -
  9 Clean up   optional     user: plinthctl scrubber [--all]             ai/work/scrub.sh (read, then run) the user reads it

  Needed: required = every run | optional = the user decides | if changes = only for scripts that change state.
  finish's DLP scan goes through the Ollama gateway (plinthctl ollama-mcp start); without it finish changes nothing:
  run it again once the gateway is up. A second check after stage 6: plinthctl review dlp <answer>.

THE SCRIPT = THE AGENT'S LINES
  The header (# AAA script: <what>, READ-ONLY or CHANGES:, RUN_NAME, AGENT_NAME, INTERACTIVE), the one fixed line
  source <(plinthctl ask lib); ..., then the steps. The answer is a YAML document (kind Answer) that only the AAA
  library writes, through its helpers:
    section "<title>"      starts a section; say and block write into it
    say <text>             one line into the section
    ... | block            stdin into the section
    fail "<why>"           stops the run as failed (in the script itself: in a pipeline or $(...) it ends only that)
    ask_yes "<question>"   true only if the user types yes (INTERACTIVE=1)
    ssh_q <user> <host> <command>, ... | scrub, title <text>: ssh without prompts, secret values masked, the tab's title
  No function definitions, no trap, nothing writing running.yml or an answer: ask lint refuses them.
  Style: 2-space indent inside if/for/while/case and { } blocks, one command per line, long pipes broken after |;
  the templates show it, and ask lint warns (it doesn't refuse) about a line whose indent is off. A <work>/ask-template.yml written before keeps its old example until the user deletes it (it
  is written again) or copies plinthctl ask template over it.

RUN STATE = FILE NAMES (no database); the run is the directory ai/work/<RUN>/
  ask.sh alone                       queued
  running.yml                        running; left over = killed (a rerun is a new run)
  answer[.FAIL].new.yml              the script ended (.FAIL: fail, or an exit other than 0); finish not run yet
  answer[.FAIL].dlp-ok.yml           finished and scanned: the only state read-answer prints
  answer[.FAIL].DLP-FAIL.yml         the DLP scan failed - the agent never reads it
  answer[.FAIL].INVALID.yml          not a valid document: a bug - the agent never reads it
  done/<RUN>/                        archived (finish moves the directory there)
  <RUN> = <adjective>-<noun>[-<issue>]; a rerun is a new run (plinthctl new-ask), never the old directory again.

READ-ANSWER, AND WHAT THE AGENT DOES NEXT (the line it prints says so too)
  0    the answer follows: read it
  10   still running: wait until the user says "<RUN> done", then read-answer again (killed: a rerun is a new run)
  11   the DLP scan failed: tell the user, who reviews the answer and decides; nothing overrides it
  12   not scanned yet: ask the user to run plinthctl finish <RUN> (again, if the gateway or Ollama was down)
  13   not run yet: ask the user to run it (from ai/work: ./<RUN>/ask.sh); no such run: check the RUN_NAME
  14   INVALID, not a valid document (a bug, never data): tell the user; a rerun is a new run

PHRASES
  exec <RUN>        the agent runs it through the IDE terminal: AAA_QUIET=1 ai/work/<RUN>/ask.sh (never an INTERACTIVE one)
  <RUN> done        the user ran it and plinthctl finish <RUN>
  done, save        publish the result to the issue

THE PLINTH MCP SERVER
  An agent without plinthctl (in a container) works through the Plinth MCP server, which the controller serves (it
  must run): guide (start here); Plinth runs, the secure default: ask_template, ask_submit, ask_status, answer_read
  (the user runs and releases them: exec, done, or the TUI); plain AAA in the agent's repo's ai/work, the informal
  way: aaa_new, aaa_lint, aaa_status, aaa_read (new-ask, ask lint, read-answer; the user runs and finishes them as
  above). Setting an agent up: docs/agent-mcp.md; plinthctl agent guide -n <ns> prints what the server tells the agent.

COMMANDS
  agent             new-ask, run-namer, ask lint, ask template, read-answer
  user              finish, review, dlp-scan, scrubber, handoff-scrubber, ollama status, ollama-mcp start | stop | status
  the script        ask lib (source <(plinthctl ask lib))
  plinthctl help <command> for each one's usage.
`
