# Privileged mode

Built 2026-10-18 for ask scripts. Once playbook runs exist, a privileged script is a playbook run in your
terminal ([ansible-runs.md](ansible-runs.md)), which may ask for your sudo or vault password through Ansible's own
prompts when its header says `# INTERACTIVE: 1`; what this page says about having no box stays true.

## In one sentence

A privileged script runs exactly as if you had typed it into your own terminal yourself: nothing stands between it and
your account, so Plinth only lets it run after you have read it and said yes.

## Why there are modes at all

Every script an agent writes runs in one of three modes, named in the script's first lines:

- **Normal** (most scripts): it runs in a sealed box, a container that holds only its own run folder and its repo's
  tools. It sees none of your files, keys or passwords, and it has no network.
- **Network** (`NETWORK=1`): the same box, but it can reach other machines over the network, for HTTP APIs. It
  gets no ssh: nothing of your `~/.ssh`.
- **Privileged** (`PRIVILEGED=1`): no box at all.

The box is what lets you accept normal scripts with a quick read. A privileged script has no box, so all of the safety
is your reading it before it runs.

## When a script needs privileged mode

When the box would stop it from doing its job, for example a script that:

- uses `sudo`, or asks you a question while it runs (`INTERACTIVE=1` always means privileged);
- needs your SSH agent, a key with a passphrase, or a hardware key (YubiKey and similar);
- has to change files outside its run folder, such as a config file in your home directory;
- talks to a program running on your machine (Docker or Podman, systemd, a database on `localhost`).

If a script asks for privileged mode and you can't see why it needs it, ask the agent to do without it.

## What a privileged script can do

Everything you can do, because it runs as you, with nothing in the way:

- read and change any of your files, your keys and passwords included;
- use the network and your SSH agent, and log in to anything you can log in to;
- **change Plinth itself**: its settings (for example, switching on network or privileged mode for scripts that come
  later), its event log, and the state of its own answer, such as marking it as done. Plinth records a fingerprint of
  its settings with every event, so a change to them shows up afterwards, but it can't stop it.

## How a privileged run happens

1. The agent submits the script; Plinth checks it and the reviewers read it, as for any script.
2. You start it from your own terminal: `plinthctl exec -n <namespace> <RUN>`, or `e` on the run in the TUI. A controller
   in the background never starts it, but it must be running: it records the run and scans the answer. Its event log
   says the run was requested from your terminal, not that you typed `yes`: the controller can't see that, and any
   program running as you (a privileged script, say) could ask it the same way.
3. Plinth shows you what the script will be able to do (the list above) and asks you to type `yes`. Anything
   else cancels.
4. The script runs in that same terminal. In the TUI, the screen gives way to the script and comes back when it ends;
   this works inside tmux, screen or an SSH session too. What the script prints goes to your terminal as it is.
5. When it ends, Plinth scans its answer for secrets, as for every run, and you decide whether the agent gets it. If
   the terminal closes while the script still runs, Plinth waits for the script to end before it scans the answer.

## Before you type `yes`

Read the whole script, not just its description. Look in particular for:

- anything that touches `~/.ssh`, key files, password stores, `~/.aws`, `~/.kube`;
- anything that touches Plinth's own folder or settings;
- downloads that are then run (`curl ... | bash`), and text that is decoded and then run (`base64 -d`, `eval`);
- commands you don't recognise.

When in doubt, don't run it: tell the agent what worries you and let it write a script that needs less.
