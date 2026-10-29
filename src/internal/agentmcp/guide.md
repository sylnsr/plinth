# Plinth: how you work with it

Plinth runs shell scripts you write, for the user, safely enough that the user doesn't have to give you a shell. You
never run anything on the user's machines yourself: you write an **ask script**, the user decides whether it runs, and
you read what it found, its **answer**, once Plinth has scanned it for secrets. This way of working is called AAA: you
**A**sk, the user **A**pproves, you read the **A**nswer.

Use it whenever you need to know something about, or change something on, a machine you can't reach yourself.

This MCP server is your only way into Plinth: you don't need `plinthctl`, and you can't run it.

## Two ways to run a script

| | Plinth runs (the default) | Plain AAA (the informal way) |
|---|---|---|
| Your tools | `ask_template`, `ask_preview`, `ask_submit`, `ask_status`, `answer_read` | `aaa_new`, `aaa_lint`, `aaa_status`, `aaa_read` |
| Where the run lives | Plinth's own work directory: you can't reach its files | your repo, `ai/work/<RUN_NAME>/` |
| Before it runs | Plinth lints the script, its automatic reviews check it, then the user decides | you lint it (`aaa_lint`), then the user reads it and decides |
| Where it runs | in a container of the namespace (or, privileged, in the user's terminal) | in the user's own terminal, as the user |
| The answer | you read it once the user has released it | you read it once its DLP scan passed |

**Use Plinth runs unless the user asks for plain AAA.** They are the secure default: the script, its state and its
answer are where you can't change them, so what the user approves is what runs, and you see an answer only after
Plinth and the user have let it through. Plain AAA is the compromise: its files are in a repo you can write to, so it
rests on the user's trust in you, and Plinth checks less. It is for quick, informal rounds the user wants that way.

## Plinth runs

1. `ask_template`: get the template every ask script starts from.
2. Write your script from it (see "Writing an ask script" below).
   **Then `ask_preview` it, fix what it finds and preview again until nothing is left, before step 3** (see "Previewing a
   script" below). Read `ai/memory/plinth-review-failures.md` first if it exists: it lists what was refused before.
3. `ask_submit`: hand it in. Plinth lints it, names it (its `RUN_NAME`, such as `calm-otter`, or `calm-otter-42` for an
   issue of this namespace's repo) and stores it; or it refuses it and says why. `issue` names the ticket the round is
   for: a number (an issue of this namespace's repo), `owner/repo#N`, or its page on this Forgejo. Plinth writes it into
   the script as `ISSUE="<page>"` (or you write that line yourself, before the library line). It is context for the
   user and for the run's issue in Plinth's runs repo; where Plinth posts never depends on it, and a number in the name
   is not an issue Plinth posts on.
4. Plinth's automatic reviews check the script against the policies listed under "This namespace" below.
5. **The user** reads the script and the reviews and decides whether it runs. Nothing runs without them, and they may
   take a while: they aren't watching you.
6. The script runs. Afterwards Plinth scans its answer for secrets (the DLP scan), and **the user** releases it.
7. `ask_status`: follow your runs. Each line says what you do next.
8. `answer_read`: read the answer once it is released.

Between steps 4 and 8 there is nothing for you to do but wait and check `ask_status` now and then. Tell the user what
you submitted and why, so they know what to look at.

- You see only your own runs: their names and states, and their answers once released. Never another agent's.
- A stored script never changes. To change one, submit a new version (a new run) and tell the user which one to run.

## Plain AAA

1. `aaa_new` (optionally with `issue`): Plinth creates `ai/work/<RUN_NAME>/ask.sh` in your repo from the plain
   template, its `RUN_NAME` filled in, and tells you the name.
2. Write your steps into that file, in your repo (see "Writing an ask script" below). Fill in `AGENT_NAME`.
3. `aaa_lint`: Plinth checks the script as `plinthctl ask lint` does. Fix what it finds and check again, until it
   passes.
4. Tell the user what it is for and ask them to run it: from `ai/work`, `./<RUN_NAME>/ask.sh`, then
   `plinthctl finish <RUN_NAME>`, which scans its answer for secrets. They tell you "`<RUN_NAME>` done".
5. `aaa_status`: the runs in `ai/work` and their states. Each line says what you do next.
6. `aaa_read`: read the answer once its DLP scan passed.

- **Read an answer only through `aaa_read`**, never the run's files themselves (`running.yml`, `answer.*.yml`), even
  though they are in your repo: `aaa_read` gives you an answer only after its DLP scan passed.
- Don't change a script once the user has run it: a rerun is a new run (`aaa_new`).
- The runs in `ai/work` are the repo's, the user's and other agents' too: work only with yours, and never move or
  delete anything there.

## What you can and can't do

- You can't run, approve, release, override or delete anything: those are the user's.
- An answer may hold what the DLP scan masked (`<masked>`); what it masked was a secret, not data you need.
- If this server stops answering, tell the user: Plinth's controller, which serves it, isn't running.

## The image your scripts run in

A Plinth run (all but a privileged one) runs in a container made from the namespace's image: the repo's
`.plinth/Containerfile`, which the user reviews on Forgejo, builds and publishes, and Plinth pins by its digest. The
container holds only what that file installs, and a run has no network unless the namespace allows it and the user
accepts it, so a script can't install a tool while it runs.

- **A tool the image doesn't have** (`go`, `jq`, a client): propose a change to `.plinth/Containerfile`, as a pull
  request if you can write to the repo, otherwise as the lines to add, and tell the user it needs their review, a
  merge and `plinthctl image publish -n <owner>/<repo>`. Only the user publishes and pins an image, and Plinth builds
  it from the default branch, so a change counts once it is merged and published. Until then, write your script for
  the tools the image has, or ask the user whether they want this round as plain AAA.
- **No `.plinth/` in the repo yet:** there is no image, and no Plinth run can start. Propose a first
  `.plinth/Containerfile`, such as the one below, and tell the user.
- **What `publish` refuses:** a base image not pinned by its digest (`FROM <image>@sha256:<digest>`), `ADD` of a URL or
  a git repo, a download that is run as it arrives (`curl ... | sh`), a download not checked against a checksum, a
  heredoc in a `RUN` (put the script in a file in `.plinth/`), `ONBUILD`, and `RUN --network=host`. Install packages
  from the distribution's package manager. You can't look a digest up yourself: leave `<digest>` for the user (`skopeo
  inspect docker://docker.io/library/debian:trixie` shows it).

```
# The image Plinth runs this repo's ask scripts in; the user publishes it with plinthctl image publish.
FROM docker.io/library/debian@sha256:<digest>
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
```

## Writing an ask script

Start from the template (`ask_template` for a Plinth run; `aaa_new` writes the plain one into the run) and keep its
header, its mode lines and its library line as they are:

- Line 2, `# AAA script: <what this run is for>`: one line, filled in.
- Line 3: `# READ-ONLY ...` when it changes nothing, or `# CHANGES: <the exact list>` when it does.
- Line 4: the sections it has, such as `#   1 disks  2 services`.
- `RUN_NAME`: leave it as it is (empty for a Plinth run, which Plinth fills in; filled in by `aaa_new`).
- `AGENT_NAME`: empty for a Plinth run; for plain AAA, your name.
- The mode lines:
  - A Plinth run has `INTERACTIVE=0`, `NETWORK=0`, `PRIVILEGED=0`: set one to 1 only when the script needs it (see
    "This namespace" for what is allowed here). `INTERACTIVE=1` needs `PRIVILEGED=1`.
  - Plain AAA has `INTERACTIVE=0` only: it always runs in the user's terminal, as the user.
- The library line (`source "$AAA_LIB"; ...` in a Plinth run, `source <(plinthctl ask lib); ...` in plain AAA):
  unchanged. Everything the answer holds goes through its helpers.

Then the steps, in sections:

- `section "1. Disks"` starts a section of the answer.
- `say "<text>"` writes one line into it; `<command> 2>&1 | block` writes a command's output.
- `<command> 2>&1 | scrub | block` masks secret values (passwords, tokens, keys) before they reach the answer: use it
  for anything that may print one, such as configs or environment variables.
- `fail "<why>"` stops the run there, as failed: use it as a gate before a step that changes something, so it never
  runs on a state you didn't expect. In the script itself, not inside a pipe or `$(...)`.
- `ask_yes "<question>"` asks the user (needs `INTERACTIVE=1`).

Rules Plinth checks for a Plinth run (the submission is refused otherwise, with the findings). `aaa_lint` checks the
basics of a plain AAA script (the header, `RUN_NAME`, the library line, bash, no functions, `trap` or answer names);
follow the rest there too:

- At most 500 lines, UTF-8, valid bash (`bash -n`).
- No `..` path component (`../x`, `cd ..`).
- Nothing naming Plinth's work directory. Naming Plinth itself (its repo, `plinthctl`) is allowed: whether a
  script reaches into Plinth is for the security and DLP reviews. A script with `PRIVILEGED=0` cannot call `plinthctl`
  (not `plinthctl`, not `/aaa/plinthctl`): its container has no controller to talk to, so the call fails. `ask_submit`
  warns about such a line (not a finding; the script is stored); a script that needs `plinthctl` must say `PRIVILEGED=1`.
- No ssh (`ssh`, `scp`, `rsync` to a host, ...) unless `PRIVILEGED=1`.
- No functions of your own, no `trap`, nothing that names the answer files: only the library writes the answer.

Write it so the user can read it at a glance: indent the bodies of `if`, `for`, `while` and `case` by two spaces, one
command per line, long pipes broken after `|` (that line and the `\` continuations two spaces deeper), never a tab.
`aaa_lint` and the reply to `ask_submit` warn about a line whose indent is off (`line 7: expected 4 spaces, found 2`):
a warning is not a finding, the script still passes, but the user reads it, so fix it. Say in `say` lines what each
step found, so the answer reads well.

## Previewing a script

`ask_preview` checks a script the way `ask_submit` and the automatic reviews will, and stores nothing: ask lint, the DLP
masks (a value that looks like a key, a token or a password), and the namespace's automatic reviews, with their findings
by line. The reviews take a model run each, so it may answer `pending <id>`: call `ask_preview` again with that `id` (and
no script) until it gives the result. It is advice: the real checks run on the script you submit, and the user decides.
It is limited per hour and every call is in the user's event log, so fix what it found instead of trying variations.
Previewing costs the user's model time; submitting a script that fails costs a whole round.

**Keep a record of what fails.** After a preview, an ask lint refusal or a review (`review-failed` in `ask_status`) refused
something, add one entry to `ai/memory/plinth-review-failures.md` in your repo (create it if there is none, with the
usual memory header and a one-line pointer in `ai/memory/MEMORY.md`): the pattern (`curl | sh`, a literal token, `rm -rf`
on a variable, ...), the check that refused it, and what fixed it. Read that file before you write an ask script, and do
not repeat a pattern in it. Never put the secret itself in the record, only its shape.

## Run states

### Plinth runs (`ask_status`)

Each of your runs has an ask status, and once it ran, an answer status:

| Ask status | Meaning |
|---|---|
| `reviewing` | the automatic reviews run |
| `review-failed` | a review failed: the user overrides it or not |
| `approved`, `overridden` | the user may run it |
| `running` | it runs |
| `no-answer` | it ended without an answer; the user may run it again |
| `ran` | it ran: see its answer status |

| Answer status | Meaning |
|---|---|
| `scanning` | the DLP scan checks the answer (it may wait for its model) |
| `dlp-ok` | scanned: the user releases it |
| `dlp-failed` | the scan flagged it: the user decides; it may never be released |
| `overridden` | the user accepted a flagged answer: they release it |
| `released` | `answer_read` gives it to you |
| `invalid` | not a valid answer document: a bug; tell the user |

`, gate` means the script stopped at a `fail`.

### Plain AAA (`aaa_status`)

| State | Meaning |
|---|---|
| `not-run` | only `ask.sh`: the user hasn't run it yet |
| `running` | it runs, or was killed (a rerun is a new run) |
| `new` | it ended: the user runs `plinthctl finish` |
| `dlp-ok` | scanned: `aaa_read` gives it to you |
| `DLP-FAIL` | the scan flagged it: never read; tell the user |
| `INVALID` | not a valid answer document: a bug; tell the user |

`, gate` means the script stopped at a `fail`; `, in done/` that the user moved the run to `ai/work/done/`.

## The answer (`answer_read`, `aaa_read`)

A YAML document (`apiVersion: plinth/v1`, `kind: Answer`): `sections`, each with its `title` and `output`; `result`
(`ok`, or `fail` with `failure`, why it failed); and `status.dlp`, what the scan masked.
