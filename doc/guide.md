# User guide

This guide takes you from installation to the first merged agent result, and then through
everything ADE offers day to day: sandbox and network, approvals, costs, factories and
triggers. All options are listed in the [configuration reference](configuration.md).

- [1. Installation](#1-installation)
- [2. First agent](#2-first-agent)
- [3. Workspaces and review](#3-workspaces-and-review)
- [4. Sandbox and network](#4-sandbox-and-network)
- [5. Approvals](#5-approvals)
- [6. Costs and budgets](#6-costs-and-budgets)
- [7. Factories and triggers](#7-factories-and-triggers)
- [8. Troubleshooting](#8-troubleshooting)

## 1. Installation

### Requirements

| Tool | Needed for | Check |
|---|---|---|
| Windows 11 with WSL2 (Ubuntu or Debian) | ADE runs in WSL, the dashboard in the Windows browser. Any Linux works as well. | `wsl --status` |
| git ≥ 2.38 | Workspaces, diff and merge (`git merge-tree --write-tree`) | `git --version` |
| [Claude Code](https://docs.claude.com/claude-code) and/or [Codex CLI](https://github.com/openai/codex) | Adapters `claude` and `codex` (locally; in the sandbox they come with the image) | `claude --version` |
| Docker Engine in WSL, Docker Desktop with WSL integration, rootless Docker or Podman | Sandbox. Without it agents only run locally. | `docker info` |
| [GitHub CLI](https://cli.github.com/), logged in with `gh auth login` | **Create PR**, factory pull requests and issue triggers | `gh auth status` |

Keep your repositories on the Linux file system (`~/git/…`). Under `/mnt/c` every file access
crosses the 9P boundary and is orders of magnitude slower.

### Install

```sh
curl -fsSL https://raw.githubusercontent.com/DavidW475/ade/main/install.sh | sh
ade version
```

The script downloads the binary of the latest release for your architecture (amd64 or arm64),
verifies it against `checksums.txt`, installs it to `~/.local/bin` and warns about missing
requirements. Options (as environment variables in front of `sh`):

- `ADE_VERSION=v1.0.0` installs a specific version.
- `ADE_INSTALL_DIR=/path` installs somewhere else.
- `sh -s -- --service` also sets ADE up as a background service (see below).

If `~/.local/bin` is not in your `PATH` yet (Ubuntu adds it after the next login), run
`export PATH="$HOME/.local/bin:$PATH"` or add it to `~/.bashrc`.

Every release is signed with a build provenance attestation; to check a downloaded archive:
`gh attestation verify ade_linux_amd64.tar.gz --repo DavidW475/ade`.

### Starting and stopping the daemon

`ade start` starts the daemon in the background if it is not running and opens the dashboard
in the Windows browser. The login link in the browser is valid once for two minutes; with
`ade start --no-browser` ADE prints it instead. Calling `ade start` again opens a new login.

To run ADE as a **systemd user service** that starts with WSL, enable systemd in WSL
(`[boot]` with `systemd=true` in `/etc/wsl.conf`, then `wsl --shutdown` in Windows) and run:

```sh
ade install-service                  # create ~/.config/systemd/user/ade.service and start it
sudo loginctl enable-linger "$USER"  # keep it running without an open WSL shell
```

The unit copies your current `PATH` (git, docker, gh, claude); run `ade install-service` again
after updating ADE or installing new tools. `ade uninstall-service` removes it.

Stopping and restarting (for example after changing `config.yaml`):

```sh
ade stop      # stops the service or the background process and waits until it has ended
ade restart   # stop, then start again (without opening the browser)
```

Both find the daemon of the configuration themselves: the systemd service if it runs as one,
otherwise the background process from `daemon.lock`. A daemon in the foreground (`ade serve`)
stops with Ctrl+C.

When the daemon stops, running agents become `interrupted` and sandbox containers stop; files
stay in the workspaces. Sessions can be resumed after the restart.

### Updating

Run the install script again (or download the new archive), then restart the daemon; with the
service, `ade install-service` does both. The database is migrated automatically on start.

## 2. First agent

### Credentials

Agents need access to a model. You have two options:

- **The CLI's own login** (e.g. `claude` logged in with your subscription). Works locally
  without any configuration. In the sandbox the container has its own login state, see
  [sandbox.md](sandbox.md#4-secrets).
- **A Claude subscription token** (Pro/Max), recommended for the sandbox. Create it with
  `claude setup-token` and store it in a file only you can read:

  ```sh
  install -m 600 /dev/null ~/.config/ade/secrets/claude-oauth-token
  nano ~/.config/ade/secrets/claude-oauth-token   # paste the token (sk-ant-oat01-…)
  ```

  ```yaml
  providers:
    anthropic: { oauth_token: file:~/.config/ade/secrets/claude-oauth-token }
  ```

  In the sandbox the egress proxy inserts the token, the agent only sees a placeholder; locally
  ADE passes it to Claude Code. The token is valid for about a year; revoke it in your Claude
  account settings if it leaks.
- **An API key** in `~/.config/ade/config.yaml`, as a reference to an environment variable or
  a file (`file:…`, like the token above):

  ```yaml
  providers:
    anthropic: { api_key: env:ANTHROPIC_API_KEY }
    openai:    { api_key: env:OPENAI_API_KEY }
  ```

  In the sandbox the egress proxy injects the key, so the agent never sees it, and counts the
  tokens. The variable must be set in the daemon's environment. The service only takes `PATH`
  from your shell; put other variables into `~/.config/environment.d/ade.conf`
  (`ANTHROPIC_API_KEY=…`) and restart it.

### Register a project

```sh
ade start
ade project add ~/git/my-repo
```

Or use **Add project** in the dashboard. ADE never changes your repository except for fetching
agent branches (`ade/<id>`) and the merge you trigger. Optional project settings go into
`.ade/project.yaml` in the repository, e.g. setup commands that run in every new workspace:

```yaml
setup:
  - npm ci
```

### Start an agent

1. Open the project in the dashboard and click **Start agent**.
2. Enter the task, choose the adapter (`claude`, `codex` or `pty` for an interactive terminal)
   and optionally a model, a permission mode and a cost budget.
3. Leave **Sandbox** on if Docker is available. The first sandbox workspace pulls the image
   `ghcr.io/davidw475/ade-agent-base` (about 1.5 GB).

ADE creates a workspace (a local clone on branch `ade/<id>`), runs the setup commands and starts
the agent. The **timeline** shows messages, tool calls with their results, and the costs. You can
send follow-up instructions while the agent waits (`idle`), stop it, or open the workspace in
the browser IDE (**Open IDE**, openvscode-server; the first start downloads it).

## 3. Workspaces and review

When the agent finishes its turn, ADE commits open changes in the workspace and fetches the
branch `ade/<id>` into your repository. The workspace page shows the **diff** against the
commit the workspace started from. Changes to `.github/workflows/`, `.github/actions/` and
`.ade/` are highlighted, because they affect CI secrets and ADE itself.

Then decide:

| Action | What happens |
|---|---|
| **Merge** | Merge commit into the base branch of your repository. With conflicts nothing changes. |
| **Create PR** | Push of `ade/<id>` to `origin` with your git credentials and `gh pr create`. Later pushes update the PR. |
| **Feedback** | A new session in the same workspace implements your comment; its result is another commit on the branch. |
| **Discard** | Stops the sessions and removes workspace, branch `ade/<id>`, container and data. A pushed branch and its PR stay. |

Merge and PR wait until no agent is working in the workspace. Agents themselves never push:
publishing is always a step of the daemon that you trigger.

A workspace can hold several sessions one after another (one agent at a time) plus terminals.
**Terminal** opens a shell in the workspace (in the container for sandbox workspaces).

## 4. Sandbox and network

With the sandbox, each workspace gets its own container that only sees its workspace
(`/workspace`), runs without capabilities and with resource limits, and reaches the internet
only through its own **egress proxy**. Internal targets (loopback, private networks, the host,
other containers) are always blocked. Details: [sandbox.md](sandbox.md).

What the container may reach is set in `.ade/sandbox.yaml`:

```yaml
egress:
  presets: [anthropic, github-readonly, npm]
  allow: [api.example.com]
  on_blocked: ask
```

- Without an `egress` section the presets `anthropic`, `openai`, `github-readonly`, `npm`,
  `pypi` and `go-proxy` apply.
- `on_blocked: ask` turns blocked requests into approval requests ("Allow `example.org` for this
  workspace?") instead of failing them.
- The **Network** tab of a workspace lists allowed and blocked requests live.
- Changes to the file apply to running workspaces within seconds; resources and image apply to
  new containers.

`image` in `sandbox.yaml` selects an own image, ideally derived from
`ghcr.io/davidw475/ade-agent-base` (it contains git, Node.js, Python, Claude Code, Codex and
openvscode-server). `resources` sets memory, CPUs and process limits.

`ade gc` removes containers and networks of workspaces that no longer exist (`--dry-run` only
lists them). The daemon does this on start as well.

## 5. Approvals

Before every tool call of Claude Code, ADE checks the project's rules and, if no rule allows the
call, asks you. Requests appear on the **Approvals** page (badge in the navigation, browser
notification) and in the session view. Without a decision a request is denied after
`approvals.timeout` (default `10m`); a reason you give when denying goes back to the agent.

Rules live in `.ade/project.yaml`, separately for the sandbox and for local agents:

```yaml
approvals:
  sandbox:
    allow: ["*"]
    deny: ["Bash(git push *)"]
  local:
    allow: ["Read(**)", "Grep(**)", "Glob(**)", "Edit(**)", "Write(**)", "Bash(go test *)"]
    ask: ["Edit(.github/**)"]
```

Defaults: in the sandbox everything is allowed (the container limits the damage); locally only
reading and editing files in the workspace. Rule syntax: [configuration.md](configuration.md#adeprojectyaml).
Rule of thumb: **generous in the sandbox, strict outside.**

The same queue holds network exceptions (`on_blocked: ask`), budget increases and factory gates.
Codex and terminal sessions have no hook; Codex runs with its own `--sandbox workspace-write`
locally.

## 6. Costs and budgets

ADE records tokens and costs per session, workspace and project; the dashboard shows them in
the timeline, on the project page and on the **Agents** page.

- In the sandbox with an API key the proxy measures the usage of every API response.
- Otherwise ADE uses what the CLI reports. Codex reports only tokens; ADE converts them with its
  price table and marks the costs as estimated.
- The built-in price table can be extended or corrected with `pricing` in `config.yaml`. Models
  without a price are counted in tokens only.

**Budgets** limit costs per session (`budget.session_usd`, or when starting an agent) and per
project and month (`budget.monthly_usd`). When a limit is reached, the agent is paused and an
approval of kind *budget* asks whether to raise the limit by one step or end the session.

**Model aliases** in `config.yaml` (`models: { opus: { provider: anthropic, id: claude-opus-5-5 } }`)
let you write `opus` instead of a model ID when starting agents and in factories.

## 7. Factories and triggers

A **factory** chains role agents with gates in between, for example planner → approval →
implementer → reviewer → approval → pull request. Each role has its own model, prompt and
permissions; read-only roles (planner, reviewer) cannot change the workspace.

```sh
cd ~/git/my-repo
ade run feature --task "Add login with GitHub" --wait
```

Or **Runs → Start run** in the dashboard. ADE ships four templates: `feature`, `bugfix`,
`refactor` and `review-only` ([factory.md](factory.md#6-templates)). The **Runs** page shows the
steps, their sessions, the artifacts (plan, review, test report) and the open gates. A note you
add when approving a gate goes to the following roles; denying a gate with `on_deny` sends the
run back with your reason as feedback.

Own factories live in `.ade/factories/<name>.yaml` with prompts next to them; a file with the
name of a template replaces it. Format: [configuration.md](configuration.md#factory-adefactoriesnameyaml)
and [factory.md](factory.md). Before `pull_request` a human gate is required unless the step
says `unattended: true`.

**Triggers** start runs automatically, configured in `.ade/project.yaml`:

```yaml
triggers:
  - name: features
    factory: feature
    github_issues: { label: "ade:feature", authors: [your-github-name] }
  - name: nightly
    factory: deps
    inputs: { task: "Update dependencies" }
    schedule: { cron: "0 3 * * 1-5" }
```

- **GitHub issues:** ADE polls `gh issue list` and starts one run per open issue with the label;
  title and text become the task, and ADE comments on the issue at start and end.
- **Schedule:** cron expression in the system time zone; after a pause ADE catches up on the
  last missed date only.
- Triggered runs use the sandbox and need a factory budget (`budget.max_cost_usd`) by default.
  Issue texts are untrusted input: in public repositories always set `authors`.
- Triggers can be paused and resumed on the project page.

## 8. Troubleshooting

**Logs.** The daemon writes to `~/.local/share/ade/daemon.log` (also as a service;
`journalctl --user -u ade` shows start failures). For more detail run it in the foreground:
`ade serve --debug`. Each session's timeline keeps the CLI's error messages; terminal output is
stored under `~/.local/share/ade/sessions/`.

**The dashboard does not open.** Open the URL from `ade start --no-browser` manually in
Windows. If `localhost` does not reach WSL, check that localhost forwarding is enabled (default;
`localhostForwarding=true` in `%UserProfile%\.wslconfig`, or `networkingMode=mirrored`). Another
program on port 7000: set `listen: 127.0.0.1:7100` in `config.yaml`.

**"Daemon already running" / lock errors.** Only one daemon may use a data directory.
`cat ~/.local/share/ade/daemon.lock` shows its PID; `ade stop` ends it. For tests use your own configuration with a
different `listen` and `data_dir` (`ade start --config …`).

**Sandbox does not start.**

- `docker info` must work as your user (group `docker`, or rootless Docker/Podman with
  `sandbox.docker` set). The dashboard shows the detected runtime on the sandbox page.
- Image pull fails: check `docker pull ghcr.io/davidw475/ade-agent-base:latest` and disk space.
- Rootless Docker in WSL: if containers have no network, load `br_netfilter`
  (`sudo modprobe br_netfilter`), see [sandbox.md](sandbox.md#6-container-runtimes).
- `.ade/sandbox.yaml` invalid: the dashboard shows the error; new containers start only after
  it is fixed.
- Leftover containers after a crash: `ade gc`.

**The agent cannot reach a host.** Look at the workspace's **Network** tab: blocked hosts are
listed there. Add them to `egress.allow` or use a preset; `on_blocked: ask` lets you allow
them case by case.

**The agent fails right away.** Is the CLI installed and logged in (`claude --version`,
`claude` once interactively)? With an API key: is the variable set in the daemon's
environment? The service only knows `PATH` from `ade install-service` and the variables in
`~/.config/environment.d/`.

**Create PR fails.** `gh auth status` must be logged in, and `origin` must point to GitHub.
Pushes use your normal git credentials.

**Slow workspaces or setup.** Repositories under `/mnt/c` are slow; move them to `~/…`.
Setup commands run in every new workspace, so keep them short or use an own image with the
dependencies preinstalled.

**Disk usage.** Workspaces are full clones under `~/.ade/workspaces`; discard workspaces you no
longer need. IDE data and run artifacts are in `~/.local/share/ade/`.
