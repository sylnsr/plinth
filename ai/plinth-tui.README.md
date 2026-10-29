# Plinth TUI - the user's window on Plinth, in the terminal

This file is the spec of `plinthctl tui`, Plinth's terminal UI (like k9s for kubectl). It is the web UI
(`plinth-web-ui.README.md`) for the user's own terminal on the host: the same pages, the same actions under the same
names, enabled and disabled for the same reasons, each writing the same request. The core and the commands are in
`plinthctl.README.md`. The code follows this file: change the spec here first, then the code.

**Status: decided.** Both UIs show entire files.
The pager opens every file of a run in full, `ask.sh` included; the web UI does the same (`plinth-web-ui.README.md`, Run
tab, Files).

## Purpose

The same as the web UI's: see every run of every agent in every namespace, its stage and state, and request the actions
that apply to it (exec, done, save, scrub, report, review, override); see the logs; later, the conversations. The agent
only writes ask scripts and sends messages. The user is the only one who starts a run or releases an answer, and does
both here, in the web UI or with the matching `plinthctl` command.

It differs from the web UI only where the terminal makes it differ:
- **No login.** It runs as the user, in the user's own terminal on the host, with the same trust as any `plinthctl`
  command. No agent can reach that terminal. It opens no port.
- **Privileged and interactive scripts run here** (see Actions): a `PRIVILEGED=1` script, and every `INTERACTIVE=1`
  one, runs only in the user's terminal, and the TUI is one.

## Where it sits

- `plinthctl tui [-n <ns>] [--work W]` is a process of its own, not part of the controller. It works whether or not the
  controller runs: requests queue, and the screen says "controller not running". `-n` starts it filtered to one namespace.
- **It only reads, and queues.** It reads the same files as the web UI and the commands do, through the same packages
  (`runstate`, `request`, `events`, `logfile`, `config`), and writes nothing but request files (package `request`, the
  same `Queue` the commands use) and its lines in the general log (`tui: queued exec of main/calm-otter in
  michael/devops`). Every action first asks `decide` the way the loop will (`request.Decide`), so a refusal shows at once
  with its rule; the controller decides again when it carries the request out.
- It never runs a tool, never talks to Ollama, Forgejo or an agent, and opens no network connection. The exceptions are
  `e` on a privileged run (Actions), which runs `plinthctl exec`, and the Image screen. It runs the user's own `plinthctl image` commands as child
  processes, handing them the terminal for anything that asks, and asks podman about this machine (`podman login
  --get-login`, `podman image exists`). Those commands talk to the registry and Forgejo and write the pin and their
  lines in the general log; the TUI itself still writes only request files and its log lines.

  | Path | What | Mode |
  |---|---|---|
  | `<work>/<owner>/<repo>/runs/` | runs | read |
  | `<work>/<owner>/<repo>/requests/` | request files | write (new files only) |
  | `<work>/<owner>/<repo>/events/` | the event log | read |
  | `<work>/<owner>/<repo>/state/` | heartbeat, registered agents | read |
  | `<work>/config.yml`, `<work>/<owner>/<repo>/config.yml` | settings | read |
  | `<work>/state/plinth.lock` | whether the controller runs (a shared `flock` test, as the commands do) | read |
  | `<work>/logs/` | the general log | read; append its own lines |
  | `<work>/requests/` | `logs-scrub.queued` | write (new file only) |

  It never reads `raw/` (the DLP scan's untouched originals), `running.yml`, an answer that hasn't been scanned
  (`answer[.FAIL].new.yml`) or an `INVALID` one: the user sees an answer only
  after its scan, with its masks, as in the web UI.

## Runtime

- Go standard library only (no tview, no tcell; `ai/README.md`). Two packages:
  - `term`: raw mode (`ioctl` `TCGETS`/`TCSETS` through `syscall`), the window size (`TIOCGWINSZ`, and `SIGWINCH` for a
    resize), job control (is the process group orphaned: `/proc/<pid>/stat`), the key decoder, the screen buffer and
    the sanitizer (see Untrusted text). The keys come through `term.Input`, which never blocks in `read(2)`: it waits
    for a byte with `select(2)`, at most 50 ms at a time, so it can pause while a command has the terminal (a read
    blocked in the kernel would take that command's first keys).
  - `tui`: the screens. A model, an update function (key or tick -> new model + effects) and a view function (model ->
    lines). Effects (loading files, queueing a request) are the only I/O, so the screens are tested without a terminal.
- **Terminal:** stdin and stdout must be terminals; otherwise `plinthctl tui: needs a terminal` and exit `1`. It uses
  the alternate screen, hides the cursor and speaks plain VT100/xterm sequences (cursor position, erase line, SGR
  colours), which every terminal in use understands; no terminfo. No mouse, no bracketed paste.
- **Size:** at least 80 x 24 (the 7-row header; it was 80 x 19 before): at 80 x 24 the
  Runs list keeps 14 rows (24 - 6 header - divider - headings - message - breadcrumb). A smaller window shows only "terminal too
  small (W x H, needs 80 x 24) - Ctrl-C quits" until it grows, and no key but Ctrl-C and Ctrl-Z acts meanwhile (nothing
  happens that the user can't see). What doesn't fit scrolls or gives way: the lists and the pager scroll; Help (`?`)
  scrolls with `↑` `↓` `PgUp` `PgDn` `g` `G`, its title saying which lines show, and any other key goes back; a
  confirmation box too long for the screen ends in "… n more lines", so its question and input line always show; its
  lines wrap.
- **Always restores the terminal:** on `Q`, Ctrl-C, `SIGTERM`, `SIGHUP` and a panic (recovered: the terminal is
  restored first, then the panic goes to stderr and the general log, exit `1`). Ctrl-Z restores it and stops the
  process; `SIGCONT` enters raw mode and the alternate screen again and redraws. Where no shell runs it as a job
  (`ssh host plinthctl tui`, `sh -c`: the kernel drops the stop to an orphaned process group, and nothing would send
  `SIGCONT`), Ctrl-Z only says "not suspended: no job control" and the TUI stays.
- **Refresh:** it reads the files again every `plinth.tui.refresh-seconds` (global, default 2, 1-60) and after every
  action. Only lines that changed are redrawn. A row is followed by an erase-to-end-of-line only when it is narrower
  than the screen: with autowrap off the cursor stays on the last column of a full row, and the erase would take that
  column's character (the `>` of a cut line, the last letter of a header).
- **Another program on the terminal** (the Image screen's commands): the input pauses first, then the TUI leaves the
  alternate screen and raw mode. When the command ends, the terminal gets back the mode saved before it, whatever the
  command left (`podman login` killed at its password prompt leaves echo off); the mode is read anew only at `SIGCONT`.
- **Colours:** the 8 basic ANSI colours, and the breadcrumb bar's 24-bit `#A863F8` (see Screens); none when `NO_COLOR` is
  set. A state is never shown by colour alone: its name is always there.
- **Glyphs**: the rule for characters (plinthctl.README.md): Unicode only on screen, with its ASCII fallback when the
  locale isn't UTF-8 (`term.UTF8` on the TUI's environment): the wrap mark `↪` or `>`, the divider `─` or `-`, what gives way `…` or `...`
  (`<work>`, the breadcrumb, a long confirmation box). The keys are written `<up>`, `<down>`, and so on, in Help.
- stdout belongs to the screen. After exit, stderr gets only errors.

## Untrusted text

Scripts, answers, reviews and file names come from agents and from what their scripts printed. A terminal acts on
escape sequences in what it is sent, so a byte that reaches it unfiltered could move the cursor, rewrite the title,
write to the clipboard (OSC 52), hide or reorder text, or forge the TUI's own lines. **Every string from a file goes
through the sanitizer before it reaches the screen**, the terminal's equivalent of the web UI's HTML escaping:
- control characters (C0 except tab, `ESC`, `DEL`, C1 `U+0080`-`U+009F`) become visible escapes such as `\x1b`, in
  reverse video; a tab becomes spaces;
- invalid UTF-8 becomes `\xNN`;
- Unicode bidi controls and invisible characters (`U+200B`-`U+200F`, `U+202A`-`U+202E`, `U+2060`-`U+2069`, `U+FEFF`)
  and any other rune `unicode.IsPrint` rejects become escapes such as `\u202e`, so text can't be reordered or hidden
  (Trojan Source);
- wide characters count as two columns; a line longer than the screen is cut, with `>` at its end.

The TUI's own text never passes through the sanitizer; file content never bypasses it. A test renders a fixture answer
full of escape sequences and checks that the frame holds none.

## Screens

Every screen has the same frame, k9s style (from testing the TUI; Plinth's users know k9s),
top to bottom:

1. **The header, 6 rows.** On the left one item per row (the Plinth version is not shown: `<ctrl-v>` says it on the message line), the labels aligned: `Work:` `<work>`
   (shortened from its left, `…/end`, when it doesn't fit), `Controller:` `PID <n>` (`running` when its PID can't be
   read, `not running` in bold; the one process, every runner inside it), `Ollama:` its status, `Namespace:`
   the filter or `all`, `Agent:` the filter or `all`, `Released:` `shown` or `hidden`. While notices are open
   (what the controller told stderr, which a controller started in the background has no terminal for; they are
   read from the general log, whether the controller runs or not), the Controller row adds `, <n> notices` in bold (`1
   notice`), and the message line, while it has nothing else, says `<n> notices: <the newest one's message> - <what to
   do>`. On the right the **hints**, the screen's own keys (the actions of the selection, the filters and the screen's actions) in two columns of 23 (the left
   block takes the rest: 34 columns at 80), each `<key> Label`, the key bold, filling the first column top to bottom,
   then the second. An action that doesn't apply to the selection isn't shown; one that applies but can't act now is,
   and pressing it says why on the message line. Navigation (`↑ ↓ k j PgUp PgDn g G Enter`), `Esc`, the `E L I`
   shortcuts and `Q` aren't shown (k9s doesn't show them either); `<?>` lists them. The general keys, `<:> Screens` and `<?> Help`, are not among the hints: they are at the right end of the breadcrumb bar. A screen with more than 12 hints The hints are light gray (`#A8A8A8`), the keys bold, so they are less bright than the text.
   shows the first 12. While a confirmation, Help, a picker or the filter is open, there are no hints:
   those say their keys themselves.
   Under it a **divider** across the screen in the bar's colour (`─`, `-` without a UTF-8 locale).
2. **The picker or the filter**, only while one is open (`:`, `n`, `a`, `/`): its input line, then a picker's entries
   (at most 6, the selected one in reverse video). The rows below move down meanwhile.
3. **The screen**: its title line where it has one (the Run screen's run, the pager's file and line, the Logs
   directory; the Runs list has only its column headings, with `filter: <text>` above them while a filter is set),
   then its list or pager.
4. **The message line**: the outcome of the last action, or an error; directly above the breadcrumb. It is orange (the colour of the operators in the coloured YAML and `ask.sh`), an error red.
5. **The breadcrumb bar**, the last row: `<michael/devops> <runs> <jolly-cloud> <events>`: the namespace filter
   (`<all>` without one), then each screen on the way here, the Run screen as its run and the pager as its file, the
   last in bold. It gives way from its left (`…`) when it doesn't fit. Black on `#A863F8` (24-bit colour, SGR
   `48;2;168;99;248`; with `NO_COLOR` reverse video, which isn't colour). Its black is 24-bit too
   (`38;2;0;0;0`): Konsole and other terminals draw bold palette black as the bright colour, near white on the bar
. The **general keys** take the right end of the bar, `<:> Screens  <?> Help` (the keys bold, a space before the bar's end), on every screen where they work: Runs, Run, Events, Logs and Image; the pager has only `<?> Help` (`:` doesn't work there). The breadcrumb gives way from its left to what is left. None while a confirmation, Help, a picker or the filter is open (this replaces the separator in the header that the issue first proposed).

**Actionable text** is light blue (the colour of the values in the coloured YAML): the rows one selects and opens with `Enter` or acts on, that is the lists of Runs, the Run screen's files, Logs (the files and the lines), Image and the screen picker; the selected row is reverse video. Text that only informs stays in the normal colour.

**Key notation**: a key is written as typed, in angle brackets, lowercase: `<e>` a plain key, `<shift-r>` with
Shift, `<ctrl-d>` with Ctrl, `<:>`, `</>`, `<?>`; so a hint can't be misread (a bold `D` in `Done` read as Shift-d).
The Ollama status is "not configured" without `ollama.host`; with it, "configured" until the controller publishes its
status (the Ollama slice).

**The screen picker** (`:`) lists `Runs`, `Events`, `Logs`, `Image` under the header (`Conversations` once it is
built). `↑`/`↓` select, typing narrows the list to the entries that hold it, whatever the case (`lo` leaves `Logs`),
`Enter` switches, `Esc` closes it and leaves the screen as it was. `E`, `L`, `I` are its shortcuts. `Runs` goes back to
the start screen; another screen opens over Runs, or over the Run screen while one is open, for its run (Events: the
run's events; Image: its namespace's); the screens above those close first, so going from screen to screen doesn't
pile them up. `:` and the shortcuts work on Runs, Run, Events, Logs and Image.

### Runs (the start screen)

The web UI's run tabs, as one list (k9s style):

| RUN | NAMESPACE | AGENT | ASK | ANSWER | ACTION STATE | AGE |
|---|---|---|---|---|---|---|

- ASK and ANSWER show the statuses of `get ask` and `get ans` (`plinthctl.README.md`, Runs, States). ACTION STATE shows the run's
  pending and failed requests (`exec running`, `done FAILED`).
- Order as in the web UI: not released first, newest first; released runs only with "show released" on (`t`).
- Columns are cut to fit (a cut cell ends in `>`); at 80 columns every column still shows, but `released (gate)` is cut.
  More width goes first to ANSWER (up to 15, that one), then a little to RUN and NAMESPACE, then to ACTION STATE.
- Filters: `n` and `a` open a picker under the header (`all`, then each namespace, or each agent: the registered
  ones, with runs or without, and any agent a run names; it works as the screen
  picker does), `/` filters by run name (Esc clears it). The header shows the namespace and agent filters, the row
  above the column headings the run filter.
- A namespace whose heartbeat is older than three `plinth.runner.poll-seconds` (or whose controller isn't running) shows
  "no controller" in its runs' ACTION STATE (short, so it fits the column; the header says `not running`);
  one the controller doesn't serve because its config doesn't load shows "not served: plinthctl config check -n <ns>".
- `Enter` opens the run.

### Run

The web UI's tab for one run:
- **Header:** run, namespace, agent, stage, ask status, answer status (an answer that waits for the model:
  `waiting-for-model` in the row, and here what it waits for and why, from the run's `dlp.scan` events), the mode (normal, network or privileged),
  `INTERACTIVE`, for a network run whether it is accepted, and the issue URL (as text; the TUI opens nothing): the page in the script's `ISSUE=` line, else for a run ending in
  `-<digits>` `<forgejo-url>/<owner>/<repo>/issues/<digits>`.
- **Script header:** the leading comment lines of `ask.sh`.
- **Reviews:** one line per review, with its verdict or failure reason; automatic types without a file are "pending".
- **Answer:** its DLP result (passed, overridden, failed, with the reason from `status.dlp`) and its first lines (a
  document's section titles as `## <title>` and their output, then its failure), once it has been scanned, before and after Done, a failed scan included (decided). While it is
  being scanned, and when it is `INVALID`: only the state.
- **Requests:** pending ones, and failed ones with their reason (the `.FAILED` content), a failed one only until a retry of the same action is under way or has succeeded (the file stays, the line goes). A finished one stays in
  `requests/` until Scrub, but isn't shown.
- Every field and line above **Files** that holds text from a file (the script header, the reviews, the answer, a
  request's reason) wraps to the screen's width, as the pager does; the file rows are cut.
- **Files:** every file with size and modification time.
- The run's actions are in the header's hints only, not in the pane. The cursor moves over
  **Files**, whose heading says that `Enter` opens one (so the hints don't): `Enter` on a review, the answer, `ask.sh` or any other
  file opens it in the pager; an answer document shows as it is. The pager refuses `running.yml`,
  `answer[.FAIL].new.yml` and `answer[.FAIL].INVALID.yml` ("not scanned yet").
- `E` shows the run's events.

### Pager

A file or the event log, read-only: `↑`/`↓`, `PgUp`/`PgDn`, `g`/`G` (start, end), `/` to search, `Esc` back. Long
lines **wrap**: word wrap, at the last space that fits, at the column
when a word is longer than a row; a wide character, a combining mark with its base and an escape of the sanitizer never
split, and a resize wraps again. A continuation row starts with the wrap mark (`↪`, `>` without a UTF-8 locale; the
rule for characters, plinthctl.README.md) and a space, dim, so it can't be taken for a line of its own; the mark is
never part of the text. `w` switches to the sideways scroll (`←`/`→`, the lines cut at the edge) and back; the title
says `scroll: at line N of M` (the file line at the top of the screen, of all of them) and `wrapped` or `not wrapped`. The line count, `/`, `j`/`k` and `g`/`G` go by the file's lines in both modes (a line
taller than the screen shows its first rows; `w` reads the rest). Every file opens in full. Plinth keeps no run
file over 16 MiB (a larger answer stays in `raw/`,
unshown, `plinthctl.README.md`, Exec), so only a file a script left behind can be larger: it opens at its first 16 MiB,
and the pager says so. A rule (the divider's ─) under the title line sets the text apart from it, in the pager and on the Events screen.

**`ask.sh` is coloured** for review: comments dim, quoted strings and here-document bodies light blue, `$name`, `${...}` and
`$(` cyan, shell keywords (`if`, `for`, `set`, `exit`, ...) bold magenta, and the operators `| & ; < > ( ) { }` yellow.
A reading aid, not a parser: quotes and here-documents carry over from line to line, nothing is dropped or changed, and
the text goes through the same sanitizer as any file (Untrusted text). No colour with `NO_COLOR` (the bold stays).

**YAML files are coloured** the same way (`*.yml`, `*.yaml`: the answer documents): mapping keys cyan, scalar values
light blue, numbers magenta, `true`/`false`/`null`, document markers (`---`), anchors, aliases and tags bold magenta, the
`-` of a list, the `:` and the `[ ] { } ,` of flow collections, and the `|` or `>` of a block scalar yellow, comments
dim. The lines of a block scalar are values (light blue), whatever they hold. Also a reading aid: a quoted scalar and a block
scalar carry over from line to line, a flow collection is followed within its line only, and nothing is dropped or
changed. Other files (reviews are Markdown) are plain.

(decided; not built yet) With answers and reviews as YAML documents (`plinthctl.README.md`, Documents), the pager
shows them as sanitized text, YAML coloured as above. A code review's `flow:` is drawn as plinthctl's ASCII
diagram above the review's text.

### Events

`E` on Runs shows the filtered namespace's event log (as `events show`; with several namespaces and none chosen with `n`,
it asks for one), on Run that run's events. The chain is checked
as it is read (`events.Walk`): a line that doesn't match stops the list, and the problem is shown in red with its file
and line (as `events verify`). `/` filters: only the lines that hold the text show; `Esc` clears the filter, then
goes back.

### Logs

`L`: the general log as **lines**, newest first, across the daily files, at most the newest
5000 that match (the title says when more do). A row is the time (`MM-DD HH:MM`, local), the level, the component, the
message, then the other fields in dim, cut at the edge; only the `level` word is coloured (`ERROR` red, `WARN` amber, `INFO`
plain, `DEBUG` dim), so `NO_COLOR` loses nothing. Every string comes from a file, so it goes through the sanitizer. A line
that isn't in the log's format (a file from an older version) is skipped; `f` shows the files. `Enter` shows the line in full in the
pager: the fixed keys, then each field on a line of its own. The screen reads the log again at every refresh; the cursor stays
on its line, and at the top it follows the new lines.
- **Filters**, combined; each one set shows in the breadcrumb bar, after `<logs>` (`<>=warn> <runner> <10-28> <14h>
  </text>`), and `Esc` clears the search, then the last one set, then goes back:
  `l` **Level** (a picker: `Open notices`, `error`, `warn`, `info`, `debug`; a level means this one and above;
  `Open notices` shows the lines of the keys still open, see Notices, and is the notices screen: there is no other),
  `c` **Component** (`agent-mcp`, `cli`, `controller`, `dlp`, `ollama-gateway`, `reviews`, `runner`, `tui`), `d` **Date**
  (the days that have a file, newest first), `h` **Hour** (`00h` to `23h`), `/` **Search** (text in the line, any case).
- **From the Run screen** `L` opens the log filtered to that run (`ns`, `agent` and `run` fields), shown as `<run:NAME>`;
  `Esc` clears it like the others.
- **`f` Files**: the files of the log with size and age, old ones (older than `plinth.log.max-age-days`) marked; `Enter`
  opens one in the pager, `/` filters the names, `f` or `Esc` goes back to the lines. **Scrub logs** (`Ctrl-D`) works in
  both: it names the old files and asks you to type `yes`, then writes `<work>/requests/logs-scrub.queued`. Pressed while the
  process is down, it says "controller not running" and writes nothing, as the web UI disables it.

### Image (decided 2026-10-15, built 2026-10-17)

`I` on Runs or Run: the namespace's runner image (`plinthctl.README.md`, Run modes, The image). With several namespaces
and none chosen with `n`, it asks for one, as Events does. It is TUI-only for now: the web UI is on hold, and a browser
can't take over a terminal.

- **Header:** the image repo (`<host>/<owner>/plinth-<repo>`), the login (`logged in to <host> as <user>`, or `not
  logged in: press l`; "checking" until `podman login --get-login` answers), and the pin from the namespace's `config.yml`: tag, digest (first 12 hex), commit, tree,
  whether this machine has it, and the drift check of the last run of the pin (its `exec.start`: no drift, drift not
  checked, or the drift with both trees and the branch's commit, hashes cut to 12) with the run and its time, or "not
  checked yet" before the first run. "No image pinned" when there is none.
  While the default branch isn't protected or needs no approval (as `image list` reports): "Branch protection is
  recommended: see docs/runner-image.md". With protection on, no note.
- **Background work:** the header's podman checks (`podman login --get-login`, `podman image exists <pin>`, 10 s each, and their output waited for at most 1 s more, so a child podman left
  holding it can't keep the header "checking")
  and `plinthctl image list -n <ns> -o yaml` run in the background when the screen opens, on `R` and after every
  command, so a podman that hangs (its storage lock held by a build or a pull) never freezes the TUI. Until the list is
  back the screen says "loading" and no row shows or is selected, so no key acts on an image that isn't on the
  screen; the list's error shows in its place. After `R` the message line says `list reloaded (<n> images)`.

  | TAG | DIGEST | COMMIT | CREATED | LOCAL | |
  |---|---|---|---|---|---|

  DIGEST and COMMIT show their first 12 hex, CREATED the age. The last column says `pinned`, or `not Plinth` for an
  image whose labels don't name the namespace (`u` on it says "not a Plinth image of <ns>"): at 80 columns it has 10.
  Newest first. Every value comes from the registry, so it goes through the sanitizer. A result that comes back after
  the screen was left, or after a later `R`, is dropped.
- **Keys:** each one hands the terminal to the command (leaving the alternate screen and cooked mode back, as
  `e` on a privileged run does), the command asks its own questions, and when it ends the TUI says `[plinthctl tui] exit
  status <n>. Press Enter to go back.` (or `signal: <name>` when the command died from one), so its output can be read
  first; only Enter goes back there, Ctrl-C doesn't quit. Then the screen reloads, and the message line shows the exit
  status. While the command runs, Ctrl-C and Ctrl-\ are the command's (the TUI drops them); a SIGTERM or SIGHUP ends
  the TUI as soon as the command has, without the Enter. Each command goes into the general log (`tui: ran plinthctl image publish
  for <ns>: exit 0`), besides the command's own lines.

  | Key | What | Runs |
  |---|---|---|
  | `l` | log in to the registry | `plinthctl image login -n <ns>` (which runs `podman login <host>`) |
  | `o` | log out of the registry (pressed while not logged in, it says so) | `plinthctl image logout -n <ns>` (which runs `podman logout <host>`) |
  | `b` | build and push the image from the branch (publish) | `plinthctl image publish -n <ns>` (you type `yes`) |
  | `u` | use the selected image as the controller image | `plinthctl image use -n <ns> sha256:<digest>` (you type `yes`) |
  | `p` | pull the selected image (with none, as before the list loads: the pin) to this machine | `plinthctl image pull -n <ns> [sha256:<digest>]` |
  | `Ctrl-D` | delete the selected image: a menu, `1` this machine's copy, `2` from the registry | `plinthctl image delete -n <ns> --local\|--registry sha256:<digest>` (the registry asks you to type `yes`) |

  `u` and `Ctrl-D` can't act on the pinned image ("pinned"); `u` can't on an image that isn't a Plinth image of the
  namespace. Without skopeo, every key but `l` and `o` can't: "needs skopeo (package skopeo)". The keys work
  whether or not the controller runs: the commands do the work themselves (`plinthctl.README.md`, The image).

### Ansible (decided 2026-10-18; not built)

`A`: the namespace's Ansible setup, which Plinth owns and the user manages here (plinthctl.README.md,
Ansible runs):

- **Inventory:** the hosts and groups of `<ns-dir>/ansible/inventory.yml`, each with its remote user and whether its
  host key is known. `i` opens the file in `$EDITOR` (the TUI hands the terminal over, as for the Image screen's
  commands) on a temporary copy, and checks the copy on return. Only a copy that passes replaces the inventory; with a
  problem, the TUI shows it and keeps the copy, and offers `e` (edit again) or `d` (discard) (decided).
- **Host keys:** `known_hosts` holds the host CA's `@cert-authority` line, so a host with a host certificate needs
  nothing here. For a host without one, `h` fetches its keys (`ssh-keyscan`), shows the fingerprints, and adds them to
  `<ns-dir>/ansible/known_hosts` only after the user types `trust`.
- **The CAs:** for the user CA and the host CA, whether `<work>/ca/` holds it or the agent does, its public key's
  fingerprint, and the lines a host's setup needs (`TrustedUserCAKeys`, the `@cert-authority` line).
- **Signing** is on the Run screen: a remote run waiting for its certificate shows `S` (Sign), which opens the
  acceptance box (hosts, remote users, the certificate's lifetime, source address, the risk score and its reasons)
  and, once the user typed `sign`, hands the terminal over to `ssh-keygen -s` for the passphrase or the agent.

### Conversations

Hidden until messages are specified: no `C` key, not in the screen picker. Then `C` opens it, following the web UI's Conversations page: one entry per `<from-agent> -
<to-agent>`, Approve and Reject.

## Actions

The web UI's buttons, as keys on the Run screen (and on the selected run in Runs). A key that doesn't apply isn't shown;
one that applies but can't run yet is in the header's hints too, and pressing it shows the web UI's note on the message
line ("Waiting for reviews", "Review
failed", "Waiting for the DLP scan", "Waiting for the model (unavailable since <time>)", "Waiting for the model: it
didn't reply in time" (the later of the two, when both happened), "Successful DLP scan needed", "No
repo configured (plinth.repo)", "Needs URL"). Pressing a key asks `decide` first; a refusal shows on the message line with
its rule, and nothing is written.

| Key | Action (request) | Confirmation |
|---|---|---|
| `e` | **Exec** (`exec`), for a run without an answer: one that hasn't run, or whose exec ended without an answer (`no-answer`) | `Exec <RUN> in <ns>? (y/N)`; for a `no-answer` run `Exec <RUN> in <ns> again? Its last exec ended without an answer. (y/N)` |
| `o` | **Override review** (`override.review`) or **Override DLP scan** (`override.dlp`), whichever blocks Exec or Done (never both at once: a review can be overridden only before the run ran, the DLP scan only after) | two steps: a box names what failed (each review with its reason, or the scan's reason from `status.dlp`); then type `yes` |
| `w` | **Accept network** (`accept.network`), for a network run (`NETWORK=1`) not accepted yet; Exec says to accept it first | two steps: a box says what the run can reach (the network; no ssh; the env file's variables, which the run can send anywhere); then type `yes` |
| `d` | **Done** (`done`) | `Release the answer of <RUN> to <agent>? (y/N)`; afterwards the message line says: Tell the agent "`<RUN> done`" |
| `s` | **Save** (`save`) | `y/N`, naming the number of files, `<RUN>.zip` and the run's issue in `plinth.repo`; dimmed while the answer waits for its scan, or after a failed one until Override DLP scan (built 2026-10-22; any run with an answer). With `plinth.autosave.runs` the header says `(runs auto-save)` instead of `Save` and the key only says why; a failed auto-save (a `FAILED` save request and no success after it) shows `Save (auto-fail)` and the key works again |
| `r` | **Report** (`report`) | a box names `plinth.repo`, the run's issue (title `<ns> <stem>: <what the script says>`, opened if the run has none), the comment and what will be attached, masked first; then `y/N`; dimmed with "No repo configured (plinth.repo)" without the repo, and without `ollama.host` (nothing can be masked) (built 2026-10-22). With `plinth.autosave.failures` the header says `(auto-report)` while the request is pending and the key only says why; a failed auto-report shows `Report (auto-fail)` and the key works again |
| `v` | **Review <type>** (`review.<type>`, as `plinthctl review -n <ns> <RUN> <type>`; built 2026-10-22): the controller reviews the script, or the answer once there is one | a menu of the review types, sorted (`1`-`9`; any other key: nothing), no menu with one type; then `Review the <script\|answer> of <RUN> with <type>? (y/N)` |
| `Ctrl-D` | **Scrub** (`scrub`) | names what goes, then type `yes`; disabled while running |

- `Ctrl-D` scrubs, k9s's delete: Scrub, Scrub logs, and the Image screen's Delete (it was `S`
  from 2026-10-18, and `Ctrl-D` before that). `S` is no longer bound. The `yes` confirmations stay. Done stays `d` and
  Exec `e`: their `y/N` is the gate.
- **Privileged scripts** (`PRIVILEGED=1`; every `INTERACTIVE=1` one is, built 2026-10-18): `e` writes no request.
  It asks decide as from the terminal (`request.DecideTerminal`), then runs `plinthctl exec -n <ns> --agent <agent>
  --work <work> <RUN>` with the terminal handed over, exactly as the Image screen's keys do: the command shows what the
  script can do and asks for `privileged` itself, the controller records the start over its socket, the script runs in
  this terminal, and the controller scans the answer (plinthctl.README.md, Run modes, Privileged mode). Then the screen
  reads everything again, and the message line shows the exit status. A TUI that can't hand the terminal over (no
  plinthctl binary of its own) gives `e` the note "privileged: runs in your terminal (plinthctl exec), which this
  TUI can't hand over".
- Every request shows its outcome in the run's ACTION STATE (queued, running, gone once done, `FAILED` with its reason).

## Keys

| Key | Where | What |
|---|---|---|
| `↑` `↓` `k` `j`, `PgUp` `PgDn`, `g` `G` | lists, pager | move (in a picker only `↑` `↓`: the letters are typed) |
| `Enter` | lists, a picker | open; switch |
| `Esc` | everywhere | back (clears a filter first); closes a picker |
| `:` | Runs, Run, Events, Logs, Image | the screen picker (see Screens); a general key, in the bottom bar |
| `/` | Runs, Logs, Events; pager | filter; search |
| `n`, `a`, `t` | Runs | namespace, agent (a picker for each), show/hide released |
| `E`, `L`, `I` | Runs, Run, Events, Logs, Image | Events, Logs, Image: the picker's shortcuts, not in the hints (`C`, Conversations, once messages are built) |
| `e`, `o`, `w`, `d`, `s`, `r`, `v`, `Ctrl-D` | Runs, Run | the run's actions (see Actions) |
| `l`, `c`, `d`, `h`, `/`, `f`, `Ctrl-D` | Logs | level (or Open notices), component, date, hour (a picker for each), search, the file list; Scrub logs (see Logs) |
| `l`, `o`, `b`, `u`, `p`, `Ctrl-D`, `R` | Image | log in, log out, publish, use, pull, delete, reload (see Image) |
| `w` | pager, Events | wrap long lines, or scroll them sideways |
| `←` `→` | pager, Events | scroll sideways, while not wrapped |
| `?` | everywhere | every key, in two groups (General, and the screen's own keys), the hints' and those not in them; a general key, in the bottom bar |
| `Ctrl-V` | everywhere | the Plinth version on the message line (not in the hints; listed on the Keys screen) |
| `Q` | Runs | quit, not in the hints (k9s doesn't show it either). Only `Q`: `q` goes back on the other screens, so a `q` pressed once too often on the way back leaves the TUI open |
| `q` | every screen but Runs | back, like `Esc` |
| `Ctrl-C` | everywhere | quit (except at the Image screen's "Press Enter to go back": the command's output stays until Enter) |

The key decoder reads `CSI` sequences for the arrows, `Home`/`End` and `PgUp`/`PgDn`; a lone `ESC` with no byte after
it within 50 ms is `Esc`. Input for a filter or a confirmation is UTF-8 and goes through the sanitizer when it is shown.

## Config keys used

Global: `forgejo-url` (issue URLs; the image repo's host), `reviewers.type.<name>` (the review menu; "Needs URL"),
`ollama.host` (the status line), `plinth.runner.poll-seconds` (a stale heartbeat), `plinth.log.max-age-days` (old
log files), `plinth.repo` (Save, Report), `plinth.autosave.runs` and `.failures` (the s and r hints), `plinth.tui.refresh-seconds` (new, default 2). Per namespace,
or inherited: `reviewers.automatic` (pending reviews); namespace only: `plinth.exec.image` (the pin, Image).

## Tests

- `term`: the key decoder on byte streams (split sequences, a lone `ESC`, UTF-8); the sanitizer on every class above;
  the screen's frames (only changed rows, cuts, styles); raw mode and the size on a pseudo-terminal (skipped where the
  build has none). The `tui` tests drive the model only, so they need no terminal.
- `tui`: keys driven through update and view on a temp `<work>` with fixture runs; each check looks at the frame and at
  the request files written (an action writes exactly its request; a refused one writes nothing; a confirmation
  answered otherwise writes nothing); a fixture full of escape sequences renders none; a resize below 80 x 24 shows only
  the size note and keys do nothing there.
- The layout (`TestLayout`, `TestPicker`, `TestImageScreen`): at 80 x 24 and 120 x 40 on Runs, Run, the pager,
  Events, Logs and Image, the header's two blocks (the left items, the hints from column w-46 and w-23), the hints of
  each selection (a key that doesn't apply is absent; none of them general), the message line and the breadcrumb with the general keys at its right end (`TestGeneralKeys`: a long breadcrumb gives way, none while Help is open); the `?` page in its two groups (a hostile file name in it
  escaped), the divider; 14 rows of the Runs list at 80 x 24. The picker opens, moves, narrows, switches with `Enter`, and `Esc`
  leaves the screen as it was; `n` and `a` set the filters. `Ctrl-D` scrubs (with its `yes`), `S` does nothing.
- Wrapping (`TestWrap`, `TestLineWrap`, `TestWriterWrap`, `TestGlyphs`, `TestTerminalWrapped`): a line longer than
  the screen wraps at a space, one without spaces at the column, wide characters whole, an escape whole, the
  continuation row starting with the mark; the line count and `j` go by file lines; `w` cuts and scrolls instead; a
  resize wraps again; a confirmation box wraps; the CLI wraps on a 40-column pty and not when piped; the locale check.
- Image screen (`TestImageScreen`, built): the commands are replaced by a fake (`Images`) that records its arguments
  and prints fixture YAML. Each key runs exactly its command with the selected digest, keys that can't act run nothing,
  without skopeo only `l` and `o` run, no key acts while the list loads, and registry values full of escape sequences
  render none. `term`'s `TestInput` pauses the input on a pipe and checks that another reader gets the bytes meanwhile.
- The hand-over in a real terminal (`TestHandover`, skipped without tmux): the test binary runs the
  TUI (`PLINTH_TUI_HELPER=1`) in `tmux -L <own socket> new-session -d -x 100 -y 30`, with a fake `plinthctl` whose
  `image login` reads a password with echo off; `send-keys` types, `capture-pane -p` reads the screen, and `stty -a`
  after the TUI checks the terminal. It checks that Ctrl-C at that prompt leaves echo on (at "Press Enter" and after
  the TUI), that a SIGTERM while the command runs ends the TUI without Enter, and that the Runs header keeps its last
  column.
