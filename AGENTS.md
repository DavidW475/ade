# AGENTS.md

Notes for AI agents (and humans) working on ADE. Applies to the whole repository.

## Project in brief

ADE is a control center for AI agents on WSL2: a Go daemon (`ade`) with an embedded React
dashboard. Plan and terms are in [`doc/`](doc/README.md), the roadmap in
[`doc/milestones.md`](doc/milestones.md), decisions in [`doc/decisions.md`](doc/decisions.md).

## Commands

```sh
make build   # build web/ (→ internal/api/dist) and produce bin/ade
make test    # go test -race ./... and TypeScript type check
make lint    # golangci-lint and ESLint
make dev     # daemon in the foreground (debug logging)
```

`make test` and `make lint` must pass before every commit. For manual testing use your own
configuration with a different port and data directory (see
[CONTRIBUTING.md](CONTRIBUTING.md#developing-locally)), never the data of an installed ADE.

## Structure

| Path | Contents |
|---|---|
| `cmd/ade/` | CLI entry point and subcommands |
| `internal/api/` | HTTP handlers, auth, host/origin check, embedded frontend (`dist/`) |
| `internal/config/` | Global configuration |
| `internal/daemon/` | Daemon start, lock, client for the CLI |
| `internal/store/` | SQLite and migrations (`migrations/NNNN_name.sql`, numbered without gaps) |
| `internal/service/` | systemd user service (`ade install-service`, `ade uninstall-service`) |
| `internal/browser/` | Opening the browser (WSL → Windows) |
| `internal/apperr/` | Error kinds (NotFound, Invalid, Conflict) → HTTP status |
| `internal/git/` | Calls of the git binary, diff via temporary index, commit/fetch/merge/push |
| `internal/project/` | Registering projects, `.ade/project.yaml` |
| `internal/workspace/` | Workspaces: clone, setup, diff, delivery (collect, merge, PR), discard |
| `internal/agent/` | Agent sessions: processes (PTY/pipes), event hub with replay, cleanup after restart, costs and budgets, read-only roles (snapshot) |
| `internal/agent/adapter/` | Adapter interface, event types + JSON schema; `claude/`, `codex/`, `pty/`, `fake/` (tests) |
| `internal/provider/` | Adapter → provider, model aliases, price table, cost calculation |
| `internal/ide/` | openvscode-server per workspace: download with checksum, start, idle stop |
| `internal/gateway/` | Reverse proxy `ws-<id>.localhost` → IDE, ticket login, connection token |
| `internal/sandbox/` | Docker containers (agent, reader container), internal network and proxy container per workspace via the Docker CLI, `.ade/sandbox.yaml` (incl. `egress`), GC |
| `internal/proxy/` | Egress proxy `ade proxy`: allowlist, protection against internal targets, secret injection, token counting, control API, questions (hook, `on_blocked: ask`) |
| `internal/approval/` | Approvals: queue, rules from `approvals` in `project.yaml`, `ade hook pretooluse`, egress exceptions, budget approvals |
| `internal/factory/` | Software factory: definitions (`.ade/factories`, templates from `examples/factories`), runner (runs, gates, artifacts, loops, parallel steps, resuming after restart), conditions and schemas |
| `internal/trigger/` | Triggers for runs: GitHub issues with a label (polling via `gh`), schedule |
| `internal/cron/` | Cron expressions (5 fields) and dates |
| `web/` | Dashboard (Vite + React + TypeScript, Monaco Diff Editor) |
| `examples/`, `images/` | Example configurations, factory templates (`examples/factories`, embedded), container images (`images/agent-base`) |

New modules follow the structure in [architecture.md](doc/architecture.md#10-repository-layout).

## Conventions

- **Language:** English for docs, comments, error messages, log and UI texts, and identifiers.
- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/), e.g. `feat(workspace): …`.
- **Security first:** The daemon executes code. Every new route goes through the host/origin
  check and needs auth (except `/api/health`). Changes to auth, gateway, sandbox or proxy need tests.
- **Dependencies** sparingly, license compatible with Apache-2.0, no cgo.
- **Migrations** are never changed afterwards; always add a new one.
- **Git** is always called through `internal/git`. A project's main repository is only read,
  except for the daemon's delivery steps (fetch to `ade/<id>`, merge, push, delete branch,
  ADR-013); workspaces have no remote to the main repository (ADR-005). Commands in a workspace
  go through `git.Worktree` (no hooks; in the container for sandbox workspaces, #41).
- **Checking the UI:** Under WSL the dashboard can be screenshotted with headless Edge
  (`msedge.exe --headless=new --screenshot=… "http://localhost:<port>/auth/login?ticket=…&next=/path"`,
  ticket via `POST /api/auth/tickets` with the daemon secret).
- **CHANGELOG:** Add changes under `[Unreleased]`. Add a version heading `## [X.Y.Z] – …` only
  when the maintainer explicitly asks for it: merging it into `main` publishes a release
  automatically (ADR-024).
- **Configuration:** Every new YAML field belongs in [`doc/configuration.md`](doc/configuration.md)
  (test `internal/config/reference_test.go`).
- **Architecture decisions** are appended as a new ADR to `doc/decisions.md`.

## Workflow: milestones, issues and PRs

Every milestone (M0, M1, …) is a GitHub milestone with issues. Work is always done against these
issues and linked to them through PRs.

1. **Read the issues before you start.** Look at all issues of the milestone, not only
   `doc/milestones.md`; the issues are more detailed:
   ```sh
   gh issue list --milestone "M11 – Release 1.0" --state open
   gh issue view <nr>
   ```
2. **Branch** from current `main`: `m<N>-<short-name>`, e.g. `m1-workspaces`. Never commit
   directly to `main`.
3. **Implement**, and at the end check every task and every "Done when" of the issues against the
   code. Whatever is missing is added or explicitly named as open in the PR.
4. **Create the PR** following [`.github/pull_request_template.md`](.github/pull_request_template.md):
   - For **every fully completed issue** a separate line `Closes #<nr>` in the PR text.
     Only then does GitHub close the issue on merge.
   - Link partially completed issues with `Part of #<nr>`; they stay open.
   - Deviations from the issue or from `doc/` with reasons under "Deviations from the plan".
   - Tick the milestone's acceptance criteria in `doc/milestones.md` in the same PR.
5. **Wait for CI** (`gh pr checks <nr> --watch`) and fix failures.
6. **After the merge, check** that all issues are closed:
   ```sh
   gh issue list --milestone "<milestone>" --state open
   ```
   - If an issue was not closed automatically (keyword missing, PR already merged): close it
     with a reference to the PR, e.g.
     `gh issue close <nr> --comment "Done in #<pr>."`, and add the line to the PR text.
   - If the milestone is empty, close it:
     `gh api -X PATCH repos/DavidW475/ade/milestones/<number> -f state=closed`.
7. **New tasks** that come up while working are filed as an issue in the matching milestone,
   instead of being done silently on the side or forgotten.

The maintainer merges into `main`; agents open PRs but do not merge themselves.
