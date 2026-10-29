# Ansible runs

Decided (amended 2026-10-18); not built yet. The spec is
`ai/plinthctl.README.md`, "Ansible runs" and "Risk score and policy".

## In one sentence

Every run becomes an Ansible playbook: Plinth checks which modules it uses and scores how risky it is, runs it with
`--check --diff` first, and for remote hosts gives it a short-lived ssh certificate that only you can sign.

## Why playbooks

A bash script can do anything, so the only safety is reading every line. A playbook is a list of documented modules
with documented arguments: Plinth can tell before the run which hosts it touches, what each task is meant to change,
and which tasks are the risky kind (`shell`, `command`, `raw`, `script`). Those raise the run's risk score; the
policy in your global config decides what happens at each score, and you can set the scores themselves, globally or
for one repo.

A playbook in the run's container never asks anything while it runs: nobody can answer there, and those runs are to
move to Kubernetes later. Work that needs your own terminal is a privileged playbook, run there, outside any container.
Only such a playbook may ask, and only with Ansible's own prompts, when its header says `# INTERACTIVE: 1`: your sudo
password for `become` on this machine, or the vault password (below). Plinth decides which prompts Ansible gives,
from what its check found in the playbook; the agent never controls the command line. You type the passwords into
Ansible itself, with nothing echoed, and they never reach the run's answer. A playbook's own questions
(`vars_prompt`, a `pause` that asks) are refused everywhere: their text is the agent's, and the playbook would read
your answer itself.

## What you set up once

1. **Two Plinth CAs:** signing keys of Plinth's, not your login key, each with a passphrase or held in your ssh agent.
   The user CA signs each run's certificate; the host CA vouches for your hosts. They are separate, so a leak of one
   doesn't give away the other.
2. **Your hosts:** a playbook you run yourself, natively, sets `TrustedUserCAKeys` to the user CA, signs each host's
   host key with the host CA, and sets `HostCertificate`. Plinth's `known_hosts` then needs one line for all of them.
3. **The source address:** `plinth.ansible.source-address`, the address your hosts see Plinth's runs come from. Without
   it no remote run starts: every certificate is tied to that address.
4. **The inventory:** the hosts and groups a namespace's runs may reach, in the TUI's Ansible screen. A run can't add
   hosts. A host without a host certificate gets its key by `ssh-keyscan` there, once you type `trust`.

## What happens per run

- Plinth checks the playbook: every module by its full name, through includes and roles. Until there is a risk score,
  whatever would score high is refused, such as `become` (except in an interactive privileged playbook), a playbook
  setting its own connection, or code that runs inside the run's container in a playbook that also reaches hosts.
- It computes the risk score, from 0 (reaches nothing, holds no secret) to 5 (secrets and the internet).
- For a remote run, the controller makes a fresh key pair. You see the hosts, the remote users, how long the certificate
  will be valid (60 minutes by default) and the score, type `sign`, and sign it with the user CA. Nobody else can: the
  controller holds no signing key.
- The playbook runs with `--check --diff`: a preview, nothing changed. Applying stays with you for now, run natively.
- The result becomes the run's answer, scanned like any other.

## Passwords from a vaulted file

An interactive privileged playbook can take your sudo password, and other secrets it needs, from
`<ns-dir>/ansible/vault.yml`: a YAML file whose values you encrypt one by one with `ansible-vault encrypt_string`.
Plinth reads the names, never the values, and refuses a name that would change how Plinth connects to hosts.

- Only a playbook whose header says `# VAULT: 1` gets the file, and that needs `# INTERACTIVE: 1` and
  `# PRIVILEGED: 1`: it runs in your terminal.
- You type the vault password each time, so the file stays protected on disk. There is no vault password file.
- Runs in the container get no vault; their secrets come from the env file, as before.

The vault hides nothing from the playbook: Ansible decrypts the values for it, so a playbook can print them. A
`# VAULT: 1` run counts as holding secrets, like one with an env file, and its answer is scanned the same way. For
`become` on your hosts, use a `NOPASSWD` sudo rule for the exact commands.

## If a certificate must stop working

Each certificate is valid only for its lifetime (60 minutes unless you set `plinth.ansible.cert-minutes`), only from
the source address, and has no forwarding; a remote run can't outlast it. To stop one sooner, add it to `RevokedKeys`
on the hosts; the event log names each run's certificate.
