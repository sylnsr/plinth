# Plinth web UI - the user's window on Plinth

This file is the spec of Plinth's web UI. **Plinth** is an agent action enforcement layer (an agent runtime security
mesh): one Go binary, `plinthctl` (`plinthctl <command> <params>`, kubectl-style, e.g. `plinthctl get ans -n <ns> <RUN>`),
running as **one host process** that holds the core (the controller), this web UI, the Plinth MCP server for agents, and that
keeps the Ollama gateway's container running. Its source is in `../src` (michael/plinth).
The code follows this file: change the spec here first, then the code.

Settings live in Plinth's work directory `<work>`, outside every repo, so agents can neither read nor change them:
`<work>/config.yml` (global) and `<work>/<owner>/<repo>/config.yml` (per namespace). `<work>` is the `--work` flag,
default `~/.plinth` (`plinthctl.README.md`, Config). Nothing of Plinth lives in the repo, and Plinth writes nothing into it.

Plinth enforces the AAA flow (the agent writes an ask script, the user reviews and runs it, the answer lands in an answer
file); AAA itself is a set of rules in the KB that also works without Plinth. This UI adds no rule of its own and changes
nothing about who may read what. No YOLO modes: every check that can't run fails closed, and only the user's two-step
override releases it.

## Purpose

The user's one place to vet and drive Plinth: see every run of every agent in every namespace, its stage and state, and
request the actions that apply to it (exec, release the answer, save, scrub, report, review, override); see the messages
agents send each other and approve or reject the gated ones.

The agent only writes ask scripts and sends messages. The user is the only one who starts a run or releases an answer, and
does both here or with the matching `plinthctl` command.

## Where the UI sits

One host process, running as the user, holds every part:

| Part | Role |
|---|---|
| web UI (this spec) | shows runs and conversations, read-only; writes request files - nothing else |
| controller | the core: submissions, automatic reviews, carries out requests, exec, DLP scan, message review and release |
| Plinth MCP server | agent tools: submit asks, read released answers, send / pick up messages, "is it up" |
| Ollama gateway | a container the controller keeps running, not in the process: the one way to Ollama, for the reviews and the DLP scan only (`plinthctl.README.md`, Ollama gateway) |

**Namespaces.** A namespace is `<owner>/<repo>`, given with `-n` like `kubectl -n`. It comes into being when an agent's
launch script registers the repo and the agent (`plinthctl register --repo <path> --agent <agent>`). It names the
subtree `<work>/<owner>/<repo>/` (`config.yml`, `ask-template.yml`, `runs/`, `requests/`, `raw/`, `messages/`) and the
Forgejo repo the runs' issues belong to (`<forgejo-url>/<owner>/<repo>`); the repo's own git can be hosted anywhere. The
UI listens once and shows every namespace the process serves: each one with at least one registered agent
(`plinthctl.README.md`, Process).

**Listeners only queue.** The process starts ask scripts with the user's environment (`SSH_AUTH_SOCK`, `~/.ssh`,
kubeconfig), so the UI's handlers never call exec, Done or any other action directly: every button writes one request
file, and only the namespace's runner reads `requests/`, re-validates the request and carries it out; its result shows up in the
run. The UI's code never runs a tool, never talks to Ollama or an agent, and talks to Forgejo only for the login (see
Login).

## Runtime

- Part of the one `plinthctl` host process; no container. Go; the only dependency is `go.yaml.in/yaml/v3` (vendored). The build runs `go test ./...` before `go build`; a failing test fails the build. No
  Node.js.
- Listens on the VM network like the Plinth MCP server (`plinth.web-ui.listen`, `plinth.web-ui.port`), so the user opens it
  directly from the VM's host. The agents can reach that network too, so **nothing works without an interactive login**
  (see Login), and the UI refuses to start when the login is not configured (`plinth.web-ui.auth.client-id`,
  `plinth.web-ui.auth.users`, `forgejo-url`).
- **No secrets**: the login needs none (see below). The UI's only outbound traffic is to `forgejo-url`, for the login.
- What the UI's code reads and writes, and nothing else:

  | Path | What | Mode |
  |---|---|---|
  | `<work>/<owner>/<repo>/runs/` | runs (the controller's private copies, answers, reviews) | read |
  | `<work>/<owner>/<repo>/messages/` | conversations | read |
  | `<work>/<owner>/<repo>/requests/` | request files | write (new files only) |
  | `<work>/config.yml`, `<work>/<owner>/<repo>/config.yml` | settings | read |
  | `<work>/state/` | heartbeats, Ollama status | read |

  Every path is built from parts that each match their pattern and is opened through Go's `os.Root`, so it can't leave its
  directory (no `..`, no symlinks; `plinthctl.README.md`, Principles). The UI reads nothing from the repo.
- Everything shown from a file (script header, answer, reviews, file names) is rendered as escaped text, never as HTML.
- When a namespace's heartbeat is stale, its runs show "controller not running": requests still queue, nothing is carried out.
- The page header shows the Ollama status: "not configured", "reachable (model X loaded)" or "unreachable since T".

## Runs and tabs

- A run is `<work>/<owner>/<repo>/runs/<agent>/<RUN>/`. Agents submit ask scripts over the Plinth MCP server
  (`ask.submit`); the controller draws the `RUN_NAME` (unique across all agents of the namespace), writes `RUN_NAME` and
  `AGENT_NAME` (from the agent's token) into the script's header and stores it there. `<RUN>` matches
  `^[a-z]+-[a-z]+(-[0-9]+)?$`; anything else is not a run. A submitted script never changes and never runs twice: a new
  version is a new submission.
- **Release:** **Done** renames the scanned answer to `answer[.FAIL].released.yml`; only then can the agent read it over MCP
  (`answer.read`), as often as it asks. Nothing is written into the repo.
- **One tab per run**, named `<RUN>`, with the namespace and agent shown next to the name. Tabs are filterable by namespace
  and by agent.
- Order: runs not yet released first, newest first (by the newest file in the run directory); released runs after them,
  behind a "show released" toggle.
- The list refreshes every `plinth.web-ui.refresh-seconds`; a tab whose directory is gone disappears.

## What a tab shows

- **Stage and state**, derived only from file names in the run directory (file names are the state; each change is one
  rename, `plinthctl.README.md`, Runs):

  | Files | Stage | State |
  |---|---|---|
  | `ask.sh` (+ `ask.sh.sha256`), nothing else | ask | queued |
  | `review.<type>.md` / `review.<type>.FAILED.md` | review | reviewed / review failed, per type |
  | `running.yml` | running | running, or no answer if it is left over (show its age): not done, it may run again |
  | `answer[.FAIL].new.yml` | answer | finished, being scanned (`.FAIL`: stopped at a gate) |
  | `answer[.FAIL].dlp-ok.yml` | answer | DLP scan passed |
  | `answer[.FAIL].DLP-FAIL.yml` | answer | DLP scan failed or not available |
  | `answer[.FAIL].INVALID.yml` | answer | not a valid answer document: a bug, never data; never shown or released |
  | `review.answer.<type>.md` / `.FAILED.md` | answer | answer reviewed / review failed, per type |
  | `override.<what>.md` | - | the user overrode a failed `review` or `dlp` check (who, when, what failed) |
  | `report.md` | - | reported: the link to the run's issue in `plinth.repo` |
  | `answer[.FAIL].released.yml` | released | released by Done; the agent can read it |

  The answer is a YAML document (`plinthctl.README.md`, Documents).

  The run directory is `<work>/<owner>/<repo>/runs/<agent>/<RUN>/`. The stage shown is the furthest one reached
  (released > answer > running > review > ask). Pending requests
  (`<work>/<owner>/<repo>/requests/<agent>/<RUN>/<action>[.<arg>].queued|.starting|.running|.FAILED`) are shown next
  to the button they came from.
- **A failed review** is an automatic review (`reviewers.automatic`) that ended in `review.<type>.FAILED.md` or with the
  verdict `Do not run`. Without Ollama (not configured or unreachable) every automatic review ends in
  `review.<type>.FAILED.md`, and its reason line says so.
- **A failed DLP scan** is an answer named `answer[.FAIL].DLP-FAIL.yml`, its reason in `status.dlp`: the scan ran and
  failed. Without Ollama the scan can't run: the answer stays `answer[.FAIL].new.yml`, not scanned, until it can
  (the event `dlp.scan` `unavailable` says why).
- **Issue:** the page in the script's `ISSUE=` line (the ticket the round is for); without one, for a `RUN_NAME` ending in `-<digits>`, a link to `<forgejo-url>/<owner>/<repo>/issues/<digits>`. It is context only: Save and Report post on the run's own issue in `plinth.repo`.
- **Script header:** the leading comment lines of `ask.sh` (what the round does, READ-ONLY or CHANGES) and its `INTERACTIVE`
  value.
- **Files:** every file in the run directory with size and modification time. Each opens in full, read-only, as escaped
  text, `ask.sh` included, so the whole script can be read before Exec (decided: both UIs show entire files), except
  `running.yml` and an answer that hasn't been scanned (`*.new.yml`) or is `INVALID`, which show only their state.
  Long lines wrap (as the TUI's pager: word wrap, a continuation row marked with `↪` so it can't be taken for a
  line of its own, the line numbers counting the file's lines), with a switch to scroll them sideways instead. The web
  UI needs no ASCII fallback: the browser has the glyphs.
- **Answer:** its content once it has been scanned, before and after Done, with its DLP result shown above it: passed
  (`dlp-ok`, `released`), overridden, or **failed** (decided). A failed scan's answer is shown too, with the reason,
  because auditors need to see why the scan failed to take corrective action; the agent never gets it (it reads only
  released answers, over MCP). It is shown with the fixed-pattern masks the scan applied before the model step
  (decided; `plinthctl.README.md`, Process), and every page showing an answer is sent with
  `Cache-Control: no-store`. While the answer is being scanned (`*.new.yml`), the tab shows only the state.
- **Reviews:** each review file, collapsed, with its verdict (`OK to run` / `Run after fixes` / `Do not run`) or its
  failure reason on the collapsed line.

## Buttons

Every button writes one request file `<work>/<owner>/<repo>/requests/<agent>/<RUN>/<action>[.<arg>].queued` and
nothing else; the controller re-validates it and carries it out. A button that does not apply is not shown; a button that
applies but cannot run yet is disabled, with the reason as a visible note next to it (not a hover, so it works on mobile).
Every button has a `plinthctl` command that writes the same request.

- **Exec** (`exec`) - shown while queued. Disabled, with the reason as its note:
  - while an automatic review has not finished ("Waiting for reviews");
  - after a failed review ("Review failed"), including a review that could not run without Ollama, until the user
    overrides it (below).

  For an `INTERACTIVE=1` script there is no request: the button copies the command for the user's own terminal
  (`plinthctl exec -n <owner>/<repo> <RUN>`) and says so.
- **Override review** (`override.review`) - shown next to a disabled Exec after a failed review. Two steps: the first click
  opens a confirmation that names the failed review(s) and their verdicts or reasons; only **Confirm override** there
  writes the request. The controller records `override.review.md`; Exec is then enabled again.
- **Done** (`done`) - shown for a run with a scanned answer that is not released. The controller renames the answer to
  `answer[.FAIL].released.yml`; nothing is written into the repo. Disabled with the note "Waiting for the DLP scan" while
  the answer is `*.new.yml` and its scan runs, "Waiting for the model (unavailable since <time>)" while the model can't
  be asked, "Waiting for the model: it didn't reply in time" between attempts that timed out (the later of
  the two when both happened; the run's
  answer status says `waiting-for-model`, with the reason), and with "Successful DLP scan needed" after a failed DLP
  scan, until the user overrides it. Afterwards the UI shows the reminder: Tell
  the agent "`<RUN> done`".
- **Override DLP scan** (`override.dlp`) - shown next to a disabled Done. The same two steps as Override review; the controller
  records `override.dlp.md`; Done and Save are then enabled.
- **Save** (`save`) - shown when an answer exists (a run needs no issue in its name). The controller zips every file of the
  run except `running.yml` into `<RUN>.zip`, adds a comment `Run artifacts` to the run's issue in `plinth.repo` (opened first when the run has none) and attaches the zip to it. With `plinth.autosave.runs` it does this by itself right after Done and the button says `(runs auto-save)`, enabled again as `Save (auto-fail)` after a failed one.
  Disabled with the same notes as Done after a failed or unavailable DLP scan, until it is overridden. The run then shows
  the link to the new comment.
- **Scrub** (`scrub`) - deletes the run fully: its directory `<work>/<owner>/<repo>/runs/<agent>/<RUN>/`, the DLP
  scan's untouched original and its request files (`plinthctl.README.md`, Runs, Scrub). It first asks "Are you sure?"
  with the run name; **Yes** becomes clickable only after 5 seconds, **No** at once. Disabled while running.
- **Report** (`report`) - shown after a failed scan: a failed review, or a failed DLP scan (overridden or not), until
  the run is reported. The first click opens a confirmation that names `plinth.repo`
  and lists what will be posted, all of it masked first (`plinthctl.README.md`, Runs, Report); only **Confirm report**
  writes the request. Disabled with the note "No repo configured (plinth.repo)" without that key. The run then shows the
  link to its issue (Report adds a comment to the run's issue there, opened first when the run has none). Messages whose review failed get the same button on the Conversations page.
- **Review &lt;type&gt;** (`review.<type>`) - one button per `reviewers.type.<type>`, for the script while queued and for the
  answer once it exists. A type with neither `urls` nor `definition` is disabled with the note "Needs URL". A type whose
  prompt does not review answers writes `review.answer.<type>.FAILED.md` with that reason (the controller decides; the UI
  can't read the prompt). Without Ollama the review ends as `.FAILED.md` with that reason. The button shows "running"
  until the review file appears.

Every request shows its outcome (queued / starting / running / finished / `.FAILED` with its one-line reason); the UI
never shows a tool's raw output.

## Conversations

A separate page for the same-namespace messages agents send each other through the Plinth MCP server (cross-repo messages
are Forgejo issue comments and don't appear here).

- **One tab per `<from-agent> - <to-agent>`**, filterable by namespace. A tab with a message waiting for the user's review
  has a different colour from the rest.
- Each message shows its type, that type's `meaning` and the `prompt` the recipient will get (from the catalogue
  `plinth-messages.yml`, which the controller reads from `plinth.messages.catalogue-url`), its one parameter - a URL under
  `forgejo-url` that opens in a new window - its state (waiting for review, rejected, available for pickup, picked up,
  expired) and the tooling and LLM review results. Without Ollama the LLM review fails, and the message waits for the user
  even if it is ungated.
- A gated message (sender not in `plinth.messages.ungated.agents` and type not in `plinth.messages.ungated.messages`) that
  passed the tooling and LLM review waits for the user:
  - **Approve** (`approve`) - the controller makes it available for pickup; `plinth.messages.expiration-minutes` start then.
  - **Reject** (`reject.<policy>`) - the user picks a policy page from `plinth-policies/` (no free text); the controller sends
    `rej` with that page's URL back to the sender, ungated.
- Request files: `<work>/<owner>/<repo>/requests/messages/<message-id>/<action>[.<arg>].queued`. The catalogue, the
  list of policy pages and the messages come from the controller through `<work>/<owner>/<repo>/messages/`. Only if Forgejo issues are chosen as the message store may the UI read the messages from Forgejo directly (the
  credential is decided then).
- The page refreshes every `plinth.messages.refresh-seconds`.

## Logs

The header has a **Logs** link to a page that lists the files of Plinth's general log, `<work>/logs/plinth-YYYY-MM-DD.log`
(one per day; `plinthctl.README.md`, Log), with their size and age; the files older than `plinth.log.max-age-days` are
marked old. A file opens read-only in the page.

- **Scrub logs** - shown while there is an old file. It names the old files and asks "Are you sure?", then writes the
  request `<work>/requests/logs-scrub.queued` and nothing else; the process deletes the files that are old when it
  carries the request out. Disabled, with the note "controller not running", while the process is down. `plinthctl logs
  scrub` writes the same request. Nothing deletes a log on its own.

## Login

The UI's buttons are the user's gate (Exec, Done, overrides, Approve), and the agents' containers can reach the UI's port.
So every page and every request needs a logged-in user; Forgejo (`forgejo-url`) is the identity provider (OpenID Connect).

- **Flow:** authorization code with PKCE against `forgejo-url`. The user registers the UI once in Forgejo as an OAuth2
  application, **public client** (no client secret), redirect URI `<plinth.web-ui.public-url>/auth/callback`, and puts its
  client ID (not a secret) in `plinth.web-ui.auth.client-id`.
- **Logging in** is interactive: the UI redirects to Forgejo's own login page (password, and 2FA if the account has it -
  recommended); an agent has no password and can't complete it.
- **Check:** the UI verifies the ID token itself (RS256 signature against Forgejo's published keys, issuer, audience =
  client ID, expiry, nonce) and accepts the login only if `preferred_username` is in `plinth.web-ui.auth.users`. That list
  holds people only - never an agent account.
- **Tokens:** the UI keeps neither the ID token nor the access token; it discards both after the check. It keeps only its
  own session: a random 256-bit ID in memory, sent as a cookie (`HttpOnly`, `SameSite=Strict`, path `/`), valid for
  `plinth.web-ui.auth.session-hours`. A restart logs everyone out. **Log out** ends the session at once.
- **Forgejo down:** nobody can log in to the UI. Every action the UI can request is also a `plinthctl` command the user can
  run in their own terminal on the host (spec: the `plinthctl` README), so an outage never locks the user out - e.g. a run
  meant to help fix the outage.

## Security

- Every request is checked: it must carry a valid session (else: redirect to the login); the `Host` header must match
  `plinth.web-ui.public-url` (DNS rebinding); every state-changing request must be a POST with the session's CSRF token
  and an `Origin` matching `plinth.web-ui.public-url`.
- The only thing the UI writes is a request file. Its path is built from parts that each match their pattern (namespace,
  agent, run name, action, review type in config, policy name) and must resolve inside
  `<work>/<owner>/<repo>/requests/` (or `<work>/requests/` for the one process-level request, Scrub logs). The controller
  never acts on a request file's content and re-validates every name against the current state.
- The UI's handlers share the process with the controller but never call it: they only queue (see Where the UI sits).
- No secret is shown, logged or written by the UI; it has none.

## Config keys used

Global, from `<work>/config.yml`:

| Key | Used for |
|---|---|
| `forgejo-url` | the identity provider (OpenID Connect) and issue links |
| `plinth.web-ui.listen` / `.port` | where the UI listens on the VM |
| `plinth.web-ui.public-url` | the URL the user opens (e.g. `http://192.168.122.66:8790`): redirect URI, `Host` and `Origin` checks |
| `plinth.web-ui.auth.client-id` | the Forgejo OAuth2 application (public client) |
| `plinth.web-ui.auth.users` | the Forgejo logins allowed in - people only |
| `plinth.web-ui.auth.session-hours` | how long a session lasts |
| `plinth.web-ui.refresh-seconds` | how often the run list refreshes |
| `plinth.messages.refresh-seconds` | how often the Conversations page refreshes |
| `plinth.log.max-age-days` | which log files Scrub logs lists and deletes (default 30) |
| `reviewers.type.<name>.urls` / `.definition` | one Review button per type; "Needs URL" when both are empty |
| `ollama` | the Ollama status in the header; absent = "not configured" (reviews and DLP scan fail closed) |

Per namespace, from `<work>/<owner>/<repo>/config.yml`, or from the global config where the namespace leaves a key out
(`plinthctl.README.md`, Config):

| Key | Used for |
|---|---|
| `reviewers.automatic` | which reviews must finish (and pass, or be overridden) before Exec |
| `plinth.messages.ungated.agents` / `.messages` | which messages wait for the user |
| `plinth.messages.expiration-minutes` | shown with a message available for pickup |
