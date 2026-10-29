# plinth

Plinth is a lightweight zero trust, agent action enforcement utility, `plinthctl`.

It makes running scripts an agent wrote safer, not safe. It's a middle ground between running whatever the agent comes
up with and a complete, fully sandboxed CI/CD pipeline in a VM, which is what to use when a script must not be able to do harm.
Plinth checks every script before it runs, makes you start every run, runs it contained by default, scans its answer
before the agent sees it, and records every decision. A script still runs as you, so you still review it before you run
it. That suits an agile, ad-hoc workflow, such as quick script prototyping and tool building, rather than a heavy, formal
CI/CD pipeline.

## Plinth's Forgejo token

Plinth uses one Forgejo token of its own, `forgejo-token` in the global config (usually
`forgejo-token: "${PLINTH_FORGEJO_TOKEN}"`, see `src/internal/workdir/config.yml.dist`). It is never an agent's token.
These settings are a first guess from what Plinth does with the token; fix them here if Forgejo says otherwise.

**Account:** a dedicated Forgejo user (e.g. `plinth`), not your own and not an agent's. A Forgejo token can't be limited
to repos; it reaches every repo its account can. So give that user collaborator access only where Plinth needs it:

| Repo | Access | Why |
|---|---|---|
| the runs repo (`plinth.repo`, a private repo: it holds every saved run) | Write | Save and Report open the run's issue there and comment on it; no namespace repo needs access for them |
| the repo with the review prompts and the message catalogue | Read | review prompts, `plinth-messages.yml` |

If Save or Report fails on an attachment, raise that repo to Write.

**Token** (Settings > Applications > Generate new token):

| Setting | Value | Used for |
|---|---|---|
| issue | Read and write | Save and Report (issues, comments, attachments); message review (read) |
| repository | Read | checking a namespace's repo exists; review prompts and catalogue (`.../raw/branch/...`); `image publish` fetches the default branch, the controller's drift check reads its `.plinth/` tree |
| activitypub, admin, misc, notification, organization, package, user | No access | |
| Repository and Organization Access | All | private repos; "Public only" blocks every call |

The web UI's login doesn't use this token; it uses its own Forgejo OAuth2 application (`plinth.web-ui.auth.client-id`).

## The runner image

Every script runs in a container made from the namespace repo's `.plinth/Containerfile`, which you review on Forgejo
and publish yourself ([docs/runner-image.md](docs/runner-image.md); not built yet). For that:

- **Branch protection** (recommended) on each namespace repo's default branch: push disabled (or allowed for you
  only), protected file patterns `.plinth/**`, at least 1 required approval with the approvals whitelist set to
  you, and stale approvals dismissed. Your approval of a pull request is the review of every `.plinth/` change;
  approval alone doesn't stop a direct push ([docs/runner-image.md](docs/runner-image.md), step 1).
- **Forgejo's container registry** (Packages) enabled, and `podman login <forgejo host>` as yourself (`plinthctl image
  login` runs it) on the machine you publish from and on every machine that pulls. Publishing, listing, pulling and
  deleting images use your login, never Plinth's token, which keeps package: No access. Plinth's token reads the
  repository (above) to fetch the branch it builds from and to check for drift.
  An image is as private as its owner, not its repo: on a shared server, keep a private namespace repo under a
  private organization.
- **skopeo** (package `skopeo`) wherever you use the `image` commands or the TUI's Image screen; the controller doesn't
  need it.

## License

Plinth is licensed under the [Mozilla Public License 2.0](LICENSE). Copyright (c) 2026 michael. You may use and build on
Plinth, including in closed products; changes to Plinth's own files must be shared under the same license, and the
copyright and license notices must be kept.
