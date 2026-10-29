# plinthctl - Plinth's core and commands

This file is the spec of `plinthctl`, the one Go binary of **Plinth**, an agent action enforcement layer (an agent runtime
security mesh). The web UI has its own spec (`plinth-web-ui.README.md`), and so has the TUI (`plinth-tui.README.md`, a proposal).
The code follows this file: change the spec here first, then the code. Items marked **(proposal)** wait for
confirmation.

## Principles

- **Plinth is the foundation; capability is built on top** (decided). The core stays minimal,
  static and secure: the container boundary, the event log, the decision rules, the risk score and its policy, and the
  checks. A capability goes into the core only if it can't live in a layer above. Work shifts from ad-hoc scripts to
  known, documented tooling (shift-to-tool): Ansible for every run (Ansible runs), heavy or long-running work set up
  in the tool built for it (a Kubernetes Job, a log pipeline) rather than done in a run, and risky patterns turned
  into vetted custom modules rather than accepted run by run.
- **`plinthctl` controls every container** (decided). The process is the **controller**
  (`plinthctl controller start`), the only part that starts, watches and removes containers: the Ollama gateway, later the
  Plinth MCP server and the web UI, and the runs. The one exception is for plain AAA, which has no controller:
  `plinthctl ollama-mcp start` and `stop` start and stop the Ollama gateway with the same description.
  Each namespace has a **runner** inside it, which handles only that
  namespace. Kubernetes gives the shape, not the implementation: each container has one description in Go and a
  podman backend, and no Kubernetes code goes into this repo. Only the controller touches `<work>`. The Ollama MCP
  is the one gateway to Ollama, on a Unix socket, for Plinth's own callers only. Built so far: the names (step 1),
  the split of package `controller` and package `runner` (step 2) and the Ollama gateway's container (step 3); step 4,
  the retirement of the earlier helper scripts, is in progress (see No backend).
- **No YOLO modes.** Every check that can't run fails closed; only the user's two-step override releases it.
- **Deny by default** (decided). Every action passes through one gatekeeper, the decision function; an action no rule
  allows is refused.
- **The agent never starts anything.** It writes ask scripts and sends messages. Exec, Done, overrides, Save, Scrub,
  Approve and Reject are the user's.
- **File names are the state, and each state change is one atomic rename** (decided). A run's stage and state come
  from file and directory names only; there is no database and no separate state file. A state file next to the data
  (e.g. a `released.yml` beside `answer.yml`) would make every change two operations: a crash between them leaves a state
  nobody can read, and clean-up has to reconcile the two. A rename either happened or it didn't. New files are written
  under a temporary name in the same directory and renamed into place. To hold after a crash or power loss too, the
  file is synced before the rename and its directory after it (package `safefs`: `WriteNew`, `Rename`).
- **Immutable pipeline** (decided). A submitted script never changes and a run never runs twice: a new version is a new
  submission. Failures are kept for analysis: they are moved, never deleted.
- **Answers only from inside the pipeline** (decided). Plinth accepts an answer only from a run it executed itself:
  the controller's child, or an interactive `plinthctl exec` it checked. Only for those does it know the script's hash, the
  environment, the timeout and the exit code. A script taken out with `get ask` and run elsewhere has left the
  guardrails for that run: its answer stays outside. Accepting it would let an unvetted result pass as a vetted one.
  This needs no rule in `decide()`: no MCP tool or command takes an answer in, and the controller scans an answer only as
  the step after an exec it ran (see Process, DLP scan after exec).
- **Deterministic first** (decided). Every scan runs fixed tooling checks first and asks Ollama only after they pass:
  the ask-script reviews, the DLP scan and message review.
- **No symlinks** (decided). All file access in `<work>`, and the plain-AAA commands' access in `<repo>/ai/work/`, goes
  through Go's `os.Root` (standard library; `go.mod` asks for Go 1.25, which added `Root.Rename`) and package `safefs`
  on top of it:
  - `os.Root` keeps every path inside its directory, but it still follows a symlink that stays inside, e.g.
    `runs/al/X/answer.released.yml -> ../../main/Y/answer.dlp-ok.yml`.
  - `safefs` refuses those: it `Lstat`s every component of the path and refuses a symlink anywhere on it, opens only
    regular files with one hard link (a hard link is another way to reach another run's file), and checks with
    `os.SameFile` that the file it opened is the one it checked.
  - A symlink in a run directory is refused and logged; Plinth itself only creates regular files and directories.
- **One document format** (decided; see Documents): answers, reviews and the run-state files
  are YAML documents of one family, like Kubernetes manifests, checked by one strict decoder. File names stay the
  state; the YAML is the content. A document that isn't valid is a bug, never data.
- **Listeners only queue.** The web UI and the Plinth MCP server hand work to the runners and never act themselves.
- **Linux only** (decided, 2026-10-14). A run's isolation is a rootless podman container, made of Linux kernel
  features (user, mount, PID and network namespaces, seccomp) with no equivalent on macOS or Windows, and the terminal code uses
  Linux's `ioctl`s. On a Mac, Plinth runs in a Linux VM (Podman machine, Lima, OrbStack, UTM), and the user works in a
  terminal on that VM, tmux included.
- **Dependencies:** the Go standard library, plus `go.yaml.in/yaml/v3` (vendored). Nothing else without an approval on the
  issue.

## Threat model (decided)

What each mode defends against:

- **Plain AAA** (the no-backend commands) guards against an honest agent's mistakes. The agent's container sees
  `<repo>/ai/work/` directly, so its file names and commands are a convention, not a control.
- **Plinth** (the controller) guards against a hostile agent, and against a hostile ask script that agent wrote:
  - The agent can't reach `<work>`. It acts only through its MCP tools, and `decide()` checks every call.
  - An ask script runs as the user, with the user's environment and tools, on purpose: running it is the same as the user
    running it. There is no sandbox around it. The defence is the vetting before exec: the tooling first
    (`ask lint --plinth`), then the automatic reviews, then the user, who starts every run. Scripts stay small (at most
    500 lines) so that the tooling, the reviews and the user can vet them completely.
  - A passing review never relaxes anything; only a failed review or the verdict `Do not run` blocks. A script can try to
    talk its own review into "OK to run", so a pass is advice to the user, never a permission.
  - The exec child gets none of Plinth's secrets through its environment or the controller's, and runs in its own user,
    mount and PID namespace, where it can't see Plinth's state or any other process. It runs as the user, so the
    rest of `$HOME`, SSH and the network stay within its reach: that part is left to vetting (see Process, Exec).
  - **Run modes** (built): normal mode closes the way out above (a container: no network, a read-only
    file system, none of the user's sockets or credentials); network and privileged mode give a script more, each
    accepted by the user, privileged mode only in the user's own terminal. See Run modes.
  - Nothing is written into the repo. All agents of a repo share its mount, so another agent could read or plant a file
    there. Answers reach an agent only through `answer.read`, after the DLP scan and the user's Done.

## Front ends

| Front end | For | Like |
|---|---|---|
| `plinthctl <command>` | the user, in their own terminal on the host | `kubectl` |
| `plinthctl tui` | the user, in their own terminal on the host | k9s |
| web UI | the user, in a browser, after the Forgejo login | Headlamp |
| Plinth MCP server | agents, with their token | the k8s MCP |

Every user action exists as a command, in the TUI and in the web UI, and all three write the same request (see Requests).

## Commands

`plinthctl help` prints the commands in the three groups below, each with its one-line summary. `plinthctl help <command>`
and `plinthctl <command> --help` print the command's usage, exit codes and what it needs. `plinthctl` with no arguments
prints the help to stderr and exits `2`. Exit codes, unless a command says otherwise: `0` ok, `1` failed (one line on
stderr saying why), `2` usage error.

**Confirmations**. Every confirmation that takes more than one key is typing `yes`, in the
CLI ("<question>? (type yes): ") and in the TUI ("? Type yes:"): privileged exec, override, accept network, report,
image publish, use and delete, agents enable. Anything else refuses. `y/N` prompts and one-key menus stay as they are.

**Untrusted text on the terminal**. A script, an answer, a review or a file name is untrusted:
it could set the terminal's title, write its clipboard (OSC 52), move the cursor, or hide and reorder lines with bidi
and invisible characters while the user reads it. So when stdout or stderr is a terminal, everything the commands write
there goes through the sanitizer (`term.Writer`): only printable text, tabs and newlines pass; every other rune becomes
the escape the TUI shows (`\x1b`, `\u202e`, invalid UTF-8 `\xNN`; plinth-tui.README.md, Untrusted text), without the
reverse video. Plinth's own text has nothing to escape. Piped or redirected, nothing changes: `plinthctl get ask -n <ns>
<RUN> > ask.sh` is byte-exact. Not behind it: the TUI, which draws the terminal itself and
sanitizes every line; and a script's own output while it runs, which the user accepted to run in their terminal: a
privileged one in `plinthctl exec`, a plain AAA one through the AAA library's `-echo`. `ask lint` refuses the same
characters in a script, so a script never needs them.

**Wrapping and the rule for characters**. On a terminal
the sanitizer also wraps every line to the terminal's width (read once, `TIOCGWINSZ`; a width it can't read, or one
under 20 columns, leaves the lines to the terminal): a continuation row starts with the **wrap mark** and a space, so it
can't be taken for a line of its own, which matters in a script. It breaks at the last space that fits within one write
and at the column in a word wider than a row; it holds nothing back past a write, so a prompt shows at once. An escape,
a wide character and a combining mark with its base never split. Piped or redirected, nothing is wrapped. **The rule
for characters, everywhere:** Unicode only for what is shown on screen, with an ASCII fallback when the locale isn't
UTF-8; plain ASCII for what is written to files, issues or logs (the review's flow diagram is ASCII); no emoji, whose
width differs between terminals. The locale is UTF-8 when the first non-empty one of `LC_ALL`, `LC_CTYPE`, `LANG`
holds `UTF-8` or `utf8`, in any case: one check, `term.UTF8`, for every glyph (`term.Glyphs`): the wrap mark `↪` or
`>`, what was left out `…` or `...`, completion's mark `✓` or `+`.

Two flags name the two directories (decided; there is no `--dir`, which was ambiguous between them):
- `--repo <path>`: the repo directory. Default: the current directory.
- `--work <path>`: Plinth's work directory, `<work>` below. Default: `~/.plinth`.

### No backend (plain AAA)

These work in `<repo>/ai/work/`, with no controller and no Forgejo. They are the plain-AAA kit: an engineer can use AAA
without the rest of Plinth. From `<work>` they read only `<work>/config.yml`, if it exists, for `plinth.aaa-raw-dir`
alone (default `<work>/aaa-raw`, see Config; the rest of the file isn't checked, so plain AAA works without
`forgejo-url` or the tokens; only a problem of that key, or a file that can't be parsed, stops them): the DLP scan's
untouched originals, which `dlp-scan` writes and `scrubber` deletes (decided: in the config, not a flag).

| Command | What it does |
|---|---|
| `new-ask [--repo R] [--work W] [--template F] [<issue>]` | draws a `RUN_NAME` and creates `ai/work/<RUN>/ask.sh` (mode 0700) from a template's script with `RUN_NAME` filled in; prints the path; never overwrites. The template: `--template F` (an `AskTemplate` manifest of mode `aaa`, or a script as it is, `#!` first), else `<work>/ask-template.yml` when it exists (read without following a symlink; one that doesn't decode, or has mode `plinth`, is an error), else the embedded base, which `ask template` prints. It holds an exclusive `flock` on `ai/work/.new-ask.lock` while it picks the name and creates the directory, so two calls can't take the same pair, even with different issue numbers; after 10 s of waiting for another `new-ask` it fails with a line naming the lock file. If a later step fails, the directory moves to `ai/work/failed/<RUN>/` (kept for analysis; the name stays taken) |
| `run-namer [--repo R] [<issue>]` | prints a free `<adjective>-<noun>[-<issue>]`: no entry with that pair exists up to three levels below `ai/work` (run directories of any agent and issue number, `done/` and `failed/` included; a file or symlink of that name also counts, so `new-ask` never collides with it). It descends only into real directories, never through a symlink; 40 adjectives x 200 nouns. It only prints; `new-ask` is what reserves a name |
| `read-answer [--repo R] <RUN>` | the only way an agent reads an AAA answer. It prints `ai/work/[done/]<RUN>/answer[.FAIL].dlp-ok.yml` only when it decodes strictly (see Documents) and its `status.dlp.result` is `ok`; a run that stopped at a gate also gets a line on stderr. In every other state it prints one fixed line, never anything of a file, which says what the agent does next (an agent that doesn't know AAA gets its direction from plinthctl; `help aaa` lists them too), and exits `10` still running (`running.yml`), `11` the DLP scan failed (`answer[.FAIL].DLP-FAIL.yml`, or a `dlp-ok.yml` whose `status.dlp` doesn't say `ok`; nothing here overrides it: the agent tells the user), `12` finished but not scanned (`answer[.FAIL].new.yml`: the user runs `plinthctl finish <RUN>`), `13` no such run or not run yet, `14` INVALID (`answer[.FAIL].INVALID.yml`, or a `dlp-ok.yml` that doesn't decode: a bug; the agent tells the user). The state comes from the file names, in this order: a failed scan, INVALID, running, not scanned, then the answer; any other file is no answer. Nothing is read through a symlink (built 2026-10-19; no other format is read) |
| `scrubber [--repo R] [--work W] [--all]` | writes `ai/work/scrub.sh` (mode 0700) with one `rm -rf` per finished run in `ai/work/done/` (a real directory with a run's name): its run directory and its untouched original in `<aaa-raw-dir>/<RUN>/` (a full scrub, decided); without `--all` it keeps runs whose name ends in `-<digits>`; anything else in `done/` (a file, a symlink) is skipped. It lists each entry (`scrub`, `keep`, `skip`) and deletes nothing itself (built 2026-10-18) |
| `handoff-scrubber [--repo R] --branch <handoff/...>` | writes `ai/work/handoff-scrubber.sh` (mode 0700) that deletes that handoff branch locally and on `origin`, then itself; a step that fails keeps the script, to run again, and each step skips a branch that is gone already. The branch must be `handoff/<...>` of letters, digits, `.`, `_`, `-` and `/` (no `..`, no part starting with `-`, no `.lock`) (built 2026-10-18) |
| `ask lint [--plinth] [--work W] <path/to/ask.sh>` | the dry-run check: UTF-8, and printable text only, naming the line and the character's escape (no control character but tab, no DEL, C1, bidi or invisible character: what the sanitizer escapes); `bash -n`; line 1 `#!/bin/bash`; header present (line 2 `# AAA script: <what the round does>`, `READ-ONLY` or `CHANGES:` in the header, and no `<...>` placeholder of the template left in it, so an agent picks one mode and names its sections); exactly one `RUN_NAME="..."`, `AGENT_NAME="..."` and `INTERACTIVE=` line, `INTERACTIVE` `0` or `1`; `RUN_NAME` equal to the directory's name when that is a run name; at most 500 lines. Without `--plinth`, a plain AAA script: the AAA library line exactly once, as `ask template` has it (`source <(plinthctl ask lib); [[ "${AAA_LOADED:-}" == 1 ]] \|\| { ... exit 2; }`), after the `RUN_NAME`, `AGENT_NAME` and `INTERACTIVE` lines, which the library reads; no function definition (`name()`, `function name`: the helpers are the library's), and outside comments nothing naming `running.yml`, an answer file (`answer*.yml`), the library's own names (`_AAA_*`, `_aaa_*`) or `trap` (only the library writes the answer). With `--plinth` (the check every submission gets): the same, but with the controller's library line, as `ask template --plinth` has it (`source "$AAA_LIB"; [[ "${AAA_LOADED:-}" == 1 ]] \|\| { ... exit 2; }`), after the `NETWORK` and `PRIVILEGED` lines too; no relative paths (no `..` path component: `../x`, `x/..`, `cd ..`; `{1..3}` is not one), and nothing naming the `--work` path. Naming Plinth itself is allowed (we work on Plinth with Plinth): whether a script reaches into Plinth (`plinthctl`, `PLINTH_*` variables, `~/.plinth`) is for the security and DLP reviews, not a word match; and, unless the script says `PRIVILEGED=1`, no ssh: outside comments, no `ssh`, `scp`, `sftp`, `slogin` or `ssh_q` as a word (so `ssh://` and `/usr/bin/ssh` too, not `~/.ssh` or `ssh-keygen`), no `GIT_SSH`, `GIT_SSH_COMMAND`, `GIT_SSH_VARIANT`, `RSYNC_RSH`, `--rsh` or `core.sshCommand`, no `user@host:path` remote, and no `rsync` to a `host:path` (`rsync://` and `host::module` are fine). It checks words, not shell syntax, so a word in a string counts too. Layout warnings (2026-10-25; warn only, 2 spaces only, plain AAA too, since the user reads every script): `asklint.Warnings` checks the indent of every line, never refusing: 2 spaces per level (no tab) inside `if`/`for`/`while`/`until`/`case`/`select`, `{ }` and an open parenthesis (`fi`, `done`, `esac`, `}`, `)`, `else`, `elif` one level out, a case pattern one level in and its commands another, `;;` with the pattern or with the commands), and a line after a trailing `\`, `\|` or `&&` two deeper than the line the command began on. It reads lines, not shell syntax: quoted text and comments are blanked first, here-document bodies and the lines inside a multi-line quote are skipped, and blank and comment-only lines are not checked. `ask lint` prints each as `<path>: layout warning: line N: expected X spaces, found Y` (at most 10, then `and N more lines`) and the exit code stays that of the findings; `Check` is unchanged, so `decide` and the runner never see a warning. `plinthctl` warnings (2026-10-27; a lint warning only, not a security finding): `asklint.PlinthctlWarnings` names each line of a script with `PRIVILEGED=0` and the controller's library line that calls `plinthctl` (or `/aaa/plinthctl`, the controller's binary mounted read-only in the run's container for the library's text filter): the container has no `plinthctl` on its `PATH` and no controller socket, so the call fails. Plain AAA and `PRIVILEGED=1` scripts run in the user's terminal, where `plinthctl` works: none. Quoted text, comments and here-document bodies are skipped, so `say "ask michael to run plinthctl approve"` is not a call; `ask lint` prints `<path>: plinthctl warning: line N: ...` (at most 10, then `and N more lines`) and the exit code stays that of the findings. The security and DLP reviewers' `plinthctl` rule is for `PRIVILEGED=1` scripts only (reviewers/security.md, devops). `ISSUE=` (optional): at most one line `ISSUE="https://<forgejo>/<owner>/<repo>/issues/<N>"` (a comment may follow), the page of the issue the round is for; with `forgejo-url` known (a submission, the runner) the host must be it. The origin ticket is context for the run's issue (see The run's issue), never where anything is posted. Exit `1` with one line per finding |
| `ask template [--plinth]` | prints the base `AskTemplate` manifest of mode `aaa` (see Documents; built 2026-10-19): the script `new-ask` starts a plain AAA run from when `<work>/ask-template.yml` doesn't exist, which the controller writes from it when it is missing (the user may edit it). Its script is the header, `RUN_NAME`, `AGENT_NAME`, `INTERACTIVE`, the AAA library line and an example step: everything else is the library's (`ask lib`). `--plinth` prints the base of mode `plinth`, which holds the script of a run the controller runs, its library line `source "$AAA_LIB"; ...`: what `<ns-dir>/ask-template.yml` starts as, and what `ask.template` gives an agent (built 2026-10-19). The example shows the style and the header states it: `# Style: 2-space indent inside if/for/while/case and { } blocks; one command per line; break long pipes after |`, the example a `for` with an `if` inside and a pipe broken after `|`; the mode lines and the library line stay at column 0, as `ask lint` matches them. A `<work>/ask-template.yml` or `<ns-dir>/ask-template.yml` written before keeps its old script (the user may have edited it) until the user deletes it (it is written again) or copies `ask template [--plinth]` over it; `help aaa` says so too |
| `ask lib` | prints the AAA library of plain AAA, which the script sources in its one fixed line, `source <(plinthctl ask lib)`: printed by the binary, it always matches it, and nothing on disk can drift or be edited. Before anything runs it refuses (exit `2`, nothing written, a line on stderr) a `RUN_NAME` that isn't a run name or isn't its directory's, a script not named `ask.sh`, one that doesn't parse (`bash -n`), `INTERACTIVE=1` with `AAA_QUIET=1`, a run that has an answer already or a `running.yml` (a rerun is a new run). Then it creates `running.yml` (mode 0600) with the document's head and an untitled first section for what is said before the first `section`, and gives the helpers: `section "<title>"`, `say <text>`, `... \| block`, `fail "<why>"`, `ask_yes "<question>"` (`INTERACTIVE=1` only), `ssh_q`, `scrub`, `title`. `say` and `block` echo to the terminal too (to nothing under `AAA_QUIET=1`); every text goes through the binary's hidden filter (`__aaa-text`, see Documents). Its `EXIT` trap ends the run: `failure:` after `fail` or an exit status other than 0 ("the script exited with status N, not through fail"), then `result: ok` or `fail`, then `running.yml` becomes `answer[.FAIL].new.yml` and the original stdout gets `<RUN> done (<result>) - next: the user runs plinthctl finish <RUN>, then the agent plinthctl read-answer <RUN>`. A run killed with `kill -9` keeps `running.yml` |
| `version`, `help [<command> | aaa]` | `help aaa` prints the AAA workflow: its stages, the script and its helpers, a run's states (its file names), `read-answer`'s exit codes with what the agent does next for each, the phrases and the commands (in plinthctl, since the knowledge base is a separate product) |
| `completion bash` | prints the bash completion script (see Shell completion) |

They replace the earlier helper scripts. `sync-issues` is dropped with its script: a local copy of the issues goes stale with every comment and saves little; agents read issues through Forgejo.

### Needs Ollama

| Command | What it does |
|---|---|
| `review [--work W] <type> <file>` | reviews an ask script (`*.sh`) or an answer document (`answer*.yml`) with the prompt of `reviewers.type.<type>` on `ollama.models.review`; writes `review.<type>.md` or `review.answer.<type>.md` next to it, or `.FAILED.md` with the reason (exit `1`); the newest result replaces the older one of that file. A file under `done/` (`done/<RUN>/`), a type the config doesn't have, or a file that is neither `.sh` nor `answer*.yml` is refused (exit `2`, nothing written). Only a prompt with a `<<<BEGIN ANSWER>>>` section reviews an answer. Built 2026-10-18, see Reviews below |
| `dlp-scan [--work W] <answer-file>` | scans an answer document (`.yml`) in a run directory, decoded strictly (see Documents), as `finish` does: each of its texts gets the two steps, then the document is written again with `status.dlp`. The steps: it masks fixed patterns first (private-key blocks, known token formats such as JWTs, Ceph keys and Plinth agent tokens (`plinth_` + 43 base64url characters), `password=`-style assignments), then the lines the model (`ollama.models.dlp`) flags; the model only chooses further lines to mask and never rewrites text. The model step (package `dlp`, built 2026-10-18) reads the text the fixed masks left in chunks of 120 numbered entries (`<n><TAB><text>`; a line over 400 bytes comes in 400-byte pieces with its number, cut between runes, so nothing past a cut goes unseen) and answers in a JSON schema, `{findings: [{line, kind}]}`, kind one of `password`, `api_key_or_token`, `private_key`, `auth_header`, `credential_in_url`, `encryption_key`, `session_cookie`, `other_secret` (temperature 0, seed 0, `num_ctx` 16384). A reply that doesn't fit (not the schema, a line outside the chunk, an unknown kind, cut off) is asked once more, then the scan fails. A flagged line becomes `[line <n> masked by dlp-scan: <kind>]`. Keeps the untouched original in `<aaa-raw-dir>/<RUN>/<name>.orig` (mode 0600; `<RUN>` is the name of the file's directory, which must be a run name). One whose `status.dlp.result` is `ok` is left alone (exit `0`); a run's `running.yml` or `answer[.FAIL].new.yml` is refused (`finish` scans those), and so is any other file (not `.yml`, or not in a run directory). Exit `1` leaves the file untouched. The controller scans its runs' answers itself, the same way (see Process, DLP scan after exec). Plain `dlp-scan` built 2026-10-18, documents only 2026-10-19 |
| `finish [--repo R] [--work W] <RUN>` | ends a plain AAA run once its script has ended (run by the user; built 2026-10-19). The run is `ai/work/<RUN>/` with `answer[.FAIL].new.yml`, which the AAA library left (`running.yml` there: still running, or killed, nothing to finish). It decodes the answer strictly and checks it: one from a script (no `status`), with a `result`, of this run, `result: fail` exactly when the name says `.FAIL`. One that fails becomes `answer[.FAIL].INVALID.yml` (exit `1`, the reason on the line). Otherwise it drops sections with no title and no output (the library's untitled first one, when the script starts with `section`), runs the DLP scan on the document (dlp-scan's two steps on each text, see Documents), keeps the untouched original in `<aaa-raw-dir>/<RUN>/answer[.FAIL].new.yml.orig` (mode 0600) and writes the document again with `status.dlp` (`result`, `model`, `chunks`, `fixed`, `masked`) as `answer[.FAIL].dlp-ok.yml`, or, when the model's replies didn't fit (asked twice), with the fixed-pattern masks, `result: fail` and the `reason` as `answer[.FAIL].DLP-FAIL.yml` (exit `1`). Then the run directory moves to `ai/work/done/<RUN>/`. A scan that can't ask the model (the gateway not running, Ollama unreachable or not configured, a model Ollama doesn't have) changes nothing: exit `1`, and `finish` runs again once it can. One line says how it ended: `finish: <RUN>: dlp-ok (<c> chunk(s), <f> fixed-pattern mask(s), <n> line(s) masked, model <m>) - <file>; the original is in <raw>` |
| `ollama status [--work W]` | whether `ollama.host` is set and reachable, with Ollama's version, and whether the review and dlp models are there (`/api/tags`) and loaded (`/api/ps`): `not configured`, `reachable, version <v>` or `unreachable: <why>`, then one line per model. Exit `1` when Ollama is not configured or unreachable, or a model is missing (built 2026-10-18; the controller's `status` will say since when it is unreachable) |
| `ollama-mcp start [--work W]` | starts the Ollama gateway, the container every model call goes through (see Ollama gateway): `ollama-mcp: started, for Ollama <host> on <work>/state/ollama/ollama.sock`, or `running` (the same runs already) or `replaced` (another binary or `ollama:`). Exit `1` when `ollama.host` is not set or it doesn't start (built 2026-10-19) |
| `ollama-mcp stop [--work W]` | stops it; none is no error. A running controller starts it again at its next check, and says so (built 2026-10-19) |
| `ollama-mcp status [--work W]` | whether it runs and since when, whether it is the one this binary and `config.yml` describe, and its last 10 lines. Exit `1` when it doesn't run (built 2026-10-19) |

`review` takes any file path: an ask script (`*.sh`) or an answer document (`answer*.yml`); `dlp-scan` an answer
document in a run directory.

`review`, `dlp-scan`, `finish` and `ollama status` read `<work>/config.yml` as the plain-AAA commands do (see No backend), with
`ollama:` too (and `reviewers:` for `review`): a problem of those keys stops them; the rest of the file isn't checked.
They ask Ollama through the Ollama gateway of `<work>`, which must run (`ollama-mcp start`, or a running controller);
none of them starts it.

Without Ollama (not configured or unreachable) `review` writes `.FAILED.md` and `dlp-scan` exits `1`, both with the reason
"not available without Ollama" (`not available without Ollama (ollama.host is not set)`, or `(<host> unreachable:
<cause>)`), or "the Ollama gateway isn't running (plinthctl ollama-mcp start)". Nothing is skipped.

### Reviews (built 2026-10-18)

Package `review`, for `review`, the controller's automatic reviews and the request `review.<type>`:
- **The prompt** is the type's URLs' contents, read in order and joined by a blank line (a Forgejo `/src/` page URL is
  read as its `/raw/` file; http(s), no redirect, at most 1 MiB, 30 s), or else its `definition`. Plinth's
  `forgejo-token` goes only to `forgejo-url`'s scheme and host, never to another one. It is the system prompt, with a
  fixed paragraph after it that asks for the reply's shape.
- **What the model sees:** a script between `<<<BEGIN SCRIPT>>>` and `<<<END SCRIPT>>>`; an answer with each line
  numbered (`<n><TAB><text>`) between `<<<BEGIN ANSWER>>>` and `<<<END ANSWER>>>` (temperature 0.2, `num_ctx` 32768).
- **A review is data** (decided 2026-10-18: the JSON reply now, the Markdown file until the documents
  are built): the model answers in a JSON schema, `{verdict, findings: [{line, severity, text, quote}]}`, `verdict` one of
  `ok-to-run`, `run-after-fixes`, `do-not-run`, `severity` one of `high`, `medium`, `low`, `line` 0 or absent for none.
  **The quote** (a model invents findings and misnumbers lines): a finding of a *script* review also has
  `quote`, the text of the line it is on, copied exactly from the script (empty for none); the schema requires the field,
  the parser accepts its absence. Plinth checks the quote against the script (whitespace runs count as one space): on the
  finding's line, the finding stays; on another line, the finding moves there and its text ends `[line corrected: the model
  said N]`; nowhere in the script (a quote over several lines counts as found when the script holds it as a whole; one
  under 3 characters isn't checked), the finding stays and counts for the verdict, its text starting `(unverified: the
  quoted text is not in the script)`, so a made-up finding is visible and a real one with a mangled quote is never hidden.
  A finding on no line whose quote is found takes that line. An *answer* review is never asked for a quote (the shape
  tells the model to leave it empty and Plinth ignores it): an answer may hold what the DLP scan looks for.
  A reply that doesn't fit (not the schema, another verdict or severity, a line outside the file, an empty text, more
  than 50 findings, cut off) is asked once more, then the review fails closed. Plinth writes the file from the data:

  ```
  # Review <type> of <file>

  ## Verdict
  OK to run | Run after fixes | Do not run

  ## Findings
  - line <n> (<severity>): <text>        (None. when there is none)

  ---
  _Review of <file> (sha256 ...) by review type '<type>' (prompt sha256 ..., from <source>) on <model>, <time>. ..._
  ```

  A finding's text is on one line (whitespace and control characters become one space, at most 1000 characters), so
  nothing the model writes can start a line such as `## Verdict`. Only `Do not run` fails a script's review, as
  before; `Run after fixes` is advice.
- **A failed review** is `review[.answer].<type>.FAILED.md`: its reason on the first line after the heading (the UIs
  show it), then the prompt's source, the model and the time.

### Needs the controller and Forgejo

Every command here takes `-n <owner>/<repo>`, the **namespace**, and `--work` (`register` takes `--repo` instead of
`-n`). The namespace names `<work>/<owner>/<repo>/` (the `<ns-dir>`) and the Forgejo repo `<forgejo-url>/<owner>/<repo>`
whose issues the runs belong to. A run is named by its bare `<RUN>`: names are unique across the agents of a namespace
(decided).

**Without `-n`**, a command takes the namespace from the git checkout the current directory
is in: its `origin` remote, read with `gitrepo`'s locked-down git (`rev-parse --show-toplevel`, `remote get-url
origin`), as `register` and `agent token-remove` already did. The checkout's `.git/config` may be an agent's to write, so the
default is guarded: the origin must be on `forgejo-url`'s host (as `register` checks), the namespace must be one
`<work>` has (`register` sets one up), and the command says on stderr which namespace it took (`plinthctl get:
namespace michael/devops (the origin of /home/u/devops)`). Outside a checkout, `-n` is still required (exit `2`). `-n`
always wins. `config check` and `tui` keep their meaning without `-n` (every namespace); `controller` takes none.

| Command | What it does |
|---|---|
| `register --repo <path> --agent <agent> [-n <ns>]` | registers the repo (namespace) and the agent, and prints a **new** token for the agent on stdout; the agent's previous token stops working. The agent acts only once the user has enabled it (`agent enable`); from another repo than its current registration the new one waits as pending and the previous token keeps working. Works whether or not the controller runs (see Register) |
| `controller start \| stop \| restart \| status \| run` | the process, which serves every namespace with at least one registered agent (see Process), managed as a service: `start` it in the background, `stop` it (refused while a run's script runs, unless `--force`), `restart` it, ask its `status`, or `run` it in the foreground. Bare `controller` lists the subcommands (exit 2) |
| `get -n <ns>` | both lists below, the asks first |
| `get ask -n <ns> [--agent A]` | the runs: name, agent, ask status (see Runs, States), age |
| `get ans -n <ns> [--agent A]` | the answers: name, agent, answer status, age; `answer` is accepted for `ans` |
| `get ask -n <ns> <RUN>` | prints the run's `ask.sh` to stdout if it is approved (every automatic review passed, or `override.review.md` exists); otherwise `Run not approved` on stderr and exit `1`. The user can save it and run it before giving the agent its next turn; a copy run that way is outside the pipeline, and its answer can't be brought in (see Principles) |
| `get ans -n <ns> <RUN>` | prints the answer to stdout once its DLP scan passed or was overridden, as often as asked. After a failed scan it never prints it (decided): one line on stderr says the scan failed, gives the reason and points to the TUI or the web UI, which show the answer with the fixed-pattern masks; exit `1`. Not scanned yet: its status on stderr and exit `1` |
| `get messages -n <ns>` | the messages, as on the Conversations page |
| `describe -n <ns> <RUN>` | one run: stage, state, header, files, reviews, DLP result, pending requests |
| `exec -n <ns> <RUN>` | writes the `exec` request. A privileged script (`PRIVILEGED=1`; every `INTERACTIVE=1` one is) runs in this terminal instead: it shows what the script can do, the user types `yes`, the controller records the start over its socket, the script runs here, and its exit goes back to the controller, which scans the answer; refused when the controller isn't running (Run modes, Privileged mode; built 2026-10-18) |
| `done`, `save`, `scrub`, `report` `-n <ns> <RUN>` | write the request of that name; `scrub` asks "Are you sure?" first, `report` names the repo and what will be posted, then asks for confirmation (type `yes`), and without `ollama.host` says it isn't available and queues nothing (Save and Report built 2026-10-22; both post on the run's issue in `plinth.repo`; with `plinth.autosave.runs` or `.failures` the controller queues `save` after `done` and `report` at the first failed check by itself) |
| `review -n <ns> <RUN> <type>` | queues the request `review.<type>`: the controller reviews the run's stored `ask.sh` again (as an automatic review does, in the background; the request stays `.running` until the review is written) and replaces that type's review, passed or failed, e.g. after Ollama was unavailable. Not while the run is running. Without `-n` the target is a file, see above (built 2026-10-18) |
| `override review\|dlp -n <ns> <RUN>` | names what failed, asks for confirmation, then writes `override.review` / `override.dlp` |
| `accept network -n <ns> <RUN>` | says what a network run (`NETWORK=1`) can reach, asks you to type `yes`, then writes `accept.network` |
| `image list -n <ns>` | the images in the namespace's image repo (The image): tag, digest, commit, tree, created, whether this machine has a copy, which one is pinned; YAML with `-o yaml` |
| `image login -n <ns>` | runs `podman login <host>` for the namespace's registry in this terminal; Plinth never sees the password |
| `image logout -n <ns>` | runs `podman logout <host>` for the namespace's registry: the counterpart of `image login`; the Image screen's `o` |
| `image publish -n <ns> [<commit>]` | fetches the protected default branch from Forgejo, checks `.plinth/` (tools decide, Ollama advises), shows the diff and findings, asks you to type `yes`, builds, pushes with your `podman login`, pins the digest in the namespace's `config.yml`, pulls it back |
| `image use -n <ns> <tag>\|sha256:<digest>\|<name>@sha256:<digest>` | pins an image from the list, or one published elsewhere, after checking its labels; asks you to type `yes` |
| `image pull -n <ns> [<tag>\|sha256:<digest>]` | copies an image (default: the pin) from the registry to this machine |
| `image delete -n <ns> --local\|--registry <tag>\|sha256:<digest>` | `--local` removes this machine's copy; `--registry` deletes the image from the registry, which can't be undone (type `yes`); never the pinned image |
| `approve -n <ns> <message-id>`, `reject -n <ns> <message-id> <policy>` | write the message request |
| `agent token-rotate -n <ns> <agent>` | `register`'s token part without its setup, for an agent registered already: a new token on stdout and nothing else, its sha256 replacing the current registration's, so the previous token stops working at once; the repo and the enabled state stay as they are, and a pending registration keeps its own token. stderr only notes an agent that isn't enabled and a controller that isn't running. An agent that isn't registered fails (`register` registers it) |
| `agent token-remove [-n <ns> \| --repo <path>] <agent>` | (was `agent remove`, renamed with `token-rotate`) deletes the agent's `state/agents/<agent>.sha256` and `.repo` (and a pending registration's); its token stops working at once. Without `-n` the namespace comes from the origin remote of the repo at `--repo` (default the current directory). A namespace with no agent left is no longer served |
| `agent enable -n <ns> <agent>` | the second step of registering: shows the repo the agent's registration comes from (and, for a pending one, the current one it replaces), asks for confirmation (type `yes`), makes a pending registration current, and sets `plinth.agents.enabled.<agent>` to that repo in the namespace's `config.yml` (edited by key, see Config) |
| `agent disable -n <ns> <agent>` | removes `plinth.agents.enabled.<agent>`: the agent stays registered but can't act until enabled again |
| `agent list -n <ns>` | each registered agent with its repo and state (`enabled`, or `pending` until `agent enable`), and a pending registration from another repo below it |
| `agent guide -n <ns>` | prints what the Plinth MCP server's `guide` tool gives the namespace's agents (see Plinth MCP server; built 2026-10-21), so the user can read it; a namespace that isn't set up is refused |
| `events show -n <ns> [<RUN>]`, `events verify -n <ns>` | the decisions behind a run; checks the hash chain across all event files |
| `config check [-n <ns>]` | loads the global config and every namespace's (or `-n`'s) the way the controller does, lists every problem at once, and for each namespace shows every namespace key's effective value and whether it comes from the namespace or the global config; exit `1` on any problem. Never prints a secret value. Needs no controller |
| `logs scrub` | lists the log files older than `plinth.log.max-age-days`, asks "Are you sure?", then writes the request `<work>/requests/logs-scrub.queued` (see Log) |
| `tui [-n <ns>]` | the TUI, over every namespace (starting with `-n`'s runs); stdin and stdout must be a terminal of at least 80 x 24; spec: `plinth-tui.README.md` (decided) |

The request commands (`exec`, `done`, `save`, `scrub`, `report`, `override`) first ask `decide` the way
the loop will, from the same facts (package `request`), so a refusal comes at once and names its rule (exit `1`). Such a
refusal is not in the event log: nothing was requested, and only the namespace loop writes the log. The loop decides
again when it carries the request out. They print `Queued <action> of <RUN>.` (or that it is queued or in progress
already) and, while the controller isn't running, say on stderr that it acts once the controller runs.

The `get` commands print to stdout and never write into the repo, like `kubectl get <kind> <name> -o yaml > file`: the
user redirects the output if they want a file.

User commands write their request files straight into `<ns-dir>/requests/`, so they queue even while the process is down
(the TUI and web UI show "controller not running"). Commands that need an answer from the process (`stop`, `status`,
interactive `exec`) talk to it over the socket. `register` and the `agent` commands only write files and never need it.

### Shell completion (decided)

- `plinthctl completion bash` prints the completion script to stdout, like `kubectl completion bash`. The user either adds
  `source <(plinthctl completion bash)` to `~/.bashrc`, or saves it once as
  `~/.local/share/bash-completion/completions/plinthctl`.
- The script calls a hidden `plinthctl __complete <words...>`, which prints the candidates: commands, flags, namespaces
  under `<work>`, runs wherever a run is typed (below), and review types from the config. It prints nothing on any
  error and nothing on stderr, so a Tab never fails.
- Standard library only (no cobra). Bash only at first; zsh can load the same script through `bashcompinit`.
- Built 2026-10-18: the script is `complete -o default -F _plinthctl plinthctl`, so where `__complete` prints nothing
  (after `--repo`, `--work`, `--template`) bash completes file names. A command's flags come from its usage line;
  `-n`, `--work`, `--repo`, `--agent`, `--template` and `--branch` always take a value. Run names come from
  `<ns-dir>/runs/<agent>/` (every namespace's without `-n`); aliases (`get answer`) aren't offered, as in the help.
- **Runs** (decided; built 2026-10-21): wherever a run is typed, Tab offers
  **every** run, like `ls -l` shows every file with its attributes. A run comes as `<name><TAB><listing line>`: on the
  listing (a second Tab with more than one candidate, `COMP_TYPE` 63, or 33 and 64 with `show-all-if-ambiguous` and
  `show-all-if-unmodified`) bash shows the line, `<name>  (<state>) <mark>` padded into columns; otherwise it inserts
  the name only. The runs the command can act on now come first (`compopt -o nosort`, bash 4.4+) and carry the
  **mark**: `✓` when the locale is UTF-8 (the first non-empty one of `LC_ALL`, `LC_CTYPE`, `LANG` contains `UTF-8` or
  `utf8`, in any case: `term.UTF8`, the one check, see Wrapping and the rule for characters), `+` otherwise, decided by
  `__complete` from its environment. No colour: bash prints the list
  itself, and escape codes would count toward its column widths. The mark means "this command can be run on this run
  now", as the `x` of `ls -l` says a file can be executed. It is a **hint**: the command still checks everything, so a
  marked run it refuses is expected.
  - **Plain AAA** (`<repo>/ai/work` and `done/`, `--repo` or the current directory), from file names only, the state in
    `read-answer`'s order (`not-run`, `running`, `new`, `dlp-ok`, `DLP-FAIL`, `INVALID`, with `done` and `gate`):
    `finish` marks a run in `ai/work/` with `answer[.FAIL].new.yml`; `read-answer` one with
    `answer[.FAIL].dlp-ok.yml`. `review <type> <file>` and `dlp-scan <file>` offer the files as paths from the current
    directory (`ai/work/[done/]<RUN>/ask.sh` and the answer document; never `running.yml`). `review` marks the files its
    own check passes (an ask script or an answer, not in `done/`: the same function, so a change to one changes both); `dlp-scan`
    marks a `.DLP-FAIL.yml` (it scans it again). A path typed another way (`./`, `/`) matches none, so bash completes
    file names.
  - **The controller's commands**, the state as `get ans` says it, else as `get ask` does: `get ask` marks an approved
    run, `get ans` one whose answer it prints (`dlp-ok`, released, `DLP-FAIL` with `override.dlp`), `describe` and
    `events show` every run. `exec`, `done`, `scrub`, `override review`, `override dlp`, `accept network` and
    `review -n` (any reviewer type) mark a run when `decide` allows the action now, asked through `request.Hint`:
    the facts of the real request, the mode and whether `ask.sh` still matches `ask.sh.sha256` included
    (`runstate.Script`), but `ask lint --plinth` doesn't run (`Lint` stays empty). `exec` asks as `plinthctl exec`
    does, a privileged script from the user's terminal, so a privileged or interactive run is marked. A run whose
    `ask.sh` is missing, unreadable, too large or changed, and every run of a namespace whose config doesn't load, is
    offered with its state and no mark. `save` and `report` mark like the others (built 2026-10-22).

## Process

One host process, the **controller** (`plinthctl controller`), running as the user, holds one **runner** per namespace
(its submissions, requests, decisions, events, reviews, DLP scans and runs), the web UI and the Plinth MCP server, and
keeps the Ollama gateway's container running (see Ollama gateway). The controller is (decided) the
only part that starts containers; until its steps are built, the web UI and the Plinth MCP server run inside this
process (the Plinth MCP server, for the MVP; its own container on the socket API is a later step).

- **As a service** (it should feel like a systemctl service; built 2026-10-22). With the Plinth MCP server
  every agent needs the controller, so it runs in the background:
  - `controller start` runs `plinthctl controller run --work <work>` (its own binary, `os.Executable`) in a session of
    its own (`setsid`), stdin `/dev/null`, stdout and stderr to `<work>/logs/controller.out` (truncated at each start,
    mode 0600, no symlink followed). It returns once the controller serves: it holds the lock, its socket answers
    `status`, the Plinth MCP server listens and the first poll is done (`Status.Ready`); it prints the PID, the MCP
    server's URL and the notices there are by then (on stderr), and the next step. A controller that ends first: "didn't
    start" with its output, exit 1. Not serving within 30 s: exit 1, pointing at `status`. Already running: says so with
    its PID, exit 0. Bare `plinthctl controller` lists the subcommands and exits 2, so a script that still runs it
    notices ("just list the subcommands").
  - `controller stop [--force]` sends `stop` over the socket and waits up to 30 s until the lock is free.
    While a run's script runs (in its container, or in the user's terminal) the controller refuses and names the runs
    (`still running: <ns> <agent>/<RUN>, ...`), since a stop stops their scripts (see Stop); `--force` stops anyway, as
    Ctrl-C does, and the wait is a minute longer (the scripts' SIGTERM, then SIGKILL 30 s later). A socket that doesn't
    answer: SIGTERM to the PID in `plinth.lock`, only while that PID still holds the lock. Not running: says so, exit 0.
  - `controller restart [--force]` is `stop`, then `start`: what a changed global `config.yml` asks for.
  - `controller status`: running or not (exit 0, or 3 as `systemctl status`; 1 when it holds the lock but its socket
    doesn't answer), the PID and since when, `<work>`, the Plinth MCP server's URL, the socket, each attached namespace
    with its last heartbeat, the Ollama gateway (`ollamamcp.State`), the runs whose script runs, the notices, and the
    last 10 lines the controller logged (`logfile.Tail`, today's and yesterday's file), whether it runs or not.
  - **Notices** (rewritten 2026-10-28): what the user has to act on goes to stderr once per message, as
    before (`plinthctl controller: <message>[: <err>] (<effect>)[; next: <next>]`), and is a notice in the general log: a
    WARN line with `key=<key>` and the fields `err`, `effect` and `next` (see Log). A notice is open from its last line
    on, unless that line is an INFO line with the same key and `cleared=true`, which the controller writes when the
    cause is gone (a namespace config that loads again, the gateway up again, a log written again; also for a notice of an
    earlier controller, which it reads at its start), and for `plinth.notice.expires` days (default 14, at most
    `plinth.log.max-age-days`) after its last line. The controller keeps none in memory: `controller status` lists the
    open ones from the log (`logfile.OpenNotices`), whether the controller runs or not, and so does the TUI, whose header
    counts them ("<n> notices") and whose message line says the newest and what to do about it.
  - Starting at login or boot (a systemd user unit, `controller enable|disable`) is planned.
- **Start.** `plinthctl controller run`, what `start` runs (decided: no `-n`; a repo is never tied to the controller's start):
  - It sets up `<work>` (see Setup). A `config.yml` it just created from the template: "fill in <path>" and no start.
  - It refuses to start while the global `config.yml` doesn't load (a `<...>` value left included), listing every
    problem on stderr, as `config check` does.
  - It refuses if another process holds `plinth.lock`, and names its PID.
  - It runs in the foreground until `SIGINT` (Ctrl-C), `SIGTERM` or `stop` over the socket, which cancel every namespace
    loop and wait for them. stderr gets only what the user has to act on (a namespace config that
    doesn't load, a changed global config, a log that can't be written, once per distinct message); everything else
    goes to the log (see Log), under the command name `controller`.
- **Register** (decided). `plinthctl register --repo <path> --agent <agent> [-n <ns>]` registers a repo and an
  agent. It is meant for the agent's own launch script; Plinth never launches an agent, whichever agent program it is:

  ```bash
  PLINTH_TOKEN="$(plinthctl register --repo "${PWD}" --agent main)"
  # ...later lines pass PLINTH_TOKEN to the agent's startup
  ```

  - `<path>` may be any directory in a git repo; the repo's top level (`git -C <path> rev-parse --show-toplevel`) is
    what counts. git runs with fixed arguments, no shell, a 10 s timeout and no terminal prompts.
  - The namespace is `-n`, or else the last two path segments of the repo's `origin` remote, `.git` stripped (scp
    style, `ssh://`, `https://` and local paths). With neither it refuses and asks for `-n`.
  - **Origin check**: without `-n`, once the global `config.yml` loads, `origin`'s host must be `forgejo-url`'s
    host (case-insensitive; user and port ignored, so `ssh://git@host:2222/...` matches `https://host`); otherwise it
    refuses before writing anything and asks for `-n`. A config that doesn't exist yet is created and the check skipped;
    one with problems skips the check with a note on stderr pointing at `config check`. No message ever quotes the
    remote URL, which may hold `user:token`: only its host.
  - It runs Setup for `<work>` and the namespace, writes `<ns-dir>/state/agents/<agent>.repo` (the top level), then
    generates a new token and writes its sha256 to `<ns-dir>/state/agents/<agent>.sha256` (see Agent tokens). Both are
    written atomically (`safefs.Replace`: a temporary file, synced, renamed over the old one), so two launch scripts
    registering at once never leave a half-written file; the last one wins.
  - **stdout carries the token and nothing else**, so `$(...)` captures exactly it. **stderr carries only what the user
    has to act on** (decided): "fill in <path>" for a newly created global `config.yml` (the one file the user has
    to edit; a namespace config inherits), a warning when the controller isn't running (nobody holds `plinth.lock`),
    and errors. Everything else, every path Setup created, the agent and namespace registered, the controller's state, goes
    to the log (see Log).
  - Every call from the same repo replaces the agent's sha256, so the previous token of that agent stops working at
    once: re-running the launch script rotates the token (`agent token-rotate` does only that, without the setup).
    Each agent of a namespace has its own name and token, so `al` registering leaves `main`'s token alone; two
    sessions under the **same** name in the same namespace can't both work, the second register cuts off the first.
  - **Two steps** (decided). Registering makes an agent known, not trusted: the namespace's `config.yml` enables
    none by default, and a registered agent that isn't enabled has every submission refused (`target.valid`). The user
    enables it with `plinthctl agent enable -n <ns> <agent>` (later the TUI and the web UI), which records the repo the
    registration comes from in `plinth.agents.enabled.<agent>`. Enabled means enabled **for that repo**: a registration
    whose `.repo` differs from the enabled one counts as not enabled. Re-registering from the same repo (every launch)
    needs no new approval; register adds a note on stderr only while the agent isn't enabled for its repo.
  - **Another repo, same agent name:** when the agent already has a registration from another repo, register doesn't
    touch it (its token keeps working) and writes the new one to the pending slot, `<agent>.pending.repo` and
    `<agent>.pending.sha256`, with a note on stderr. `agent enable` shows both repos and, once confirmed, makes the pending
    one current: it renames `.pending.repo` over `.repo`, then `.pending.sha256` over `.sha256`, then sets the enabled
    repo. Every state in between fails closed: after the first rename the old token's repo no longer matches the enabled
    one, and until the config is edited the new repo isn't enabled either. A later register from yet another repo
    replaces the pending slot. So an agent's name can't be taken over from another checkout without the user seeing
    the repo and typing `yes`.
  - It never needs the process: the controller finds the registration by polling (below). It works the same whether the
    controller runs or not.
  - A launch script must not trace that line (`set -x`), or the token ends up in the terminal.
- **Setup** (package `workdir`; it only ever creates, never changes or deletes), run by `controller` for `<work>` and by
  `register` for `<work>` and the namespace, so a missing piece comes back on the next call:
  - `<work>`: the directory itself (mode 0700), `state/`, `aaa-raw/`, `logs/` and `requests/`, each with its
    `.gitignore`, and `config.yml` from the built-in global template if there is none;
  - the namespace: `<owner>/<repo>/`, its directories `runs/`, `submissions/`, `rejected/`, `requests/`, `raw/`,
    `messages/`, `events/`, `state/` and `state/agents/`, each with its `.gitignore`, `config.yml` from the built-in
    namespace template and `ask-template.yml`, the base `AskTemplate` of mode `plinth` (see Documents; what
    `ask.template` gives an agent; its script passes `ask lint --plinth` once its header is filled in), if there is none
.
  - The namespace template sets nothing (every key commented out), so a new namespace takes every namespace key from
    the global config (see Config); the global one has `<...>` values to fill in.
  - An existing file or directory is never touched; one reached through a symlink is refused (`safefs`).
  - Set-ups at the same moment (launch scripts registering together, the controller starting meanwhile) all succeed,
    and each file is created exactly once: `safefs.WriteNew` writes a temporary file with a random name and links it
    to its name, which fails if the name exists, then removes the temporary name (on a file system without hard
    links: check, then rename).
- **Which namespaces it serves.** Every `plinth.runner.poll-seconds` the process lists `<work>/<owner>/<repo>/state/agents/`:
  a namespace with at least one `<agent>.sha256` is attached (its goroutine starts, and the process checks that
  `<forgejo-url>/<owner>/<repo>` exists; if Forgejo can't be reached, that's a warning only). One whose last agent was
  removed (`agent token-remove`) is detached once none of its runs is running (an answer that only waits for the model
  doesn't count).
  - A namespace's config is loaded when it attaches. One that doesn't load keeps it from being served: stderr names it
    and its problems once and points at `config check -n`; once fixed, it attaches on the next poll. Its warnings (e.g.
    an empty `reviewers.automatic`) go to the log.
  - A namespace whose `config.yml` changes (modification time or size) is loaded again on the next poll and its loop
    restarted with it; if it no longer loads, the namespace is detached until it does (fail closed).
  - The global `config.yml` is read once, at start. When it changes, stderr says once "restart the controller to apply
    it".
- **Lock and socket**, in `<work>/state/` (mode 0700):
  - `plinth.lock` (`flock`, with the PID): a second process refuses to start and names the PID. A shared lock held for a
    moment (`register` asking whether the controller runs) is waited out: up to 10 tries 100 ms apart. The file stays
    after the process ends; only the lock counts.
  - `plinth.sock`, a Unix socket, mode 0600. The process accepts a connection only if the peer's uid (`SO_PEERCRED`)
    is its own. Built for runs in the user's terminal (Run modes, Privileged mode; 2026-10-18); `status` (a `Status`,
    one line of JSON) and `stop` (with `force`) over it for `controller status`, `start` and `stop`.
- **One runner per namespace, a supervised goroutine:**
  - In the code: package `controller` is the process (start, lock, socket, attaching,
    supervising and detaching the runners, the process-level requests, the check at start); package `runner` is a
    namespace's part. The controller hands every runner the same services: `<work>` and the global config, the log
    and stderr, what its check at start found, the process's one Ollama client and the review prompts' fetcher, and
    the smoke containers' results, which the whole process shares.
  - It runs under `recover()`. A panic marks the namespace failed (its heartbeat stops) and restarts it with backoff
    (1 s doubling to 60 s; a loop that ran longer than 60 s starts again at 1 s). The panic and its stack trace, on
    one line, go to the log.
  - It has its own `context`, cancelled when the namespace is detached or the process stops.
  - Every `plinth.runner.poll-seconds` (default 1) it beats `<ns-dir>/state/heartbeat` (written with the PID when the
    loop starts; each beat sets its modification time, without a rename or sync) and polls
    `<ns-dir>/requests/` and `<ns-dir>/submissions/`. Polling, not inotify: `fsnotify` pulls in `golang.org/x/sys`.
  - It is the only code that execs, renames a run's state, releases or deletes.
- **Exec:**
  - Each run is a rootless podman container (package `container`): `podman create` makes it, then its `podman
    start --attach` client, a child process (`os/exec`), runs it, both from the run's own goroutine, at most
    `plinth.runner.max-parallel-exec` (default 1) per namespace; a further `exec` request stays queued until one ends (an exec ends once its answer's DLP scan is
    written, see DLP scan after exec).
  - Before the start, `decide` checks that `ask.sh` still matches the `ask.sh.sha256` stored with it at submission and
    doesn't say `INTERACTIVE=1` (`state.valid`). Right before the start the controller checks the sum once more, since the
    file could have changed after the decision, and records it in the event `exec.start`, with the pinned image and
    its commit. The bytes it checked are kept as `<ns-dir>/raw/<agent>/<RUN>/ask.sh`, out of the script's reach, and that
    copy is what the container runs. The controller writes the empty `running.yml` itself (the AAA library writes the
    answer into it), so a script that dies before it sourced the library still shows it ran, without an answer
. `decide` runs `ask lint --plinth` on the stored `ask.sh` once more: a script that doesn't pass, though it passed
    at its submission (a later `plinthctl` lints more, e.g. one without the library line `source "$AAA_LIB"`), doesn't
    run (`ask.lint`: the findings, and "next: the agent writes it again from ask.template and submits it as a new run"). The sum checked right before the start says the bytes that run are the ones it linted.
  - **The image**: the namespace's pin, `plinth.exec.image` in its `config.yml`, an image published by the user
    and named by digest (Run modes, The image; decided 2026-10-14; built 2026-10-17). Before each
    run the controller checks that the pinned image is there and that its labels match the pin, and compares the pin's
    `.plinth/` tree with the default branch's on Forgejo (the drift check, which only warns); it builds nothing, pulls
    nothing, and runs no git. Its build from the agent's checkout at `plinth.exec.image-ref` (`image.build`), which
    the review showed the agent can steer, went with that.
  - **The container**: `podman create --init --read-only --cap-drop=all --security-opt=no-new-privileges
    --userns=keep-id --network=none --memory=<m> --memory-swap=<m>` (podman's default seccomp; private `/tmp`;
    `<m>` is `plinth.exec.memory`, default `512m`, swap included), named after the run and labelled
    `plinth.run=<owner>/<repo>/<agent>/<RUN>` and `plinth.work=<work>`, running `bash /aaa/run/ask.sh`
    (`--entrypoint=bash`, so an `ENTRYPOINT` of the image doesn't get the script as its arguments) with stdin and
    stdout on `/dev/null` (the script writes its answer itself). It mounts only:
    - the run directory, read-write, at `/aaa/run`, its working directory; files the script writes there stay the
      user's (`keep-id`);
    - the checked copy of `ask.sh`, read-only, over `/aaa/run/ask.sh`: the script runs exactly what was checked and
      can't change it;
    - the run's AAA library, read-only, at `/aaa/lib.bash` (`$AAA_LIB`): the controller writes it right before
      the start to `<ns-dir>/raw/<agent>/<RUN>/lib.bash` (mode 0600, under a temporary name renamed into place), out of
      the script's reach. Between that write and the start nothing but the controller writes it: no agent reaches `<work>` (see Threat model), no run's container gets `raw/` (only this file, read-only,
      besides its own run directory), and each exec writes it afresh, so no earlier copy counts;
    - the controller's own binary, read-only, at `/aaa/plinthctl`: the library's text filter (`__aaa-text`), as the
      smoke container's probe is (a static binary, so it runs in any image);
    - an empty tmpfs at the user's `$HOME`.

    Nothing of `<work>` but the run directory is there: no request can be queued, no other run's state forged, no
    `config.yml` read. Its own PID namespace: the script sees only its own processes. Its environment is
    `plinth.exec.env-file` (a file name in the namespace's directory, given to `--env-file`; default `""`: none), plus
    `HOME`, `AAA_QUIET=1` and `AAA_LIB=/aaa/lib.bash`; nothing of the user's or the controller's environment. No network in normal mode (a network
    run gets one of its own once accepted, and no ssh in any mode: `accept.network`, Run modes). The `podman` client
    itself gets the controller's environment **minus every variable the config refers to** (named by a `-env` key, or used
    as `${VAR}` in a value, e.g. `PLINTH_FORGEJO_TOKEN`), every variable whose name starts with `PLINTH_`, and
    `AAA_LIB` and `AAA_LOADED` (the library is the controller's), in its own session (no controlling terminal).
  - **No podman, no run**: at start the controller checks that `$HOME` is an absolute path, that podman answers
    (it lists the containers this `<work>` left), and that podman can limit memory (cgroups v2 with the memory
    controller delegated to the user: `podman info`; a run without its memory limit isn't what the config asks for).
    If not, it says why on stderr and in the log, still takes in submissions and carries out every other request, and
    fails each `exec` with that reason. It never runs a script outside a container.
  - **What a script can't read**: anything not mounted, above all `<work>` with its `config.yml` and the
    event log, and the controller's own environment: besides the container, the controller makes itself not dumpable at start
    (`prctl(PR_SET_DUMPABLE, 0)`), so its `/proc/<pid>/environ` and memory belong to root and no process of the user
    reads them; it refuses to start without that.
  - It is stopped after `plinth.runner.exec-timeout-minutes` (default 120): `podman stop`, `SIGTERM` to the script
    (`--init` passes it on; bash as PID 1 would ignore it), then `SIGKILL` to everything in the container 30 s later.
    The container's own `--timeout` is that plus 60 s, for a controller that died: 30 s after the controller's SIGKILL, so the
    two never race (at the same moment, podman lost the exit code). The leftover `running.yml` then shows it ended
    without an answer. Once the script has ended, `podman inspect` reads how the container ended and `podman rm` removes
    it, with whatever the script left running in it, however it left the process group (`setsid`, a double fork).
    `exec.end` gives the script's exit code and, for 129 to 192, the signal it means (`exit 137 (signal 9, killed)`); a
    SIGKILL the controller didn't send adds `over its memory limit (plinth.exec.memory 512m), unless the script sent SIGKILL
    itself` (podman's `OOMKilled` wasn't set on the test host, so the signal tells it; with `OOMKilled`: `killed over
    its memory limit`), a container podman couldn't create says `the container wasn't created:` with podman's message
    (no script ran, so its stderr is podman's alone), one that never started says `the container didn't run:` with
    podman's message, a client exit of 125 with no container left to inspect says `podman exit 125, and no container
    left to inspect:` with inspect's error (podman's failure or a script's `exit 125`: the words fit both), and an exit
    code podman didn't record says `exit code lost (podman recorded none)`.
  - **A run's state is the controller's to write**: a state file the script created in its own run directory (a
    review, an override, `report.md`, `issue.md`, an answer in any state but `new`) is moved to `<ns-dir>/raw/<agent>/<RUN>/unexpected/`,
    and the `exec.end` event names it. So is an `answer[.FAIL].new.yml` that isn't a regular file with one hard link
    (the DLP scan reads only such a file): `exec.end` says `answer refused`. Of `answer.new.yml` and
    `answer.FAIL.new.yml`, written both, the gate's counts. This holds when the controller
    stopped while the script ran, too: which files were there at the start, the controller knows from its
    `exec.start` event (see Events), out of the script's reach, not from the run directory.
  - **One bad run never stops the namespace**: a run that can't be read (two answer files, a hard-linked state
    file) is listed as `broken` and skipped, and an error in one run's or one submission's step is logged once and
    tried again at the next poll, while the loop goes on with the others. Only an event log that can't be written
    stops the loop: nothing may act unrecorded.
  - **The exec request's name says how far the exec got**: `.starting` from when the loop takes it until right
    before the script starts (the decision, the checks, `exec.start`, `running.yml`), `.running` from then until the
    exec is finished (`exec.end` and the DLP scan), then `.finished`. The script can't reach it (the container),
    unlike `running.yml`. So a run is running while its exec request is `.running` and the controller runs (the
    controller itself asks its own children), with or without its `running.yml`, and can't be scrubbed meanwhile; a
    left-over `running.yml` means no answer.
  - **A script that removes its `running.yml`** ends without an answer (the AAA library's `EXIT` trap writes no new
    one), gets it back when its exec is finished, at any end and for a left-over exec at the next start, and
    `exec.end` says so: the run's names still say it ran, without an answer.
  - **Only an answer says a run is done**: an answer file in the run directory, in any
    state, and nothing else. A run whose exec ended without one (killed, stopped, timed out, a container podman
    couldn't create, a controller that stopped, a script that removed its `running.yml`) is `no-answer`: not done, so the
    user can exec it again, or scrub it. The next exec runs the same checked `ask.sh` (decide checks it against its
    sum once more; its review override and `accept.network` still hold) as attempt n+1, n the run's `exec.end` events
    in the event log since its `ask.stored`, out of the script's reach; `exec.start` says `; attempt 2, after 1
    without an answer` after the mode. Right before that start the controller moves the left-over `running.yml` (what the
    last attempt wrote of its answer) to `<ns-dir>/raw/<agent>/<RUN>/running.<n>.yml` and writes a fresh one; the state
    files attempt n wrote itself go to `raw/<agent>/<RUN>/unexpected.<n>/` (`unexpected/` for the first), so no
    attempt overwrites another's. A run with an answer is done: exec is refused (`state.valid`), and a new version is
    a new submission. `get ask` shows `no-answer`; `get ans` says the last exec ended without an answer and that
    `plinthctl exec` runs it again; the TUI offers `e` (Exec), asking `Exec <RUN> in <ns> again? Its last exec ended
    without an answer.`; `answer.read` tells the agent the run has no answer (`answer.unreleased`).
  - **Stop** (`SIGINT`/`SIGTERM`): running scripts' containers are stopped as at the timeout (`SIGTERM`, `SIGKILL` 30
    s later); stderr names each, and it ends without an answer unless it wrote one. Once a namespace's scripts have
    ended, each exec is finished as at any other end (state files the script wrote moved out, `exec.end`, the DLP scan)
    before the process ends. A namespace that loses its last agent, or whose `config.yml` changes, is detached or
    reloaded only once its scripts have ended and no scan of their answers is in flight (the log says it waits);
    an answer that only waits for the model holds back neither (see DLP scan after exec).
  - At start, an `exec` request left `.starting` (the controller stopped before the script started) is carried out again,
    from the decision on; an empty `running.yml` in the run directory is the one that controller wrote, and goes first: no
    attempt left it. One left `.running` (the controller stopped after the start: a crash, a kill) is finished, never
    carried out again: the script started. A container outlives a controller that dies (until its own `--timeout`), so first
    the controller removes every container labelled with its `<work>` (`podman rm --force`, logged), before any namespace's
    loop starts; containers of another `<work>` stay. Unless the event log has
    its `exec.end`, first the state files the script wrote itself are moved out as above, with the names its
    `exec.start` recorded; for an `exec.start` without them, every state file a run can't hold before its start (an
    answer in any state but `new`, `override.dlp.md`, an answer review). Then `exec.end` is recorded (`no answer`,
    unless the script wrote one), a left-over `answer[.FAIL].new.yml` gets its DLP scan, and the request is
    finished. `exec.end` is recorded once, unless the event log has it already. An error leaves the request `.running`,
    never `.FAILED`, and the next poll tries again.
  - Interactive scripts never run here; they run in the user's terminal (`plinthctl exec` or the TUI).
- **DLP scan after exec** (decided): the controller, not the script, scans the answer once the exec has ended (its child
  exited, or the interactive `plinthctl exec` reported its exit code), so no script can skip it. The scan is the next
  step of that exec; the controller never looks for answer files otherwise. The AAA library's `EXIT` trap only renames
  `running.yml` to `answer[.FAIL].new.yml`. The controller decodes it strictly (`doc.ParseEnded`: a document from
  a script, of this run, with a result, `fail` exactly when its name says `.FAIL`), scans its texts (fixed patterns,
  then Ollama: `dlp.ScanAnswer`, as `finish` does) and writes it again with `status.dlp`, as `answer[.FAIL].dlp-ok.yml`,
  or as `answer[.FAIL].DLP-FAIL.yml` when the scan ran and failed. The fixed-pattern masks stay when the model step
  then fails (decided), so a `DLP-FAIL` answer, which the user and auditors see in the UIs (see Runs, Who sees an
  answer), never shows a private key or a known token format; the untouched original is in
  `<ns-dir>/raw/<agent>/<RUN>/`.
  - **The model step runs in the background**, so a scan of minutes holds up no other run: the
    controller reads the answer, keeps its untouched original and starts the scan (package `dlp`: the fixed patterns, then
    the model step, as `dlp-scan`) behind the Ollama semaphore; a later poll writes its result. The fixed patterns
    (private-key blocks, Plinth agent tokens, JWTs, Ceph keys, GitHub, GitLab, Slack and AWS key formats, `sk-` API
    keys, bearer and basic credentials, URL passwords, `password=`-style assignments) mask a value, never a line; the
    model's masks replace a line. Until the result is written the exec isn't finished: the run counts as running, its
    exec request stays `.running`, and it keeps its place in `plinth.runner.max-parallel-exec`.
  - A passed scan ends as `answer[.FAIL].dlp-ok.yml`, `status.dlp.result: ok` with the model, chunks and masks, and
    the event `dlp.scan` `ok`. A failed one (the model's replies didn't fit, asked twice: `dlp.ReplyError`; an error
    of an Ollama that took the request: HTTP 400, 500 and the other statuses not below, a reply that is too large;
    or no reply within `ollama.timeout-seconds` three times, see below) ends as
    `answer[.FAIL].DLP-FAIL.yml` with the fixed-pattern masks, `status.dlp.result: fail` and the reason, and the event
    `dlp.scan` `failed`. Both events: caller `runner`, with the original's sha256. The user checks a failed one, then
    `override dlp` and `done`, or `scrub`. `get ans`, `override dlp` and the TUI read `status.dlp`.
  - **A model that can't be asked fails nothing**, as with `finish`: Ollama not configured
    or unreachable (connection refused), the gateway not running or not answering, the model missing, HTTP 502, 503 or
    504 (Ollama loading a model, a proxy in front of it). Which it is, `ollama.KindOf` reads from the
    error, whose text is all that crosses the gateway. The answer stays `answer[.FAIL].new.yml`, ended and not
    scanned; the first such scan of the exec records the event `dlp.scan` `unavailable` (rule `dlp.unavailable`) with
    the reason, and the scan starts again a minute later, until the model can be asked. Until then the exec isn't
    finished: the run counts as running, as while a scan runs. So `DLP-FAIL` always means the scan ran and failed. An
    outage shows in the events and the logs (the metrics, and the TUI's Ollama status, will show it too).
    Such an exec holds back no detach or reload of its namespace (the fix may be a config
    change): its exec request stays `.running` and the answer `answer[.FAIL].new.yml`, as after a stop, and the next
    runner scans it (`leftoverExec`), with no second `unavailable` for the exec (it reads the last one from the event
    log). A scan that timed out ends the outage, in the running runner as in one that reads the event log: a model that
    can't be asked after it records `unavailable` again. A script still running, or a scan in flight, holds them
    back as before.
  - **No reply in time**: a scan whose model call gets no whole reply within
    `ollama.timeout-seconds` (the deadline covers reading the reply's body too: one that runs out there is a
    timeout, not retried by the client, while a connection that breaks mid-reply for another reason can't ask the
    model) is tried again a minute later, three attempts in all (`runner.ScanAttempts`). Each attempt
    that times out but the last records `dlp.scan` `timeout` (rule `dlp.timeout`, "attempt <n> of 3"); the third fails
    the scan as above, the reason `... no reply within <t> (ollama.timeout-seconds), 3 attempts`. The attempts are
    counted per exec from those events, so a restart doesn't begin them anew. Between them the exec waits as for a
    model that can't be asked.
  - **The run says what its answer waits for**: `get ans`, `describe` and the TUI read the exec's `dlp.scan`
    events (`runner.ScanWaiting`). An answer waiting for the model has the status `waiting-for-model`, and its line
    says `waiting for the model: unavailable since <time> (<why>)`, or `waiting for the model: <n> of 3 attempts timed
    out (<why>)`, with the next step (`get ans`: check `ollama status` and `ollama-mcp status`). The last of those
    events says which: an outage after a timeout says `unavailable since <time> (<why>); <n> of 3 attempts timed
    out before`, its time that of the first `unavailable` since the timeout.
  - **INVALID**: an answer that doesn't decode or check, or is over 16 MiB, isn't scanned: it is
    renamed as it is to `answer[.FAIL].INVALID.yml`, a bug, never data, kept for debugging, and the event `dlp.scan`
    `invalid` (rule `answer.invalid`) says why, with the original's sha256. It is never released (`state.valid`: "next:
    tell the agent, whose rerun is a new run"), reviewed, or shown by `get ans` or the TUI; the run is done.
  - A stop cuts the model step short and keeps nothing of it: the request stays `.running`, and the next start scans
    the answer again (`leftoverExec`), as it does an answer a stopped controller left unscanned.
  - An answer over 16 MiB isn't read: its original is kept in `raw/` and it is `INVALID`.
- **Shared by all namespaces:**
  - the HTTP listeners. A request reaches a namespace by its path (web UI) or by the agent's token (MCP); one token
    belongs to exactly one namespace.
  - the Ollama gateway, the one way to Ollama (see Ollama gateway): its client's semaphore (`ollama.max-parallel`,
    default 1) is shared by every namespace and by the plain commands. The controller starts it at its start while
    `ollama.host` is set, starts it again when it is gone, and leaves it running when it stops. One that can't start is
    said once on stderr and in the log; the controller goes on, and the reviews and DLP scans fail closed meanwhile.
- **Stop.** A stop (`SIGINT`, `SIGTERM`, or `controller stop --force`) stops every running script: SIGTERM, SIGKILL
  `killGrace` (30 s) later; the run counts as killed and the user is told. So `controller stop` refuses while a run's
  script runs, and names the runs.

## Documents (decided)

Built for plain AAA (2026-10-19; kind `Answer` end to end and the ask template as a
manifest, first plain AAA, then the controller's runs): package `doc` (the decoder, the kinds `Answer` and
`AskTemplate`, Plinth's encoder and the text filter), the AAA library (`ask lib`), `finish`, `read-answer` and
`dlp-scan` on documents, `ask lint` for plain AAA. Built for the controller's runs (2026-10-19, round B): the answer document end to end (the AAA library provided read-only as `$AAA_LIB`, `running.yml`, the scan of
the decoded document, `INVALID`, release, `get ans`, the TUI, the answer reviews), `ask lint --plinth` with the
controller's library line, and `<ns-dir>/ask-template.yml`; then no old format
read anywhere, an unscanned answer while the model can't be asked, and `ask lint --plinth` at exec. Not built yet: reviews as documents, the flow diagram and
the other kinds. Decided: answers are YAML with a strict schema, in Plinth and in plain AAA; an answer
that isn't valid YAML is a bug, never data; the script is easy to read, nothing in it but what the agent wrote; only
code reviews have a flow diagram, which plinthctl draws (ASCII, tooling-first), never a model; one YAML family for
every file, so plinthctl works like `kubectl` with manifests. The details below were proposed and accepted
(2026-10-14).

- **One family.** Every document starts with `apiVersion: plinth/v1`, `kind:` and `metadata:` (an answer's `run:`, a
  manifest's `name:`), so a tool can check any file without guessing from its name. Kinds and files:

  | Kind | File | Written by |
  |---|---|---|
  | `Answer` | `running.yml`, then `answer[.FAIL].<state>.yml` | the script, through the helpers; `status` by the controller (plain AAA: `finish`) |
  | `AskTemplate` | `<work>/ask-template.yml` (plain AAA's) and `<ns-dir>/ask-template.yml` (the controller's; both built) | the user (like a manifest submitted to a controller); `ask template` prints the bases |
  | `Review` | `review.<type>[.FAILED].yml`, `review.answer.<type>[.FAILED].yml` | the review tool, from the model's data |
  | `Override` | `override.review.yml`, `override.dlp.yml` | the controller |
  | `Acceptance` | `accept.network.yml` | the controller |
  | `Report` | `report.yml` | the controller |
  | `Rejection` | `rejected/<agent>/<id>/reason.yml` | the controller |

- **`status` is Plinth's.** In every kind, `status:` holds what the controller or a tool records (the DLP result, the
  event receipt, the model and hashes); a document that came from a script with a `status` is invalid.
- **One strict decoder** (yaml.v3): exactly one document; unknown keys, duplicate keys, anchors and aliases, and
  explicit tags refused; size capped as now (16 MiB). Every reader (controller, commands, TUI) uses it, so nothing reads a
  document another reader would refuse.
- **An answer:**

  ```yaml
  apiVersion: plinth/v1
  kind: Answer
  metadata:
    run: "keen-tapir-14"
    started: "2026-10-14T03:12:00Z"
  sections:
    - title: |2
        1. ceph status
      output: |2
        cluster:
          health: HEALTH_OK
  failure: |2               # after fail, or an exit status other than 0
    OSD 3 still down
  result: fail              # ok | fail; missing: the script didn't finish (killed)
  status:                   # Plinth's: finish in plain AAA, the controller (with exit: too) in its runs
    dlp:
      result: ok            # ok | fail
      model: "qwen3:8b"
      chunks: 1
      fixed: 0              # fixed-pattern masks
      masked: 2             # lines the model flagged
  ```

  - Every text from the script is a literal block (`|2`, an explicit indentation indicator), titles too: no quoting
    for a script to get wrong, and a mask never meets a quote. A text decodes ending in exactly one newline (an empty
    one stays empty; trailing empty lines go), and Plinth's encoder (`finish`, `dlp-scan`) writes every text the same
    way, so a document it writes again decodes to the same texts.
  - Append-only: `sections` only grow, `result` comes last, so a killed run still leaves valid YAML. The AAA library
    opens an untitled first section for anything said before the first `section`; `finish` drops a section with no
    title and no output.
  - **Invalid is a bug** (decided): after every run Plinth decodes the answer (`finish` in plain AAA, the controller
    in its runs). One that fails is never data: it gets the state `INVALID` (`answer[.FAIL].INVALID.yml`), isn't
    released, counted or reviewed, stays for debugging, and its event records why (the controller: `dlp.scan`
    `invalid`; plain AAA: `finish` says why).
  - **States** (built; the controller's are the same names): `running.yml` while it runs, then
    `answer[.FAIL].new.yml` (the script ended; `.FAIL` with `result: fail`), then, after `finish`,
    `answer[.FAIL].dlp-ok.yml`, `.DLP-FAIL.yml` or `.INVALID.yml` in `done/<RUN>/`; under the controller, after its
    scan, in the run directory, and `answer[.FAIL].released.yml` after Done (see Runs, States).
- **The script holds only the agent's lines** (decided: easy to read). A header (line 2 `# AAA script: <what>`,
  `READ-ONLY` or `CHANGES:`, `RUN_NAME`, `AGENT_NAME`, `INTERACTIVE`, and `NETWORK`/`PRIVILEGED`), one
  library line (plain AAA: `source <(plinthctl ask lib)`, see `ask lib`; under the controller `source "$AAA_LIB"`),
  then the steps:

  ```bash
  section "1. Forgejo version"
  curl -fsS https://forgejo.example.org/api/v1/version | block

  section "2. open issues"
  n="$(curl -fsS -H "Authorization: token $FORGEJO_TOKEN" "https://forgejo.example.org/api/v1/repos/o/r/issues?state=open" | grep -o '"number":' | wc -l)"
  say "open: $n"
  [[ "$n" -lt 50 ]] || fail "too many open issues"
  ```

  - The helpers (`section`, `say`, `block`, `fail`, `ask_yes`, `ssh_q`, and writing the document) are one library. Plain AAA gets it from the binary, `plinthctl ask lib` (built
    2026-10-19; it always matches the binary, and nothing on disk can drift or be edited);
    the controller provides it read-only to each run, `$AAA_LIB` naming it: the
    same library, printed by its binary in a controller variant (`plain.ControllerLib`) per run to
    `<ns-dir>/raw/<agent>/<RUN>/lib.bash`, which leaves the run's checks to the controller (its name, state and sum;
    the run directory is `/aaa/run` in the container), wants the empty `running.yml` the controller wrote, and ends
    with `<RUN> done (<result>) - next: the controller scans the answer; the user releases it (Done), then the agent
    reads it`. Its text filter is the controller's binary: mounted at `/aaa/plinthctl` in the container, by its path in
    a privileged run. `ssh_q` is in it too: a privileged run has ssh, and `ask lint --plinth` refuses ssh in any other.
    A script that removed `running.yml` ends without an answer: the `EXIT` trap writes none; nor does it, under the
    controller, for a run that HUP, INT or TERM ended (the controller's stop or timeout): the controller variant traps
    them, bash runs the handler once the command in hand has ended, and the run has no answer and may run again (a command that outlasts the grace gets SIGKILL, which leaves `running.yml` as it is too; plain AAA writes `result:
    fail`). The script ends without
    `finish`: the library's `EXIT` trap writes `result` (`ok`; `fail` after `fail` or an exit status other than 0)
    and renames `running.yml` to `answer[.FAIL].new.yml`.
  - Bash escapes nothing itself: `section`, `say`, `block` and the trap pass text through a filter in Go, the hidden
    command `plinthctl __aaa-text [-echo] -indent N <answer>` (built): it turns every byte YAML can't hold in a literal
    block (control characters but tab, CR among them, DEL, the C1 controls, NEL, LS, PS, a byte order mark, U+FFFE,
    U+FFFF, invalid UTF-8) into `\xNN`, indents each line and appends them to the answer in one write (at most 16 MiB
    per text, then a line `[cut: more than 16777216 bytes]`); `-echo` copies the text to the terminal as it is. A fuzz test sends hostile output through it (control characters, invalid UTF-8, YAML-looking lines,
    `---`, CRLF, very long lines, nothing) and decodes every result.
  - `ask lint` refuses a script that defines a function, writes to the answer any other way, or whose header isn't
    the fixed form: there is no boilerplate to hide anything in (built for plain AAA and, with the controller's
    library line, `--plinth`; see `ask lint`). `get ask` and the TUI show the mode lines, then the steps.
- **DLP scan on the decoded document**, not on lines: the controller (and `finish` and `dlp-scan` in plain AAA, built
  2026-10-19) decodes the answer, masks within each text value, and writes the document again with its own encoder,
  so the result is valid whatever was masked; the scan's result goes into `status.dlp`. The untouched original stays in `raw/` (plain AAA: `<aaa-raw-dir>/<RUN>/`) as now. The fixed patterns mask
  each text on its own (a private-key block shrinks to one line, so a text can't be cut back out of one joined
  text); the model step reads every text's lines in one series of chunks, numbered across the document, so a document
  costs no more model calls than one text of all its lines; a flagged line becomes `[line <n> masked by dlp-scan:
  <kind>]` in its own text, `<n>` its number there (package `dlp`, `ScanTexts`). A reply that doesn't fit, asked
  twice, fails the scan (DLP-FAIL); a model that can't be asked fails nothing: `finish` changes nothing and runs again,
  the controller leaves the answer unscanned and scans it again.
- **A review is data.** The model answers in a fixed shape (Ollama's structured output, a JSON schema); the review tool
  checks it and writes the document. (Built for the Markdown files, see Reviews: the same JSON reply, written as
  `review.<type>.md`.) `verdict:` is one of `ok-to-run`, `run-after-fixes`, `do-not-run`; anything
  else, or a reply that doesn't fit, makes `review.<type>.FAILED.yml`. That closes the gap of a Markdown verdict that
  fails open. `findings:` is a list (`line`, `severity`, `text`). `status:` records the model, the script's and the
  prompt's sha256 and the time.
- **The flow diagram** (code reviews only, decided): the `code` reviewer returns `flow:`, a list of steps:

  ```yaml
  flow:
    - step: check ceph health
    - step: drain osd.3
      gate: OSD did not drain
    - step: restart osd
      loop: each host
  ```

  Wherever a review is shown (the TUI, `describe`), plinthctl draws it in plain ASCII (decided): steps top to bottom,
  a gate leaving sideways to `FAIL: <reason>`, a loop marked on its step. Labels are sanitized, wide runes count two
  columns, and long ones are cut. A flow that doesn't fit this shape is shown as a numbered list. Golden-file tests.

  ```
  +---------------------+
  | 1 check ceph health |
  +---------------------+
             |
             v
  +---------------------+
  | 2 drain osd.3       |---- fail ---> FAIL: OSD did not drain
  +---------------------+
             |
             v
           Done
  ```

- **Commands:** `get` and `describe` print YAML with `-o yaml` as well as the table. `get ans` prints the answer
  document. The TUI shows documents as plain sanitized text; coloured YAML is for later.
- **The ask template is a manifest** (built 2026-10-19 for plain AAA): kind `AskTemplate`, one kind with
  two modes, `aaa` (plain AAA) and `plinth` (the controller's runs), so a template is a document like any other, as a
  manifest submitted to a controller is in Kubernetes:

  ```yaml
  apiVersion: plinth/v1
  kind: AskTemplate
  metadata:
    name: "base"
    namespace: "michael/devops"   # optional: the namespace it belongs to
  spec:
    mode: "aaa"                   # aaa | plinth
    script: |2
      #!/bin/bash
      ...
  ```

  `metadata.name` is a Kubernetes-style name (lower-case letters, digits and `-`, at most 63); `spec.script` starts
  with `#!`. The binary has a base of each mode (`ask template [--plinth]` prints them); the controller writes the
  `aaa` one into `<work>/ask-template.yml` when it is missing, and `new-ask` starts from that file, the user's to
  edit. The `plinth` one goes into `<ns-dir>/ask-template.yml` when a namespace is set up and it is missing (built
  2026-10-19): what `ask.template` gives an agent, its script's one fixed line `source "$AAA_LIB"; ...`.
- **Plain AAA:** built 2026-10-19 (see above); `read-answer` reads `status.dlp`.
- **No old formats**: Plinth reads no format from before the documents, and has no
  compatibility code for one; what exists in an old format is converted once, or scrubbed, by an ask script through
  Plinth itself.

## Run modes

Every run becomes an Ansible playbook; its section, Ansible runs, is not built, and the modes below hold
for ask scripts until then and for playbooks after.

Built so far: every run of the controller is a container (Process, Exec), leftover containers removed at
start; package `isolate` and the pipe lifeline are gone. Normal and network mode with `accept.network`, the header lines
and rules `ask.mode` and `exec.mode` are built, and so is the image as the review decided (published by the user, pinned
by digest, the controller runs only the pin: The image; 2026-10-16 and 17), and the smoke container per mode (Requirements;
2026-10-18), and privileged mode in the user's terminal (2026-10-18). The helper file belongs to the documents,
and the image's Ollama review to the reviews (The image). Plinth is a middle ground, not a sandbox, so each mode says
what it guards against and what stays with the vetting, and the user chooses when a script gets more.

All decided: the modes and their acceptance, no ssh in a run (2026-10-16; it replaces the earlier SSH decisions), and that every run of
the controller is a rootless podman container built from the repo's own `.plinth/Containerfile`, mounting only what the
run needs. The hand-made sandbox planned earlier (`mount_setattr`, our own seccomp filter, a minimal
`/dev`, credential covers, a probe per restriction) is superseded: the container gives all of it, and an allowlist of
mounts replaces the blocklist of hidden directories.

| Mode | Header | Runs | Network | Guards against | Left to the vetting |
|---|---|---|---|---|---|
| **normal** (default) | `NETWORK=0`, `PRIVILEGED=0` | the controller's child, in a container | none | acting as the user outside the run directory; reading anything not mounted | kernel and podman bugs |
| **network** | `NETWORK=1` | as normal | its own (pasta): the internet, not this machine's local services | as normal | what it does over the network, with what the env file holds |
| **privileged** | `PRIVILEGED=1` | in the user's terminal, never as the controller's child | the host's | nothing: as if the user ran it | everything |

- **Header:** exactly one `NETWORK=` and one `PRIVILEGED=` line, `0` or `1`, like `INTERACTIVE` (ask lint `--plinth`;
  plain AAA scripts have none, and a line there must still say `0` or `1`). `INTERACTIVE=1` needs `PRIVILEGED=1` (an
  interactive script already runs in the user's terminal, unsandboxed). The header is covered by `ask.sh.sha256`, so
  the mode can't change after vetting; the controller reads it once more from the checked bytes before the start. A run
  stored before the header lines existed is normal. `describe` and the TUI's Run screen show the mode; `get ask` and
  the reviewers show the script, header on top.
- **Config:** `plinth.exec.network` and `plinth.exec.privileged`, namespace keys with global defaults, both `false`; a
  submission asking for a mode its namespace doesn't allow is refused (rule `ask.mode`), and so are `exec` and
  `accept.network` of a network run once its namespace no longer allows it (rule `exec.mode`). `plinth.exec.env-file`
  and `plinth.exec.memory` (see the container). `plinth.exec.image` (`ref`, `commit`, `tree`; namespace only, written by `image publish` and
  `image use`) is the pin (The image). `plinth.exec.image-ref`, the build from the checkout, is retired (2026-10-17):
  a config that still has it loads, with a warning to remove the key.
- **The image** (decided 2026-10-14 in the review; the `image` commands, the pin and the
  controller's switch to it and the TUI's Image screen built 2026-10-16 and 17; the Ollama review not built yet). The
  namespace's repo defines it in `.plinth/Containerfile`, with the packages its scripts need, as a CI runner would. The
  user publishes it; the controller only runs it, by digest.
  - **Where the images live:** the namespace's image repo, `<host>/<owner>/plinth-<repo>` in Forgejo's container
    registry, lowercased. `<host>` is `forgejo-url`'s host (and port), so there is no config key for it (decided
    2026-10-15).
  - **Registry access is the user's `podman login`** (decided 2026-10-15). `image login` runs `podman login <host>` in
    the user's terminal, and every registry call after it uses that login, as if the user had typed the command:
    skopeo for all of them (list, inspect, push, pull, delete; skopeo shares podman's registry library and its login
    file), `podman` only to build, to check what this machine has and to remove its copies. Plinth passes no `--creds` and no `--authfile`,
    never reads the login file, and its token keeps "package: No access". Without a login a command says "not logged
    in to <host>: run `plinthctl image login -n <ns>`" (`podman login --get-login <host>`). The login lasts until the
    user's last session on the machine ends (podman keeps it in `$XDG_RUNTIME_DIR`).
  - **Tags:** each published image is tagged `<12-hex commit>-<UTC time, yyyymmddThhmmssZ>`, unique per publish, so a
    rebuild of the same commit doesn't untag the earlier image and every published image stays in the list. The tag is
    for people; the pin is the digest.
  - **Why not the agent's checkout.** Plinth runs no git in the agent's checkout: its `.git/` is the agent's to write,
    and git takes orders from it. A filter in `.git/config` or `.git/info/attributes` makes `git archive` run a
    command, `core.fsmonitor`, hooks and `core.sshCommand` do the same for other commands, and `git replace` makes git
    report the reviewed tree's hash for other content. A local `refs/remotes/origin/<branch>` is a file the agent can
    point at an unpushed commit, so Forgejo's branch protection never applies to it. (The controller's build from the
    checkout, `plinth.exec.image-ref`, went with this, 2026-10-17.)
  - **The stop-gap until the controller used the pin** (decided and built 2026-10-15; the controller's
    build went 2026-10-17, and the lockdown stays for every git call in `gitrepo`: `publish`'s fetched repo).
    The controller's build read `.plinth/` from the agent's checkout with `git archive`, so a filter the agent sets in that
    checkout's `.git/` ran a command as the user, on the host, outside any container (checked with git 2.47). Now
    `gitrepo.Files` lists the tree with `git ls-tree -r -z` and reads it with `git cat-file --batch`, which apply no
    filter, and Plinth writes the build context itself: regular files only (a symlink or a submodule is refused), each
    path inside the tree, at most 16 MiB. Every git call in `gitrepo` is locked down:
    - no setting from outside the repo: `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, and no inherited
      `GIT_*` variable (`GIT_DIR`, `GIT_SSH_COMMAND` and the like);
    - `GIT_NO_REPLACE_OBJECTS=1`;
    - no transport at all: `GIT_NO_LAZY_FETCH=1` and `-c protocol.allow=never`, since in a partial clone `cat-file` of
      a missing object starts a fetch, which runs the ssh command `.git/config` names (`GIT_NO_LAZY_FETCH` needs git
      2.44; on 2.43, the host's, `protocol.allow=never` alone stops it, and `TestFiles` passes);
    - `-c core.fsmonitor=false` and `-c core.hooksPath=/dev/null`.
    - no password prompt: `GIT_TERMINAL_PROMPT=0` stops only the terminal's, so `GIT_ASKPASS=` (empty) and no
      `SSH_ASKPASS` stop the desktop's dialog and a repo's `core.askPass`, and `-c credential.helper=` any helper
      (a 401 in `TestFetch` opened the desktop's dialog on the host before this).

    A test sets a filter, a textconv, an fsmonitor, an ssh command for a partial clone's lazy fetch, a replace object
    and `GIT_DIR` in a checkout, and proves no command runs and the committed bytes come back. That stops command
    execution on the host. It is still a denylist of what git is known to do, and git doesn't check that an object's
    bytes match its id, so the agent can still choose what gets built; that runs inside the container, and only the
    pin fixes it.
  - **Review on Forgejo.** A change of `.plinth/` reaches the repo's default branch only through a PR the user
    approves. Required approval alone doesn't do that: Forgejo checks approvals only when a PR is merged, so any
    account allowed to push still changes the branch directly, by `git push` or the contents API.
    Recommended, on the default branch's protection (README, `docs/runner-image.md`, which say why):
    - **Push:** disabled, or whitelisted to the user only.
    - **Protected file patterns:** `.plinth/**`, so a direct push touching them is refused even for an account allowed
      to push; only a merged PR changes them.
    - **Required approvals:** at least 1, with the **approvals whitelist** set to the user, so only their approval
      counts.
    - **Dismiss stale approvals:** a commit pushed to the PR after the approval needs a new one.

    An agent may write the Containerfile; it can't merge or publish it. This replaces the decided "an agent may change
    the file; git records that (no acceptance step)": the acceptance is the PR approval plus `publish`. Plinth's read
    token sees only part of the protection (`GET /repos/{owner}/{repo}/branches/{branch}`: `protected`,
    `required_approvals`); the whitelists and file patterns need admin access to read. `publish` and the TUI recommend
    protection while that part is off (below).
  - **`plinthctl image publish -n <owner>/<repo> [<commit>]`** (the user, in a terminal; built 2026-10-16, packages
    `image` and `forgejo`, but no Ollama review yet):
    - **First** it checks the user's login (`podman login --get-login <host>`), then takes `<ns-dir>/state/image.lock`
      (one publish per namespace at a time). `<commit>` is a full commit id, and must be on the branch.
    - **The test** (`TestPublish`) runs all of it against a stand-in for Forgejo on one local https server: the API, git
      http-backend, and the least of a registry that skopeo needs, behind Basic auth, so the test proves skopeo uses
      the user's login (`REGISTRY_AUTH_FILE` points podman and skopeo at a test login). skopeo verifies the server's
      certificate as it would Forgejo's, through `SSL_CERT_FILE`; skopeo 1.13 ignores `CONTAINERS_REGISTRIES_CONF`, so
      `insecure = true` there never reached it.
    - **Source.** It fetches the protected default branch (named by Forgejo's API), and only that branch, from Forgejo
      into a bare repo at `<ns-dir>/state/git/` (in `state/`, which is ignored), never from the agent's
      checkout (`gitrepo.Fetch`; built 2026-10-15). The repo is new for each publish, made without a template (no
      hooks); every git call is locked down as all of `gitrepo`'s are (the stop-gap, above), with https the one
      protocol allowed (`-c protocol.https.allow=always`) and `-c http.followRedirects=false`, so a redirect never
      takes the token anywhere. Plinth's token (repository read) reaches git through the environment
      (`GIT_CONFIG_COUNT`: `http.<url>.extraHeader`, Basic auth with the token, scoped to that URL), never argv or a
      config file; no message quotes the URL. Without `<commit>` it builds the branch's head; a commit that isn't on
      the branch (`gitrepo.OnBranch`) is refused. The build context is that commit's `.plinth/`, read with
      `gitrepo.Files`. `TestFetch` serves a repo over https with `git http-backend` and checks the header, a wrong
      token, a redirect (not followed: the server sees nothing more), a URL that isn't https and a branch name that
      isn't one.
    - **Branch protection, recommended**. It reads the branch's `protected` and
      `required_approvals` (Review on Forgejo). When the branch isn't protected or needs no approval, it says above the
      findings "Branch protection is recommended: see docs/runner-image.md", which explains why it is the safer
      workflow; the user still types `yes`. With protection on, it says nothing. `image list` reports the same, so
      the TUI shows the note only while protection is off.
    - **git, locked down,** as every git call in `gitrepo` (the stop-gap, above), so no setting from outside that
      repo applies; the fetch is the one call that allows a transport (`-c protocol.allow=never -c
      protocol.https.allow=always`). The bare repo (`<ns-dir>/state/git/`) has no working tree, hooks or fsmonitor,
      and the agent can't write it (like the namespace's `config.yml`).
    - **Checks by tools, which decide** (`container.Check`; built 2026-10-15). Refused, so nothing is built: a missing
      `Containerfile` or one without `FROM`; a `FROM`, `COPY
      --from` or `RUN --mount from=` that names neither an earlier stage (by name or number) nor an image pinned by
      digest (`@sha256:`), or names it through a variable; an `ADD` of anything but local files (a URL, a git repo,
      `--keep-git-dir`, a variable); a `RUN --network=` other than `none` or `default` (`host` would give the step
      this machine's local services, which runs never reach) and a `RUN --security=` other than `sandbox`
      (`insecure`); a heredoc (`RUN <<EOF`), which the check doesn't read, since a body it misread could hide an
      instruction or make up a stage name (a script goes in a file in `.plinth/` instead); `ONBUILD`, whose
      instruction runs in a later build `FROM` the image, where no check sees it, and which a runner image has no use
      for (devops review of a6e9ad6, 2026-10-18); a base image (each `FROM` image, in every stage) whose config has
      `OnBuild` triggers, which the build would run unchecked (podman keeps them from an image in
      Docker format): `skopeo inspect --config --raw` reads the config as stored (without `--raw`, skopeo converts it
      to OCI, which drops `OnBuild`), from this machine's copy when it has the image by its digest (what the build
      uses), else from the registry with the user's login, for the `FROM`'s `--platform`; a config that can't be read
      refuses the build too. The check reads the Containerfile as podman build does where it matters: the `escape`
      parser directive, continuation lines, comments inside them. **Download-and-run is refused too** (decision 2; built 2026-10-16), in every text file of the context, not only the Containerfile, since a script it
      `COPY`s and `RUN`s is built in too: a download run as it arrives (`curl ... | sh`, `| python`, `sh -c "$(curl
      ...)"` and the like), and any `curl` or `wget` used as a command (not as an argument: `apt-get install curl` is
      fine) whose line (a Containerfile instruction, or a line of a script, continuation lines joined) doesn't check
      what it got against a checksum written out there (`sha256sum -c`, `sha384sum`, `sha512sum`, `b2sum` or `shasum -a
      256|384|512`, with the digest's hex on that line, not inside a URL). The tool can't tell whether a downloaded file
      is run later, so every unpinned download counts; a commented-out one counts too, since a script's comments aren't
      parsed. The message names the allowed patterns: vendor the file into `.plinth/` (preferred), or `curl -fsSLo f
      <url> && echo '<sha256> f' | sha256sum -c && <use f>`. An `ansible-galaxy` call anywhere in `.plinth/` is flagged,
      not refused (Ansible runs, The image; 2026-10-18). Package managers (`apt-get`, `dnf`) aren't checked: they verify
      their own signatures. No DLP scan: `.plinth/` is reviewed source that goes to Forgejo's own registry, and a DLP
      scan is for what leaves a run, not for a build.
    - **Ollama review, which only advises:** it can add findings, never clear one the tools raised: the Containerfile
      is untrusted input to the model. When Ollama isn't available (not built yet, or not
      reachable) the findings say "Ollama review: not available" and the user can still publish.
    - **The user confirms:** it shows the diff of the whole `.plinth/` tree since the pinned image's commit (every
      changed file; on the first publish, with no pin, every file in full), and every finding, and builds only after
      the user types `yes`. This diff is the one review Plinth itself guarantees, whatever Forgejo's settings are.
      It is `git diff --no-ext-diff --no-textconv` in Plinth's bare repo (`gitrepo.Diff`), so no diff command the repo
      names runs; control characters show as `?`; it stops at 256 KiB and says the rest is to be read on Forgejo. A
      pinned commit that is no longer on the branch (a force-push) shows every file in full, and says why.
    - **Build:** `podman build --format oci` (never Docker format, whatever `BUILDAH_FORMAT` says: OCI has no
      `ONBUILD`, so the label build below runs nothing) with the labels `org.opencontainers.image.revision=<commit>`,
      `plinth.tree=<tree of .plinth/>`, `plinth.containerfile.sha256=<sum>` and `plinth.image=<owner>/<repo>`; then
      `plinth.ansible-core` (Ansible runs, The image), added by a second build that only labels; the first build is
      removed.
    - **Push:** to the image repo with its tag (`skopeo copy --digestfile containers-storage:<image ID>
      docker://<repo>:<tag>`), with the user's login.
    - **Pin:** it writes `plinth.exec.image` (`ref: <name>@sha256:<digest>`, `commit:`, `tree:`) into the namespace's
      `config.yml`, editing it by key like `agent enable`, and logs the publish (commit, tree, tag, digest) in the
      general log.
    - **Pull back:** it pulls the pushed image by its digest (as `image pull`), so this machine has exactly the pinned
      bits: a push can re-encode layers, so the local build isn't the pushed digest. When the pulled image is another
      one than the build, the build (untagged by then) is removed.
    - **Clean up:** after the push and the pin, and on any failure or abort, it deletes `<ns-dir>/state/git/` and the
      build context; a retry fetches again, and a `git/` a crash left is deleted at the next `publish`. Building is
      rare, and a lasting copy would keep history that a force-push removed from Forgejo (a secret, say), so there is
      none.
  - **`plinthctl image use -n <ns> <tag>|sha256:<digest>|<name>@sha256:<digest>`** pins an image from the namespace's
    image repo, or one published elsewhere, after checking its labels with `skopeo inspect` (`plinth.image` names the
    namespace; `revision` and `plinth.tree` are set). A tag is resolved to its digest once, and the pin is that digest.
    It shows the commit, the tree and the drift from the current pin, and the user types `yes`. It logs the change in the
    general log. Built 2026-10-17: "the drift" is whether the image was built from the same `.plinth/` tree as the
    pin; the pin's commit and tree come from the image's labels; an image that is the pin already changes nothing;
    one this machine doesn't have is pinned all the same, with the `image pull` to run.
  - **`plinthctl image list -n <ns>`:** `skopeo list-tags`, then `skopeo inspect` per tag (digest, labels, created),
    newest first, marking the pinned image and whether this machine has each one (`podman image exists`). An image
    whose labels don't name this namespace is listed as "not a Plinth image of <ns>" and can't be used. It also reports the
    default branch's protection (`protected`, `required_approvals`, through Plinth's read token), for the TUI's note.
    Built 2026-10-17, with `-o yaml` for the TUI (`repo`, `pin`, `protection`, `images`: tag, digest, commit, tree,
    created, `plinth`, `pinned`, `local`); a tag that isn't a tag name is left out, since the text is the registry's.
  - **`plinthctl image pull -n <ns> [<tag>|sha256:<digest>]`** (default: the pin): `skopeo copy docker://<repo>@<digest>
    containers-storage:<repo>:<tag>`, always by digest, so a moved tag pulls nothing else. Afterwards `podman image
    exists <repo>@sha256:<digest>` must find it by that manifest digest, which is what the controller looks for (the
    slice's host test proves it, with `--pull=never` by digest). The controller checks the digest and labels before each run.
    Built 2026-10-16: a tag is resolved to its digest first (`skopeo inspect --format {{.Digest}}`); without a tag the
    local name is `<repo>@<digest>`; the pin is pulled from its own repo, which may be another than the namespace's
    (`image use` of an image published elsewhere). It writes `image: pulled <name>@<digest> for <ns>` to the general log.
  - **`plinthctl image delete -n <ns> --local|--registry <tag>|sha256:<digest>`:** `--local` removes this machine's copy
    (`podman rmi`). `--registry` deletes the image from the registry (`skopeo delete`), which
    can't be undone, so it names the image and warns that another machine may pin it (its next `image pull` then
    fails, and only this machine's pin is checked); the user types `yes`, and it logs the delete (tag, digest) in the
    general log. Neither works on the pinned image ("pinned: use another image first"). Built 2026-10-17: `--local`
    refuses a name that is the same local image as the pin (podman's image ID), whatever name it uses.
  - **`plinthctl` does every image action; the controller only observes** (the kubectl shape).
    `publish`, `use`, `pull` and `delete` work whether or not the controller runs, and write no event: only the controller
    appends to the event log (it keeps the chain's last hash), so the commands write their lines to the general log
    instead (`image: published <tag> <digest> for <ns>`), as `agent enable` edits `config.yml` without an event. What ran
    stays traceable without them: every controller event carries the config's sha256, so a pin change shows in the next
    one; `exec.start` records the digest and commit of the image a run used; the pin holds commit, tree and digest,
    and the image's labels repeat them. The controller's own `image.drift` stays.
  - **The pin is the only image a namespace runs:** a digest, never a tag; only `publish` and `use` change it.
  - **The controller builds nothing, pulls nothing, runs no git, and calls no skopeo** (built 2026-10-17: `startExec`,
    `image.CheckLocal`). Before each run:
    - Without a pin the run fails before the start with "no image pinned: plinthctl image publish -n <ns>".
    - `podman image exists <pin>`; without it the run fails before the start with "image not pulled: run `plinthctl
      image pull -n <ns>`", which the TUI shows too (the exec request's failure). Pulling is the user's, with their
      own login.
    - The image's labels must match the pin: `plinth.image` the namespace, `revision` and `plinth.tree` the pin's
      commit and tree. A mismatch fails the run ("image <ref> doesn't match the pin: its label <l> is ..., the pin says
      ...").
    - It runs `--pull=never` by that digest, and `exec.start` records the digest and the commit: `...; image
      <name>@sha256:<digest>; commit <commit>; no drift|drift: ...|drift not checked; run directory: ...`.
    - **Drift check:** it asks Forgejo's API (repository read, no git) for the default branch (`GET
      /repos/{owner}/{repo}`), its head (`.../branches/{branch}`) and the tree of its `.plinth/` (`.../git/trees/{commit}`:
      the root tree's `.plinth` entry; none counts as a drift too), within 5 s. When that differs from the pin's tree, it
      warns in the event log (`image.drift`, target the namespace, outcome `drift`: `<branch> at <commit> has .plinth
      tree <tree>, the pin has <tree>`) and on the controller's stderr, once per pair of trees while the namespace's
      config is unchanged; the TUI's Image screen shows the last run's check (`plinth-tui.README.md`). It only
      warns, never blocks: the pinned image is the one the user vetted, and republishing is the user's decision. When
      Forgejo can't be reached, the run goes ahead, `exec.start` says "drift not checked" and the general log says why.
    - **Tests:** the controller's tests build an image on the test image with a published image's labels and pin it by its
      local digest (`pinned`; `TestMain` removes them); `TestExecPin` fails a run for no pin, a pin not on this
      machine and labels that don't match; `TestExecDrift` runs against package `imagetest's stand-in for Forgejo, with
      and without a drift; `globalCfg` points `forgejo-url` at a port nothing listens on, so the other tests say "drift
      not checked".
  - **A chain of hashes from review to run:** the reviewed commit on Forgejo → its `.plinth/` tree (a git hash) → the
    labels in the image's config → the image digest (which covers the config) → the pin in the user's `config.yml` →
    the controller runs only that digest. Every link but the build is a hash, and the build is the user's, by a static
    tool, from a fresh fetch. Anyone can check it: `podman image inspect <pin>` gives the commit and tree, and Forgejo
    (or any fresh clone) must give the same tree for `<commit>:.plinth`.
  - **The same image on every host:** a digest names the same bits everywhere. Building the same Containerfile again
    isn't bit-identical (package installs change), which is why the pin is the digest, not the commit.
  - Later, optional: signing (`podman push --sign-by` and a `policy.json`); the digest covers integrity, a signature
    adds who published it.
  - The user's guide, with the reasons: `docs/runner-image.md`.
- **The container**: `podman create --init --read-only`, then `podman start --attach` (no `--rm`: see the
  review's fixes) with a private `/tmp`,
  `--cap-drop=all`, `--security-opt no-new-privileges`, podman's default seccomp, `--userns=keep-id` (files in the run
  directory stay the user's), `--timeout` (`plinth.runner.exec-timeout-minutes`), `--label` with
  `<owner>/<repo>/<agent>/<RUN>`, `--entrypoint=bash` with `/aaa/run/ask.sh` (an image's own `ENTRYPOINT` would get the
  script as its arguments; 2026-10-18), stdin, stdout and stderr on `/dev/null`. It mounts only:
  - the run directory, read-write, as the working directory;
  - `ask.sh` (the checked bytes) and the helper file (Documents), read-only;
  - `$HOME` is an empty tmpfs, in every mode: nothing of the user's `~/.ssh` (No ssh, below);
  - the variables: `--env-file` with `plinth.exec.env-file`, a file in the namespace's directory in `<work>` (the
    user's, never the repo: its values are often secrets), holding exactly what the namespace's scripts need, plus
    `AAA_QUIET=1` and `AAA_LIB`. Nothing else of the user's environment.
  - **Fixes from the review** (the first three built 2026-10-15):
    - **Telling podman's failure from the script's exit** (built). `Failure()` read the client's exit code, which is
      the script's own whenever the container ran: a script whose last command isn't found (127) or isn't executable
      (126) was reported as "the container may not have run", and one that ended with `exit 125` put up to 4 KiB of
      its own stderr, never DLP-scanned, into `exec.end`. Now the container runs without `--rm`; once the client
      ends, `podman inspect` (`State.ExitCode`, `State.OOMKilled`, `State.Error`, `State.StartedAt`) says whether
      the script started and how it ended, then `podman rm` removes the container. podman's message goes into
      `exec.end` only for a container that exists and never started; with no container at all, only inspect's error
      and the client's exit code do, since its stderr may be the script's. So nothing the script
      wrote reaches the event log.
    - **podman's own reason when it can't make the container**: `podman run`
      became `podman create`, then `podman start --attach`. A create that fails ran no script, so its stderr is
      podman's alone and goes into `exec.end` (`the container wasn't created: <its message>`, e.g. `Error: ... image not
      known`); before, a missing image left only inspect's "no such container". A create podman refused removes nothing,
      since it made nothing: a name in use stays its user's. A create cut short after 2 minutes, which may have made the
      container, and a client that doesn't start after the create remove the run's container, whose config holds the
      env file's values; when that removal fails, `exec.end` and the controller log say so (`its
      container <name> couldn't be removed and stays until the controller restarts: ...`). The
      exit-125 wording became `podman exit 125, and no container left to inspect:`, since "the container didn't
      run" was untrue for a script's own `exit 125`.
    - **No comma in a mount path** (built): a comma in a `--mount` path would add options (`source=<p>`), a colon in
      the `--tmpfs` path the tmpfs's; a run whose `Dir`, `Ask`, `Lib`, `Home` or env file has a comma, or whose `Home`
      has a colon, is refused.
    - **A memory limit** (built): `plinth.exec.memory` (namespace key, global default `512m`: Plinth runs small ad-hoc
      scripts, not applications), `<n>k`, `<n>m` or `<n>g` from `6m` to `64g`, given as both `--memory` and
      `--memory-swap`, so no swap on top. A tmpfs the script writes to (`/tmp`, `$HOME`) counts against it. The
      user's guide: `docs/README.md`, "A run's memory limit".
    - The review asked to mount only what a network run needs of `~/.ssh`; a run now gets none of
      it (No ssh, below).
    - A smoke test that a network run (pasta) can't reach this machine's own addresses: the smoke container
      checks the loopback services at each first run (Requirements, below); the test TestNetworkHostAddresses measures
      which of this machine's other addresses a network run reaches, and logs it (built 2026-10-18).
    - Later, maybe: a container that ended while the controller was down is still there at the next
      start (no `--rm`); reading it (without `podman wait`, which would block on one still running) before removing it
      would finish that exec with its real exit code instead of "the controller stopped while the script ran". One still
      running is removed, as now.
    - Open: a lifeline into the container (`--preserve-fds`), so a script ends with a controller that dies, not at its
      `--timeout`. It needs a process in the container that reads the pipe (the `--init` process doesn't).
- **Normal mode:** `--network none`. Nothing to reach but the run directory.
- **Network mode:** `--network pasta`: its own network namespace with the internet, without this machine's loopback
  services. It is for scripts that call HTTP APIs, with any token they need in the env file. This machine stays
  reachable at its other addresses (TestNetworkHostAddresses logs which), but not at its loopback services: the
  smoke container checks that before the first network run of each image (Requirements). Measured (round
  lucky-thistle-19, 2026-10-18): a network run didn't reach this machine at its one LAN address, since pasta gives the
  container that address, so a connection to it stays inside; a machine with more addresses may be reachable at
  the others.
  - **Acceptance:** `exec` of a network run is held until `accept.network` (rule `exec.mode`, `require-approval`), a
    user-only request in two steps like the overrides, from the CLI (`plinthctl accept network`) and the TUI (`w`)
    only. A box says what the run can reach (the network; no ssh; the env file's variables, which the run can send
    anywhere) and the user types `yes`. The controller writes `accept.network.md` (the time, what the run can reach,
    the event's receipt); it becomes `accept.network.yml`, kind `Acceptance`, with the documents. Only before the
    run ran.
  - Without pasta (package `passt`) at the controller's start, network runs fail with that reason; normal ones run.
- **No ssh** (decided, 2026-10-16): no mode that runs in a container gets ssh. Nothing of the user's
  `~/.ssh` is mounted (no key, no `known_hosts`, no config), and no socket of the user's (no agent). Remote work over
  ssh, reading or writing, belongs to Ansible and IaC in general, run by the user natively; a script may help write a
  playbook as a reviewed change, never run it. A script that needs the user's own ssh is a privileged script. This
  replaces the earlier `~/.ssh` mount and the one-key design (`plinth.exec.ssh-key`, a
  generated ssh config, the self-access check; built in c686a91, removed again), whose gaps depended on the image
. Decided: `ask lint --plinth` refuses ssh in a script that isn't
  privileged (`ssh`, `scp`, `sftp`, `rsync` over ssh, `GIT_SSH*`, ssh remotes; the rules: `ask lint` above), so a key
  in the env file can't bring ssh back unchecked; and git in a run goes over https, with a token from the env file. Git
  over ssh in a run may come later, as its own issue.
- **A run and its controller:** a container outlives a controller that dies, until its `--timeout`. At start the controller removes
  every container labelled with its `<work>` (`podman rm -f`; another `<work>`'s stay) before it finishes their exec
  requests as now (leftovers: no answer, state files the script wrote moved out, the answer scanned). This replaces the
  lifeline. Built.
- **Privileged mode** (built 2026-10-18): never the controller's child. Once playbook runs exist, a privileged run is a
  playbook run natively in the user's terminal, not a bash script (decided; Ansible runs); what
  follows holds for both.
  - **Only from the user's terminal:** `plinthctl exec` on a privileged run (every `INTERACTIVE=1` one is), or `e` on
    it in the TUI, which runs that command with the terminal handed over (as the Image screen does). Its `exec`
    request, queued as for any run, is refused (rule `exec.mode`).
  - **The steps:** `plinthctl exec` asks decide as from the terminal (`request.DecideTerminal`: the fact `Terminal`
    lets a privileged or interactive run through `exec.mode` and `state.valid`; a run that isn't privileged doesn't
    run there), needs the controller running, shows what the script can do (the box below), and the user types
    `privileged`. Then it takes the run's lock, `<ns-dir>/state/terminal/<agent>/<RUN>.lock` (an exclusive `flock`;
    a second terminal is refused), and asks the controller over its socket.
  - **The socket:** `<work>/state/plinth.sock` (Lock and socket). One JSON line asks `exec` of a run; the main loop
    passes it to the namespace's loop, which owns the run's state. That loop refuses unless the terminal holds the
    run's lock, asks decide (the event log records it, caller user, "from the user's terminal"), needs the agent
    enabled, checks `ask.sh` against its sum once more, records `exec.start` (`ask.sh sha256 <sum>; mode privileged;
    requested over the socket from the user's terminal (plinthctl exec asks the user to type yes); run
    directory: ...`), writes `running.yml` and the exec request as `.running`, writes the run's AAA library to
    `<ns-dir>/raw/<agent>/<RUN>/lib.bash` (its text filter the controller's binary by its path here), and answers
    with the run directory, its `ask.sh`, that sum and the library, which `plinthctl exec` gives the script as
    `$AAA_LIB` (the user's own `AAA_LIB` and `AAA_LOADED` never reach it). Between that write and the script's start
    nothing but the controller and the user's own processes can write the library: no agent and no run of the
    controller's reaches it, and a process of the user's is, under the Threat model, the user (a privileged script
    running at the same time is one). `exec.start` says only what the controller knows: the typed
    `privileged` is `plinthctl exec`'s, which the controller can't see, and any process of the user's that holds the
    run's lock can ask over the socket (a privileged script, for one), which under the Threat model is the user.
  - **A start no one waits for never happens**: the connection waits a minute for the namespace's loop, then
    tells the terminal `the controller didn't answer within a minute; nothing started`. One token decides between the
    two: the loop takes it when it takes the request up, the connection when it stops waiting. A loop that comes to the
    request after that drops it untouched (logged): no `exec.start`, no `running.yml`, the run as it was. A connection
    that loses the token waits for the loop's answer, however late.
  - **The run:** `bash ask.sh` in the run directory (the ask template finds its run directory from its own path, so
    the run directory's own copy runs, checked by the terminal against the controller's sum first), as the user, with the
    user's environment less Plinth's variables and secrets and less `AAA_QUIET`, stdin, stdout and stderr the
    terminal's. The script gets the run's lock on fd 9 (not 3 or 4, which the template takes): it is held as long as
    the script, or a process it left, runs. Ctrl-C and Ctrl-\ are the script's.
  - **The end:** the terminal sends how the script ended (`exit 0`, `exit 3`, `signal 2 (interrupt)`) on the same
    connection; the controller finishes the exec as for its own child: the state files the script wrote itself moved out,
    `exec.end` (`in the user's terminal: exit 0`), the DLP scan, the request `.finished`. A terminal gone without
    saying leaves the run running while its lock is held; once it is free, the exec ends with "the terminal's
    plinthctl ended without saying how the script ended". A controller that stops meanwhile leaves the exec `.running`;
    the next one finishes it (as a left-over exec, Exec) only once the lock is free, and logs once that it waits.
  - **The box** says the script runs as the user with nothing in the way: it can read and change any of the user's
    files, keys and passwords included; use the network and the SSH agent; and change Plinth itself (`config.yml`,
    turning on network or privileged mode for later runs, the event log, its own `done`). Every event records the
    config's sha256, so a change shows in the next one.
  - The script's own output reaches the terminal raw; the answer stays sanitized and is scanned.
- **Events:** `exec.start` records the mode, the pinned image's digest and commit, and the drift check (`ask.sh sha256
  <sum>; mode network; image <name>@sha256:<digest>; commit <commit>; no drift; run directory: ...`); `image.drift`
  (the controller) replaces `image.build`; the `image` commands write the general log, not the event log (The image) (built 2026-10-17).
- **Requirements:** rootless podman (with `/etc/subuid` and `/etc/subgid` for the user) and, for network mode, pasta
  (package `passt`); for the `image` commands only, skopeo (package `skopeo`; the controller never calls it). Plinth calls
  the `podman` and `skopeo` commands (`os/exec`), no Go dependency. At start the controller checks podman and pasta
  and runs one smoke container per mode it may use; a mode whose check fails takes no `exec` (fail closed, logged).
  Built: the check that podman answers (No podman, no run) and that pasta is there (Network mode), at start; and the
  smoke container (built 2026-10-18), before the first run of each image and mode, since a container needs an image
  and the pinned image is what the runs use. Its result holds for the controller's life. It is a container made with
  exactly the run's flags (`container.Smoke`), with the controller's own binary mounted read-only at `/aaa/probe` and run
  in place of `bash ask.sh` (`plinthctl __container-probe`, not in the help; a static binary, so it needs nothing of
  the image). From inside it checks: the user's uid and gid; no capability (`CapEff`, `CapPrm`, `CapBnd` zero);
  `NoNewPrivs` 1 and a seccomp filter (`Seccomp` 2); a read-only root; an empty `$HOME`; `<work>` not visible; normal
  mode loopback only, network mode an interface besides it. In network mode the controller also listens on 127.0.0.1 and
  the probe dials the container's default gateway and whatever `/etc/hosts` names `host.containers.internal` or
  `host.docker.internal`, at that port: a connection that arrives fails the check (this machine's loopback services
  must stay out of reach). A failed check refuses the exec with its reason, in the log and once on stderr. The tests
  that run scripts skip without podman and the test image, unless `PLINTH_TEST_PODMAN=required`, which makes that a
  failure: the host test round sets it; the build's own `go test`, in a container without podman, skips them.
- **What went**: package `isolate` and the pipe lifeline. What stays: the exec request states,
  leftovers, `running.yml` written by the controller, the event log, the DLP scan, the documents.
- **Order:** normal and network mode with `accept.network` ship together (normal mode alone breaks every script
  that needs the network). The stop-gap came first, then the image slices (the controller runs only the pin since
  2026-10-17, with the TUI's Image screen), then privileged mode with the socket. Later, maybe: an interactive
  normal run (the container with the terminal attached), so `INTERACTIVE` no longer needs `PRIVILEGED`; macOS (podman
  runs containers in its own Linux VM there; the controller and TUI need their Linux-only parts ported).

## Ansible runs (not built)

Decided: every run
becomes an Ansible playbook, for local work, remote hosts and APIs alike. Plinth checks the playbook, owns Ansible's
config and inventory, runs `--check --diff` before any apply, never signs anything itself, and turns the JSON callback
into the YAML answer. Nothing in this section is built. Bash ask scripts stay as they are until AAA itself moves to
playbooks (later); until then the two run types live side by side, and everything above about ask
scripts holds for them. The few details still marked **(proposal)** are still to be confirmed; the rest is
decided.

- **Why** (shift-to-tool): a playbook is a list of documented modules with documented arguments, so the tool
  checks, the reviewers and the risk score (Risk score and policy) work on what a run will do, not on arbitrary bash.
  Plinth's own analysis stays shallow: it reads module names and resolved values and scores them; what a module does
  comes from Ansible's documentation.
- **A playbook run** is one playbook, run by `ansible-playbook` in the run's container (Process, Exec: the same
  container, pinned image, mounts and limits), against `localhost` (connection `local`, inside the container) or hosts
  of the namespace's inventory (connection `ssh`, with a per-run certificate, below). API work (`uri`,
  `kubernetes.core.k8s`) runs from the container, in network mode.
- **Where a playbook comes from** is a risk factor:
  - **ad hoc** (decided): the agent submits it as it submits an ask script today: `ask.submit` with
    `kind: playbook`, one YAML file of at most 1000 lines, stored as `<ns-dir>/runs/<agent>/<RUN>/playbook.yml` with
    `playbook.yml.sha256`, in place of `ask.sh`; no includes or roles of its own, only those of the image's
    collections;
  - **committed** (a later slice; **(proposal)** for how): the agent names a playbook in the namespace's repo and a
    commit on the protected default branch; the controller fetches that commit from Forgejo as `image publish` does
    (locked-down git, never the agent's checkout, The image), checks the playbook with its includes and roles, and
    records the commit.
- **A playbook's header** (decided): its first lines are YAML comments of a fixed form, like an ask
  script's header: `# plinth: <what>`, `# RUN_NAME: <RUN>`, `# AGENT_NAME: <repo>/<agent>`, `# NETWORK: 0|1`,
  `# PRIVILEGED: 0|1`, `# INTERACTIVE: 0|1`, `# VAULT: 0|1`. They are covered by the sha256, so they can't change
  after vetting. `# INTERACTIVE: 1` needs `# PRIVILEGED: 1`, as `INTERACTIVE=1` needs `PRIVILEGED=1` in an ask script
  (below), and `# VAULT: 1` needs `# INTERACTIVE: 1` (Vaulted variables, below). Where a playbook
  runs comes from the plays' `hosts:`, not from the header.
- **Only a privileged playbook may be interactive, and only through Ansible's own prompts** (decided
  2026-10-18; the earlier rule that no bash runs and the `become` exception were amended to match):
  - **Normal and network playbooks never prompt:** they run in the controller's container, where nobody can answer, and
    are the runs that move to Kubernetes later. Ansible's prompts are never passed for them, and `become` stays
    refused (phase 1) or scored.
  - **A playbook with `# PRIVILEGED: 1` and `# INTERACTIVE: 1`** runs in the user's own terminal (below) and may use
    Ansible's own prompts: `--ask-become-pass` for `become` with `sudo` on this machine, and `--ask-vault-password` for
    the vaulted variables. **Plinth passes them, never the agent** (decided): Plinth builds the
    `ansible-playbook` command line from what the playbook check found, `--ask-become-pass` when the playbook uses
    `become` and `--ask-vault-password` when its header says `# VAULT: 1` (Vaulted variables, below); the agent never
    controls the command line. With `# INTERACTIVE: 0` either is refused, as is `become`. A `!vault` value in the
    playbook itself is refused everywhere: the check and the reviewers couldn't read it. Ansible reads the passwords
    from the terminal (`/dev/tty`), with echo off; they never reach stdout, so the answer (the JSON on stdout) and its
    DLP scan see none of them.
  - **`vars_prompt` and a `pause` that prompts stay refused everywhere,** privileged playbooks included. Their text
    is the agent's: a playbook could ask the user for anything, a password included, and read the answer itself.
    Ansible's own prompts have fixed text and Ansible itself takes the password, for `sudo` or the vault (the AAA
    reviewer rule: a tool prompting for its own password is intended, a script reading one itself is a finding).
  - An interactive playbook is terminal-only for good: it never moves to Kubernetes. A `pause` without a prompt
    (`seconds:` or `minutes:`) is no prompt and is allowed.
- **Privileged work is a playbook too** (decided: no bash runs): a
  playbook with `# PRIVILEGED: 1` runs natively in the user's terminal, outside the container, the way a privileged
  ask script runs (Run modes, Privileged mode): through `plinthctl exec` or the TUI, never as the
  controller's child, and only where `plinth.exec.privileged` allows it. Once playbook runs exist there are no privileged
  bash ask scripts. **Plinth sets the local connection, never the playbook:** it runs `ansible-playbook
  --connection=local` with an inventory of `localhost` alone, and the playbook's plays name `hosts: localhost`. A
  `connection:` keyword or connection variable in the playbook stays a connection setting, refused like any other
  (phase 1) and scored later, privileged playbooks included.
- **The image** (the CVE report; decided 2026-10-18):
  - `ansible-core` from the distro package (never pip) in `.plinth/Containerfile`, on **Debian trixie** (2.19.11 when
    decided) or later, the base the docs recommend.
  - **No `ansible-galaxy`:** its install paths had three High CVEs in 2026, one (CVE-2026-89091, crafted collection
    tarballs) unfixed in Debian when decided. Plinth's way is collections vendored in `.plinth/collections/` as
    extracted trees, reviewed in the PR like any file, and copied into the image (`COPY collections/
    /usr/share/ansible/collections/`), so no ansible-galaxy code runs, at build time or run time. What the user puts in
    their image is their call: `image publish` flags an `ansible-galaxy` call in `.plinth/` (a finding that doesn't
    stop the build) and names the CVE. Built 2026-10-18 (`container.Check`).
  - **The version is recorded, never judged:** after the build, `image publish` runs `ansible --version` in the image
    (no network, a read-only root, no capability, `--entrypoint=ansible`, so the image's own `ENTRYPOINT` doesn't
    take `--version` as its arguments) and labels it `plinth.ansible-core=<version>`, or `none`, with a second build
    (also `--format oci`) of `FROM <image>` and that `LABEL` alone. It shows the version and logs it. There is no
    floor: a distro build carries fixes its version number doesn't show. Built 2026-10-18.
  - What isn't in the image can't be used: the playbook check refuses a module whose collection the pinned image
    doesn't have.
- **Plinth owns Ansible's config**:
  - `ANSIBLE_CONFIG=/aaa/ansible.cfg`, a file Plinth writes and mounts read-only. `ANSIBLE_CONFIG` comes first in
    Ansible's search order, so the run directory's `ansible.cfg` (the agent's to write) and `~/.ansible.cfg` are never
    read.
  - No inherited `ANSIBLE_*` variable: the container's environment is only the env file and Plinth's own (The
    container), and **(proposal)** an env file that sets an `ANSIBLE_*` variable is a problem (`config check`; the
    run fails before the start).
  - Its settings **(proposal for the list)**: `host_key_checking = True`; `inventory = /aaa/inventory/inventory.yml`;
    `stdout_callback = json` (`ANSIBLE_STDOUT_CALLBACK=json`) and no other callback; `retry_files_enabled = False`;
    `collections_path` only the image's; and for ssh `-F /dev/null` (no ssh config),
    `UserKnownHostsFile=/aaa/known_hosts`, `GlobalKnownHostsFile=/dev/null`, `StrictHostKeyChecking=yes`,
    `IdentitiesOnly=yes`, `IdentityAgent=none` and the run's key and certificate (below). No `ControlPersist`
    (decided): Ansible opens a connection per task, so OpenSSH checks the certificate at each login.
- **The inventory** (decided) is the user's, per namespace, never the run's:
  `<ns-dir>/ansible/inventory.yml` and `<ns-dir>/ansible/known_hosts`, mounted read-only, the inventory alone in a
  directory of its own (`/aaa/inventory/`), so no `host_vars/` or `group_vars/` sits beside it.
  - **The playbook is mounted the same way**: read-only, alone in `/aaa/playbook/`, never run
    from the run directory. Ansible loads `host_vars/` and `group_vars/` from beside the playbook and the inventory when
    it needs the variables, after any check, so a first `localhost` play that wrote them into a writable directory
    would change the plays after it. This way the check sees every variable that applies.
  - Managed in the TUI (its Ansible screen, `plinth-tui.README.md`), or by hand; Plinth loads and checks it: a YAML
    inventory only (no script or plugin inventory), and no variable that would loosen the pinned settings
    (`ansible_ssh_private_key_file`, `ansible_ssh_common_args`, `ansible_ssh_extra_args`, `ansible_connection` other
    than `ssh` or `local`, and the like).
  - **Host keys come from the Plinth host CA** (below): `known_hosts` holds one `@cert-authority <host pattern> <host
    CA public key>` line, so a host whose key the host CA signed needs no keyscan. For a host without a host
    certificate, the TUI fetches its keys (`ssh-keyscan`), shows the fingerprints, and adds them once the user types
    `trust`.
  - A play's `hosts:` names hosts or groups of the inventory, or `localhost`.
- **Vaulted variables, in interactive privileged playbooks only** (decided): the user's, per
  namespace, never the run's, like the inventory.
  - **The file:** `<ns-dir>/ansible/vault.yml`, a YAML map of variables whose values are each vault-encrypted (`!vault`,
    `ansible-vault encrypt_string`), not a file encrypted whole. So Plinth reads the names without decrypting anything
    and checks them like the inventory's: no variable that would loosen the pinned settings (Plinth owns Ansible's
    config). `ansible_become_password` is allowed: holding a sudo password is what the file is for.
  - **Which runs get it:** only a playbook whose header says `# VAULT: 1`, which needs `# INTERACTIVE: 1` (and so
    `# PRIVILEGED: 1`). Plinth checks the names again right before the run and passes the file itself
    (`--extra-vars @<ns-dir>/ansible/vault.yml`); the playbook names no vars file for it.
  - **The vault password is always typed** (`--ask-vault-password`, Plinth's to pass), so the file is protected at
    rest, and `become` with the vault's sudo password still needs the user at the terminal. There is no vault
    password file and no vault for container runs (decided): a password file there would be no
    safer than the env file, which already carries container runs' secrets.
  - **The vault hides nothing from the playbook:** Ansible decrypts the values inside the process that runs the plays,
    so a task can print one (`no_log` is no boundary). A `# VAULT: 1` run holds secrets: the Secrets risk factor (2,
    like the env file), and the answer's DLP scan is the backstop. A privileged playbook runs as the user anyway;
    what `# VAULT: 1` decides is whether Ansible gets the vault's values decrypted.
  - **`become` on remote hosts** (container runs, scored by the risk score): the docs recommend `NOPASSWD` sudo rules for the
    specific commands on the hosts; container runs have no vault to hold a sudo password.
- **Two Plinth CAs** (decided), separate keys, so a leak of one doesn't give
  away the other, and neither is the user's login key:
  - the **user CA** signs each remote run's user certificate;
  - the **host CA** signs each host's host key, once.

  `plinthctl ca init` makes both in the user's terminal (`ssh-keygen -t ed25519`, each with a passphrase) at
  `<work>/ca/user-ca` and `<work>/ca/host-ca`, the public keys beside them, or the user keeps them in their ssh agent
  only; `plinthctl ca show` prints both public keys. The controller never reads either CA key: signing happens in the
  user's terminal. Hosts are set up once by a playbook the user runs natively, with their own access
  (`docs/ansible-runs.md`): it sets `TrustedUserCAKeys` to the user CA, signs the host's host key with the host CA, and
  sets `HostCertificate` in `sshd_config`.
- **Per run,** the controller makes a fresh ed25519 key pair (`ssh-keygen`, no passphrase) in
  `<ns-dir>/raw/<agent>/<RUN>/ssh/`, out of the run's reach. The container gets the private key and the certificate
  read-only at `/aaa/ssh/`. Both are deleted when the run ends.
- **The user certificate:** key id `<owner>/<repo>/<agent>/<RUN>`, a serial of its own, principals the inventory's
  remote users for the playbook's hosts, no port, agent or X11 forwarding, and:
  - **`source-address`, required** (decided): `plinth.ansible.source-address`, a global key the user
    sets to the address the hosts see the controller's runs come from. While it is unset, a remote run is refused with a
    note to set it; a certificate is never signed without it.
  - **A lifetime of `plinth.ansible.cert-minutes`** (decided; default 60, a namespace key with a
    global default), from the signing. A remote run's timeout is capped at it: more work after it expires is another
    run, with a new certificate.
- **The user signs at acceptance:** whatever its score, a remote run's `exec` is held until the user accepts it in the
  TUI or with `plinthctl accept remote -n <ns> <RUN>`. The box shows the hosts, the remote users, the lifetime, the
  source address, and the score with its reasons; the user types `sign`, and plinthctl runs `ssh-keygen -s` with the
  user CA in that terminal (passphrase, or `-U` with the agent). The certificate goes to the run's `ssh/`, and the
  acceptance document (`accept.remote.yml`, kind `Acceptance`) records its serial and key id. A certificate only exists
  once the user signed, so no policy can let a remote run start without the user. Unattended signing (by the controller, or
  later by a Kubernetes controller holding the CA as a Secret) is for the Enterprise Edition, gated by the policy.
- **Emergency stop:** `RevokedKeys` on the hosts. `exec.start` records the certificate's serial and key id, so the
  user can revoke exactly that one.
- **Check before apply**: a playbook runs `ansible-playbook --check --diff` first. **Phase 1
  runs `--check` only**; applies stay with the user, run natively. Phase 2 (later, on top of the risk score): once the check's
  answer is in, the user accepts the apply as the policy says, and the same playbook bytes run without `--check`, with
  a new certificate. In check mode `command`, `shell`, `raw` and `script` tasks are skipped, and a task with
  `check_mode: false` runs for real.
- **The playbook check** takes `ask lint`'s place for playbooks, deterministic, at submission and
  again before exec (rule `ask.lint`):
  - It reads the playbook with the strict decoder (Documents: no anchors, aliases or tags) and lists every task's module
    by fully qualified name (short names mapped to `ansible.builtin.*`), through `include_tasks`, `import_tasks`,
    `include_role`, `import_role` and `roles:`. An include whose name comes from a variable is refused, since it
    can't be listed.
  - **Connection settings in the playbook**: play and task `vars:`, `vars_files`,
    `set_fact` and `include_vars` take precedence over inventory variables, and the keywords `connection`, `port` and
    `remote_user` override them, so a playbook could point an inventory name at another address, add ssh options (a
    `ProxyCommand`), or switch the connection. The connection variables (`ansible_host`, `ansible_port`,
    `ansible_user`, `ansible_connection`, `ansible_ssh_*`, `ansible_become_*`, `ansible_password`,
    `ansible_private_key_file` and the like, by name pattern) and those keywords, anywhere in the playbook, are a high
    risk factor once there is a score, and refused until then.
  - **Controller-side code**: in a playbook with remote hosts, every `localhost` play,
    `local_action`, `delegate_to: localhost`, and every lookup that runs on the controller (`file`, `template`, `env`,
    `pipe` and the like), since the run's container holds the key and certificate in `/aaa/ssh/` for the whole
    playbook. A risk factor once there is a score; refused until then.
  - **Phase 1 allows or refuses only, with no score** (decided: while there is no score, whatever
    would score high is refused). Refused: a module the image doesn't have; `check_mode: false`; any prompt but
    Ansible's own in an interactive privileged playbook (above); a `!vault` value in the playbook (above);
    `add_host` and `group_by`, so a run can't add hosts; `delegate_to` a host outside the inventory; `become` but in
    an interactive privileged playbook (and, for good, any `become_method` other than `sudo`); connection settings in
    the playbook; controller-side code in a playbook with remote hosts. Allowed and flagged: `command`, `shell`, `raw`
    and `script` (skipped in a check, so the check shows nothing of them), and a module without check-mode support.
    The flags go into the answer's `status` and the acceptance box. With the risk score each refusal that is a factor (`become`,
    connection settings, controller-side code) becomes a score factor, and the policy decides.
  - **Resolved values:** the reviewers (Ollama, once built) see the variables and the inventory as they will apply,
    and the `--check --diff` output; they can add findings, never clear one.
- **The answer** (Documents; decided): `ansible-playbook` writes the JSON
  callback's result on its stdout, which the controller reads from the container's client (at most 16 MiB). That JSON is
  never the answer itself: the controller converts it to the run's `Answer` document (`kind: Answer`) and scans that
  like any answer. One section per task and host (`title`: `<play> / <task> / <host>`; `output`: the task's result as
  YAML: `changed`, `failed`, `skipped`, `msg`, `diff` and the module's return values); `result: fail` when any host
  failed or was unreachable; `status.ansible` the per-host stats (`ok`, `changed`, `failed`, `skipped`,
  `unreachable`). A result censored by `no_log` stays censored; `slurp`'s base64 is decoded before the DLP scan, so the
  masks see the text. Output that isn't the callback's JSON makes the answer `INVALID` (Documents).
- **Network:** a playbook with remote hosts, or one whose header says `NETWORK: 1`, runs in network mode (pasta) and
  needs `plinth.exec.network`. LAN only, a network that reaches the LAN but not the internet, comes later (decision 3).
- **Order** (decided; a slice each, with a host test round): the report on Ansible's CVE
  history; Ansible in the runner image; the pinned config and inventory; playbook runs with `--check` only, local
  first, then remote with the certificate signed at acceptance; the answer from the JSON callback; the playbook check,
  allowing or refusing. Then the risk score (Risk score and policy), then module scoring. Later: check-then-apply, LAN only, AAA
  itself as playbooks.
- **Not yet:** custom modules: Plinth's own collection in `michael/plinth-modules`, reviewed
  through pull requests the user approves and pinned in runner images like any vetted collection, each module scored by
  its own entry in the factor table, set at review time; started once the user has created the repository and a first
  module is needed. Unattended signing: the Enterprise Edition.

## Runs

- **Submission.** `ask.submit` (MCP) writes the script to `<ns-dir>/submissions/<agent>/<id>[-<issue>].sh` (`<id>`: 16
  lowercase hex characters the server draws; `-<issue>`: the optional issue number) and waits up to 30 s for the
  namespace loop. The loop:
  1. checks it with the tooling, through `decide()`: `ask lint --plinth`, at most 500 lines, UTF-8;
  2. draws a `RUN_NAME` unique across all agents of the namespace (the one loop per namespace draws names one at a time,
     so two runs never get the same pair);
  3. writes `RUN_NAME` and `AGENT_NAME` (`<repo-name>/<agent>`, from the token) into the header;
  4. stores it as `<ns-dir>/runs/<agent>/<RUN>/ask.sh` (mode 0700) with `ask.sh.sha256`;
  5. returns the `RUN_NAME`, or the rule that refused it.

  A refused submission moves to `<ns-dir>/rejected/<agent>/<id>/`, with `reason.md` holding the rule and the reason, and
  is kept for analysis (decided). The loop never writes into a run directory again except the files below.
  - **Atomic**: the run is written as `runs/<agent>/.new.<RUN>.<id>/` (`ask.sh`, then `ask.sh.sha256`), the
    submission is removed, and the directory is renamed to `<RUN>`: a run appears complete or not at all. At every poll
    the loop finishes a `.new.*` a crash left: a complete one (its `ask.sh.sha256` matches) is put in place after its
    submission is removed; an incomplete one is removed, so its submission is taken in again. A `.new.*` name counts as
    taken when names are drawn.
  - A submission from an agent that is no longer registered, or registered but not enabled for its repo, is
    refused (`target.valid`); a file in `submissions/`
    that isn't `<id>[-<issue>].sh` is logged once and left alone.
  - Events: `ask.submit` (the decision, target `<agent>/<id>`), then `ask.stored` (target `<agent>/<RUN>`, with the
    submission id and the script's sha256).
  - **Automatic reviews** (built 2026-10-18): every type in the namespace's `reviewers.automatic` reviews the stored
    `ask.sh` (its bytes as `ask.sh.sha256` says) once it is stored. The prompt and the model call run in the
    background, behind the Ollama semaphore, so a review of minutes holds up nothing else; a later poll writes
    `review.<type>.md` or `review.<type>.FAILED.md` (`not available without Ollama` when `ollama.host` is unset or
    Ollama can't be reached) and the event `review.<type>` by caller `runner`: `ok`, or `failed` (rule
    `review.failed`) for a failed review or a `Do not run` verdict, with the verdict, the script's and the prompt's
    sha256 and the model. Until then the run is `reviewing`; a failed one needs `override.review` before exec (fail
    closed). A review cut short by a stop is never written: the next start reviews again. One whose run was scrubbed
    meanwhile, or is another run of that name now (another `ask.sh` sum), is dropped.
- **States**, from names only (also in `plinth-web-ui.README.md`). The answer is a `.yml` document (built
  2026-10-19; see Documents); the other `.md` files become documents later. Files in `<ns-dir>/runs/<agent>/<RUN>/`:

  | File | State |
  |---|---|
  | `ask.sh` (+ `ask.sh.sha256`) | queued; the automatic reviews run |
  | `review.<type>.md` / `review.<type>.FAILED.md` | reviewed / review failed (a `Do not run` verdict also fails) |
  | `override.review.md` | the user overrode a failed review |
  | `running.yml` | written empty by the controller right before the start; the AAA library writes the answer into it and its `EXIT` trap renames it to the answer. Left over = no answer: the run isn't done (only an answer file says that) and may run again. Whether the run is running is the controller's record (its exec request), not this file; one the script removed is written back when it ends without an answer. |
  | `answer[.FAIL].new.yml` | finished, not scanned yet, also while the model can't be asked (`.FAIL`: the script stopped at a gate, `result: fail`) |
  | `answer[.FAIL].dlp-ok.yml` | DLP scan passed (`status.dlp`) |
  | `answer[.FAIL].DLP-FAIL.yml` | DLP scan failed: it ran, and the model's replies didn't fit (`status.dlp` says why) |
  | `answer[.FAIL].INVALID.yml` | not a valid answer document: a bug, never data; never released, reviewed or shown |
  | `override.dlp.md` | the user overrode a failed DLP scan |
  | `accept.network.md` | the user accepted a network run (`accept.network`) |
  | `report.md` | reported: the link to the run's issue in `plinth.repo` (see Report) |
  | `issue.md` | the run's issue in `plinth.repo`: the line `Issue: <page>`, written when Save or Report opened it (see The run's issue) |
  | `answer[.FAIL].released.yml` | released by Done; `answer.read` returns it |
  | `review.answer.<type>.md` / `.FAILED.md` | answer reviewed / review failed |

  The answer goes `running.yml` -> `answer[.FAIL].new.yml` -> `answer[.FAIL].dlp-ok.yml`, `.DLP-FAIL.yml` or
  `.INVALID.yml` -> `answer[.FAIL].released.yml`, each step one rename or one write of the scanned document (decided).

  Status in `get ask`: `reviewing`, `review-failed`, `approved`, `overridden`, `running`, `no-answer` (an exec ended
  without an answer; it may run again), `ran`, and `broken` for a run that can't be read (`describe` and `get ask
  <RUN>` say why). Status in `get ans`: `scanning`, `waiting-for-model`, `dlp-ok`, `dlp-failed`, `overridden`, `invalid`, `released`,
  each with `(gate)` for a `.FAIL` answer. `get ans <RUN>` prints the answer document as it is; every other state gets
  one line that says what comes next.
- **Automatic reviews:** the types in `reviewers.automatic` run on the script once it has passed the tooling at
  submission, and on the finished answer once its DLP scan passed (`dlp-ok` or `released`; the scanned, masked answer),
  for the types whose prompt reviews answers: `review.answer.<type>.md` or `.FAILED.md`, event `review.answer.<type>`.
  The loop remembers a type whose prompt doesn't review answers while it runs (it asks again after a restart or a
  config change). An answer review never blocks anything; without `ollama.host` none is written. A failed script
  review is `review.<type>.FAILED.md` (Ollama unavailable included) or the verdict `Do not run`.
- **Done** (the web UI button, the TUI key or `plinthctl done`) renames `answer[.FAIL].dlp-ok.yml`, or after `override.dlp`
  the `answer[.FAIL].DLP-FAIL.yml`, to `answer[.FAIL].released.yml`. Only then does `answer.read` return the answer, as
  often as the agent asks. Done writes nothing into the repo (decided).
- **Who sees an answer** (decided). The user sees it in the TUI and the web UI once it has been scanned, before and
  after Done, **a failed scan included**, with the fixed-pattern masks: auditors need to see why a scan failed to take
  corrective action. `get ans` prints only a passed or overridden answer; for a failed one it gives the reason and
  points to the TUI or the web UI. The agent only ever gets the released answer, through `answer.read`; never a
  `DLP-FAIL` answer that wasn't overridden and released.
- **Save** (built 2026-10-22; where it posts changed 2026-10-27):
  - zips the run (without `running.yml`) into `<RUN>.zip` and posts a `Run artifacts` comment with it on the run's own
    issue in `plinth.repo` (see The run's issue), whatever the run's name ends in: the number a `RUN_NAME` may end in
    is no longer an issue Save posts on (it was read as an issue of the namespace's repo, which posted a run for
    another repo's issue on an unrelated one);
  - uses Plinth's own Forgejo token (`forgejo-token`), never an agent's; without one the request fails. Without
    `plinth.repo` there is no Save (rule `repo.set`);
  - with `plinth.autosave.runs` the loop queues Save by itself right after Done carried out (never before: the answer
    must be scanned and released first); the event log has `autosave.save` by `runner`, and the request is decided like
    the user's. The TUI's `s` then says `(runs auto-save)` and does nothing; a failed save (the request is `FAILED`, no
    success after it) brings the key back as `Save (auto-fail)`, and the failure is in the general log.
  - The loop decides and reads the files at once, then posts in the background (package `forgejo`, a comment, then
    its attachment), so a slow Forgejo holds up nothing else; the request stays `.running` until then. A comment
    whose zip didn't upload fails the request, naming the comment: Save may run again, as often as the user asks.
    A request a stop left `.running` is carried out again at the next start, which can post a second comment.
- **Scrub** is a full scrub (decided): it deletes the run directory, the DLP scan's untouched original in
  `<ns-dir>/raw/<agent>/<RUN>/` and the run's request files in `<ns-dir>/requests/<agent>/<RUN>/`. Plinth keeps no
  history of a run's content; a pipeline with histories is a CI/CD server's job.
  - The loop first renames the run directory to `runs/<agent>/.scrub.<RUN>`, one rename that takes the run out of every
    list (the name no longer matches `<RUN>`). Then it deletes the original and the request files, and that directory
    last. At start, the loop finishes any `.scrub.*` directory a crash left behind.
  - The event log keeps the run's lines: they hold names, decisions and hashes, never content, and the hash chain can't
    lose a line (see Events).
  - The run's issue in `plinth.repo`, and what Save and Report posted on it, are in Forgejo, not in Plinth, and stay.
- **The run's issue**: Save and Report post into one repo, `plinth.repo` (`<owner>/<repo>` on
  `forgejo-url`; it replaces `plinth.failed-scans.repo`, and `config check` refuses a config that still has the old key,
  naming the new one). It holds every saved run, not only failures: make it a private repo. Each run has one issue
  there, and each action adds a comment to it.
  - The first of Save and Report opens it. Title: `<ns> <stem>: <what the script says>`, the run's name without any
    `-<digits>` and the text of the script's `# AAA script:` line, cleaned (one line, printable), cut at 100
    characters and fixed-pattern masked. A run name is not unique over time (`names.Draw` avoids only runs that still
    exist), so the title does not identify a run; `issue.md` does. Body: namespace, agent, the full run name, the sha256
    of `ask.sh`, the origin ticket (the script's `ISSUE=` line, when it names an issue on `forgejo-url`; context only)
    and when and by which action it was opened; fixed-pattern masked.
  - **`issue.md`** in the run directory is the pointer: the line `Issue: <page>`. It is written right after the issue is
    created, before anything else is posted, and Save and Report find the run's issue by it; one process-wide lock makes
    "find or open" one step, so Save and Report at the same time open one issue. A scrub removes it with the run, so a
    new run of the same name has its own issue. If `issue.md` can't be written the request fails and says the next one
    would open another issue.
  - **The origin ticket decides nothing.** Nothing is posted on the issue the script was for, and where Plinth posts
    never depends on a number in a run name or on `ISSUE=`.
  - `plinth.autosave.runs` and `plinth.autosave.failures` (both `false` by default, YAML booleans) let the loop queue
    Save (right after Done) and Report (at a run's first failed check) by itself; see each.
- **Report** (decided; where it posts changed): failed scans feed later analysis, to improve a policy, an
  agent's rules or its context.
  - When the user reviews a failed scan (a failed ask-script review, a failed DLP scan, or a failed message review),
    they can have Plinth add a comment about it to the run's issue in `plinth.repo` (opened first when the run has
    none). The web UI and the TUI offer it; for a run, `plinthctl report` writes the same request (its form for
    messages follows once message ids are specified). With `plinth.autosave.failures` Plinth does
    it by itself, once per run, at the run's first failed check: the review's result or the DLP scan's result is applied
    (a failed review of the script, or a `DLP-FAIL` answer), the event is written, and the loop queues Report unless the
    run is reported already. Later failed checks of the run are not posted (a second Report is refused). The TUI's `r` then
    says `(auto-report)` and does nothing while the request is pending; a failed one (`FAILED`, no success after it)
    brings the key back as `Report (auto-fail)`, and the failure is in the general log. Report needs Ollama (it masks
    first), so with autosave on and no Ollama the request fails and says so.
  - The comment: `### Report of failed scans: <checks>`, the time and the `seq` of the report's event, and per check the
    rule, the `seq` and time of the deciding event and its reason; attachments: the ask script, the review files and,
    for a failed DLP scan, the answer (for a message: the message and its review).
  - **Everything Report posts is masked first**, whatever ran before: the comment and every attachment go through
    the DLP scan (fixed patterns, then Ollama). Nothing is posted unmasked, and nothing at all, not even the run's
    issue, when the comment's own masking fails. An attachment whose masking fails is left out, and the comment says so
    with the reason. Without Ollama nothing is posted: the request fails with "not available without Ollama", and the
    user can report again later. (Save posts the run's files as they are: the user has seen the script and reviews the
    answer; only the issue it opens has its title and body masked with the fixed patterns.)
  - How it is built (2026-10-22): the loop decides, reads the files and finds the deciding events (the run's last
    `review.<type>` and `dlp.scan` with outcome `failed` by `runner`), then masks and posts in the background; the
    request stays `.running` until the comment is posted. A reason goes in a code fence, so nothing in it is markdown.
    The attachments go as one zip, `<RUN>.report.zip` (entries `<RUN>/<file>`, each file masked before it goes in), attached to the comment: Forgejo's default attachment types allow `.zip` but not `.sh` or `.yml`,
    and Save attaches a zip too. The comment lists the files in it and those left out. A zip that doesn't upload once
    the comment is posted is named in `report.md` and the general log.
  - One report per run or message: the loop writes `report.md` with the issue link, and the UIs show the link.
    Plinth's own `forgejo-token` posts it; its account needs access to `plinth.repo` (`../README.md`). Scrub leaves the
    issue alone.
  - Without `plinth.repo` there is no Report and no Save (rule `repo.set`; the UIs show "No repo configured
    (plinth.repo)").

## Requests

A request is a file `<ns-dir>/requests/<agent>/<RUN>/<action>[.<arg>].queued` (messages:
`<ns-dir>/requests/messages/<message-id>/<action>[.<arg>].queued`). A command given a bare `<RUN>` finds the agent from
the run's directory.
- The loop renames it to `.running` (an `exec` to `.starting`, and to `.running` right before its script starts, see
  Exec), then to `.finished` once the result is in the run, or to `.FAILED` with the rule and reason as its content
  (`Rule:`, `Reason:`, `Policy:` lines, or `Error:`). A finished or failed request stays, and so does a `.FAILED`
  an earlier try of the same action left: only Scrub deletes a run's requests, with its request directory. The
  same request `.queued`, `.starting` or `.running` already can't be queued again; after a finished or failed one,
  `decide` says whether the action can happen again. `describe` lists a run's requests, a failed one with its content
.
- The name carries the whole request. The loop never acts on a file's content and re-validates every part: the patterns,
  the path resolving inside `<ns-dir>`, the review type in the config, and the current state.
- Actions: `exec`, `done`, `save`, `scrub`, `report`, `review.<type>`, `override.review`, `override.dlp`,
  `accept.network`; for messages `approve`, `reject.<policy>`, `report`.
- One run's requests are carried out in a fixed order: `override.review`, `override.dlp`, `accept.network`,
  `review.<type>`, `exec`, `done`, `save`, `report`, `scrub`, so an override or acceptance queued with the action it
  releases goes first.
- Built: `exec`, `done`, `scrub`, `override.review`, `override.dlp`, `accept.network`, `review.<type>`
  (2026-10-18), `save` and `report` (2026-10-22). `requests/messages/` is left alone until the messages exist; a name
  that isn't a request, an agent or a run is logged once and left alone.
- Every decision is in the event log before anything acts (caller `user`). Overrides write `override.review.md` /
  `override.dlp.md` with what failed, when, and the receipt (`seq`, hash) of the event that allowed them. Done renames
  the scanned answer to `answer[.FAIL].released.yml`. Scrub works as described in Runs. Only an event log that can't be
  written fails the loop; any other error fails the request.
- Process-level requests (not tied to a namespace) are files in `<work>/requests/`, handled by the process's own loop
  the same way, except that a carried-out one is deleted: there is no run to scrub it with. The only one so far:
  `logs-scrub.queued` (see Log). A `.running` left by a crash is carried out again at the next poll; a file the
  loop doesn't know is logged once and left alone.
- Patterns:
  - `<RUN>` is `^[a-z]+-[a-z]+(-[0-9]+)?$`;
  - `<agent>` is `^[a-z][a-z0-9-]{0,31}$`, except the reserved names `user`, `runner`, `worker`, `controller` and
    `messages`: `user` and `runner` are the event log's callers next to the agents (`worker` in older lines), `controller` names the process, and `requests/messages/` sits next to `requests/<agent>/`. `register` refuses them
    and every listing skips them;
  - `<owner>` and `<repo>` are `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$` (not `.` or `..`).

## Log (decided)

The terminal shows only what the user has to act on; everything else Plinth has to say goes to its general log
(package `logfile`). It is separate from the event log (`events/`, the hash-chained decisions) and never holds a secret:
no token, no key, no answer or script content.

- **Where:** `<work>/logs/plinth-YYYY-MM-DD.log`, one file per local day. A new day starts a new file; that is the
  rotation, with nothing to configure.
- **Lines** (2026-10-28; no older format is read): Go's `log/slog` text format, one line:
  `time=<RFC 3339, seconds, with the offset> level=<DEBUG|INFO|WARN|ERROR> msg="<text>" component=<name> pid=<n> [ns=<ns>]
  [agent=<a>] [run=<RUN>] [<field>=<value> ...]`. The handler quotes and escapes a value that holds a space, a quote, `=`
  or a control character (`\n` stays two characters), so a message or a field can't break a line or hide a control
  character; `logfile.Parse` reads a line back. `component` is one of `controller`, `runner`, `reviews`, `dlp`,
  `ollama-gateway`, `agent-mcp`, `tui`, `cli` (the CLI's lines also have `command=<name>`). Callers give a short
  message and the details as fields (`ns`, `agent`, `run`, `rule`, `reason`, `err`, ...): `Log.Info`, `Warn`, `Error`,
  `Debug`; `Component` and `With` make a log with another component or more fields. `Once(key, ...)` logs a WARN line
  unless it is the same line as the last one for the key, `Clear(key)` forgets the key. The Ollama gateway's container
  writes the same format to its stdout (`logfile.NewTo`), which the controller reads with `podman logs`. Each line is one
  `O_APPEND` write (`safefs.Append`), so `register` from several launch scripts and the controller can write at the same
  time without mixing lines. The file is mode 0600, a regular file with one hard link; a symlink is refused.
- **A failing log never fails the command:** the first write error is reported once on stderr ("registered all the
  same"), and the rest of that command's lines are dropped. The controller, which runs for days, tries again at every
  line instead and reports on stderr once per distinct error.
- **Max age:** `plinth.log.max-age-days` (global, default 30). A file is old when its date, from its name, lies more
  than that many days before today. Today's file is never old; files with other names in `logs/` are never touched.
- **Scrub, never automatic:** "Scrub logs" in the web UI and the TUI, or `plinthctl logs scrub`, lists the old files,
  asks for confirmation and writes the request `<work>/requests/logs-scrub.queued`; the process deletes exactly the
  files that are old when it carries the request out (the front ends only queue, see Principles). Nothing deletes a log
  on its own.

## Decision function: one gatekeeper (decided)

`decide(caller, namespace, action, target, state) -> {outcome, rule, reason, policy-page}`, outcome `allow`, `deny` or
`require-approval`. It runs on every MCP call and every request; no code path acts without asking it first.

Why one gatekeeper rather than checks spread through the code:
- **Deny by default.** The last rule refuses anything no earlier rule allowed, so a new action that nobody wrote a rule
  for is blocked, never let through by accident.
- **Every refusal names its rule.** The agent gets the rule id and its policy page; the event log records the same id.
- **The whole policy is one table in one place**, so a reviewer (human or agent) can read all of it at once.

Rules are evaluated in the order below. Rules marked "invariant" are Go code that no config can loosen; the others read
the config. Each rule has a stable id, so a denial names its rule, and a `rej` names the rule and its policy page.

| Rule | Kind | Outcome |
|---|---|---|
| `caller.user-only` | invariant | `deny` an agent calling exec, done, save, scrub, report, review, override, accept.network, approve, reject, logs-scrub |
| `caller.own-runs` | invariant | `deny` an agent reading, asking about or submitting for another agent's run, or naming no agent where the action is per agent |
| `target.valid` | invariant | `deny` a caller, namespace, agent or run name that fails its pattern (so nothing resolves outside `<ns-dir>`), a review type not in the config, or a symlink in the run's path; a submission from an agent not registered, or registered but not enabled for its repo |
| `state.valid` | invariant | `deny` an action the run's state doesn't allow (e.g. done without a scanned answer, exec of a run with an answer (only an answer says a run is done; one whose exec ended without an answer runs again) or whose last exec isn't finished (its request `.running`); override.review or accept.network of a run that already ran: an answer, a left-over `running.yml`, or its exec request `.running` or `.finished`; report without a failed scan or a second time; exec of an `INTERACTIVE=1` run unless from the user's terminal; done or save of an `INVALID` answer) |
| `ask.lint` | invariant | `deny` a submission that fails `ask lint --plinth`, and the exec of a stored `ask.sh` that no longer passes it (a later `plinthctl` lints more; e.g. one without the AAA library line), with the findings and the agent's next step |
| `ask.size` | invariant | `deny` a submission over 500 lines or not UTF-8 |
| `ask.mode` | config | `deny` a submission whose header asks for network or privileged mode (`NETWORK=1`, `PRIVILEGED=1`) that `plinth.exec.network` or `.privileged` doesn't allow |
| `review.pending` | invariant | `deny` exec while an automatic review is still running |
| `review.failed` | invariant | `require-approval` (`override.review`) for exec after a failed review |
| `exec.mode` | config | for exec and accept.network: `deny` a privileged run unless the exec comes from the user's terminal (`plinthctl exec` over the controller's socket) and the namespace allows privileged mode, accept.network of a privileged run, an exec from the terminal of a run that isn't privileged, and a network run its namespace no longer allows; `require-approval` (`accept.network`) for exec of a network run |
| `dlp.failed` | invariant | `require-approval` (`override.dlp`) for done and save after a failed or unavailable scan |
| `answer.unreleased` | invariant | `deny` `answer.read` before Done; for a run whose exec ended without an answer, the reason says so. A released answer counts only when the controller's exec request says the script was started (`exec.running` or `exec.finished`) and its exec ended, and the run isn't running: the file name `answer.released.yml` alone doesn't (a script can write it into its own run directory). `ask_status` and `answer_read` show such an answer as no answer, never as released |
| `message.catalogue` (not yet) | config | `deny` a type not in `plinth-messages.yml`, or a URL not under `forgejo-url` |
| `message.recipient` (not yet) | config | `deny` a recipient that isn't an agent of the namespace |
| `message.gated` (not yet) | config | `require-approval` (Approve / Reject) unless sender or type is in `ungated` |
| `repo.set` | config | `deny` save and report while `plinth.repo` isn't set |
| `policy.score` (not yet) | config | the action `plinth.policy` names for the run's risk score: `require-approval` (a typed acceptance) for `require-acceptance`, `deny` for `terminal-only` and `deny` (Risk score and policy) |
| `allow` | invariant | `allow` an action the policy knows (guide, ask.template, ask.submit, ask.preview, ask.status, answer.read, aaa.new, aaa.read, aaa.status, aaa.lint, agent.up, exec, done, save, scrub, report, review, override.review, override.dlp, accept.network, logs-scrub) once every rule above passed |
| `default.deny` | invariant | `deny` anything no rule above allowed |

`require-approval` is released only by the user's override or approval, which is itself a request going through
`decide`, and is recorded.

How it is built (package `decide`):
- `Decide` is a pure function. The caller gathers the facts first: the run's state from its file names (exists,
  reviews pending or failed, overridden, running, ran, the answer's state, reported, a symlink on the way), the
  submitted script and its `ask lint --plinth` result, and the config (review types, `plinth.repo`).
  `Decide` only reads them, so the whole policy is one table in one file, tested row by row.
- **Authentication decides who the caller is, never a name**. Package `auth` is the only place a caller is made,
  one constructor per channel, and a channel can only make its own kind:
  - the user at the host (`HostUser`): the CLI and the TUI, which run as the user on the host, and the controller carrying
    out request files from `<work>`. Writing into `<work>` (mode 0700) is the authentication: no agent reaches it, and
    no executed script, which runs isolated from it (Process, Exec). It needs no Forgejo, so an outage never locks the user out;
  - an agent (`Submission`): the controller taking in `submissions/<agent>/`, which only the MCP server writes, after the
    agent's token matched `<agent>.sha256`;
  - the MCP server's token check and the web UI's Forgejo login come with those slices, as their own constructors.

  A caller of neither kind (the zero value) is refused (`target.valid`), and an agent named `user` is still an agent.
  The event log writes the caller as `user`, `<agent>` or `runner` (`worker` in older versions), which the reserved names
  keep apart.
- **Messages are denied for now:** until message ids and the catalogue are specified, the three
  `message.*` rules are not built and `message.send`, `message.pickup`, `approve` and `reject` fall through to
  `default.deny`.
- State rules per action: `exec` needs a run that exists, isn't running, whose last exec is finished (its exec request
  isn't `.running`, which a stopped controller's next start finishes) and has no answer, whose `ask.sh` still matches
  its `ask.sh.sha256` and doesn't say `INTERACTIVE=1` (that one runs only in the terminal); `done` a scanned answer
  (`dlp-ok` or `DLP-FAIL`) not yet released; `save` a scanned or released answer (a run needs no issue in its name); `report` a failed review or DLP scan
  not yet reported; `review` and `scrub` a run that isn't running; `override.review` a failed review, not yet
  overridden, of a run that hasn't run; `override.dlp` a `DLP-FAIL` answer not yet overridden. Running and ran come from
  the controller's record as well as the names in the run directory: a run is running while the controller's child runs it, and
  it has run once its exec request is `.running` or `.finished`, so a script that removes its `running.yml` changes
  neither.

No rate limits (decided).

## Risk score and policy (not built)

Decided: every
analyzed run gets a **risk score** from 0 to 5, which Plinth computes from facts it has, and **one policy** in the
global config says what Plinth does at each score. A namespace may tighten the policy, never loosen it. When a question
can't be settled once in code (should `ask lint` refuse this?), it becomes a factor here instead. Nothing in this
section is built; the one detail still marked **(proposal)** is still to be confirmed.

- **How a score is computed is Plinth's; the numbers and the policy are the user's.** This spec fixes the factors and
  how they combine, so a score is computed the same way everywhere. The factor scores below are defaults, which the
  user can set in the global config and per namespace (decided: some repos are more sensitive than
  others), and the policy maps scores to actions. The final behaviour is the factor scores combined with the actions
  the policy allows. An agent changes none of it: it writes the run, and the score comes from the run.
- **Deterministic.** The score is the highest score of the factors that apply, raised by the combinations below, at
  most 5. 0 is a run that reaches nothing and holds no secret (a build in its container); 5 is, at the least, secrets
  plus the internet. A model reviewer can raise a score (its finding is a factor), never lower it.
- **Factors, with their default scores** (decided as defaults):

  | Factor | Value: score |
  |---|---|
  | Mode | normal: 0; network: its reach; privileged: 5 (it runs as the user, outside any container) |
  | Reach | none: 0; LAN only: 2 (needs the LAN-only network, later); internet: 3 |
  | Secrets | none: 0; any env-file variable, the vaulted variables (`# VAULT: 1`, Ansible runs), or a signed ssh certificate: 2 (any env-file variable is a secret) |
  | Hosts (playbooks) | `localhost` only: 0; inventory hosts over ssh: 2, and their reach (LAN or internet) |
  | Modules (playbooks) | read-only modules (`stat`, `setup`, `find`, `slurp`, `uri` with GET, and the like): 0; other modules that support check mode: 1; `command`, `shell`, `raw`, `script`: 1 with no reach, 4 with reach; `check_mode: false`: 4; a custom module: its own entry, set at review |
  | Controller-side code (playbooks with remote hosts) | a `localhost` play, `local_action`, `delegate_to: localhost`, a controller lookup (`file`, `template`, `env`, `pipe`): 4 |
  | Privilege (playbooks) | no `become`: 0; `become` with `sudo`: 3; any other `become_method`: refused, never scored |
  | Connection settings (playbooks) | none: 0; a connection variable or the keyword `connection`, `port` or `remote_user` anywhere in the playbook: 4 |
  | Source (playbooks) | committed at a reviewed commit: 0; ad hoc: 1 |
  | Findings | a flag of `ask lint` or the playbook check: its entry in the check's table; a model reviewer's finding: low 1, medium 3, high 4 |

  A local `command` in a container with no network scores 1, not like `shell` against a remote host. Until the score exists (phase 1), the playbook check refuses `become`, connection settings and
  controller-side code instead (Ansible runs). `become` in an interactive privileged playbook (Ansible runs) is
  allowed: privileged mode scores 5 and runs only in the user's terminal anyway, where the user types the sudo
  password.
- **Combinations** (decided as defaults): secrets and the internet: 5; secrets and LAN only: 4; an ad hoc playbook with
  any reach: one more than its other factors give.
- **Setting the factor scores** (decided; **(proposal)** for the key): `plinth.risk-factors`, a map
  from a factor's name, fixed by this spec (`reach.internet`, `secrets`, `become`, `connection-settings`,
  `combination.secrets-internet` and so on, one per value in the tables above), to a score from 0 to 5. In the global
  config it replaces the defaults; in a namespace's config it replaces the global value for that repo, per name (not
  taken whole, unlike the other namespace keys), and may **raise or lower** it. An unknown name or a score out of range
  is a problem. Lowering a factor in a namespace can loosen what tighten-only guarantees for the policy: fine locally,
  where the user writes both files; **later, for the EE**, per-factor floors in the global config, which a namespace
  can't go below, for when an admin owns the global policy.
- **Action types**, loosest to strictest; the order is what tightening means (decided):

  | Action | What Plinth does |
  |---|---|
  | `allow` | the user's `exec` runs it. The agent never starts anything (Principles), so `allow` never means a run nobody asked for. The policy's `require-approval` ("held until the user approves, as today") is this: every exec is the user's request already |
  | `require-acceptance` | `exec` is held until a typed, two-step acceptance, like `accept.network` today; the box shows the score and its reasons |
  | `require-admin-approval` | held until the admin approves on the run's issue (AICC); later |
  | `terminal-only` | runs only in the user's own terminal, like privileged mode |
  | `deny` | refused, with the score and its reasons |

- **The policy key** (decided): global `plinth.policy`, one action per score, every score from 0 to 5
  given, and a higher score never looser than a lower one (a problem otherwise). The template and the built-in default
  reproduce today's rules, so adopting the score changes nothing at first:

  ```yaml
  plinth:
    policy: {0: allow, 1: allow, 2: allow, 3: require-acceptance, 4: require-acceptance, 5: require-acceptance}
  ```

  Normal runs score 0 to 2 (2 with an env file) and run on the user's exec; network runs score 3 to 5 and need their
  acceptance, as `accept.network` today; privileged runs stay terminal-only by the invariant below.
- **Tighten only** (decided): a namespace's `plinth.policy` may name a stricter
  action for any score, never a looser one. Unlike the other namespace keys it isn't taken whole: per score, the
  stricter of the global and the namespace action applies. A namespace entry looser than the global one is a problem
  (`config check`), and the namespace isn't served: fail closed, as the loader does everywhere. In Kubernetes later:
  the global policy is a cluster-scoped resource only the admin writes, the namespace policy a namespaced one in the
  same YAML shape with its `kind:`; a validating admission webhook refuses a looser namespace policy when it is
  written, and the controller checks again before each run, showing the namespace `Ready=False` (reason
  `LooserThanGlobal`) when the global policy was tightened later; the runs' pods decide nothing.
- **What stays code:** the invariants of the decision function (`ask.lint`, `review.failed`, `dlp.failed` and the
  others) and these: a privileged run runs only in the user's terminal, and a remote playbook needs the user's
  signature (Ansible runs), whatever the policy says. The policy can only add to them.
- **When:** at submission (`ask.stored` records `score <n>: <reasons>`) and again at exec, since the env file, the
  inventory, the factor scores and the policy can change in between. The acceptance records the score it accepted; an
  exec whose score is now higher asks again. `exec.start` records the score and its reasons.
- **Where it shows:** `describe`, `get ask` (a score column), the TUI's Runs and Run screens, and every acceptance box.
- **In `decide`:** a rule `policy.score` (config), after `review.failed`: the action for the run's score, with
  `require-acceptance` released by the acceptance request (`accept.network` today), and `terminal-only` and `deny`
  refusing the controller's exec. `exec.mode` keeps refusing what its namespace doesn't allow and privileged runs; its
  `require-approval` for network runs moves into the default policy.
- **Order**: after phase 1: the score with the factors Plinth has today (mode, reach, secrets,
  findings), the factor scores and the policy in the config and in `decide`, display in the CLI and the TUI, and the
  move of today's mode rules into the default policy (3 to 4 slices); then module scoring on top of the playbook check.

## Events (decided)

One append-only, hash-chained log per namespace in `<ns-dir>/events/`, one JSON object per line.
- Fields, in this order: `seq`, `time` (RFC 3339, UTC), `caller` (`user`, `<agent>`, or `runner` for a namespace's runner's own steps such as the automatic reviews; `worker` in older lines), `action`, `target` (`<agent>/<RUN>`
  for a run), `outcome`, `rule`, `reason`, `config` and `prev` (sha256 of the previous line without its newline,
  including the last line of the previous file; `genesis` for the first). `config` is the sha256 of the two config
  files in force: of the global file's sha256 and the namespace file's sha256 (of nothing when it has none), one per
  line.
- Each line is one write, synced (and the directory synced when the line starts a new file) before the action it
  records goes ahead; its receipt is its `seq` and hash.
- **Fail closed**: a namespace's loop opens its event log before anything else. A last line cut short by a crash
  (no newline) is cut off: it was never complete, so nothing acted on it. Any other damage (a line that doesn't parse,
  a field that isn't one of the above) keeps the namespace from acting; its loop retries with backoff, and the log
  says why. The controller never repairs the chain; `events verify` names the damage.
- **Rotation:** a file holds at most `plinth.event-log-max` lines (default 10,000, about 3 MB). The next line starts a new
  file named after its first `seq` (`<first-seq>.jsonl`). The chain continues across files; rotation never deletes a file.
- Written for every decision and for every outcome of an action (exec started and ended with its exit code, answer
  scanned, released). Run actions: `ask.submit`, `ask.preview`, `ask.stored`, `review.<type>`; the user's `exec`, `done`,
  `scrub`, `override.review`, `override.dlp` (the decision, target `<agent>/<RUN>`); the controller's `exec.start` (the
  sha256 of the `ask.sh` it starts, and the names in the run directory at the start, after `; run directory: `) and `exec.end` (outcome `answered`, `answered at a gate`, `answer refused` or
  `no answer`; reason the exit status, why it was stopped, and any state file moved
  out) and `dlp.scan` (with the sha256 of the untouched original).
- `plinthctl events verify -n <ns>` walks the chain across all files and names the first line that doesn't match: a
  file whose name isn't the seq after the file before it, a seq out of order, a `prev` that doesn't match, a line that
  doesn't parse, a file that isn't `<first-seq>.jsonl`. `events show -n <ns> [<RUN>]` prints the events (all, or the
  run's) as it walks, and stops at the first line that doesn't match. Both need no controller.
- Receipts: `override.*.md` records the `seq` and line hash of the event that allowed it; a release is its `done` event.

## Plinth MCP server

The agents' way into Plinth (built 2026-10-21 for the MVP; package `agentmcp`): an agent reads the guide and the
ask template, submits ask scripts, follows its own runs and reads their answers once the user has released them. How an
agent is set up: [docs/agent-mcp.md](../docs/agent-mcp.md).

Plain AAA goes through it too: an agent in a container has no `plinthctl`, so the server is its only way
into Plinth either way. The difference is where a run lives: a **Plinth run** in the controller's namespace (`ask_*`,
`answer_read`), or a **plain AAA run** in the agent's own repo, `<repo>/ai/work/<RUN>/` (`aaa_*`). Plinth runs are the
secure default, which the guide tells the agent to use unless the user asks for plain AAA: the agent can't reach the
run files there. `<repo>/ai/work` is the compromise, the informal way: its files are in a repo the agent can write to,
the script runs as the user in their terminal, and it gets only the plain lint the agent asks for (`aaa_lint`), no
automatic review. The
controller must run for both, since it serves the server (any developer should be comfortable keeping MCP
servers alive for their agents).

- **Where:** inside the controller's process, at `http://<plinth.mcp.listen>:<plinth.mcp.port>/mcp`
  (default `0.0.0.0`:8791, on the VM, so agents in containers reach it through `host.containers.internal`). The
  controller starts it at its start (a port it can't listen on keeps the controller from starting, naming the keys),
  logs where it listens, and stops it first when it stops, before it detaches the runners. Plain HTTP for now; its
  hardening (TLS, or binding only where the agents are) is still to do.
- **Transport:** MCP over HTTP (JSON-RPC 2.0), stateless, the same core as the Ollama gateway's (package `mcp`). The
  `initialize` result's `instructions` say to call `guide` first; the read-only tools carry `annotations.readOnlyHint`.
- **Token:** every request needs `Authorization: Bearer <token>`. The server computes the sha256 of the token it
  received and compares it, in constant time, with every namespace's `<ns-dir>/state/agents/<agent>.sha256`
  (`agents.ByToken`; a pending registration's never matches). An unknown token gets 401 with `WWW-Authenticate: Bearer`
  and a message naming nothing; no tool runs.
- The token decides the agent and its namespace. No tool takes an agent or namespace parameter.
- **A namespace the controller doesn't serve now** (its config doesn't load, or it is being reloaded): every call is
  refused, with the next step (try again; `plinthctl config check -n <ns>`).
- **decide first:** every tool asks `decide` as the agent (`target.valid`: registered and enabled; `caller.own-runs`)
  with the namespace's facts. A refusal is the tool's error: `refused by rule <rule>: <reason> - next: <what to do>`, and
  goes into the namespace's event log through its runner (`runner.Record`, so the chain has one writer; nothing is
  recorded once a runner is detached). Not recorded: allowed `guide`, `ask_template`, `ask_status`, `aaa_status` and
  `aaa_lint` calls (reads and checks an agent repeats; the general log has them), and `answer.unreleased`, which is a run's state, not a
  refusal: `answer_read` then says `not released yet: <its ask_status line>`; nor `aaa_read` of a run whose answer isn't
  scanned, likewise its state. A read answer is recorded (`answer.read allow`, `aaa.read allow`), and so is a plain AAA
  run `aaa_new` created (`aaa.new allow`). A plain AAA run's events name it `<agent>/ai/work/<RUN>`, apart from the
  namespace's runs, whose names are drawn elsewhere and may be the same.
- **Every reply that isn't what was asked for says what the agent does next**, as every plinthctl status line does.
- **Tool names** use `_` (the Claude API allows only `[a-zA-Z0-9_-]`); the decide actions and the events
  keep theirs (`ask.submit`, ...). Every tool's description ends with "Call guide first if you don't know Plinth".

### Agent tokens: Plinth stores only a fingerprint (decided; where and how)

Plinth never stores an agent's token, only its sha256 (a one-way fingerprint), the way Linux keeps password hashes in
`/etc/shadow` instead of passwords.

1. `plinthctl register --repo <path> --agent <agent>` (see Process, Register) generates a long random token,
   `plinth_` and 43 characters of base64url (256 bits from `crypto/rand`; the prefix gives the DLP scan a fixed
   pattern), prints it once on stdout, and writes only its sha256, in hex, to `<ns-dir>/state/agents/<agent>.sha256`.
2. The agent's launch script captures it (`PLINTH_TOKEN="$(plinthctl register ...)"`) and passes it to the agent's
   startup. That is the only copy, and it lives as long as the session; nothing to keep in a secrets manager.
3. On every MCP call the agent sends its token; Plinth hashes what it received and compares the result with the stored
   fingerprints. A match identifies the agent and its namespace.
4. The fingerprints are files on disk, so they survive a restart of `plinthctl`.

Why a fingerprint: if `<work>` ever leaks (a backup, an accidental commit, a screen share), the fingerprints are useless,
because a sha256 can't be turned back into the token. Because the token is long and random, a plain sha256 is enough;
slow password hashes (bcrypt, argon2) are only needed for short, human-chosen secrets. A lost token can't be looked up:
the user runs `register` again and gets a new one.

Why a state file and not the namespace `config.yml` (decided; it replaces `plinth.agents.<agent>.token-sha256`): `register` rewrites the fingerprint at every launch, and two agents may register at the same moment. One file per
agent, replaced atomically, needs no lock and never rewrites the user's hand-edited, commented `config.yml`.

Enabling is the other way round: `plinth.agents.enabled` is the user's decision, written only by `agent enable`
and `disable` (or by hand), never by register, so it belongs in the namespace `config.yml`. The fingerprints stay in
`state/agents/`; the config names only the agent and its repo, never a token or its sha256.

Registration (`state/agents/`) and enabling are separate from `plinth.messages.ungated.agents`: being registered and
enabled lets an agent act at all; being ungated only lets its messages skip the user's approval.

Not an agent's Forgejo token instead: all agents share one Forgejo account, so Forgejo can't tell them apart;
every MCP call would depend on Forgejo being up; and Plinth would receive a token that can write to Forgejo on every call.

| Tool (decide action) | Parameters | Result |
|---|---|---|
| `guide` (`guide`) | - | the agent's AGENTS.md: a fixed part embedded in the binary (`agentmcp/guide.md`: what Plinth and AAA are, the flow, what the agent can and can't do, the image its scripts run in (the repo's `.plinth/Containerfile`; for a tool the image lacks, or no `.plinth/` yet, the agent proposes a change as a pull request and tells the user, who merges and publishes; what `publish` refuses; a minimal Containerfile), how to write an ask script, the states), then the namespace's: the run modes it allows, the run time limit, its automatic reviews, and every review type's policy URLs from the **global** config, for the agent to read with its own tools; a type defined in the config itself (`.definition`) is named, not quoted. A namespace's own policy, and refusing policy URLs in the namespace's repo, are planned. `plinthctl agent guide` prints the same |
| `ask_template` (`ask.template`) | - | the namespace's `ask-template.yml` (an `AskTemplate` of mode `plinth`), as it is |
| `ask_preview` (`ask.preview`) | `script` | what `ask_submit` and the automatic reviews would find, stored nowhere (decided D1-D3: warn on masks, refuse without Ollama, the lessons live in the agent's own file; the guide, the tool descriptions and the server instructions tell the agent to preview before it submits): `- ask lint:` ok or its findings (`ask lint --plinth`, as the runner runs it), `- DLP masks:` none or the number of values `dlp.Fixed` would mask (the count only, never the text; a warning, not a refusal, decided: the reviews are given the script with those values masked), and per automatic review type `- review <type>: <verdict>, N finding(s):` with the findings by line, or `no review (<why>)` (the prompt can't be read, the model failed); without `ollama.host` the call is refused by `ask.preview.ollama` (decided D2: a preview with no review would pass a script no model has seen); ends with `- next: ...` and the reminder to record what fails in `ai/memory/plinth-review-failures.md` of the agent's repo (the guide's "Previewing a script"; `ask_status` repeats it for a `review-failed` run); or `refused by rule <rule>: ...`; or `at most N previews per hour (plinth.preview.limit) - next: ...`; or `pending <id> - ...` when the reviews outlast 30 s (a model run each; the listener closes a reply after 2 minutes, so the reviews run in the background): `ask_preview` with that `id` and no script gives the result or pending again, for an hour |
| `ask_submit` (`ask.submit`) | `script`, optional `issue` (a number, an issue of the namespace's repo and the number its RUN_NAME ends in; `owner/repo#N`; or a page on `forgejo-url`, the number then not in the name unless the repo is the namespace's): it goes into the script as `ISSUE="<page>"` before the library line, context only; another host, a malformed value, or a script that says another `ISSUE=` is refused | `stored as <RUN_NAME> - next: ...`, followed by `- layout warning (not a finding; ...)` and the layout warnings of `asklint.Warnings` when the script's indent is off (the script is stored all the same), and `- plinthctl warning (not a finding; ...)` with the lines of a `PRIVILEGED=0` script that call `plinthctl` (stored all the same); or `refused by rule <rule>: <reason> - next: ...`; or, without an outcome within 30 s, `pending <id>` (a slow runner is fine) |
| `ask_status` (`ask.status`) | optional `run` | one line per run of the agent's own (as `get ask` orders them: not released first, then newest): `<RUN>  ask: <ask status>  answer: <answer status or -> - next: <what to do>` (names and states only, never content); then its submissions not taken in yet (`pending <id>`) and those refused in the last 24 h (`refused <id> (<time>) by rule <rule>: <reason>`, from `rejected/<agent>/<id>/reason.md`) |
| `answer_read` (`answer.read`) | `run` | the released answer (`answer[.FAIL].released.yml`, the document as it is, read through `safefs`) of the agent's own run, as often as asked; otherwise `not released yet: <its ask_status line>`, or the refusal |
| `aaa_new` (`aaa.new`) | optional `issue` (1-9 digits) | plain AAA: `plain.NewAskIn` in the agent's registered repo's `ai/work` (created, mode 0700, when missing), from `<work>/ask-template.yml` as `new-ask` takes it: `created ai/work/<RUN>/ask.sh in your repo (RUN_NAME <RUN>) - next: ...` (write it, the user runs it from `ai/work` and `plinthctl finish`es it, then `aaa_read`) |
| `aaa_status` (`aaa.status`) | optional `run` | the runs in `ai/work` and `ai/work/done` (`plain.RunsIn`: names only, never content): `<RUN>  state: <not-run, running, new, dlp-ok, DLP-FAIL or INVALID>[, gate][, in done/] - next: <what to do>`; `no runs in ai/work yet - next: aaa_new ...` without any (or without `ai/work`). They are the repo's runs, the user's and other agents' too |
| `aaa_lint` (`aaa.lint`) | `run` | `ask lint` without `--plinth` (`asklint.Check`, `RunName` the run) of `ai/work/<RUN>/ask.sh`, read through `safefs` (no symlink, no hard link; at most 1 MiB): `ai/work/<RUN>/ask.sh passes ask lint - next: ...`, or `<n> finding(s):` with one line each and `- next: fix them ..., then aaa_lint <RUN> again`; the layout warnings of `asklint.Warnings` follow the findings, or the passing line, as `- layout warning (not a finding; ...)` and one line each, and change neither. Not recorded (a check the agent repeats); a refusal is |
| `aaa_read` (`aaa.read`) | `run` | `plain.ReadAnswerIn`: the answer only as `read-answer` prints it (`answer[.FAIL].dlp-ok.yml`, decoded strictly, `status.dlp.result` ok); otherwise `read-answer`'s status line, naming `aaa_read` and `aaa_new` for `read-answer` and `new-ask`, then `(read-answer's exit code <10-14>)` |
| `message.send` | `to`, `type`, `url` | message id, or the rule that refused it (not built) |
| `message.pickup` | - | messages available to this agent: type, prompt, URL (not built) |
| `agent.up` | `agent` | when that agent's token last called (same namespace only; not built) |

- **`ask_submit` in detail:** the script is at most 1 MiB, and `decide` checks it at once as the runner will
  (`ask.size`: UTF-8, at most 500 lines; `ask.mode`: the modes the namespace allows), so a script refused there is
  never written. Otherwise it is written as `<ns-dir>/submissions/<agent>/<id>[-<issue>].sh` (`<id>`: 16 hex
  characters from `crypto/rand`), first under a dot name the runner skips and then renamed, so the runner never reads
  one half-written; the runner is asked for a poll at once (`runner.Kick`), and the server waits up to 30 s for its
  outcome (`runner.Submitted`: the runner keeps the last 256; a reload hands the submission to the next runner, which
  the wait follows). The runner's own checks (`ask lint --plinth`, the `RUN_NAME`, the events `ask.submit` and
  `ask.stored`, `rejected/`) are as for any submission (Runs, Submission).
- **`ask_preview` in detail**: `decide` checks the script as for `ask_submit` (`ask.size`, `ask.mode`, per agent), then the
  server counts the agent's previews of the last hour against `plinth.preview.limit` (default 60; the one over it is
  refused by `ask.preview.limit`, recorded, and costs no model time; its reply says when the next is free and not to retry in a loop). Nothing is written under a run or `submissions/`.
  The lint is `asklint.Check` with `Plinth: true`; the DLP masks are `dlp.Fixed` on the script (the model DLP step is for
  answers); the reviews are `runner.Preview`: the namespace's `reviewers.automatic` types, each with the prompt and model
  of a stored script's review (`review.Run`), one preview at a time per namespace behind the same Ollama semaphore. The
  result is advice and never a verdict on the later submission: the stored script is checked and reviewed on its own.
  The reviews run in a background job (the listener closes a reply after 2 minutes, a model run takes longer): the call waits
  up to 30 s, then answers `pending <id>`, and `ask_preview` with the `id` (`pv-` and 8 hex characters: a bare run of digits would reach the tool as a number) gives the result for an hour; at most 3 of an
  agent's previews are unfinished at once. Every preview that ran is the event `ask.preview` (target `<agent>`, reason: the script's sha256, the lint and mask
  counts, each type's verdict), so the user sees an agent that iterates; it is a free oracle otherwise (D1).
- **Plain AAA in detail**: the repo is the agent's current registration's (`agents.Get`: the top level
  `register` recorded); without one every `aaa_*` call says so, with the next step. The agent can write in that repo,
  so the server opens its `ai/work` as an `os.Root` of its own (`plain.OpenWorkDir`): neither `ai` nor `ai/work` may be
  a symlink, nothing below the repo leads out of it, and inside it everything is as for `new-ask` and `read-answer`
  (no symlinked run directory, no symlinked or hard-linked file read, `new-ask`'s lock). `aaa_status` and `aaa_read`
  never create `ai/work`: without it there are no runs. The agent can read the run's files itself in its repo: what
  `aaa_read` guarantees is only that it gives no answer before its DLP scan passed, which is why the guide tells the
  agent to read answers only through it, and why Plinth runs are the default.

## Ollama

- Used for the reviews, the DLP scan and the LLM step of message review, always after the fixed tooling checks, and
  through the Ollama gateway (see Ollama gateway), which holds the one
  client, with timeouts (`ollama.timeout-seconds`) and one retry after `ollama.retry-seconds` (default 10) for transient
  errors: unreachable, HTTP 5xx, empty reply.
- Every failure is an error that names its cause and never takes the process down.
- Not configured, or unreachable: every review that needs it fails (that blocks until the user's override), and the
  DLP scan of an answer waits: the answer stays unscanned until the model can be asked (`finish`
  changes nothing). Nothing is skipped.
- Package `ollama` (built 2026-10-18) is that client, only in the gateway (step 3; until then one per process); it
  follows no redirect. The semaphore is around the model calls (`/api/chat`); `/api/version`, `/api/tags` and `/api/ps` don't
  wait for it. A timeout isn't retried (it would double the wait). Its errors: `not available without Ollama
  (ollama.host is not set)`, `not available without Ollama (<host> unreachable: <cause>)`, `Ollama: HTTP <code>:
  <Ollama's error, printable, at most 200 characters>`, `Ollama: no reply within <t> (ollama.timeout-seconds)`,
  `Ollama: empty reply`. A test fake is `ollama/ollamatest`.

## Ollama gateway (built 2026-10-19)

The one way to Ollama, for Plinth's own callers only: the controller's reviews and DLP scans, and `review`,
`dlp-scan` and `ollama status`. No agent talks to it; an agent has its own model, and reads only a review written to
disk. It is the Ollama MCP, served by plinthctl itself in a container of its own on a Unix socket below `<work>`.
Package `ollamamcp`, on package `mcp`, the JSON-RPC core the Plinth MCP server will use too. It replaces the earlier start script and reviewer script (`plinthctl review` replaces them).

- **The container** (`ollamamcp.Gateway`, a `container.Service`: one description in Go, a podman backend):
  - the host's own plinthctl binary, mounted read-only at `/plinthctl` and run as `plinthctl __ollama-gateway` (a
    hidden first argument, not in the help), in an empty root (`<work>/state/ollama-root`), read-only, with no
    capabilities, `no-new-privileges`, the user's uid (`--userns=keep-id`) and 256 MiB of memory. Start makes the
    root's mountpoints itself (`dev`, `proc`, `sys`, `etc/hosts`, `etc/resolv.conf`, ...), so they are the user's: made
    by podman they could belong to another uid of its user namespace, which the user couldn't remove;
  - the host's network: Ollama may be anywhere on the LAN or a VPN (hardening after the MVP);
  - mounted: only `<work>/state/ollama/` (mode 0700), at `/sock`, where it creates `ollama.sock` (mode 0600; a
    left-over socket is replaced), and the host's CA bundle read-only (the first of Go's usual places that exists), with
    `SSL_CERT_FILE` pointing at it, for an `https` `ollama.host`;
  - the `ollama:` settings as arguments (`-host`, `-timeout-seconds`, `-retry-seconds`, `-max-parallel`), so it reads
    nothing of `<work>`: no config, no secret;
  - the binary must be static (`CGO_ENABLED=0`, as `../build.sh` builds it): the root has no C library. A binary that
    isn't is refused: `the gateway's binary <path> is not static (build it with CGO_ENABLED=0)`;
  - labelled `plinth.service=ollama-gateway`, `plinth.work=<work>` and `plinth.spec=<digest>`: the digest of the
    description and of the binary's sha256. Its name is derived from `<work>`, so each `<work>` has its own gateway.
    The controller's removal of left-over containers takes only the runs' (`plinth.run`), never a service's.
- **Its life:** `plinthctl ollama-mcp start` starts it; one of the same digest that runs already is left alone
  (`running`), another is replaced (`replaced`: a new binary, or changed `ollama:` settings). Start waits up to 10 s
  until the socket takes connections, else it fails with the container's last lines. `plinthctl ollama-mcp stop` ends
  it (`podman rm --force`, 5 s before SIGKILL; a stop lets the calls in flight end).
  - The controller starts it at its start while `ollama.host` is set, before its runners' first reviews, checks every
    10 s that it runs and starts it again when it is gone, and replaces it when the binary or `ollama:` changed (at its
    start: the global config is read only then). One that can't start is said once on stderr and in the log; the
    controller goes on, and the reviews and DLP scans fail closed until it runs.
  - When the controller stops, the gateway stays: plain AAA shares it. `ollama-mcp stop` while the
    controller runs says that the controller starts it again.
  - `plinthctl ollama-mcp status`: running since when, or stopped with podman's error, or not running (exit `1`);
    whether it is the one this binary and `config.yml` describe; its last 10 lines.
- **Its lines:** stdout, kept by podman: the start (`serving /sock/ollama.sock for Ollama <host> (max-parallel <n>)`),
  a line per tool call (`ollama_chat: 4.2s, ok`) and the stop. Nothing a call sends or gets goes into a line.
- **The callers' side** (`ollamamcp.Client`, an `ollama.Chatter` for the reviews and the DLP scan): a tool call per
  request over the socket, a new connection each time, no `initialize` first (the server is stateless).
  - Without `ollama.host` it asks nothing: `not available without Ollama (ollama.host is not set)`.
  - Nobody at the socket (no socket, or one nobody serves): `the Ollama gateway isn't running (plinthctl ollama-mcp
    start)`. Nothing starts it but `ollama-mcp start` and the controller: `review` writes `.FAILED.md`
    with it, `dlp-scan` exits `1`, `ollama status` says `unreachable: <it>`.
  - A tool's error comes back as it is: the gateway's client's, which names its cause (`Ollama: HTTP 404: model 'x'
    not found`, `not available without Ollama (<host> unreachable: <cause>)`).
- **Its client** is the one Ollama client (package `ollama`, see Ollama): the semaphore (`ollama.max-parallel`), the
  timeouts and the retry are the gateway's, the same for every caller of the `<work>`, the controller's runners and the
  plain commands alike.
- **The protocol:** MCP over HTTP, JSON-RPC 2.0, stateless, on the socket (the socket's mode is the access control: no
  key, no listen address).
  - One JSON-RPC message per `POST /mcp`, the response as its body (`application/json`); a notification gets 202.
  - No session id and no SSE stream (`GET` gets 405); a batch gets 400; only `Content-Type: application/json` is
    taken (415 otherwise).
  - `initialize` answers with the protocol version the client asks for when it is one of `2025-06-18`, `2025-03-26`
    and `2024-11-05` (else `2025-06-18`), and the capability `tools`. Besides it: `ping`, `tools/list` and
    `tools/call`; any other method gets -32601.
  - A request is at most 8 MiB, and at most 256 are handled at once (they wait at the semaphore: every review and DLP
    scan of every namespace queues here); more get 503 with `Retry-After: 5`.
  - Timeouts: 10 s for the headers, 1 minute for the request. A call waiting for Ollama ends when its client goes away
    or the gateway stops; a stop lets the calls in flight end for 5 s.
- **The tools:** a tool's error is a result with `isError` set and the error's text. An unknown argument is an error,
  not ignored.

  | Tool | Arguments | Result (text) |
  |---|---|---|
  | `ollama_chat` | `model`, `prompt`; optional `system`, `format` (`"json"`, or a JSON schema the reply must follow: the reviews' and the DLP scan's structured output), `options` (Ollama's model options: `temperature`, `seed`, `num_ctx`, ...), `keep_alive` (e.g. `5m`) | JSON: `model`, `reply`, `done_reason` (`length`: cut off), `eval_count`, `total_duration_ms`. One user message, not streamed; an empty reply is retried once |
  | `ollama_list_models` | - | Ollama's `/api/tags`, as it answered |
  | `ollama_ps` | - | `/api/ps` |
  | `ollama_show` | `model` | `/api/show` |
  | `ollama_version` | - | `/api/version` |

  No tool pulls, deletes or creates a model.
- **Until step 3** (2026-10-18) the Ollama MCP listened on TCP (`ollama.mcp.listen`, `.port`, an API key in
  `ollama.mcp.env.api-key`) and the controller served it inside its process. Those keys are gone: a config that still
  has `ollama.mcp` is refused (`field mcp not found`).

## Config

Two levels: `<work>/config.yml` (global) and `<ns-dir>/config.yml` (namespace); a namespace inherits every namespace
key it doesn't set from the global config (see below). `<work>` comes from `--work` (default
`~/.plinth`), never from a file (decided). The templates Plinth writes when a config is missing (see Process,
Setup) are also the annotated examples with every key: `src/internal/workdir/config.yml.dist` and
`src/internal/workdir/ns-config.yml.dist`, embedded in the binary. Real configs are never tracked in git. A value still
holding a `<...>` placeholder from a template stops Plinth at start, like an unknown key.

The loader (package `config`) is strict and fails closed: an unknown key, a wrong type, a value out of range, a review
type that doesn't exist, a `<...>` placeholder left from a template, more than one YAML document or a file that is a
symlink stops the process at start. It lists **every** problem of a file at once (`plinthctl config check` prints them). An unknown key or a wrong type names
its key path and that key's line in the file: `ollama.mcp: field mcp not found in type config.Ollama (line 6)`,
so the plain commands, which stop only on a problem of their own keys, stop on it too.
A secret can come from the environment through `${VAR}`, e.g. `forgejo-token: "${PLINTH_FORGEJO_TOKEN}"`, or be written
literally; keys ending in `-env` hold the **name** of an environment variable. If `<work>` is a git repo, the user keeps
the config files out of it (`.gitignore`). Agent tokens are never stored, only their sha256 (see Agent tokens).
Workflow files (ask scripts and answers) never carry secrets.

- **Expansion:** in string values a leading `~` becomes `$HOME` and every `${VAR}` its value. An unset `${VAR}` expands
  to an empty string and is a **warning**, not a problem: `ollama.host: "${OLLAMA_HOST}"` with `OLLAMA_HOST` unset
  means Ollama is not configured (the documented "absent"), while a required key that ends up empty (`forgejo-url`) is
  a problem. `config check` names every unset variable, and a problem of a key whose `${VAR}` expanded to nothing names
  it: `forgejo-url: required (${FORGEJO_URL} is not set)`.
- **Review prompts are literal:** `reviewers.type.<name>.definition` is neither expanded nor searched for placeholders,
  since a prompt about bash contains `${...}` and `<...>` itself; it counts as a placeholder only when the whole value
  is one, as in the template.
- **Environment the controller strips:** every variable named in `${VAR}` or by a `-env` key (see Process, Exec).
- Ranges (problems outside them): seconds and minutes from 1 (`ollama.retry-seconds` from 0) to an hour or a day,
  ports 1-65535 and the two listener ports distinct, URLs `http(s)://` with a host and no `user:password`,
  `aaa-raw-dir` absolute, agent names and review and message type names by their patterns.

The top-level keys are `forgejo-url`, `forgejo-token`, `reviewers`, `ollama` and `plinth` (decided: `plinth:`, not
`plinthctl:`).

Global:

| Key | Default | Used for |
|---|---|---|
| `forgejo-url` | - (required) | issues, Save, message URLs, the web UI login |
| `forgejo-token` | - | Plinth's own Forgejo token (Save, message review, the review prompts, Report), usually `"${PLINTH_FORGEJO_TOKEN}"`; scopes: issue Read and write, repository Read, nothing else, on a dedicated account with access only to the repos Plinth uses (`../README.md`) |
| `reviewers.type.<name>.urls` / `.definition` | - | the review prompt: the URLs' contents joined in order, or the definition |
| `ollama.host` | - (absent = not configured) | the Ollama URL |
| `ollama.models.review` / `.dlp` | - | the models |
| `ollama.timeout-seconds`, `ollama.retry-seconds`, `ollama.max-parallel` | 300, 10, 1 | |
| `plinth.mcp.listen` / `.port` | `0.0.0.0`, 8791 | the Plinth MCP server |
| `plinth.web-ui.listen` / `.port` / `.public-url` | -, 8790, - | see the web UI spec; on the VM network, behind the Forgejo login |
| `plinth.web-ui.auth.client-id` / `.users` / `.session-hours` | -, -, 12 | the login; the UI refuses to start without them |
| `plinth.web-ui.refresh-seconds`, `plinth.messages.refresh-seconds` | 5, 5 | |
| `plinth.tui.refresh-seconds` | 2 | how often `plinthctl tui` reads the files again (1-60; see `plinth-tui.README.md`) |
| `plinth.messages.catalogue-url` | - | `plinth-messages.yml`; unparsable = the bus stops |
| `plinth.runner.poll-seconds`, `.max-parallel-exec`, `.exec-timeout-minutes` | 1, 1, 120 | |
| `plinth.exec.env-file` | `""` (none) | the env file in the namespace's directory (Process, Exec); a namespace key with a global default |
| `plinth.exec.image` | - | the namespace's pinned image: `ref` (`<name>@sha256:<digest>`), `commit`, `tree`; namespace only, written by `image publish` and `image use` (Run modes, The image; written since 2026-10-16; the controller runs only it since 2026-10-17) |
| `plinth.exec.image-ref` | - | retired 2026-10-17 with the build from the agent's checkout: still read, so an old config loads, with a warning to remove it |
| `plinth.exec.memory` | `512m` | a run's memory limit, swap included: `<n>k`, `<n>m` or `<n>g`, `6m` to `64g` (The container); a namespace key with a global default |
| `plinth.exec.network`, `.privileged` | false, false | allow scripts that ask for network or privileged mode (Run modes; privileged runs aren't built yet); namespace keys with global defaults |
| `plinth.event-log-max` | 10000 | lines per events file before a new file starts |
| `plinth.log.max-age-days` | 30 | the general log: files older than this are what Scrub logs deletes (see Log) |
| `plinth.notice.expires` | 14 | days a notice stays open after its last line unless cleared (see Notices); 1 to `plinth.log.max-age-days` |
| `plinth.preview.limit` | 60 | the `ask_preview` calls an agent may make per hour; 1 to 1000 |
| `plinth.aaa-raw-dir` | `<work>/aaa-raw` | plain AAA: where `dlp-scan` keeps the untouched originals (`<RUN>/`) and `scrubber` deletes them (decided) |
| `plinth.repo` | - | `<owner>/<repo>` on `forgejo-url` where Save and Report post, one issue per run (see The run's issue); neither without it. Replaces `plinth.failed-scans.repo` (`config check` refuses the old key). Make it a private repo |
| `plinth.autosave.runs` | `false` | `true`: Save runs by itself right after Done |
| `plinth.autosave.failures` | `false` | `true`: Report runs by itself at a run's first failed check, once per run |
| `plinth.policy` | `{0: allow, 1: allow, 2: allow, 3: require-acceptance, 4: require-acceptance, 5: require-acceptance}` | the action at each risk score; a namespace may tighten it, never loosen it (Risk score and policy; not built) |
| `plinth.ansible.source-address` | - | the address the hosts see the controller's runs come from, for the user certificates' `source-address`; required for remote runs (Ansible runs; not built) |
| `plinth.ansible.cert-minutes` | 60 | a user certificate's lifetime from the signing, and the cap on a remote run's timeout; a namespace key with a global default (Ansible runs; not built) |
| `plinth.risk-factors` | the defaults in Risk score and policy | the factor scores, by factor name (Risk score and policy; the key is a proposal; not built) |

Namespace keys, set in the global config as the default for every namespace and in a namespace's config to replace it
for that repo (decided):

| Key | Built-in default | Global template | Used for |
|---|---|---|---|
| `reviewers.automatic` | `[]` | `[security, dlp]` | review types that run on their own and must pass (or be overridden) |
| `plinth.messages.expiration-minutes` | 60 | 60 | |
| `plinth.messages.ungated.agents` / `.messages` | `[]` | `[]` | messages that skip the user's approval (never the tooling or LLM review); agent names only |
| `plinth.policy` | the global policy | (not set) | stricter actions for some scores; not taken whole like the others: per score the stricter of the two applies, and a looser entry is a problem (Risk score and policy; not built) |
| `plinth.risk-factors` | the global scores | (not set) | factor scores for this repo, raised or lowered; per factor name, not taken whole (Risk score and policy; not built) |
| `plinth.ansible.cert-minutes` | 60 | 60 | the user certificate's lifetime for this repo (Ansible runs; not built) |

- **Precedence:** the namespace's value, else the global value, else the built-in default. The value is taken whole: a
  namespace list replaces the global list, it is never merged with it. `automatic: []` in a namespace is how a repo
  turns automatic reviews off; leaving the key out keeps the global list. An empty list, in the global config or a
  namespace's effective one, loads but is warned about (`config check`, the controller's log): scripts there are approved
  without any review.
- **Why:** a new repo must start with the global defaults (the automatic reviews above all) without anyone having to
  remember to edit its config. So `ns-config.yml.dist`, which `register` writes for a new namespace, has every key
  commented out: it sets nothing, and a test keeps it that way. A global-only key (the table above) in a namespace
  config is refused like an unknown key.

Namespace-only key (decided), refused in the global config like an unknown key:

| Key | Default | Used for |
|---|---|---|
| `plinth.agents.enabled` | none | `<agent>: <repo>`: the agents the user enabled, each with the top level its registration must come from (an absolute, clean path); see Process, Register |

- **Edited by key:** `agent enable` and `disable` (later the TUI and the web UI) edit the file through yaml.v3's node
  tree, so every other key and every comment stays; a file with no document yet (the template: comments only) keeps
  its text as it is and gets the block appended. The edited file must load; otherwise it is put back as it was and
  the command says why. `config check` shows the key's effective value like the others.

From the earlier `aaactl` config this replaces:
- `aaactl.*` becomes `plinth.*`, and `aaactl.messages.port` becomes `plinth.mcp.port`.
- `git-stack.forgejo.*` goes: the repo is the namespace, and the agent's `mcp-token` is the launcher's business, not
  Plinth's.
- `work-dir` leaves the file: it is the `--work` flag.
- Plain AAA's `~/aaa-raw` becomes `plinth.aaa-raw-dir`, default `<work>/aaa-raw`. Originals already in `~/aaa-raw` stay
  there until the user moves or deletes them.

## Layout of `<work>`

```
~/.plinth/                            (mode 0700; --work; may be a git repo, see Config)
  config.yml  ask-template.yml      (the AskTemplate new-ask starts plain AAA runs from; written from the base when missing)
  state/            .gitignore        plinth.lock, plinth.sock; ollama/ollama.sock, ollama-root/  (the Ollama gateway's)
  aaa-raw/          .gitignore        <RUN>/  (plain AAA: the DLP scan's untouched originals; plinth.aaa-raw-dir)
  logs/             .gitignore        plinth-YYYY-MM-DD.log  (the general log, one file per day)
  requests/         .gitignore        process-level requests: logs-scrub.queued
  ca/               .gitignore        user-ca(.pub), host-ca(.pub)  (the two Plinth CAs; not built)
  <owner>/<repo>/                     the <ns-dir>
    config.yml  ask-template.yml    (the AskTemplate, mode plinth, ask.template gives; written from the base when missing)
    runs/           .gitignore        <agent>/<RUN>/...
    submissions/    .gitignore        <agent>/<id>.sh
    rejected/       .gitignore        <agent>/<id>/  (refused submissions with reason.md, kept for analysis)
    requests/       .gitignore        <agent>/<RUN>/<action>.queued, messages/<id>/...
    raw/            .gitignore        <agent>/<RUN>/  (the DLP scan's untouched originals; unexpected/: state files a script wrote; ask.sh and lib.bash: what the run ran and sourced)
    messages/       .gitignore
    events/         .gitignore        <first-seq>.jsonl
    ansible/        .gitignore        inventory.yml, known_hosts, vault.yml  (the user's; not built)
    state/          .gitignore        heartbeat, image.lock; git/ (only during image publish)
      agents/       .gitignore        <agent>.sha256, <agent>.repo  (written by register)
                                      <agent>.pending.sha256, <agent>.pending.repo  (from another repo, until agent enable)
```

Each `.gitignore` holds `*` and `!.gitignore`. Plinth creates every directory with mode 0700 and every file with 0600
(ask scripts 0700).

Plain AAA, in `<repo>/ai/work/`: `<RUN>/` (queued, running and finished runs), `done/<RUN>/`, `failed/<RUN>/` (`new-ask`
failures, kept for analysis) and `.new-ask.lock`.

## Source and build

- Source in `../src` (Go module `plinth`, `go 1.25`; the module name is local only and names no host):
  - `cmd/plinthctl/` (`main`);
  - `internal/` packages: `cli` (dispatch, `help`), `config`, `names` (patterns, `run-namer`), `plain` (the no-backend
    commands, the AAA library), `doc` (the YAML documents: the strict decoder, the kinds, Plinth's encoder, the
    text filter), `safefs` (symlink-free, durable file access below an `os.Root`), `workdir` (the
    `<work>` and namespace setup, config templates, the namespaces set up), `agents` (tokens, `register`,
    `agent token-rotate` and `token-remove`, the pending slot and `Promote`, `Enabled`), `logfile` (the general log, its max age and scrub),
    `gitrepo` (a repo's top level, its `origin`'s namespace and host), `runstate` (the shared view model: a run's
    state from its file names), `asklint` (ask lint),
    `decide`, `auth` (callers, one constructor per channel), `events`, `request` (a run's request files, and the
    facts `decide` needs for them), `controller` (the process, its socket and the terminal's side of it; also `Running`
    and `Live`: whether it runs, which runs are live, for the commands and the TUI; its tests run a whole controller,
    its runners included), `runner` (a namespace's part: submissions, requests, reviews, DLP scans and runs), `container` (a run's podman container and the namespace's image; a service's container), `mcp` (the JSON-RPC core of the MCP servers, and a client), `ollamamcp` (the Ollama gateway: its MCP, its container, its client), `ollama`, `review`, `dlp` (the fixed-pattern
    masks so far), `forgejo`, `webui`, `tui` (the screens: a model, its keys and its view), `term` (raw mode, keys, the
    screen and the sanitizer).
- Each command and package has Go tests; they replace the `test-*` scripts.
- Build in rootless Podman: `../build.sh` runs `podman build --network none` on the two-stage `../Containerfile` (build
  context: an allowlist in `../.containerignore` of `src/go.mod`, `go.sum`, `cmd/`, `internal/` and `vendor/` only, so
  `src/config.yml`, `.claude/` and anything else in `src/` never reach the build; a step right after `COPY` fails the
  build if anything else, or any `*.yml`/`*.yaml` outside `vendor/`, got in, except the two embedded config templates in
  `internal/workdir/`).
  - **Version** (decided): `build.sh` takes it from `git describe --tags --always --dirty` on the host (the build
    context has no `.git`) and passes it as a build argument. No date. A build from a modified tree gets the suffix
    `-dirty` (decided).
  - Stage 1, the builder image `docker.io/library/golang:<tag>@sha256:<digest>`, pinned by tag and digest (decided),
    copies `src/` and runs, offline (`GOTOOLCHAIN=local`, `GOPROXY=off`), each step in POSIX `sh` with `set -eu` (Podman's
    default OCI format ignores `SHELL`, so a bash step could pass without running its check):
    1. `gofmt -l cmd internal` must list nothing (our code; `vendor/` stays as upstream released it);
    2. `go mod verify`, only while there is no `vendor/` (it needs the module cache; see Vendoring);
    3. `go vet ./...`;
    4. `go test ./...`;
    5. `CGO_ENABLED=0 go build -trimpath -ldflags "-X plinth/internal/cli.Version=<v>"`
       (`-mod=vendor` once `src/vendor/` exists);
    6. `plinthctl version`.
  - Stage 2, `scratch`, holds only the binary; `podman build --output type=local,dest=out` writes it to
    `../out/plinthctl` (no container to create and remove). `build.sh` has it written to a directory in `out/` and
    renames it into place only once the build succeeds: a failed build leaves the old binary and says so, so
    `plinthctl` keeps working meanwhile. It ends with `out/plinthctl version`.

  A failing step fails the build. The output is a static `plinthctl`, installed to `~/.local/bin/`.

- **Vendoring**: the one dependency, `go.yaml.in/yaml/v3` (approved), lives in `src/vendor/`
  and is committed, so the build never downloads anything. It is (re)made on the host in the same pinned builder image,
  the only step with network: `go get <module>@<exact version>`, `go mod tidy`, `go mod verify` (module cache against
  `go.sum`, `go.sum` against the checksum database `sum.golang.org`), `go mod vendor`. The version is pinned exactly in
  `go.mod`; the diff of `src/vendor/` is reviewed and committed like any other code. Updating it is the same steps with
  a new version, in its own commit.
