# ADE – AI Developer Environment

> A control center for AI agents: start agents, watch them, let them work in secure sandboxes,
> review and approve their results – all in the browser, optimized for WSL.

## Vision

A classic IDE is built for humans who write code. In an **ADE**, **agents** write the code – the
human **plans, watches, reviews and approves**.

```
ade start  →  http://localhost:7000
```

1. The **dashboard** shows all projects and agents with status, costs and open approvals.
2. A click on an agent opens its **workspace in a VS Code-compatible browser IDE**
   (openvscode-server) – including terminal, files and diff.
3. Agents optionally run in a **Docker sandbox** behind an **egress proxy** that only allows
   approved targets.
4. Several agents with roles (planner, implementer, reviewer, tester) and different models can be
   chained into a **software factory**.

## Documents

| Document | Contents |
|---|---|
| [guide.md](guide.md) | User guide: installation, first agent, review, sandbox, approvals, costs, factories, troubleshooting |
| [configuration.md](configuration.md) | Configuration reference: every option of `config.yaml`, `project.yaml`, `sandbox.yaml` and factories |
| [adapters.md](adapters.md) | Contributor guide: writing an adapter for another agent CLI |
| [architecture.md](architecture.md) | Overall architecture, components, data model, API, security, tech stack, repository layout |
| [sandbox.md](sandbox.md) | Docker sandbox, network isolation, egress proxy, secret handling |
| [threat-model.md](threat-model.md) | Threat model: assets, attackers, trust boundaries, attack paths, residual risks |
| [factory.md](factory.md) | Software factories: roles, flows, gates, models |
| [milestones.md](milestones.md) | Milestones M0–M11 with scope and acceptance criteria |
| [decisions.md](decisions.md) | Architecture decisions (ADRs) with reasons |

## Terms

| Term | Meaning |
|---|---|
| **Project** | A registered Git repository (on the Linux file system of WSL). |
| **Workspace** | An isolated working copy of a project for exactly one agent (local Git clone on its own branch `ade/<id>`). |
| **Agent / session** | A running agent process (e.g. the Claude Code CLI) with a task in a workspace. |
| **Adapter** | Translates a specific agent CLI (claude, codex, generic terminal) into ADE events. |
| **Sandbox** | Docker container in which agent and IDE of a workspace run. |
| **Egress policy** | Rules for where a sandbox may go on the network (domain allowlist). |
| **Approval** | An action a human must confirm (tool call, merge, policy exception). |
| **Factory / run** | A defined pipeline of role agents; a run is one execution of it. |

## Target platform

- **Primary:** WSL2 (Ubuntu/Debian) on Windows 11, browser on Windows via `localhost`.
- **Also:** any Linux with Docker works the same – WSL specifics are optional.

## License

**Apache-2.0** (see [decisions.md](decisions.md#adr-009-license-apache-20)).
