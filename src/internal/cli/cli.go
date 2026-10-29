// Package cli dispatches plinthctl's commands and prints help grouped by what each command needs.
// Spec: plinthctl.README.md, "Commands".
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"plinth/internal/container"
	"plinth/internal/ollamamcp"
	"plinth/internal/plain"
	"plinth/internal/term"
)

// Version is set at build time (-ldflags "-X .../cli.Version=...").
var Version = "dev"

// Exit codes shared by every command, unless a command says otherwise.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

type group int

const (
	groupPlain group = iota
	groupOllama
	groupController
)

var groupTitles = []string{
	groupPlain:  "No backend (plain AAA) - works in <repo>/ai/work (--repo, default the current directory)",
	groupOllama: "Needs Ollama",
	groupController: "Needs the controller and Forgejo - commands take -n <owner>/<repo> (default: the origin of the git checkout you are in;\n" +
		"register: --repo) and --work (default ~/.plinth)",
}

type command struct {
	name    string   // one or two words, e.g. "new-ask", "ask lint"
	aliases []string // other names, e.g. "get answer" for "get ans"; not listed in the help
	group   group
	summary string
	usage   string // full usage text after "Usage: plinthctl "
	run     func(c *command, args []string, stdout, stderr io.Writer) int
}

func commands() []*command {
	return []*command{
		{name: "new-ask", group: groupPlain, summary: "start an ask script from the template with a fresh RUN_NAME",
			usage: "new-ask [--repo R] [--work W] [--template F] [<issue>]\n" +
				"Draws a free RUN_NAME, creates R/ai/work/<RUN>/ask.sh (mode 0700) from the template's script with RUN_NAME filled\n" +
				"in, and prints its path. It never overwrites. The template: F (an AskTemplate manifest of mode aaa, or a script as\n" +
				"it is), else <work>/ask-template.yml if it exists, else the embedded base (ask template prints it).\n" +
				"An exclusive flock on ai/work/.new-ask.lock keeps two calls from taking the same pair; it gives up after 10 s.\n" +
				"A run whose ask.sh can't be written moves to ai/work/failed/<RUN>/. R defaults to the current directory.",
			run: runNewAsk},
		{name: "run-namer", group: groupPlain, summary: "print a free RUN_NAME <adjective>-<noun>[-<issue>]",
			usage: "run-namer [--repo R] [<issue>]\n" +
				"A pair is taken when an entry with it exists up to three levels below R/ai/work (any agent, any issue\n" +
				"number, done/ and failed/ included); symlinks are not followed. It only prints: new-ask reserves a name.",
			run: runRunNamer},
		{name: "read-answer", group: groupPlain, summary: "print a finished, DLP-scanned answer; otherwise only its state",
			usage: "read-answer [--repo R] <RUN>\n" +
				"The only way an agent reads an AAA answer. Prints R/ai/work/[done/]<RUN>/answer[.FAIL].dlp-ok.yml only when it\n" +
				"decodes strictly and its status.dlp.result is ok; a run that stopped at a gate also gets a line on stderr.\n" +
				"Otherwise one fixed line, never any file content, that says what to do next, and exit 10 still running, 11 the\n" +
				"DLP scan failed (nothing overrides it here: tell the user), 12 not scanned (answer[.FAIL].new.yml: the user runs\n" +
				"plinthctl finish), 13 no such run or not run yet, 14 INVALID (not a valid document: a bug; tell the user).\n" +
				"plinthctl help aaa lists what an agent does next for each.",
			run: runReadAnswer},
		{name: "scrubber", group: groupPlain, summary: "write scrub.sh to delete finished runs (deletes nothing itself)",
			usage: "scrubber [--repo R] [--work W] [--all]\n" +
				"Writes ai/work/scrub.sh (mode 0700): per finished run in ai/work/done/ (a run directory), rm -rf of it and of\n" +
				"its untouched original in plinth.aaa-raw-dir (read from <work>/config.yml if it exists, that key alone;\n" +
				"default <work>/aaa-raw). Without --all, runs whose name ends in -<issue> stay. Deletes nothing itself: read\n" +
				"scrub.sh, then run it.",
			run: runScrubber},
		{name: "handoff-scrubber", group: groupPlain, summary: "write handoff-scrubber.sh to delete one handoff branch",
			usage: "handoff-scrubber [--repo R] --branch <handoff/...>\n" +
				"Writes ai/work/handoff-scrubber.sh (mode 0700): it deletes the branch locally and on origin, then itself\n" +
				"(a step that fails keeps it, to run again). Deletes nothing itself: read it, then run it.",
			run: runHandoffScrubber},
		{name: "ask lint", group: groupPlain, summary: "dry-run checks of an ask script (bash -n, header, RUN_NAME, size)",
			usage: "ask lint [--plinth] [--work W] <path/to/ask.sh>\n" +
				"Checks without running: bash -n, line 1 #!/bin/bash, the header (# AAA script: <what>, READ-ONLY or CHANGES:),\n" +
				"exactly one RUN_NAME, AGENT_NAME and INTERACTIVE (0 or 1) line, at most 500 lines, and RUN_NAME equal to the run\n" +
				"directory's name when that is a run name. Plain AAA: the AAA library line once (as ask template has it), after\n" +
				"RUN_NAME, AGENT_NAME and INTERACTIVE, no function definition, nothing naming running.yml, an answer file, a trap\n" +
				"or the library's own names (_AAA_*, _aaa_*). --plinth (the check every submission gets): the same, with the\n" +
				"controller's library line (source \"$AAA_LIB\"; ask template --plinth) after NETWORK and PRIVILEGED too, and adds:\n" +
				"no .. path component, nothing naming the work directory, no ssh unless PRIVILEGED=1. Exit 1 with one\n" +
				"line per finding.",
			run: runAskLint},
		{name: "ask template", group: groupPlain, summary: "print a fresh base ask template (an AskTemplate manifest)",
			usage: "ask template [--plinth]\n" +
				"Prints the base AskTemplate manifest of mode aaa: what new-ask starts a plain AAA run from without\n" +
				"<work>/ask-template.yml (the controller writes that one from it when it is missing). Its script is the header,\n" +
				"the AAA library line (source <(plinthctl ask lib)) and an example step. --plinth prints the one of mode plinth,\n" +
				"which holds the script of a run the controller runs (source \"$AAA_LIB\"): what <ns-dir>/ask-template.yml\n" +
				"starts as, and what ask.template gives an agent.",
			run: runAskTemplate},
		{name: "ask lib", group: groupPlain, summary: "print the AAA library a plain AAA script sources",
			usage: "ask lib\n" +
				"Prints the AAA library, which a plain AAA script sources in its one fixed line, source <(plinthctl ask lib): the\n" +
				"checks before anything runs, the helpers (section, say, block, fail, ask_yes, ssh_q, scrub, title), the answer\n" +
				"running.yml (kind Answer) written only through them, and the EXIT trap that ends the run as\n" +
				"answer[.FAIL].new.yml for plinthctl finish. The library calls this binary to escape what YAML can't hold.",
			run: runAskLib},
		{name: "completion bash", group: groupPlain, summary: "print the bash completion script",
			usage: "completion bash\n" +
				"Prints the script to stdout. Use it with source <(plinthctl completion bash) in ~/.bashrc, or save it once\n" +
				"as ~/.local/share/bash-completion/completions/plinthctl.",
			run: runCompletion},
		{name: "version", group: groupPlain, summary: "print the version", usage: "version", run: runVersion},
		{name: "help", group: groupPlain, summary: "this list, one command's usage, or the AAA workflow (help aaa)",
			usage: "help [<command> | aaa]\n" +
				"help aaa: the AAA workflow: its stages, a run's states (its file names), the phrases and the commands.",
			run: runHelp},

		{name: "review", group: groupOllama, summary: "review an ask script or an answer with a reviewer type",
			usage: "review [--work W] <type> <file>\n" +
				"   or: review -n <ns> [--agent A] [--work W] <RUN> <type>\n" +
				"Reviews an ask script (*.sh) or an answer (answer*.yml) with the prompt of reviewers.type.<type> on ollama.models.review\n" +
				"and writes review.<type>.md (an answer: review.answer.<type>.md) next to it, or .FAILED.md with the reason (exit 1).\n" +
				"The model answers in a fixed shape (a verdict and findings); a reply that doesn't fit fails. A file in done/ is\n" +
				"refused (exit 2). With -n it queues the request review.<type> of the run: the controller reviews its ask.sh again and\n" +
				"replaces that type's review.",
			run: runReview},
		{name: "dlp-scan", group: groupOllama, summary: "mask secret lines in an answer, keep the original host-only",
			usage: "dlp-scan [--work W] <answer-file>\n" +
				"Scans an answer document (.yml) in a run directory: masks the fixed patterns (private keys, known token formats,\n" +
				"password=-style values) in its texts, then the lines the model (ollama.models.dlp) flags, and writes the result\n" +
				"into status.dlp (a run's running.yml or answer[.FAIL].new.yml is finish's). The untouched original goes to\n" +
				"plinth.aaa-raw-dir/<RUN>/<name>.orig (default <work>/aaa-raw); <RUN> is the file's directory. A scanned file is\n" +
				"left alone. Exit 1 leaves the file untouched.",
			run: runDLPScan},
		{name: "finish", group: groupOllama, summary: "end a plain AAA run: check its answer, DLP-scan it, move it to done/",
			usage: "finish [--repo R] [--work W] <RUN>\n" +
				"The user runs it once the script has ended (the AAA library left R/ai/work/<RUN>/answer[.FAIL].new.yml). It\n" +
				"decodes the answer strictly: one that isn't a valid document becomes answer[.FAIL].INVALID.yml (exit 1). Otherwise\n" +
				"the DLP scan masks its texts (dlp-scan's two steps, through the Ollama gateway) and it is written again with\n" +
				"status.dlp as answer[.FAIL].dlp-ok.yml, or as .DLP-FAIL.yml when the model's replies didn't fit (exit 1); the\n" +
				"untouched original goes to plinth.aaa-raw-dir/<RUN>/. Then the run directory moves to ai/work/done/<RUN>/. A scan\n" +
				"that can't ask the model (the gateway not running, Ollama unreachable or not configured) changes nothing: exit 1,\n" +
				"run finish again once it is up.",
			run: runFinish},
		{name: "ollama status", group: groupOllama, summary: "is Ollama configured and reachable, are its models there",
			usage: "ollama status [--work W]\n" +
				"Says whether ollama.host is set and reachable (its version), and whether the review and dlp models are there\n" +
				"and loaded. Exit 1 when Ollama is not configured or unreachable, or a model is missing.",
			run: runOllamaStatus},
		{name: "ollama-mcp start", group: groupOllama, summary: "start the Ollama gateway (the controller does too)",
			usage: "ollama-mcp start [--work W]\n" +
				"Starts the Ollama gateway: plinthctl itself in a container of its own (an empty, read-only root, no\n" +
				"capabilities, the host's network), serving the Ollama MCP (ollama_chat, ollama_list_models, ollama_ps,\n" +
				"ollama_show, ollama_version) for the Ollama of ollama.host on <work>/state/ollama/ollama.sock. Every model call of\n" +
				"Plinth goes through it: the controller's reviews and DLP scans, review, dlp-scan and ollama status. One of this\n" +
				"binary and config.yml's ollama: that runs already is left alone; another is replaced. The controller starts it\n" +
				"at its start and again when it is gone, and leaves it running when it stops. The binary must be static\n" +
				"(CGO_ENABLED=0). Exit 1 when ollama.host is not set or it doesn't start.",
			run: runGatewayStart},
		{name: "ollama-mcp stop", group: groupOllama, summary: "stop the Ollama gateway",
			usage: "ollama-mcp stop [--work W]\n" +
				"Stops the Ollama gateway; none is no error. A running controller starts it again at its next check.",
			run: runGatewayStop},
		{name: "ollama-mcp status", group: groupOllama, summary: "is the Ollama gateway running, its last lines",
			usage: "ollama-mcp status [--work W]\n" +
				"Says whether the Ollama gateway runs and since when, whether it is the one this binary and config.yml describe,\n" +
				"and its last 10 lines (a line per tool call: the tool, how long, how it ended; never its content). Exit 1 when\n" +
				"it doesn't run.",
			run: runGatewayStatus},

		{name: "register", group: groupController, summary: "register a repo and an agent; prints a new token for the agent",
			usage: "register --repo <path> --agent <agent> [-n <ns>] [--work W]\n" +
				"Sets up <work> and the namespace (it only creates what is missing), records the agent's repo, generates a new\n" +
				"token and prints it on stdout, nothing else; all other output goes to stderr. Only the token's sha256 is kept,\n" +
				"in <ns-dir>/state/agents/<agent>.sha256, so the agent's previous token stops working. <path> may be any\n" +
				"directory in the repo. The namespace comes from the repo's origin remote unless -n is given. Works whether or\n" +
				"not the controller runs. In an agent's launch script: PLINTH_TOKEN=\"$(plinthctl register --repo \"$PWD\" --agent main)\"",
			run: runRegister},
		{name: "controller", group: groupController, summary: "the process that serves every namespace with a registered agent: its subcommands",
			usage: "controller start | stop | restart | status | run\n" +
				"Manages the controller as a service: start it in the background, stop or restart it, ask how it is, or\n" +
				"run it in the foreground. Without a subcommand it lists them.",
			run: runControllerHelp},
		{name: "controller start", group: groupController, summary: "start the controller in the background; returns once it serves",
			usage: "controller start [--work W]\n" +
				"Starts plinthctl controller run in a session of its own (its output in <work>/logs/controller.out) and returns\n" +
				"once it serves: its socket answers, the Plinth MCP server listens and its first poll is done. Prints its PID, the\n" +
				"MCP server's URL and what needs you. If it can't start, prints why and exits 1. Already running: says so, exit 0.",
			run: runControllerStart},
		{name: "controller stop", group: groupController, summary: "stop the controller; waits until it has ended",
			usage: "controller stop [--force] [--work W]\n" +
				"Asks the controller over its socket to stop, as Ctrl-C does, and waits up to 30 s until it has released\n" +
				"<work>/state/plinth.lock. While a run's script runs (in its container or in your terminal) it refuses and names\n" +
				"the runs, since a stop would kill them; --force stops them too (SIGTERM, SIGKILL 30 s later; they count as\n" +
				"killed) and waits a minute longer. One whose socket doesn't answer gets SIGTERM, if it still holds the lock.\n" +
				"Not running: says so, exit 0. Exit 1 when it refused, or still runs after the wait.",
			run: runControllerStop},
		{name: "controller restart", group: groupController, summary: "controller stop, then controller start",
			usage: "controller restart [--force] [--work W]\n" +
				"Stops the controller (controller stop, --force as there; none running is fine), then starts it (controller\n" +
				"start): what a changed global config.yml needs.",
			run: runControllerRestart},
		{name: "controller status", group: groupController, summary: "is the controller running: PID, MCP server, namespaces, what needs you, last lines",
			usage: "controller status [--work W]\n" +
				"Says whether the controller runs, its PID and since when, <work>, the Plinth MCP server's URL, its socket, every\n" +
				"namespace it serves with its last heartbeat, the Ollama gateway, the runs running now, what needs you, and its\n" +
				"last 10 log lines.\n" +
				"Exit 0 running, 3 not running (as systemctl status), 1 running but not answering.",
			run: runControllerStatus},
		{name: "controller run", group: groupController, summary: "run the controller in the foreground (for a terminal you watch, or a service manager)",
			usage: "controller run [--work W]\n" +
				"Runs in the foreground until Ctrl-C, SIGTERM or controller stop. Refuses to start while the global config doesn't\n" +
				"load or another controller holds <work>/state/plinth.lock (it names that PID). Serves every namespace with a\n" +
				"registered agent whose config loads, found by polling; stderr gets only what you have to act on (controller\n" +
				"status shows it too), the rest goes to <work>/logs/.",
			run: runController},
		{name: "get", group: groupController, summary: "the run list and the answer list", usage: "get [-n <ns>] [--agent A] [--work W]", run: runGet},
		{name: "get ask", group: groupController, summary: "the run list, or one approved ask script to stdout",
			usage: "get ask [-n <ns>] [--agent A] [--work W] [<RUN>]\n" +
				"Without <RUN>: the runs with their ask status. With <RUN>: prints its ask.sh to stdout if it is approved,\n" +
				"otherwise \"Run not approved\" on stderr and exit 1. Never writes into the repo.",
			run: runGetAsk},
		{name: "get ans", aliases: []string{"get answer"}, group: groupController, summary: "the answer list, or one answer to stdout",
			usage: "get ans [-n <ns>] [--agent A] [--work W] [<RUN>]   (get answer works too)\n" +
				"Without <RUN>: the answers with their status. With <RUN>: prints the answer to stdout once its DLP scan\n" +
				"passed or was overridden. Otherwise its status on stderr and exit 1; after a failed scan that line gives the\n" +
				"reason and points to the TUI or the web UI, which show the answer masked. Never writes into the repo.",
			run: runGetAns},
		{name: "get messages", group: groupController, summary: "the agent messages", usage: "get messages [-n <ns>]", run: notImplemented},
		{name: "describe", group: groupController, summary: "one run in detail", usage: "describe [-n <ns>] [--work W] <RUN>", run: runDescribe},
		{name: "exec", group: groupController, summary: "run an approved ask script (the controller runs it; a privileged one runs in this terminal)",
			usage: "exec [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Asks the gatekeeper as the controller will (a refusal names its rule), then writes the exec request. The controller runs\n" +
				"the script in its container, then scans the answer. A privileged script (PRIVILEGED=1, and every INTERACTIVE=1\n" +
				"one) runs here instead: it says what the script can do, you type yes, the controller records the start over\n" +
				"its socket, the script runs in this terminal as you, and the controller scans the answer once it ends.",
			run: runExec},
		{name: "done", group: groupController, summary: "release an answer to its agent",
			usage: "done [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Writes the done request: the controller renames the scanned answer (dlp-ok, or DLP-FAIL after override dlp) to\n" +
				"answer[.FAIL].released.yml, which answer.read returns to the agent. Writes nothing into the repo.",
			run: runDone},
		{name: "save", group: groupController, summary: "post a run's files on the run's issue in plinth.repo",
			usage: "save [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Writes the save request: the controller zips the run directory (without running.yml) into <RUN>.zip and posts\n" +
				"it as a Run artifacts comment on the run's issue in plinth.repo (opened first when the run has none), with\n" +
				"Plinth's own forgejo-token. Needs a scanned or released answer; after a failed DLP scan, override dlp first.\n" +
				"With plinth.autosave.runs the controller does this by itself right after done; this command then retries a save\n" +
				"that failed.",
			run: runSave},
		{name: "report", group: groupController, summary: "post a failed scan on the run's issue in plinth.repo",
			usage: "report [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Names plinth.repo and what will be posted, then asks for confirmation (type yes). The controller adds a\n" +
				"comment to the run's issue there (opened first when the run has none) for the run's failed reviews or DLP scan,\n" +
				"with the events that decided them, and attaches ask.sh, the review files and a failed answer as\n" +
				"<RUN>.report.zip: all of it masked first (the DLP scan), so not without Ollama. Once per run: report.md holds\n" +
				"the issue's link. With plinth.autosave.failures the controller does this by itself at the run's first failed check.",
			run: runReport},
		{name: "scrub", group: groupController, summary: "delete a run fully",
			usage: "scrub [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Deletes the run directory, the DLP scan's untouched original and the run's request files; asks\n" +
				"\"Are you sure?\" first. The event log keeps the run's lines (names and decisions, no content).",
			run: runScrub},
		{name: "override review", group: groupController, summary: "release exec after a failed review (two steps)",
			usage: "override review [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Names the failed reviews and what each says, asks you to type yes, then writes the request; the controller\n" +
				"writes override.review.md with the receipt of its event. Only before the run ran.",
			run: runOverrideReview},
		{name: "override dlp", group: groupController, summary: "release done and save after a failed DLP scan (two steps)",
			usage: "override dlp [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Names why the scan failed, asks you to type yes, then writes the request; the controller writes\n" +
				"override.dlp.md with the receipt of its event. Done then releases the masked answer as it is.",
			run: runOverrideDLP},
		{name: "accept network", group: groupController, summary: "let a network run (NETWORK=1) have the network (two steps)",
			usage: "accept network [-n <ns>] [--agent A] [--work W] <RUN>\n" +
				"Says what the run can reach (the network, no ssh), asks you to type yes, then writes the request; the controller\n" +
				"writes accept.network.md with the receipt of its event. Exec of a network run waits for it. Only before the run\n" +
				"ran, and only where plinth.exec.network allows it.",
			run: runAcceptNetwork},
		{name: "image login", group: groupController, summary: "log in to the namespace's image registry (podman login)",
			usage: "image login [-n <ns>] [--work W]\n" +
				"Runs podman login <host> in this terminal, <host> being forgejo-url's: the password goes to podman, never to\n" +
				"Plinth. Every image command after it uses that login. It lasts until your last session on this machine ends.",
			run: runImageLogin},
		{name: "image logout", group: groupController, summary: "log out of the namespace's image registry (podman logout)",
			usage: "image logout [-n <ns>] [--work W]\n" +
				"Runs podman logout <host>, <host> being forgejo-url's: the image commands need image login again afterwards.",
			run: runImageLogout},
		{name: "image publish", group: groupController, summary: "build the image from .plinth/ on Forgejo, push it and pin it (two steps)",
			usage: "image publish [-n <ns>] [--work W] [<commit>]\n" +
				"Fetches the repo's default branch from Forgejo (never an agent's checkout), checks .plinth/ at its head (or at\n" +
				"<commit>, which must be on it), shows every change since the pinned image's commit and every finding, and asks\n" +
				"you to type yes. Then it builds, pushes to <host>/<owner>/plinth-<repo> with your podman login, pins the\n" +
				"digest in the namespace's config.yml (plinth.exec.image) and pulls the image back. Needs image login first.",
			run: runImagePublish},
		{name: "image pull", group: groupController, summary: "copy an image (default: the pin) from the registry to this machine",
			usage: "image pull [-n <ns>] [--work W] [<tag>|sha256:<digest>]\n" +
				"Copies the image from the registry by its digest (a tag is resolved to its digest first), so this machine has\n" +
				"exactly those bits. Without an argument it pulls the pinned image. The controller never pulls.",
			run: runImagePull},
		{name: "image use", group: groupController, summary: "pin another image of the registry (two steps)",
			usage: "image use [-n <ns>] [--work W] <tag>|sha256:<digest>|<name>@sha256:<digest>\n" +
				"Pins an image of the namespace's image repo, or one published elsewhere, once its labels say it is a Plinth image\n" +
				"of the namespace. A tag is resolved to its digest once: the pin is the digest. It shows the image's commit and\n" +
				"tree beside the current pin's, and asks you to type yes.",
			run: runImageUse},
		{name: "image list", group: groupController, summary: "the images in the namespace's image repo, newest first",
			usage: "image list [-n <ns>] [--work W] [-o yaml]\n" +
				"Each tag with its digest, commit and creation time, marked pinned, here (this machine has it) or not a Plinth\n" +
				"image of the namespace; and the default branch's protection, as far as Plinth's token sees it.",
			run: runImageList},
		{name: "image delete", group: groupController, summary: "remove an image here (--local) or from the registry (--registry)",
			usage: "image delete [-n <ns>] [--work W] --local|--registry <tag>|sha256:<digest>\n" +
				"--local removes this machine's copy. --registry deletes the image from the registry, with every tag on it; that\n" +
				"can't be undone, and another machine may pin it, so you type yes. Neither works on the pinned image.",
			run: runImageDelete},
		{name: "approve", group: groupController, summary: "approve a gated message", usage: "approve [-n <ns>] <message-id>", run: notImplemented},
		{name: "reject", group: groupController, summary: "reject a message with a policy page", usage: "reject [-n <ns>] <message-id> <policy>", run: notImplemented},
		{name: "agent enable", group: groupController, summary: "let a registered agent act in the namespace (two steps)",
			usage: "agent enable [-n <ns>] [--work W] <agent>\n" +
				"Shows the repo the agent's registration comes from (or a pending one from another repo), asks you to type\n" +
				"enable, then records the agent with that repo in the namespace's config.yml (plinth.agents.enabled). A pending\n" +
				"registration becomes the current one, so the previous token stops working. register alone never lets an agent act.",
			run: runAgentEnable},
		{name: "agent disable", group: groupController, summary: "stop a registered agent from acting in the namespace",
			usage: "agent disable [-n <ns>] [--work W] <agent>\n" +
				"Removes the agent from plinth.agents.enabled in the namespace's config.yml. It stays registered.",
			run: runAgentDisable},
		{name: "agent list", group: groupController, summary: "the namespace's agents: enabled or pending, and their repos",
			usage: "agent list [-n <ns>] [--work W]\n" +
				"One line per registration: its agent, enabled or pending, and the repo it comes from.",
			run: runAgentList},
		{name: "agent guide", group: groupController, summary: "print the guide the Plinth MCP server gives the namespace's agents",
			usage: "agent guide [-n <ns>] [--work W]\n" +
				"Prints what the guide tool gives an agent of the namespace: what Plinth is and how to work with it, then\n" +
				"the namespace's run modes, its automatic reviews and every review type's policy URLs. docs/agent-mcp.md says\n" +
				"how to set an agent up.",
			run: runAgentGuide},
		{name: "agent token-rotate", group: groupController, summary: "print a new token for a registered agent; its previous one stops working",
			usage: "agent token-rotate [-n <ns>] [--work W] <agent>\n" +
				"register's token part, without its setup: the agent keeps its repo and whether it is enabled. Prints the new\n" +
				"token on stdout, nothing else, and keeps only its sha256. A pending registration from another repo keeps its\n" +
				"own token. Fails if the agent is not registered: plinthctl register does that.",
			run: runAgentTokenRotate},
		{name: "agent token-remove", group: groupController, summary: "remove an agent's registration; its token stops working",
			usage: "agent token-remove [-n <ns> | --repo <path>] [--work W] <agent>\n" +
				"Deletes <ns-dir>/state/agents/<agent>.sha256 and .repo. The namespace comes from -n, or else from the origin\n" +
				"remote of the repo at --repo (default the current directory). A namespace with no agent left is no longer served.",
			run: runAgentTokenRemove},
		{name: "events show", group: groupController, summary: "the decisions behind a run",
			usage: "events show [-n <ns>] [--work W] [<RUN>]\n" +
				"Prints the namespace's events (all, or those of <RUN>) after checking each against the hash chain: seq, time,\n" +
				"caller, action, target, outcome, rule and reason. A broken chain stops it with exit 1 and names the line.",
			run: runEventsShow},
		{name: "events verify", group: groupController, summary: "check the event log's hash chain",
			usage: "events verify [-n <ns>] [--work W]\n" +
				"Walks the chain across all event files and names the first line that doesn't match (exit 1). Needs no controller.",
			run: runEventsVerify},
		{name: "config check", group: groupController, summary: "check the global and the namespace configs; show where each namespace value comes from",
			usage: "config check [-n <ns>] [--work W]\n" +
				"Loads <work>/config.yml and every namespace's config.yml (or only -n's) the way the controller does, and lists every\n" +
				"problem at once. For a namespace it shows each namespace key's effective value and whether it comes from the\n" +
				"namespace or the global config. Exit 1 when a file has a problem. Works whether or not the controller runs.",
			run: runConfigCheck},
		{name: "logs scrub", group: groupController, summary: "delete log files older than plinth.log.max-age-days",
			usage: "logs scrub [--work W]\n" +
				"Lists the files in <work>/logs/ older than plinth.log.max-age-days (default 30), asks \"Are you sure?\", then writes\n" +
				"the request <work>/requests/logs-scrub.queued; the controller deletes them, like the Scrub logs button.",
			run: runLogsScrub},
		{name: "tui", group: groupController, summary: "the terminal UI: every run, its state and its actions, the events and the logs",
			usage: "tui [-n <ns>] [--work W]\n" +
				"The web UI's screens in this terminal (like k9s): Runs, a run with its files, the events, the logs. Actions are\n" +
				"keys (e exec, o override, d done, Ctrl-D scrub); each asks decide first and for confirmation, then writes the\n" +
				"request the controller carries out. -n starts with that namespace's runs. Needs a terminal of at least 80 x 24;\n" +
				"NO_COLOR turns colours off. : switches screens, ? lists the keys, Q quits.",
			run: runTUI},
	}
}

// Run dispatches args (without the program name) and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return exitUsage
	}
	if args[0] == "--help" || args[0] == "-h" {
		printHelp(stdout)
		return exitOK
	}
	if args[0] == container.ProbeArg { // the controller's smoke container runs plinthctl itself as its probe; not in the help
		return container.ProbeMain(args[1:], stdout)
	}
	if args[0] == ollamamcp.MainArg { // the Ollama gateway in its container (ollama-mcp start); not in the help
		return ollamamcp.Main(args[1:], stdout, stderr)
	}
	if args[0] == plain.TextArg { // the AAA library's text filter (ask lib); not in the help
		return plain.TextMain(args[1:], os.Stdin, stdout, stderr)
	}
	if args[0] == CompleteArg { // the completion script's question; not in the help
		complete(args[1:], stdout)
		return exitOK
	}
	c, rest := lookup(args)
	if c == nil {
		fmt.Fprintf(stderr, "plinthctl: unknown command %q - see plinthctl help\n", strings.Join(args[:min(2, len(args))], " "))
		return exitUsage
	}
	// Text from files reaches a terminal only through the sanitizer; piped or redirected, output stays as it is,
	// so get ask > ask.sh is byte-exact.
	stdout, flushOut := sanitized(stdout)
	stderr, flushErr := sanitized(stderr)
	defer flushErr()
	defer flushOut()
	return c.run(c, rest, stdout, stderr)
}

// sanitized is w behind term.Writer when w is a terminal, with the flush to call at the end; w itself otherwise.
// There it also wraps to the terminal's width, continuation rows starting with the locale's mark; a width it can't
// read, or one too narrow to wrap in (under 20), leaves the lines to the terminal.
func sanitized(w io.Writer) (io.Writer, func()) {
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		width := 0
		if t, err := term.NewTTY(int(f.Fd())); err == nil {
			if width, _, err = t.Size(); err != nil || width < 20 {
				width = 0
			}
		}
		s := term.NewWrapWriter(f, width, term.GlyphsFor(term.UTF8(os.Getenv)).Wrap)
		return s, func() { s.Flush() }
	}
	return w, func() {}
}

// lookup finds the command named by the first one or two words; a two-word command wins over a one-word one.
func lookup(args []string) (*command, []string) {
	cmds := commands()
	if len(args) >= 2 {
		for _, c := range cmds {
			if c.is(args[0] + " " + args[1]) {
				return c, args[2:]
			}
		}
	}
	for _, c := range cmds {
		if c.is(args[0]) {
			return c, args[1:]
		}
	}
	return nil, nil
}

// is reports whether name is c's name or one of its aliases.
func (c *command) is(name string) bool {
	return c.name == name || slices.Contains(c.aliases, name)
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "plinthctl - Plinth, an agent action enforcement layer.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: plinthctl <command> [flags] [args]   (flags before args)")
	cmds := commands()
	for g := range groupTitles {
		fmt.Fprintln(w)
		fmt.Fprintln(w, groupTitles[g]+":")
		for _, c := range cmds {
			if int(c.group) == g {
				fmt.Fprintf(w, "  %-20s %s\n", c.name, c.summary)
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "plinthctl help <command> or plinthctl <command> --help: that command's usage and what it needs.")
}

func printUsage(w io.Writer, c *command) {
	fmt.Fprintf(w, "Usage: plinthctl %s\n", c.usage)
	fmt.Fprintf(w, "Needs: %s\n", strings.ToLower(groupTitles[c.group][:1])+groupTitles[c.group][1:])
}

func runHelp(_ *command, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return exitOK
	}
	if len(args) == 1 && args[0] == "aaa" {
		fmt.Fprint(stdout, aaaHelp)
		return exitOK
	}
	c, rest := lookup(args)
	if c == nil || len(rest) > 0 {
		fmt.Fprintf(stderr, "plinthctl help: unknown command %q\n", strings.Join(args, " "))
		return exitUsage
	}
	printUsage(stdout, c)
	return exitOK
}

func runVersion(_ *command, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: plinthctl version")
		return exitUsage
	}
	fmt.Fprintln(stdout, "plinthctl", Version)
	return exitOK
}

func notImplemented(c *command, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		printUsage(stdout, c)
		return exitOK
	}
	fmt.Fprintf(stderr, "plinthctl %s: not implemented yet\n", c.name)
	return exitFail
}

// parse parses c's flags from args. It returns the positional args, or an exit code when parsing ended the command:
// exitOK after --help (usage printed), exitUsage after a bad flag.
func parse(c *command, fs *flag.FlagSet, args []string, stdout, stderr io.Writer) ([]string, int, bool) {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout, c)
			return nil, exitOK, false
		}
		fmt.Fprintf(stderr, "plinthctl %s: %v\n", c.name, err)
		fmt.Fprintf(stderr, "Usage: plinthctl %s\n", strings.SplitN(c.usage, "\n", 2)[0])
		return nil, exitUsage, false
	}
	return fs.Args(), 0, true
}

func usageError(c *command, stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "plinthctl %s: %s\n", c.name, msg)
	fmt.Fprintf(stderr, "Usage: plinthctl %s\n", strings.SplitN(c.usage, "\n", 2)[0])
	return exitUsage
}

func runRunNamer(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := fs.String("repo", ".", "the repo directory")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 1 {
		return usageError(c, stderr, "too many arguments")
	}
	issue := ""
	if len(pos) == 1 {
		issue = pos[0]
	}
	run, err := plain.RunName(plain.WorkDir(*repo), issue)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl run-namer: %v\n", err)
		return exitFail
	}
	fmt.Fprintln(stdout, run)
	return exitOK
}

func runNewAsk(c *command, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	repo := fs.String("repo", ".", "the repo directory")
	work := fs.String("work", defaultWork, "Plinth's work directory, for its ask-template.yml")
	tpl := fs.String("template", "", "an AskTemplate manifest of mode aaa, or a script (default: <work>/ask-template.yml, else the embedded base)")
	pos, code, ok := parse(c, fs, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 1 {
		return usageError(c, stderr, "too many arguments")
	}
	issue := ""
	if len(pos) == 1 {
		issue = pos[0]
	}
	w, err := expandWork(*work)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl new-ask: %v\n", err)
		return exitFail
	}
	out, err := plain.NewAsk(plain.WorkDir(*repo), w, *tpl, issue)
	if err != nil {
		fmt.Fprintf(stderr, "plinthctl new-ask: %v\n", err)
		return exitFail
	}
	fmt.Fprintln(stdout, out)
	return exitOK
}
