# Changelog

All notable changes to ADE. Format following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
versions following [Semantic Versioning](https://semver.org/). The release workflow uses the
section of each version as release notes.

## [Unreleased]

### Added

- **`ade stop`** and **`ade restart`** (M11): stop the daemon of the configuration – the systemd
  service if it runs as one, otherwise the background process from `daemon.lock` – and wait until
  it has ended; `restart` starts it again, e.g. after changing `config.yaml`.
- **Documentation for 1.0** (M11): [user guide](doc/guide.md) from installation to the first
  merged result including troubleshooting, [configuration reference](doc/configuration.md) with
  every option of `config.yaml`, `.ade/project.yaml`, `.ade/sandbox.yaml` and factories (a test
  checks it against the Go structs), and a [contributor guide for adapters](doc/adapters.md).
- **Install script** (M11): `curl -fsSL https://raw.githubusercontent.com/DavidW475/ade/main/install.sh | sh`
  downloads the matching release binary, verifies the checksum, installs to `~/.local/bin` and
  warns about missing requirements (git ≥ 2.38, docker/podman, gh, claude/codex). `--service` also
  sets up the service. The script is attached to every release as well.
- **`ade install-service`** and **`ade uninstall-service`**: daemon as a systemd user service under
  WSL (`systemd=true`). The unit takes over the `PATH`, writes to the daemon log and restarts on
  failure; `ade start` uses the service if it is installed for the same configuration.
- **Releases with attestation** (M11): every tag builds binaries and the images `ade-agent-base`
  and `ade-dev` with the same version (`:X.Y.Z`), with SBOM and build provenance
  (`gh attestation verify …`). A snapshot build including `install.sh` runs in CI when the
  release configuration changes. Release process in CONTRIBUTING.md.
- **Automatic releases** (M11): if, after a merge to `main`, the top of the CHANGELOG has a new
  version `## [X.Y.Z] – YYYY-MM-DD`, `auto-release.yml` sets the tag and starts the release run.
  `scripts/changelog-version.sh` reads and checks the heading, already in the PR.
- **Factory templates** (M10): `bugfix` (reproduce the bug with a test, fix it, tests green),
  `review-only` (two reviewers in parallel, summary, no PR) and `refactor` (step by step with a
  test run after each step); `feature` with review and test in parallel and a loop back on change
  requests. New input type `branch`: the run's workspace starts from it.
- **Docker sandbox** (M5): workspaces can be created with a sandbox (switch when starting an agent
  and when creating a workspace, default `defaults.sandbox`). One container per workspace sees
  only the workspace directory (`/workspace`), runs with your UID, without capabilities and with
  limits for memory, CPU and processes. Agent sessions, terminals, setup commands, the browser IDE
  and the daemon's git commands in the workspace run inside it.
- `.ade/sandbox.yaml` (image, resources) and `sandbox.image` in the global configuration.
- Image `ghcr.io/davidw475/ade-agent-base` (Debian, git, Node.js, Python, ripgrep, Claude Code,
  openvscode-server); `make image` builds it locally.
- `ade gc` removes orphaned sandbox containers; the daemon reconciles containers on start.
- **Egress proxy** (M6): the sandbox container is attached to an internal Docker network without
  a route to the outside. The only way to the internet is a proxy container per workspace
  (`ade proxy`) that only lets through targets from `egress` in `.ade/sandbox.yaml` (presets
  `anthropic`, `github-readonly`, `npm`, `pypi`, `go-proxy`, own `allow`/`deny` entries). Changes
  apply without a restart.
- **Secret injection:** if `providers.anthropic.api_key` is set, Claude Code in the sandbox talks
  to the API through `http://proxy:8080/p/anthropic`; the proxy inserts the key.
- **Network** view in the workspace: policy, targets with allowed and blocked requests, latest
  requests.
- **Approvals** (M7): before every tool call, Claude Code asks ADE through the hook
  `ade hook pretooluse` – locally the daemon directly, in the sandbox through the egress proxy.
  Whatever the rules in `approvals` (`.ade/project.yaml`, separately for `sandbox` and `local`) do
  not allow or deny waits for a decision in the dashboard (**Approvals** page with badge, browser
  notification, display in the session). Without a decision the request counts as denied after
  `approvals.timeout` (default 10m).
- `egress.on_blocked: ask`: requests to targets that are not allowed wait for an approval
  ("Once", "For this workspace", "Always (project)", "Deny"). Blocked hosts can be allowed
  permanently with one click in the network view; the daemon stores the exceptions, the
  repository stays unchanged.
- **Adapter `codex`** (M8): Codex CLI locally and in the sandbox, with timeline (messages,
  commands, file changes) and resuming via `codex exec resume`. Codex is included in the image
  `ade-agent-base`; new egress preset `openai` (also in the defaults).
- **Secret injection for OpenAI:** with `providers.openai.api_key`, Codex in the sandbox talks to
  the API through `http://proxy:8080/p/openai/v1`; locally it gets the key as `CODEX_API_KEY`.
- **Costs:** the egress proxy reads the token usage from the API responses (Anthropic, OpenAI;
  JSON and streaming). Costs and tokens appear live in the session, the workspace, the project
  (month and total) and the project overview. Without measurement at the proxy, the CLI's report
  counts; ADE computes Codex costs from tokens. `GET /api/usage`.
- Built-in price table (estimates, as of 2026-11) and `pricing` in the global configuration to
  extend it.
- **Model aliases:** `models` in the global configuration (e.g. `opus` → provider and model ID);
  selectable in the start dialog, the provider must match the adapter.
- **Budgets:** `budget.session_usd` and `budget.monthly_usd` in `.ade/project.yaml`, per session
  also at start. When exceeded, the agent pauses (status "paused") and waits for a budget
  approval; "Raise budget" resumes it, "Deny" ends it.
- **Software factory** (M9): factories in `.ade/factories/<name>.yaml` chain role agents
  (adapter, model alias, permissions, prompt) into a run: linear steps, gates (`gate: human`) as
  an approval in the dashboard and `action: pull_request` at the end. A role's last answer becomes
  its artifact (e.g. `plan.md`) and goes to the following roles. The template `feature` ships with
  ADE (planner → approval → implementer → reviewer → approval → PR). Runs survive a daemon restart.
- **Read-only roles** (`permissions: read`) cannot change the workspace: in the sandbox an own
  container with `/workspace` read-only, without sandbox a snapshot.
- `ade run <factory> --task "…"` (with `--wait`), **Runs** pages and run view with pipeline,
  gates, artifacts and costs; start dialog in the project. `budget.max_cost_usd` and
  `budget.max_duration` per factory.
- API: `GET /api/factories`, `GET/POST /api/runs`, `GET /api/runs/{id}`,
  `POST /api/runs/{id}/cancel`, `GET /api/runs/{id}/artifacts/{name}`.
- **Factory v2** (M10): structured artifacts (JSON, optionally with a schema; the role corrects
  invalid answers up to twice), conditions (`when`), decisions with loops (`goto`,
  `max_iterations`), gates with `on_deny` (denying with a reason jumps back) and parallel steps
  (`parallel`). Earlier passes stay visible in `history`.
- **Triggers** (M10): `triggers` in `.ade/project.yaml` start runs automatically – per open GitHub
  issue with a label (polling via `gh`, comment at start and end, the PR closes the issue) or on a
  schedule (cron). Triggered runs use the sandbox by default and need a factory budget. API:
  `GET /api/projects/{id}/triggers`, `POST /api/projects/{id}/triggers/{name}/pause|resume`.

### Fixed

- Egress exceptions “for this workspace” no longer disappear on every workspace update (WIP commit,
  end of setup, PR); they are now removed when the workspace is discarded (#79).
- Release binaries use the sandbox image with the same version (`ade-agent-base:X.Y.Z`, also for
  pre-releases `-rc.N`) instead of `:latest`; GoReleaser now writes the version with `v` into the
  binary.
- `ade gc` and the container reconciliation no longer fail when a container disappears between
  listing and inspecting (#66).

### Security

- An agent can no longer store commands in its workspace's repository configuration that the
  daemon runs on the host (filters, fsmonitor): for sandbox workspaces, WIP commit, diff and file
  contents run in the container (#41).
- Sandbox workspaces are cloned without hardlinks, so the container cannot change object files of
  the main repository.
- No API key in the sandbox container (neither in the environment nor in `docker inspect`).
  Programs that bypass the proxy reach nothing; external names do not resolve in the container.
  The proxy never connects to internal addresses (host, containers, private networks).
- **Threat model** (M11, `doc/threat-model.md`): assets, attackers, trust boundaries, attack paths
  with countermeasures and residual risks; review of gateway, proxy and container hardening.
- The database (`ade.db`, `-wal`, `-shm`) is only readable by the user; the daemon sets a data
  directory readable by others to `0700` on start.
- The proxy also blocks IPv4-compatible IPv6 addresses (`::127.0.0.1`) and site-local
  (`fec0::/10`).
- Binaries are built with Go 1.26.11 (`toolchain` in `go.mod`); so far CI used Go 1.26.0 with
  known vulnerabilities in the standard library (among others `crypto/x509`, `net/http`).
- `monaco-editor` 0.58 (DOMPurify without known vulnerabilities); `govulncheck` and `npm audit`
  on every PR and weekly (`audit.yml`).
- Factories no longer push unreviewed agent code (#103): `action: pull_request` needs a gate
  (`gate: human` without `when`) before it, after which no role with write permission runs.
  Without a gate only with `unattended: true`; if the diff then changes `.github/workflows/`,
  `.github/actions/` or `.ade/`, the run waits for an approval before the push. Gates and the
  review view highlight such changes. **Own factories without a gate before the PR only load
  again once they get a gate or `unattended: true`.**

### Changed

- **Language:** documentation, comments, messages and the dashboard are in English (#107).
- Sandbox containers get the ade binary read-only for the approval hook and are therefore
  recreated once on the next start.
- Claude Code's permission mode only applies when the approval hook is not available.
- ADE recreates sandbox containers from M5 (normal network) on the next start; files in the
  workspace stay, changes outside `/workspace` are lost.
- Invalid `.ade/sandbox.yaml`: new sandbox containers only start after it is fixed, running ones
  keep the last valid network policy.
- `make dev` builds without cgo; the daemon needs a static binary for the proxy container.
- The proxy requests responses for `/p/<provider>/` uncompressed (for token counting).
- `/api/adapters` additionally returns modes per adapter, providers and model aliases.

## [0.1.0] – 2026-11-26

First usable version: let agents work locally under WSL2, review their result and take it over.
**No sandbox yet** – agents run with your permissions; only use it in trusted repositories
(the sandbox follows with M5).

### Added

- **Daemon and dashboard** (M0): `ade start` starts the daemon on `127.0.0.1:7000` and opens the
  dashboard in the Windows browser; login with a one-time ticket, host/origin check, SQLite store,
  a single binary with embedded frontend.
- **Projects and workspaces** (M1): register repositories (`ade project add`), `.ade/project.yaml`
  with base branch and setup commands; every workspace is a local clone with its own branch
  `ade/<id>` without a remote to the main repository.
- **Agent sessions** (M2): adapters `claude` (Claude Code with `stream-json`) and `pty` (any
  command, e.g. a shell); live timeline with tool calls and costs, terminal in the browser,
  follow-up instructions, resuming after a daemon restart.
- **Browser IDE** (M3): openvscode-server per workspace at `ws-<id>.localhost:7000`, starts on
  demand and stops when idle.
- **Review and delivery** (M4):
  - At the end of every agent turn the daemon commits open changes (`ade: wip <session>`) and
    fetches the branch `ade/<id>` into the project repository.
  - Diff view with Monaco Diff Editor (side by side or inline), file list with status and line
    counts; file contents are only loaded when selected.
  - **Merge** into the base branch with a merge commit; conflicts are detected and reported
    beforehand without changing anything.
  - **Create PR**: push from the host and `gh pr create` with a generated description; later
    pushes update the PR. The agent itself has no push rights.
  - **Give feedback**: new session in the same workspace with the comment and the current diff;
    its result becomes another commit on the same branch.
  - **Discard** removes workspace, branch in the project repository, sessions, logs and IDE data.
- Release binaries for linux/amd64 and linux/arm64.

### Fixed

- Workspaces can also be created when `workspaces_dir` is on a different file system than the
  repository (#39).

[Unreleased]: https://github.com/DavidW475/ade/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/DavidW475/ade/releases/tag/v0.1.0
