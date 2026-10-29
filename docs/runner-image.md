# The runner image

Built so far: every `plinthctl image` command (`login`,
`publish`, `pull`, `use`, `list`, `delete`), the pin, and the controller's side: it runs only the pinned image and checks
for drift, and the TUI's Image screen. Not built yet: the Ollama review (publish says "Ollama review: not
available").
`plinth.exec.image-ref`, from when the controller built the image from the agent's checkout, is no longer used: remove it
from your `config.yml` (`plinthctl config check` warns while it is there).

## In one sentence

Every script runs in a container made from your repo's `.plinth/Containerfile`. You review that file on Forgejo, you
build and publish the image, and Plinth pins it by its digest and runs only that image.

## Setting it up

1. **Create `.plinth/Containerfile`** at the top level of the repo the namespace is for (`<owner>/<repo>`). A repo
   without it has no image, and no Plinth run can start. An agent may write it, and the MCP guide tells it to propose
   one, or a change, as a pull request when its scripts need a tool the image lacks. A minimal one:

   ```
   # The image Plinth runs this repo's ask scripts in.
   FROM docker.io/library/debian@sha256:<digest>
   RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
   ```

   Pin the base image by its digest: `skopeo inspect docker://docker.io/library/debian:trixie` shows it (`Digest`).
   Add what your scripts need from the distribution's packages (`ansible-core`, `git`, `jq`, `golang`). A script has
   no network while it runs (unless the namespace allows network runs and you accept each one), so it can't install
   anything itself. Files the build needs go in `.plinth/` too; `publish` checks them all (step 3 below).
2. **Merge it to the default branch.** `publish` builds from the default branch on Forgejo, never from a checkout, so
   a Containerfile counts once it is merged. Then follow the steps below.

## The steps

1. **Review the change on Forgejo (branch protection, recommended).** A change of `.plinth/` should reach the
   repo's default branch only through a pull request that you approve. Requiring approval isn't enough on its own:
   Forgejo checks approvals only when a pull request is merged, so any account allowed to push can still change
   the branch directly. Set all of these on the default branch's protection; it is the safer workflow, because
   then no change of `.plinth/` reaches the branch without your review:
   - **Push:** disabled, or allowed for you only.
   - **Protected file patterns:** `.plinth/**`. A direct push that touches them is refused, even from an account
     allowed to push.
   - **Required approvals:** at least 1, with the approvals whitelist set to you.
   - **Dismiss stale approvals:** on, so a commit pushed after your approval needs a new one.

   An agent may write the Containerfile, but then it can't merge it. Plinth can see only whether the branch is
   protected and needs an approval; while it isn't or doesn't, `publish` and the TUI's Image screen say "Branch
   protection is recommended" and point here. Without protection you can still publish: the diff `publish` shows
   you is then your only review.
2. **Log in to the registry:** `plinthctl image login -n <owner>/<repo>` (or `l` on the TUI's Image screen) runs
   `podman login <forgejo host>` for you. That login is yours: Plinth never sees the password, and its own Forgejo
   token has no access to packages. It lasts until your last session on the machine ends.
3. **Publish:** `plinthctl image publish -n <owner>/<repo>`. It:
   - fetches the default branch from Forgejo into a temporary bare repo, never from the agent's checkout;
   - checks `.plinth/` with its own tools. It refuses to build when a base image (`FROM`, `COPY --from`, `RUN --mount
     from=`) isn't pinned by digest, when `ADD` fetches a URL or a git repo or names a variable, when a step asks for
     the host's network (`RUN --network=host`) or `RUN --security=insecure`, or when a step uses a heredoc (`RUN
     <<EOF`; put the script in a file in `.plinth/` instead), or when it has `ONBUILD` (its instruction would run in
     a later build, unchecked), or when a base image carries `ONBUILD` triggers of its own, which the build would
     run unchecked. In any of the folder's files, it also refuses a
     download that is run as it arrives (`curl ... | sh` and the like), and a `curl` or `wget` download whose line
     doesn't check it against a checksum written out there. Put the file in `.plinth/` (best), or pin it:
     `curl -fsSLo f <url> && echo '<sha256>  f' | sha256sum -c && <use f>`;
   - asks Ollama for a second opinion, which can only add findings, never remove one (when Ollama isn't available,
     it says so and you can still publish);
   - shows you every change in `.plinth/` since the pinned image (every file, the first time), and every finding,
     then waits for you to type `yes`;
   - builds, shows the image's ansible-core version (or `none`) and labels the image with it, pushes to
     `<forgejo host>/<owner>/plinth-<repo>` with your login, tagged `<commit>-<time>`, writes the pin into the
     namespace's `config.yml`, pulls the image back, and deletes the temporary repo.
4. **Pull it on each other machine that runs scripts:** `plinthctl image pull -n <owner>/<repo>`. The controller
   never pulls. Without the image, a run fails before it starts and tells you that exact command (the TUI shows it
   too); without a pin, it tells you to publish. An image whose labels don't match the pin (commit, tree, namespace)
   doesn't run either.

All of this is also on the TUI's Image screen (`I`), see below.

## Ansible in the image

Runs are becoming Ansible playbooks ([ansible-runs.md](ansible-runs.md), not built yet), so the image will need
Ansible. The recommended way:

```Dockerfile
FROM docker.io/library/debian:trixie@sha256:<digest>
RUN apt-get update && apt-get install -y --no-install-recommends ansible-core && rm -rf /var/lib/apt/lists/*
COPY collections/ /usr/share/ansible/collections/
```

- **Debian trixie or later,** pinned by digest like any base image. Its `ansible-core` package has the fixes for the
  runtime CVEs of 2024, which matter here; Ansible comes from the distro package, never from pip.
- **Collections as files in `.plinth/collections/`** (`ansible_collections/<namespace>/<name>/...`), extracted and
  reviewed in the pull request like the rest of `.plinth/`, then copied in. No `ansible-galaxy` at all: its install
  commands had three High CVEs in 2026, one of them (a crafted collection archive writes files anywhere,
  CVE-2026-89091) not fixed in Debian yet. If you use it all the same, that's your call: `publish` points it out but
  still builds.
- `publish` shows the image's ansible-core version and labels the image with it (`plinth.ansible-core`). It doesn't
  refuse an old one: a Debian build can carry fixes its version number doesn't show.

## The pin

The pin is `plinth.exec.image` in the namespace's `config.yml`:

```yaml
plinth:
  exec:
    image:
      ref: forgejo.example.org/michael/plinth-devops@sha256:<digest>
      commit: <the reviewed commit>
      tree: <the git hash of its .plinth/ folder>
```

The digest is a hash of the image itself, so it names exactly the same bits on every machine. A tag such as `:latest`
can be moved; a digest can't. Only `image publish`, or `image use` for an image published elsewhere, changes the pin.

To check an image yourself: `podman image inspect <pin>` shows the commit and the tree in its labels, and Forgejo (or
any fresh clone) must give the same tree for `<commit>:.plinth`. Each step from the reviewed commit to the running
image is a hash, except the build, which you do yourself.

## The drift check

Before each run, the controller asks Forgejo's API which `.plinth/` folder the default branch has now, and compares it with
the pin's tree.

- **They match:** nothing to do.
- **They differ:** `.plinth/` changed since you published. The event log (`image.drift`), the controller's output and
  the TUI's Image screen warn. The run still uses the pinned image, the one you vetted. Look at what changed
  and, if you want it, publish again.
- **"Drift not checked":** Forgejo couldn't be reached (within 5 seconds). The run went ahead with the pinned image;
  nothing else changed. The general log says why.

Each run's `exec.start` event says which it was, next to the image's digest and commit.

The check only warns, never blocks: a newer `.plinth/` on the branch never replaces your image without a new `publish`.

## The Image screen

`I` in `plinthctl tui` shows the namespace's image repo, whether you're logged in, the pinned image, and every
image in the registry (tag, digest, commit, created, whether this machine has it), and the drift check of the last
run. Its keys run the same commands, in your terminal, and each asks its own question; when one ends, press Enter to
go back to the screen:

| Key | What |
|---|---|
| `l` | log in (`image login`, which runs `podman login`) |
| `o` | log out (`image logout`, which runs `podman logout`) |
| `b` | build and push from the branch (`image publish`) |
| `u` | use the selected image (`image use`): it becomes the pin |
| `p` | pull the selected image to this machine (`image pull`) |
| `Ctrl-D` | delete the selected image: this machine's copy, or from the registry (can't be undone) |

The pinned image can't be deleted; use another one first. Deleting from the registry can't be undone, and Plinth
knows only this machine's pin: another machine that pins the image can't pull it any more.

## Why it works this way

- **A fresh fetch from Forgejo, not the agent's checkout.** The agent can write the checkout's `.git/` folder, and git
  takes orders from it: settings there can make git run a program, or report a hash that doesn't match the content.
  And a local copy of a branch is just a file the agent can point anywhere, so Forgejo's branch protection never
  applies to it.
- **You publish, not the controller or the agent.** The agent may write the Containerfile but can't publish it, and the
  person who vetted the file is the one who publishes it.
- **A digest, not a tag.** Tags move; a digest gives the same bits on every machine. Building the same Containerfile
  twice doesn't give the same bits (package installs change), which is why the pin is the digest, not the commit.
- **The controller doesn't pull.** The registry login is yours, and Plinth holds no registry credential.
- **Ollama only adds findings.** The Containerfile is untrusted input to the model: a comment in it could talk the
  model out of a finding, so only the tools decide.
- **The drift check only warns.** What you vetted is the pin, not the branch.
- **The fetched repo is deleted after each publish.** Building is rare, and a lasting copy would keep history that
  Forgejo may have dropped, such as a secret removed by a force-push.
- **skopeo for the registry.** podman builds and runs images, but it can't list a registry's images with their labels
  or delete one there; skopeo can. It comes from the same project as podman, uses the same registry code and reads the
  same login, so it adds little of its own: the one CVE filed against it is from 2019 (CVE-2019-10214, in the
  registry library it shares with podman, so it hit podman too), and its updates since are mostly rebuilds for
  Go's own fixes. Keep it updated with the
  system. Only the `image` commands use it, never the controller.
- **The tag has the commit and the time.** You can read it at a glance, and a rebuild of the same commit gets a new
  tag instead of taking the old one's.
- **`plinthctl` does the image work, the controller only watches** (like kubectl and a controller). The commands work
  whether or not the controller runs and write the general log. The event log still shows what ran: every controller event
  carries the config's hash, so a pin change shows in the next one, each run records its image's digest and commit,
  and the controller warns of drift.
- **An image is as private as its owner, not its repo.** Forgejo's packages belong to the owner (a user or an
  organization), not to a repository. On a shared server, every account that can sign in could read the image of a
  private repo whose owner is public. The image repo has the namespace's owner, so on a shared server keep such a
  repo under a private organization.
