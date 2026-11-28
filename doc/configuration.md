# Configuration reference

ADE reads four kinds of YAML files. All of them are optional, and unknown keys are an error,
so a typo never goes unnoticed.

| File | Scope | Read from |
|---|---|---|
| [`config.yaml`](#configyaml) | Daemon, global | `$ADE_CONFIG`, else `$XDG_CONFIG_HOME/ade/config.yaml` (`~/.config/ade/config.yaml`) |
| [`.ade/project.yaml`](#adeprojectyaml) | One project | The project's main repository (never a workspace) |
| [`.ade/sandbox.yaml`](#adesandboxyaml) | Sandbox of a project | The project's main repository |
| [`.ade/factories/<name>.yaml`](#factory-adefactoriesnameyaml) | One factory | The project's main repository; built-in templates otherwise |

Conventions in the tables below: `<name>` stands for a key you choose (a map), `[]` for a list
entry. Durations use Go syntax (`90s`, `15m`, `2h`). Credentials (`api_key`, `oauth_token`) are references:
`env:VAR` reads the environment variable `VAR` of the daemon, `file:PATH` reads a file (absolute
or `~/…`, leading and trailing whitespace removed) that only its owner may access (`chmod 600`).

The tables are checked against the Go structs by a test (`internal/config/reference_test.go`):
every YAML field must appear here, and every row must name an existing field.

## `config.yaml`

Global settings of the daemon. Changes take effect after a restart of the daemon
([guide](guide.md#starting-and-stopping-the-daemon)). A commented example is in
[`examples/config.yaml`](../examples/config.yaml).

| Key | Type | Default | Description |
|---|---|---|---|
| `listen` | string | `127.0.0.1:7000` | Address of the HTTP server. Only loopback addresses with a fixed port are allowed (ADR-008). Under WSL, Windows forwards `localhost` to it. |
| `data_dir` | path | `~/.local/share/ade` (`$XDG_DATA_HOME/ade`) | SQLite database, daemon secret, lock, log, run artifacts and the downloaded IDE. Absolute or `~/…`. |
| `workspaces_dir` | path | `~/.ade/workspaces` | Workspace clones. Must be on the Linux file system, not under `/mnt/c`. |
| `providers.<name>.api_key` | string | – | API key of a provider (`anthropic`, `openai`) as a reference, e.g. `env:ANTHROPIC_API_KEY`, never in plain text. In the sandbox the egress proxy injects it; locally it is passed to the CLI. Without a key the CLI uses its own login. |
| `providers.<name>.oauth_token` | string | – | Subscription token from `claude setup-token` (Claude Pro/Max) as a reference, e.g. `file:~/.config/ade/secrets/claude-oauth-token`. Only for `anthropic`, not together with `api_key`. In the sandbox the egress proxy sends it as `Authorization: Bearer`, the container only sees a placeholder; locally it is passed to Claude Code as `CLAUDE_CODE_OAUTH_TOKEN`. |
| `models.<name>.provider` | string | – | Provider of a model alias: `anthropic` or `openai`. Must match the adapter (`claude` → `anthropic`, `codex` → `openai`). |
| `models.<name>.id` | string | – | Model ID at the provider, e.g. `claude-opus-5-5`. Aliases work when starting agents and in factories (`model: opus`). |
| `pricing.<name>.input` | number | built-in table | USD per million input tokens. The key is a model ID or its prefix; the longest match wins. |
| `pricing.<name>.output` | number | built-in table | USD per million output tokens. |
| `pricing.<name>.cache_read` | number | built-in table | USD per million tokens read from the prompt cache. |
| `pricing.<name>.cache_write` | number | built-in table | USD per million tokens written to the prompt cache. |
| `defaults.adapter` | string | `claude` | Adapter for new agents when neither the dashboard nor `project.yaml` sets one. |
| `defaults.sandbox` | bool | `true` | New workspaces run in the Docker sandbox (can be switched per start). |
| `ide.binary` | path | – | Own openvscode-server binary. Empty: ADE downloads the pinned version on first use and verifies its checksum (ADR-012). |
| `ide.idle_timeout` | duration | `15m` | An IDE without an open tab stops after this time (at least `1m`). |
| `sandbox.image` | string | `ghcr.io/davidw475/ade-agent-base:<version>` | Default image for sandbox containers; releases use the image of the same version, development builds `:latest`. Projects can override it in `sandbox.yaml`. |
| `sandbox.docker` | path | `docker` from `PATH` | Container CLI. Rootless Docker and `podman` work too; ADE detects the runtime ([sandbox.md](sandbox.md#6-container-runtimes)). |
| `approvals.timeout` | duration | `10m` | How long an approval request waits for a decision in the dashboard before it counts as denied (`10s`–`2h`). |

Example:

```yaml
listen: 127.0.0.1:7000
providers:
  anthropic: { api_key: env:ANTHROPIC_API_KEY }
models:
  opus:   { provider: anthropic, id: claude-opus-5-5 }
  sonnet: { provider: anthropic, id: claude-sonnet-5-5 }
pricing:
  my-model: { input: 1, output: 4 }
defaults: { adapter: claude, sandbox: true }
approvals: { timeout: 10m }
```

## `.ade/project.yaml`

Settings of one project, committed in its repository. ADE reads the file from the main
repository whenever it needs it, so changes apply to the next agent or run without a restart.
If the file is invalid, the project shows the error in the dashboard and no approval rule
allows anything.

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | Directory name | Display name (at most 100 characters). |
| `base_branch` | string | Branch at registration | Branch that workspaces start from and that **Merge** targets. |
| `adapter` | string | `defaults.adapter` | Default adapter for new agents: `claude`, `codex` or `pty`. |
| `setup` | list of strings | – | Shell commands that run in every new workspace (in the container for sandbox workspaces), e.g. `npm ci`. Agents start after setup succeeded. |
| `approvals.sandbox.allow` | list of rules | `["*"]` | Tool calls that run without asking, for agents in the sandbox. |
| `approvals.sandbox.ask` | list of rules | – | Tool calls that always need an approval in the sandbox. |
| `approvals.sandbox.deny` | list of rules | – | Tool calls that are always denied in the sandbox. |
| `approvals.local.allow` | list of rules | Reading and editing files in the workspace, CLI-internal tools | Tool calls that run without asking, for agents without sandbox. |
| `approvals.local.ask` | list of rules | – | Tool calls that always need an approval without sandbox. |
| `approvals.local.deny` | list of rules | – | Tool calls that are always denied without sandbox. |
| `budget.session_usd` | number | `0` (none) | Cost limit per agent session in USD; can be overridden at start. |
| `budget.monthly_usd` | number | `0` (none) | Cost limit for all sessions of the project per calendar month (UTC). |
| `triggers[].name` | string | – | Unique name of the trigger (lowercase letters, digits, `-`, `_`). |
| `triggers[].factory` | string | – | Factory the trigger starts. |
| `triggers[].inputs` | map | – | Fixed inputs of the run. With `github_issues`, `task` comes from the issue. |
| `triggers[].sandbox` | bool | `true` | Run in the sandbox. Triggers process untrusted input, so keep it on. |
| `triggers[].allow_unbudgeted` | bool | `false` | Allow factories without `budget.max_cost_usd`. |
| `triggers[].github_issues.label` | string | – | Starts one run per open issue with this label (1–50 characters, no comma). |
| `triggers[].github_issues.authors` | list of strings | all | Only issues opened by these GitHub users. Recommended for public repositories. |
| `triggers[].github_issues.interval` | duration | `5m` | Polling interval of `gh issue list` (`1m`–`24h`). |
| `triggers[].github_issues.comment` | bool | `true` | Comment on the issue when the run starts and ends. |
| `triggers[].schedule.cron` | string | – | Cron expression with 5 fields or `@daily`, `@hourly`, …, in the system time zone. |
| `triggers[].schedule.catch_up` | bool | `true` | After the daemon was off, run the last missed date (only the last one). |

Each trigger has exactly one of `github_issues` and `schedule`; at most 20 triggers per project.

**Approval rules** are `Tool` or `Tool(pattern)`:

- `*` matches every tool, `mcp__*` every tool whose name starts with `mcp__`.
- `Bash(go test *)` matches commands that start like this; a trailing ` *` also matches without
  arguments. `allow` never matches compound commands (`;`, `&&`, `|`, redirections, `$(…)`),
  while `ask` and `deny` also match a single part of them.
- File tools (`Read`, `Edit`, `Write`, `Glob`, `Grep`, …) take paths relative to the workspace:
  `*` within a directory, `**` across directories. Absolute paths and `~/…` match targets
  outside the workspace.
- `WebFetch(*.example.com)` matches the host of the URL.

Order: `deny` before `ask` before `allow`; anything no rule allows becomes an approval request.
The defaults apply per environment only if that part (`sandbox` or `local`) is missing.

Example:

```yaml
name: My service
base_branch: main
adapter: claude
setup:
  - npm ci
approvals:
  sandbox:
    allow: ["*"]
    deny: ["Bash(git push *)"]
  local:
    allow: ["Read(**)", "Grep(**)", "Glob(**)", "Edit(**)", "Write(**)", "Bash(go test *)"]
    ask: ["Edit(.github/**)"]
budget:
  session_usd: 5
  monthly_usd: 100
triggers:
  - name: features
    factory: feature
    github_issues: { label: "ade:feature", authors: [octocat] }
  - name: nightly
    factory: deps
    inputs: { task: "Update dependencies" }
    schedule: { cron: "0 3 * * 1-5" }
```

## `.ade/sandbox.yaml`

Container and network settings for the sandbox of a project. The daemon pushes changes to
`egress` to running proxies within seconds; `image` and `resources` apply to new containers. If
the file is invalid, running proxies keep the last valid policy and new containers do not start
until it is fixed. Details: [sandbox.md](sandbox.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `image` | string | `sandbox.image` from `config.yaml` | Image for agent containers, typically derived from `ade-agent-base`. |
| `resources.memory` | string | `4g` | Memory limit in the format of `docker --memory` (`512m`, `4g`). |
| `resources.cpus` | number | `2` | CPU limit (`0 < cpus ≤ 256`). |
| `resources.pids` | integer | `512` | Process limit (at least 32). |
| `egress.presets` | list of strings | all presets (see below) | Built-in host lists: `anthropic`, `openai`, `github-readonly`, `npm`, `pypi`, `go-proxy`. |
| `egress.allow` | list of strings | – | Additional allowed hosts, wildcards like `*.example.com`, optional port (`host:443`). |
| `egress.deny` | list of strings | – | Blocked hosts; `deny` wins over `allow`, an entry without port blocks all ports. |
| `egress.on_blocked` | string | `deny` | `deny` blocks requests to other hosts; `ask` creates an approval request in the dashboard instead. |

Without an `egress` section all presets apply. With a section only what it lists applies;
`egress: {}` blocks everything. Internal targets (loopback, private networks, the host) are
always blocked.

Example:

```yaml
image: ghcr.io/davidw475/ade-agent-base:1.0.0
resources: { memory: 8g, cpus: 4, pids: 1024 }
egress:
  presets: [anthropic, github-readonly, npm]
  allow: [api.example.com]
  deny: ["*.pastebin.com"]
  on_blocked: ask
```

## Factory (`.ade/factories/<name>.yaml`)

A factory is a pipeline of role agents with gates in between ([factory.md](factory.md)). Prompts
and schemas are paths relative to the factory file and must stay inside `.ade/factories/`. A
factory with the name of a built-in template (`feature`, `bugfix`, `refactor`, `review-only`)
replaces it.

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | – | Name of the factory (lowercase letters, digits, `-`, `_`); used by `ade run <name>`. |
| `description` | string | – | Short description for the dashboard (at most 500 characters). |
| `inputs.<name>.type` | string | `text` | `text`, or `branch`: the run's workspace starts from this branch (at most one per factory). |
| `inputs.<name>.required` | bool | `false` | The run cannot start without this input. |
| `inputs.<name>.description` | string | – | Help text in the start form. |
| `roles.<name>.adapter` | string | `claude` | `claude` or `codex` (structured adapters only). |
| `roles.<name>.model` | string | CLI default | Model alias from `models` in `config.yaml` or a model ID. |
| `roles.<name>.permissions` | string | `write` | `read`: the role cannot change the workspace (read-only container in the sandbox, snapshot otherwise); `write`. |
| `roles.<name>.permission_mode` | string | Adapter default | Passed to the CLI: for `claude` `default`, `acceptEdits`, `auto`, `bypassPermissions`, `dontAsk`, `plan`; for `codex` `read-only`, `workspace-write`, `danger-full-access`. |
| `roles.<name>.prompt` | path | – | Markdown file with the role's instructions (at most 64 KiB). |
| `roles.<name>.inputs` | list of strings | – | Artifacts of other roles that are added to the prompt. |
| `roles.<name>.outputs` | list | – | At most one artifact: the role's last answer. A file name (`plan.md`) or an object with `name` and `schema`. |
| `roles.<name>.outputs[].name` | string | – | File name of the artifact. Names ending in `.json` must be JSON. |
| `roles.<name>.outputs[].schema` | path | – | JSON Schema (JSON or YAML) the artifact must match; supported subset in [factory.md](factory.md#3-execution). |
| `roles.<name>.sandbox` | – | – | Not allowed: the sandbox applies to the whole run and is chosen at start. |
| `flow[].step` | string | – | Unique name of the step; conditions refer to it (`review.verdict`). |
| `flow[].role` | string | – | Runs the role as an agent session. |
| `flow[].gate` | string | – | `human`: waits for an approval in the dashboard (valid for 7 days). |
| `flow[].action` | string | – | `pull_request`: pushes the branch and opens a PR; only as the last step. |
| `flow[].when` | string | – | Condition. On `role`, `gate` or `action`: the step is skipped if false. Without those (a decision): jump to `goto` if true. |
| `flow[].goto` | string | – | Target step of a decision's loop. |
| `flow[].max_iterations` | integer | `3` | Maximum passes of a loop (decision or gate with `on_deny`, at most 20). |
| `flow[].on_exhausted` | string | `fail` | Decision after `max_iterations`: `fail` ends the run, `continue` goes on without jumping. |
| `flow[].on_deny.goto` | string | – | Gate: denying with a reason jumps back to this step instead of cancelling the run. |
| `flow[].parallel` | list of steps | – | 2–8 role steps (same fields as `flow[]`) that run at the same time; at most one with write permission. |
| `flow[].unattended` | bool | `false` | On `pull_request`: allowed without a preceding gate. Changes to `.github/workflows/`, `.github/actions/` or `.ade/` still wait for an approval. |
| `budget.max_cost_usd` | number | – (no limit) | Cost limit of all sessions of the run. Required for triggers unless `allow_unbudgeted`. |
| `budget.max_duration` | duration | – (no limit) | Working time of the roles (waiting at gates does not count, at most `720h`). |

Without `unattended`, `pull_request` needs a gate without `when` before it, after which no role
with write permission runs. Conditions use paths into structured artifacts of earlier steps
(`review.verdict == 'changes' || test.failed > 0`) with strings, numbers, `true`/`false`/`null`,
comparisons, `&& || !`, parentheses and `len(x)`.

Example (shortened `feature` template):

```yaml
name: feature
description: From task to pull request
inputs:
  task: { type: text, required: true }
roles:
  planner:
    model: opus
    permissions: read
    prompt: prompts/planner.md
    outputs: [plan.md]
  implementer:
    model: sonnet
    prompt: prompts/implementer.md
    inputs: [plan.md]
  reviewer:
    model: opus
    permissions: read
    prompt: prompts/reviewer.md
    outputs:
      - { name: review.json, schema: schemas/review.json }
flow:
  - { step: plan, role: planner }
  - { step: approve-plan, gate: human }
  - { step: implement, role: implementer }
  - { step: review, role: reviewer }
  - { step: decide, when: "review.verdict == 'changes'", goto: implement, max_iterations: 3 }
  - { step: final, gate: human, on_deny: { goto: implement } }
  - { step: deliver, action: pull_request }
budget:
  max_cost_usd: 5
  max_duration: 2h
```

The full templates are in [`examples/factories/`](../examples/factories/).
