# Setting up an agent (the Plinth MCP server)

Built 2026-10-21 for the MVP. The Plinth MCP server is how an agent works with
Plinth: it reads what Plinth is, writes ask scripts, follows its runs and reads their answers. An agent in a container
has no `plinthctl`, so this server is its only way into Plinth, for both ways of running a script. You don't need to
explain Plinth to the agent: its first call, `guide`, does that.

## Two ways to run a script

| | Plinth runs (the default) | Plain AAA (the informal way) |
|---|---|---|
| The agent's tools | `ask_template`, `ask_preview`, `ask_submit`, `ask_status`, `answer_read` | `aaa_new`, `aaa_lint`, `aaa_status`, `aaa_read` |
| Where the run lives | Plinth's work directory, which the agent can't reach | the agent's repo, `ai/work/<RUN>/` |
| Before it runs | lint, the automatic reviews, then you | the agent's own lint (`aaa_lint`), then you |
| Where it runs | a container of the namespace (privileged: your terminal) | your terminal, as you |
| You | `plinthctl exec`, then `plinthctl done` (or `e`, `d` in the TUI) | `./<RUN>/ask.sh` from `ai/work`, then `plinthctl finish <RUN>` |
| The agent reads the answer | once you released it | once its DLP scan passed |

**Plinth runs are the secure default**, and the guide tells the agent to use them unless you ask for plain AAA: the
agent can't reach the run's files, so the script you approve is the one that runs, and it sees an answer only after
Plinth and you let it through. **Plain AAA is the compromise**: its files are in a repo the agent can write to, so it
rests on your trust in the agent, and Plinth checks less before it runs. It suits quick, informal rounds.

## What the agent gets

| Tool | What it is for |
|---|---|
| `guide` | Start here: what Plinth and the AAA workflow are, both ways to run a script and which to use, how to write an ask script, what you do between its calls, and the review policies its scripts are checked against |
| `ask_template` | The template every Plinth run's script starts from |
| `ask_preview` | Checks a script as `ask_submit` and the automatic reviews would (lint, DLP masks, reviews), stores nothing, limited per hour (`plinth.preview.limit`) |
| `ask_submit` | Hands in a script: Plinth checks it, names it (its `RUN_NAME`) and stores it, or says why it refused it |
| `ask_status` | The agent's own Plinth runs and their states, each with what it does next |
| `answer_read` | A Plinth run's answer, once you have released it (Done) |
| `aaa_new` | Plain AAA: creates `ai/work/<RUN>/ask.sh` in the agent's repo from the plain template (as `plinthctl new-ask`) |
| `aaa_lint` | Plain AAA: checks a run's `ask.sh` as `plinthctl ask lint` does, so the agent fixes it before asking you to run it |
| `aaa_status` | Plain AAA: the runs in the repo's `ai/work` and `ai/work/done`, with their states |
| `aaa_read` | Plain AAA: a run's answer once `plinthctl finish` scanned it (as `plinthctl read-answer`) |

The agent never runs, approves, releases or deletes anything, and sees only its own Plinth runs. Its plain AAA runs
are in the repo it registered from (`register --repo`); the token decides which, never an argument. What it would read
is what `plinthctl agent guide -n <owner>/<repo>` prints: read it once, so you know what your agents are told.

## Setting it up

1. **The controller runs** (`plinthctl controller start`; `controller status` says how it is), as long as your agents
   work: the MCP server is part of it, at
   `http://<plinth.mcp.listen>:<plinth.mcp.port>/mcp`, by default port 8791 on every interface of the machine. Keep it
   running as you would any MCP server your agents use; plain AAA needs it too. The controller's log says where it
   listens.
2. **Register the agent** from its repo. `register` prints a new token, once, and keeps only its fingerprint; capture
   it in the script that starts the agent, so it is never written down:

   ```bash
   PLINTH_TOKEN="$(plinthctl register --repo "$PWD" --agent main)"
   export PLINTH_TOKEN
   ```

   Each `register` replaces the agent's previous token (`plinthctl agent token-rotate` does the same without the
   setup).
3. **Enable it** once for that repo: `plinthctl agent enable -n <owner>/<repo> main`. A registered agent that isn't
   enabled gets every call refused, and is told to ask you.
4. **Give the agent the server.** For Claude Code, in the repo's `.mcp.json` (the `${PLINTH_TOKEN}` is read from the
   environment the agent starts in, so the token stays out of the file):

   ```json
   {
     "mcpServers": {
       "plinth": {
         "type": "http",
         "url": "http://host.containers.internal:8791/mcp",
         "headers": { "Authorization": "Bearer ${PLINTH_TOKEN}" }
       }
     }
   }
   ```

   `host.containers.internal` is the machine Plinth runs on, as seen from an agent in a podman container. An agent
   running on that machine itself uses `http://127.0.0.1:8791/mcp`. Other MCP clients need the same two things: the URL,
   and the header `Authorization: Bearer <token>`.

5. **The image for Plinth runs.** A Plinth run happens in a container made from the repo's `.plinth/Containerfile`,
   which you publish and Plinth pins: no run starts without it. Set it up as [runner-image.md](runner-image.md) says.
   The guide tells the agent to propose a change to that file, as a pull request, when its scripts need a tool the
   image lacks; publishing it stays yours.

The agent can now start with `guide`. In Claude Code its tools show up as `mcp__plinth__guide`,
`mcp__plinth__ask_submit` and so on.

## What you do

**A Plinth run:** the agent submits; everything after that is yours, as for any run: read the script and its
reviews, run it (`plinthctl exec`, or `e` in the TUI), then release its answer once the DLP scan passed (`plinthctl
done`, or `d`). The agent sees each step in `ask_status` and reads the answer with `answer_read`.

**Plain AAA:** the agent creates the run with `aaa_new`, writes `ai/work/<RUN>/ask.sh` in its repo, checks it with
`aaa_lint` and asks you to run it. Read it, run it from `ai/work` (`./<RUN>/ask.sh`), then `plinthctl finish <RUN>`
(it scans the answer), and tell the agent "`<RUN>` done". It reads the answer with `aaa_read`. The lint is only the
plain one: no automatic review reads the script, you do.

## When something doesn't work

| The agent gets | What it means |
|---|---|
| HTTP 401 | No token, or not a current one: register again and restart the agent with the new `PLINTH_TOKEN` |
| `refused by rule target.valid: ... not enabled` | `plinthctl agent enable` for that repo |
| `Plinth doesn't serve your namespace ...` | The namespace's config doesn't load, or the controller is reloading it: `plinthctl config check -n <owner>/<repo>` |
| `pending <id>` | The controller hasn't taken the script in within 30 s; `ask_status` shows it once it has |
| `ai/work in your repo can't be opened (...)` | Plain AAA: `ai` or `ai/work` in the agent's repo is a symlink, or not a directory: Plinth works only in a real `ai/work` |
| `no repo is registered for you ...` | Register the agent again from its repo (`plinthctl register --repo ...`) |
| No connection at all | The controller isn't running, or the agent's container can't reach the port |

## What to know about its security

- **Plain HTTP for now.** The token travels unencrypted between the agent and this machine, and the
  port is open on every interface by default. Anyone who can watch that traffic can act as that agent: submit scripts
  (which still need you to run them) and read its released answers. Until it is encrypted, keep the port off networks you don't
  trust (a firewall rule, or `plinth.mcp.listen` set to the address the agents use).
- **The token is the agent's identity.** Plinth keeps only its fingerprint; treat the token like a password, and
  register again if it may have leaked: the old one stops working at once.
- **Plain AAA's files are in the agent's repo.** Plinth reads and writes there only below `ai/work`, refuses a
  symlinked `ai` or `ai/work` and every symlink or hard link inside it, and gives the agent an answer only once its
  DLP scan passed; but the agent can read the files itself, and change a script before you run it. Read what you run,
  and prefer Plinth runs for anything that matters.
- **The review policies are shown to the agent** (the URLs of `reviewers.type.<type>.urls`), so it can write scripts
  that follow them. That is the point; it also means an agent knows what the reviews look for, which is one reason you
  still read every script yourself. Keep the policies in a repo your agents can't change.
