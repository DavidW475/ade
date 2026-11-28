# ADE – AI Developer Environment

A control center for AI agents, built for **WSL2**: start and watch agents, let them work in
**Docker sandboxes** behind an **egress proxy**, look at their workspaces in a
**VS Code-compatible browser IDE**, and review and approve their results – all in the browser.

> **Status:** 1.0 – agents locally or in Docker sandboxes behind an egress proxy, review and
> delivery in the browser, approvals, costs and budgets, software factories with triggers
> (GitHub issues, schedule), install script and systemd service. Start with the
> [user guide](doc/guide.md). The plan is in [`doc/`](doc/README.md), progress in the
> [milestones](https://github.com/DavidW475/ade/milestones).

## Idea

```
ade start  →  http://localhost:7000
```

- **Dashboard:** projects, running agents, costs, open approvals
- **Workspaces:** every agent works in its own copy on its own branch
- **Browser IDE:** one click opens the workspace in openvscode-server
- **Sandbox:** a Docker container per workspace that only sees its workspace; internet only
  through an egress proxy with an allowlist, and the proxy inserts API keys (the container never
  sees them)
- **Review:** look at the diff, merge, create a PR, give feedback or discard
- **Software factories:** planner, implementer and reviewer as a pipeline with approvals in
  between, each role with its own model and permissions (planner and reviewer read-only)

## Requirements

- **Windows with WSL2** (tested with Ubuntu); ADE runs in WSL, the dashboard in the Windows
  browser. It works the same on a regular Linux (browser via `xdg-open`).
- **Git ≥ 2.38** (`git --version`; for `git merge-tree --write-tree`)
- **[Claude Code](https://docs.claude.com/claude-code)**, installed and logged in – for the
  adapter `claude`. Alternatively `providers.anthropic.api_key` in the configuration.
- **Docker Engine** in WSL (or Docker Desktop with WSL integration, rootless Docker, Podman) –
  for the sandbox. Without Docker, agents only run locally (switch "Sandbox" off). Docker pulls
  the image `ghcr.io/davidw475/ade-agent-base` with the first sandbox workspace (~1.5 GB).
- **[GitHub CLI](https://cli.github.com/)** (`gh`), logged in with `gh auth login` – only for
  "Create PR", factories and issue triggers. Pushes use your normal git credentials.
- Repositories preferably on the WSL file system (e.g. `~/git/…`), not under `/mnt/c`.

## Installation

The install script downloads the binary of the [latest release](https://github.com/DavidW475/ade/releases/latest)
for your architecture (amd64, arm64), verifies the checksum from `checksums.txt`, puts it into
`~/.local/bin` and reports missing requirements:

```sh
curl -fsSL https://raw.githubusercontent.com/DavidW475/ade/main/install.sh | sh
ade version
```

With `sh -s -- --service` it also sets ADE up as a background service (see below).
`ADE_VERSION=v1.0.0` selects a specific version, `ADE_INSTALL_DIR` another directory. If
`~/.local/bin` is not in your `PATH` (Ubuntu only adds it after a new login), run
`export PATH="$HOME/.local/bin:$PATH"` once or add it to `~/.bashrc`.

It works by hand as well (for ARM `ade_linux_arm64.tar.gz`):

```sh
curl -fsSL https://github.com/DavidW475/ade/releases/latest/download/ade_linux_amd64.tar.gz \
  | tar -xz -C ~/.local/bin ade
```

### As a background service

If systemd is active in WSL (`[boot]` `systemd=true` in `/etc/wsl.conf`, then `wsl --shutdown`),
the daemon runs as a systemd user service and starts with WSL:

```sh
ade install-service      # create the unit ~/.config/systemd/user/ade.service and start it
ade uninstall-service    # stop and remove it
```

The unit takes over the current `PATH` (git, docker, gh, claude); after updating the binary or
installing new tools, run `ade install-service` again. `ade start` then starts the service
instead of starting a daemon itself. The log stays `~/.local/share/ade/daemon.log`. To keep the
service running without an open WSL shell: `sudo loginctl enable-linger "$USER"`.

## Quick start

```sh
ade start                          # start the daemon and open the dashboard in the Windows browser
ade project add ~/git/my-repo      # register a repository (also possible in the dashboard)
```

1. Open the project in the dashboard and **start an agent**: enter the task, adapter `claude`.
   ADE creates a workspace (local clone, branch `ade/<id>`) and starts the agent in it – with
   **sandbox** (default when Docker runs) in its own container that only sees this workspace.
   Claude Code needs an API key there (`providers.anthropic.api_key`); the egress proxy inserts
   it, the container only sees a placeholder. Which internet targets are reachable is set by
   `egress` in `.ade/sandbox.yaml`; the workspace's **Network** view shows allowed and blocked
   requests.
2. Watch the agent in the timeline; open the **IDE** or give follow-up instructions if needed.
3. When the agent is done, ADE commits its changes and fetches the branch `ade/<id>` into your
   repository. In the workspace, the **diff view** shows all changes.
4. Decide:
   - **Merge** – merge commit into the base branch of your repository (with conflicts nothing
     changes),
   - **Create PR** – push to `origin` and `gh pr create`,
   - **Give feedback** – a new agent session in the same workspace implements your comment,
   - **Discard** – remove workspace and branch without leftovers.

### Software factory

```sh
cd ~/git/my-repo
ade run feature --task "Add login with GitHub"
```

The factory `feature` lets a planner (read-only) write a plan, waits for your approval, lets an
implementer implement it and a reviewer (read-only) check it, and creates a PR after a second
approval. Progress, artifacts (plan, summary, review) and the approvals are shown in the
dashboard under **Runs**; a note you add when approving goes to the next role. Own factories
(other roles, models, prompts) live in `.ade/factories/`, see [doc/factory.md](doc/factory.md)
and [`examples/factories/`](examples/factories/).

`ade start --no-browser` prints the login URL instead of opening the browser.

`ade gc` removes orphaned sandbox containers and networks (`--dry-run` only lists them).

Configuration: `~/.config/ade/config.yaml` and per repository `.ade/project.yaml` and
`.ade/sandbox.yaml` (all optional, every option in the [reference](doc/configuration.md),
examples in [`examples/`](examples/)). Data lives in `~/.local/share/ade/`, workspaces in
`~/.ade/workspaces/`.

## Building from source

Requirements: Go and Node.js 22+ (see [CONTRIBUTING.md](CONTRIBUTING.md)).

```sh
make build        # build the dashboard and embed it into bin/ade
bin/ade start
```

Changes per version are listed in the [CHANGELOG](CHANGELOG.md).

## Documentation

- [User guide](doc/guide.md) – from installation to the first merged result, troubleshooting
- [Configuration reference](doc/configuration.md) – every option of all configuration files
- [Writing an adapter](doc/adapters.md) – connecting another agent CLI
- [Overview and terms](doc/README.md)
- [Architecture](doc/architecture.md)
- [Sandbox and egress proxy](doc/sandbox.md)
- [Threat model](doc/threat-model.md)
- [Software factories](doc/factory.md)
- [Milestones](doc/milestones.md)
- [Architecture decisions](doc/decisions.md)
- [Contributing](CONTRIBUTING.md)

## License

[Apache-2.0](LICENSE)
