# Plinth docs

What to know as a user of Plinth. The spec is in `ai/plinthctl.README.md`; this is the short version of what matters
when you run scripts.

## The controller

The controller is the one Plinth process: it serves the Plinth MCP server your agents use, runs the scripts you start,
scans their answers. Keep it running while your agents work, as a service:

| Command | What it does |
|---|---|
| `plinthctl controller start` | starts it in the background; returns once it serves, with its PID, the MCP server's URL and anything that needs you |
| `plinthctl controller status` | whether it runs (exit 0, or 3 when it doesn't), its namespaces, the runs running now, what needs you, its last log lines |
| `plinthctl controller stop` | stops it, and waits up to 30 s until it has ended |
| `plinthctl controller restart` | stop, then start: what a change of the global `config.yml` needs |
| `plinthctl controller run` | runs it in the foreground instead, until Ctrl-C |

**`stop` won't stop the controller while a script is running.** It names the scripts still running and stops
nothing. Stopping the controller stops every script it runs (SIGTERM, SIGKILL 30 s later), and those runs count as
killed, so wait until they have ended. `plinthctl controller stop --force` stops it anyway, as Ctrl-C does; `restart`
follows the same rule and takes `--force` too.

Started in the background, the controller has no terminal for what it would tell you: `status` lists it under "needs
you", and the TUI's header counts it. Its output goes to `<work>/logs/controller.out`.

## Agents

An agent works with Plinth through the Plinth MCP server, which explains Plinth to it itself (its `guide` tool): how to
set one up is [agent-mcp.md](agent-mcp.md).

## Why every run is a container

> **There is more to hide than there is to provide.**

A script needs very little: its own run folder, the tools of its repo, a few variables, and sometimes the network. Your machine holds a great deal more: your keys and passwords, your shell's startup files, the programs
running in your session, Plinth's own state, every other project. Making a script safe by hiding things means listing
everything it must not touch, and anything the list forgets is open. That list only grows.

So Plinth turns it around: every run gets a rootless podman container that starts empty, and Plinth adds only what the
run needs. Anything not added simply isn't there, and nothing has to be remembered to be hidden. The
container's tools come from your repo's `.plinth/Containerfile`, like a CI runner's: you review it on Forgejo and
publish the image, and Plinth runs only that image, by its digest ([runner-image.md](runner-image.md)). Privileged
scripts are the one exception: they run in your own terminal, see [privileged-mode.md](privileged-mode.md). Every run
will become an Ansible playbook ([ansible-runs.md](ansible-runs.md), not built yet).

## A run's memory limit

Built. Plinth is a safer way to run small ad-hoc scripts, not applications, so every run gets a
memory limit of **512m** by default, with no swap on top. That's plenty for `kubectl`, `curl`, `git` or a small
Python tool. Files a script writes to `/tmp` or `$HOME` count too: inside the container both are held in memory.

A script that goes over the limit is killed (SIGKILL) inside its container; nothing outside it is affected. The run ends
without an answer, so it isn't done: you can run it again (`plinthctl exec`, or `e` in the TUI) or scrub it. Its
`exec.end` says `exit 137 (signal 9, killed); over its memory limit (plinth.exec.memory 512m), unless the script sent
SIGKILL itself`: podman doesn't reliably report the kernel's kill, so Plinth reads it from the signal (only the kernel
or the script itself sends SIGKILL in there; a stop or timeout is said as such).

**To raise or lower it for one namespace,** set `plinth.exec.memory` in that namespace's `config.yml`
(`<work>/<owner>/<repo>/config.yml`):

```yaml
plinth:
  exec:
    memory: 1g
```

**To change the default for every namespace,** set the same key in the global `<work>/config.yml`. A namespace's own
value always wins.

The value is a number with `k`, `m` or `g`, from `6m` to `64g`. `plinthctl config check -n <owner>/<repo>` shows the
limit that applies and whether it comes from the namespace or the global config. The controller picks up a namespace's
changed `config.yml` by itself; a change of the global one needs `plinthctl controller restart`. A run already going keeps the limit
it started with. A value Plinth can't read stops the namespace's config from loading, and the controller serves that
namespace no more until it is fixed; `config check` names the problem.

Podman can only enforce the limit when your user has the memory controller (cgroups v2, delegated by systemd, the
default on most distributions). Without it the controller runs no script at all, rather than one without its limit, and
says why when it starts.

## A script in network mode

Built. Your namespace has to allow it first: `plinth.exec.network: true` in its
`config.yml` (or in the global one).

A script runs in normal mode unless its header says otherwise: no network, and nothing outside its run directory to
write to. A script that says `NETWORK=1` gets a network of its own once you accept that run (`plinthctl accept
network`, or `w` in the TUI, then type `yes`): the internet, but not the services running on this machine, and no
Unix socket of yours.

Network mode is for scripts that call HTTP APIs (`curl`, a REST client, `git` over https). A token such a script needs
goes in the namespace's env file (`plinth.exec.env-file`). The script can read that token and send it anywhere, so
give it one scoped to what the namespace's scripts need, and read a network run's steps for what it sends where before
you accept it.

**No ssh.** A run gets nothing of your `~/.ssh`: no key, no
`known_hosts`, no config, and not your SSH agent. Remote work over ssh, reading or writing, belongs to Ansible (or
other IaC) that you run yourself; a script may help write a playbook as a reviewed change, but doesn't run it. Git
in a run goes over https, with a token from the env file (`git clone https://...`); git over ssh may come later.
Don't put a private key in the env file to get around this: the env file is for API tokens. Ask lint refuses a
script that isn't privileged when it uses `ssh`, `scp`, `sftp`, rsync over ssh, `GIT_SSH*` or an ssh remote
(`git@host:...`, `ssh://`). It checks words, not shell syntax, so `echo "ssh is off"` is refused too: reword it. A script that really needs your own ssh (your agent, a key with a passphrase, a hardware key) is
a privileged script (`PRIVILEGED=1`). It runs only in your own terminal (`plinthctl exec` or the TUI), after you
accept it there (`privileged`). See [privileged-mode.md](privileged-mode.md).
