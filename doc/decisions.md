# Architecture decisions (ADRs)

Short form: context → decision → consequences. Append new decisions at the bottom; mark outdated
ones as *superseded by ADR-xxx* instead of deleting them.

## ADR-001: Daemon + web UI instead of a desktop app

- **Context:** agents run for a long time; the UI should be pleasant on Windows. WSLg windows have
  quirks (scaling, clipboard, window frames).
- **Decision:** a daemon in WSL, the UI in the Windows browser via `localhost`.
- **Consequences:** no WSLg needed; agents survive closing the UI; a desktop wrapper (e.g. Wails)
  remains possible later. The security of the localhost server must be taken seriously (ADR-008).

## ADR-002: Go for the backend

- **Context:** many processes, PTYs, Docker, proxy, concurrency; simple distribution desired.
- **Decision:** Go, one static binary with an embedded frontend.
- **Consequences:** the Docker and proxy ecosystem in Go is mature; the proxy can live in the
  same binary (`ade proxy`).

## ADR-003: openvscode-server as the browser IDE

- **Context:** VS Code experience in the browser, open source, without maintaining an own fork.
- **Decision:** openvscode-server (MIT) per workspace; extensions from Open VSX.
- **Consequences:** no Microsoft marketplace, no proprietary Microsoft extensions (e.g. Pylance,
  remote extensions). The alternative code-server stays interchangeable.

## ADR-004: Subdomain routing for IDEs

- **Context:** VS Code under a sub-path is error-prone; several IDEs should not share
  cookies/storage.
- **Decision:** `ws-<id>.localhost:7000` instead of `/ide/<id>/`.
- **Consequences:** own origin per IDE; the auth cookie must be set for the subdomain (token
  handover on the first call).

## ADR-005: Workspace = local Git clone instead of worktree

- **Context:** a `git worktree` points to the main repository through a `.git` file. In the
  container this only works if the main repository is mounted as well – then the agent could
  change its hooks, configuration and other branches.
- **Decision:** `git clone --local` per workspace (objects via hardlink, therefore cheap); the
  daemon fetches results into the main repository with `git fetch`.
- **Consequences:** one mechanism for local and sandbox; full isolation; somewhat more disk space
  for working files.

## ADR-006: Network isolation through an internal Docker network + proxy container

- **Context:** `HTTP_PROXY` is only a request to programs.
- **Decision:** agent containers only in an `--internal` network; the only exit is the proxy
  container with an allowlist.
- **Consequences:** isolation is enforced; programs without proxy support have no network
  (intended). The DNS behavior must be checked.

## ADR-007: Secrets via proxy injection

- **Context:** agents can be manipulated through prompt injection.
- **Decision:** API keys stay in the daemon/proxy; agent CLIs talk to the proxy through base URLs,
  and the proxy inserts the key. Git push only by the daemon.
- **Consequences:** keys cannot be read from the container; token counting and budgets are
  possible at the proxy. CLIs without a base URL option need special treatment.

## ADR-008: localhost hardening

- **Context:** every website in the browser can send requests to `localhost` (CSRF, DNS
  rebinding).
- **Decision:** bind only `127.0.0.1`, `Host` and `Origin` check, session cookie via a one-time
  token from `ade start`.
- **Consequences:** a bit more effort at start; a basic requirement because the daemon can execute
  code.

## ADR-009: License Apache-2.0

- **Context:** open source on GitHub; usage as broad as possible; dependencies are MIT/Apache.
- **Decision:** Apache-2.0 (patent clause, compatible with all planned dependencies).
- **Consequences:** commercial use and closed forks are allowed. License notices of the
  dependencies (openvscode-server, Monaco, xterm.js …) are shipped (`NOTICE`/image labels).
  Trademarks (VS Code, Claude, Docker) not in the product name.

## ADR-010: Provider access only via API keys or official CLIs

- **Context:** providers usually do not allow third-party applications to use subscription logins
  (e.g. Claude Pro/Max, ChatGPT Plus) for their own purposes.
- **Decision:** ADE uses the user's API keys or starts official CLIs that the user installed and
  logged in to themselves. ADE does not build own subscription login flows.
- **Consequences:** check the current terms of use of the supported providers before every
  release.

## ADR-011: Claude Code as a process with stream-json input and output

- **Context:** for a timeline ADE needs structured events; follow-up instructions should be
  possible without a restart, and a session should be resumable after a daemon restart.
- **Decision:** the `claude` adapter starts `claude -p --input-format stream-json
  --output-format stream-json --verbose` with pipes (no PTY). Instructions go to stdin as JSON
  lines; the process stays alive between turns (status `idle`). The `session_id` from the output
  is stored; after the process ends, `--resume <id>` continues the conversation. If the daemon
  stops, sessions count as `interrupted` and are not continued.
- **Consequences:** a follow-up instruction during a turn is handled afterwards. No terminal stream
  for Claude sessions; whoever wants to work interactively opens a `pty` terminal in the same
  workspace. Until M7, permissions go through `--permission-mode` (default in the dashboard:
  `acceptEdits`); in print mode the CLI rejects tools that would need a confirmation. The
  stream-json format is not formally specified: the parser ignores unknown lines, tests use
  recorded output (`internal/agent/adapter/claude/testdata`).

## ADR-012: Browser IDE locally with a pinned openvscode-server version and an own subdomain session

- **Context:** until M5 the IDE runs without a container. openvscode-server is not installed and
  not distributed through a package manager. The IDE executes code (terminal, extensions) and must
  not be able to control the dashboard.
- **Decision:** ADE downloads a pinned version (`ide.Version`, checksum per architecture in the
  code) on first start to `<data_dir>/ide/` and aborts on a wrong checksum; `ide.binary` in the
  configuration replaces the download. Per workspace a process runs on `127.0.0.1` with a free port
  and a random connection token that is only stored in a file (`--connection-token-file`). The
  gateway sets the token cookie `vscode-tkn` itself. Login to `ws-<id>.localhost` through a one-time
  ticket from `POST /api/workspaces/{id}/ide/ticket` that is bound to the workspace and the
  dashboard session; it is exchanged for a host-only cookie `ade_ide` (HttpOnly, `SameSite=Lax`).
  Logging out of the dashboard also ends the IDE sessions. All IDEs share the extensions; settings
  and server data are per workspace. Without an open connection ADE stops an IDE after
  `ide.idle_timeout` (default 15 min).
- **Consequences:** Lax instead of Strict, because `localhost` and `ws-<id>.localhost` are
  different sites and the jump from the dashboard is a cross-site navigation. Origin check: on an
  IDE subdomain only its own origin is allowed, on the dashboard no IDE. The dashboard CSP does not
  apply to IDE subdomains. The IDE inherits the same filtered environment as agents (no
  `GH_TOKEN`, no `SSH_AUTH_SOCK`). VS Code loads webviews from `vscode-cdn.net`, which needs
  internet. When raising the version, both checksums must be recomputed.

## ADR-013: Delivery into the project repository by the daemon

- **Context:** until M3 ADE only read the project repository (ADR-005). For review and delivery
  (M4) an agent's result must get there without giving the agent access to the repository or push
  rights, and without changing the user's working tree unasked.
- **Decision:** the daemon writes into the project repository only through `internal/git` and
  only like this:
  1. without a user action exclusively `refs/heads/ade/<id>` (WIP commit in the workspace, then
     `fetch` with a force refspec – the namespace `ade/*` belongs to ADE);
  2. "Merge": always a merge commit (`--no-ff`). Conflicts are detected beforehand with
     `git merge-tree --write-tree`; then nothing changes. If the base branch is checked out,
     `git merge` runs in that working tree (git itself refuses the merge when local changes are
     touched), otherwise `commit-tree` + `update-ref` with the expected old value;
  3. "Create PR": `git push origin ade/<id>` and `gh pr create` with the user's credentials on the
     host;
  4. "Discard": `git branch -D ade/<id>` (not if the branch is checked out).
  Commands in the workspace run with `core.hooksPath=/dev/null` and `core.fsmonitor=false`,
  commits without a signature. If there is no Git identity, ADE commits as `ADE <ade@localhost>`.
- **Consequences:** Git ≥ 2.38 is required. Merge commits in the main repository go through the
  user's hooks and signing setting like a normal `git merge`. An agent can store further commands
  in the workspace's repository configuration (e.g. clean filters) that run during the daemon's
  WIP commit; until the sandbox (M5) agents run with user permissions anyway, after that the daemon
  must ignore this configuration or commit in the container (#41). The Monaco editor is bundled
  locally (no CDN, dashboard CSP only `'self'`) and loaded as its own chunk only on the workspace
  page.

## ADR-014: Docker sandbox through the Docker CLI, the daemon's git in the container

- **Context:** M5 isolates agent, setup and IDE in one container per workspace. For that ADE needs
  container management, `docker exec` with a PTY and signals to processes in the container. In
  addition the daemon runs git commands in the workspace, whose repository configuration belongs
  to the agent (#41).
- **Decision:**
  1. ADE calls the **Docker CLI** (like the git binary), not the Go client. `docker exec -it` runs
     in the same PTY code as local processes, the CLI passes on size changes, and no big
     dependency is added.
  2. Container `ade-ws-<id>`: the only mount is the workspace as `/workspace`, user = UID/GID of
     the user, `--cap-drop ALL`, `no-new-privileges`, `--init`, limits from `.ade/sandbox.yaml`
     (read from the project repository), main process `sleep infinity`. Labels `ade.managed`,
     `ade.workspace`, `ade.data_dir`; `ade.data_dir` separates several ADE instances.
  3. Since the CLI does not pass signals to `docker exec`, ADE ends processes in the container by
     their environment (`ADE_SESSION_ID=<id>` or `ADE_IDE_OWNER`): a small script in the container
     signals all processes with this variable – like a process group.
  4. Environment variables go to `docker exec --env KEY` without a value and with their value into
     the CLI's environment, so that e.g. API keys do not show up in the process list.
  5. Sandbox is a property of the workspace (set at creation). Sandbox workspaces are cloned
     without hardlinks: shared inodes would allow the container to overwrite object files of the
     main repository.
  6. **#41:** for sandbox workspaces all of the daemon's git commands in the workspace (WIP
     commit, diff, file contents, temporary index) run in the container via `docker exec`
     (`git.Worktree` with runner). Filters, fsmonitor or hooks from the workspace configuration
     therefore only take effect in the sandbox. The `fetch` into the main repository still runs on
     the host; `upload-pack` does not run commands from the repository configuration.
  7. On daemon start orphaned containers are removed and the others stopped (ends processes of
     the last run); they start again on demand. When stopping, the daemon stops its containers.
     `ade gc` removes orphaned containers even without a running daemon.
  8. The IDE runs in the container on port 3000, Docker publishes it only on `127.0.0.1`.
- **Consequences:** every git query of a sandbox workspace costs a few `docker exec`
  (~50–100 ms). The workspace list starts no containers and shows no change counts for stopped
  ones. Until M6 the container has a normal network; an API key from the configuration is
  readable in the container. Rootless Docker and different UIDs work, but the file owners in the
  workspace only match the user with classic Docker.

## ADR-015: Egress proxy as its own container, secret injection through the control API

- **Context:** M6 restricts the sandbox's network to approved targets and keeps API keys out of
  the container (doc/sandbox.md). Docker does not publish ports for containers in `--internal`
  networks, and the daemon only listens on the host's `127.0.0.1`.
- **Decision:**
  1. Per workspace an internal network `ade-ws-<id>` and a proxy container `ade-proxy-<id>` in the
     shared network `ade-egress` (without traffic between containers) and in the internal network
     (alias `proxy`). The isolation is enforced by the network; `HTTP(S)_PROXY` is a convenience.
  2. The proxy is `ade proxy` from the same binary: the daemon copies its (static) binary to
     `<data_dir>/sandbox/ade-proxy` and mounts it read-only into the default image. Proxy and
     daemon therefore always have the same version, without an own image.
  3. The daemon controls the proxy through an HTTP control API on a published port (`127.0.0.1`),
     protected by a random token per container (passed through the Docker CLI's environment, not
     in the process list). Policy and keys only go through it; the proxy keeps them in memory.
     The daemon collects events by long polling.
  4. The daemon reaches the IDE through a TCP forwarding of the proxy to `agent:3000`.
  5. The proxy never connects to internal addresses; the IP is checked at `connect`.
  6. External DNS resolution in the agent container is additionally disabled via
     `--dns 192.0.2.1`.
  7. Containers from M5 (without label `ade.net`) are recreated on the next start.
  8. With an invalid `sandbox.yaml` the defaults do not apply: running proxies keep the last valid
     policy, new containers do not start.
- **Consequences:** one additional container (~10 MB RAM) per running workspace. The daemon must
  be built without cgo (`make build`, `make dev`); it reports a dynamically linked binary at start.
  Without TLS interception the allowlist only applies at host level. If programs other than agent
  sessions (IDE terminal) use the Anthropic API, they use the CLI's own login through the
  allowlist.

## ADR-016: Approvals through a PreToolUse hook, in the sandbox through the egress proxy

- **Context:** M7 lets a human decide about critical tool calls and network exceptions,
  asynchronously in the dashboard. Claude Code runs in print mode (ADR-011) and rejects tools that
  would need a question without a hook. The proxy container does not reach the daemon (it only
  listens on the host's `127.0.0.1`), but the daemon reaches the proxy through the control API
  (ADR-015). The daemon only changes a project's main repository during delivery.
- **Decision:**
  1. The Claude adapter registers `ade hook pretooluse` via `--settings` as a `PreToolUse` hook for
     all tools. The hook always answers `allow` or `deny`, `deny` on any error (fail closed). Its
     timeout is `approvals.timeout` plus some margin.
  2. The daemon decides: first the rules from `approvals` in `.ade/project.yaml` (deny → ask →
     allow, separately for sandbox and local, defaults: everything in the sandbox, locally reading
     and editing files in the workspace), otherwise an approval in the queue that blocks until the
     decision or the timeout. Bash allow rules never match compound commands; path patterns apply
     relative to the workspace, locally after resolving symlinks. With an invalid `project.yaml` no
     rule allows anything.
  3. Locally the hook calls `POST /api/hook/pretooluse` with a token per session (only valid for
     questions of this session, ends with the process).
  4. In the sandbox the ade binary is mounted read-only at `/opt/ade/ade`; the hook asks
     `http://proxy:8080/ade/hook/pretooluse`. The proxy holds the question, the daemon collects open
     questions by long polling through the control API (`GET /questions`) and answers them
     (`POST /questions/{id}`). The same queue carries egress questions with
     `egress.on_blocked: ask`: the request waits (default 2 min), equal targets share one
     question, after "Deny" the proxy does not ask again until it restarts.
  5. The daemon stores egress approvals "for this workspace" and "always (project)" as exceptions
     in SQLite and adds them to the policy instead of changing `.ade/sandbox.yaml` in the main
     repository. Wildcards still belong in the file.
  6. Open approvals of an earlier daemon run count as expired at start.
- **Consequences:** every tool call costs a process start and a local HTTP request. Containers
  without the hook binary are recreated once (`ade.net=proxy-2`); the state in the container file
  system is lost (e.g. a subscription login under `/home/ade`). The CLI's permission mode only
  applies when no hook is available. Terminal sessions (`pty`) have no hook. Exceptions from the
  dashboard are not in the repository; the network view shows them and can remove them.

## ADR-017: Costs from one source per session, budgets through pause and approval

- **Context:** M8 should show costs per session and enforce budgets, also for CLIs that do not
  report USD (Codex). In the sandbox the egress proxy sees every API response when it inserts the
  key (ADR-007); Claude Code reports its costs itself at the end of every turn. Both together would
  count twice. Codex ignores `OPENAI_BASE_URL`, and `codex exec` handles one instruction per
  process with cumulative token counts per conversation.
- **Decision:**
  1. Every usage is an entry in `usage` (on the project, not the workspace, so monthly budgets
     include discarded workspaces) and is added to the session. Per session one source applies: if
     the proxy measures (sandbox with secret injection for the adapter's provider), only it counts,
     otherwise the CLI's report. Proxy usage without a measuring session only counts for the
     workspace.
  2. The proxy reads the responses of `/p/<provider>/` while passing them through (JSON up to
     16 MiB, SSE line by line) and requests them uncompressed for that. It only reports tokens and
     model; the daemon computes USD with a built-in, overridable price table (prefix of the model
     ID, longest match).
  3. Codex gets the proxy base URL as an own provider via `-c`; the parser offsets its cumulative
     numbers against the session's tokens counted up to the process start.
  4. The daemon checks budgets (session, the project's calendar month) after every usage. When
     exceeded, it ends the process (status `paused`) and creates an approval of kind `budget`
     (24 h). Allowing raises the budget by one step and resumes the session; denying or expiry ends
     it (`stopped`).
- **Consequences:** costs from the price table are estimates; unknown models only count tokens and
  are marked. A budget only takes effect after the response that exceeds it (a running request is
  not aborted); without the proxy only at the end of a turn. Whatever bypasses the proxy
  (subscription login via CONNECT) cannot be counted. Pausing ends the process; only adapters with
  resume can continue.

## ADR-018: Factory runs from normal sessions, artifacts outside the workspace, read-only roles through an own container

- **Context:** M9 chains role agents into a run (doc/factory.md). Every role should stay a normal
  session (timeline, approvals, costs, sandbox), the run should survive a daemon restart, and
  read-only roles must not change the shared workspace. A running container cannot be remounted
  afterwards; `docker exec` cannot add mounts.
- **Decision:**
  1. The runner (`internal/factory`) keeps the state in `runs` and `run_steps` and advances it
     under a lock per run. It is driven by status changes of the sessions (listener on the agent
     manager), decisions at gates, a tick and the start. The definition is stored as JSON at start.
  2. A role is done when its session becomes `idle` or `done`. Its last answer is the artifact; ADE
     stores it under `<data_dir>/runs/<id>/` (not `.ade/runs/` in the workspace as planned before)
     and passes it to the following roles in the prompt. A waiting agent is ended as soon as the
     step is done; the workspace is saved in the process.
  3. Gates are approvals of kind `gate` (7 days). After a restart the runner recreates open gates
     and resumes interrupted sessions.
  4. In the sandbox, read-only roles run in a second container per workspace (`ade-ws-<id>-ro`,
     role `reader`) with `/workspace` as a read-only bind mount, in the same internal network
     behind the same proxy. Without a sandbox they work in a snapshot (`git clone --shared` of the
     workspace) under `<data_dir>/snapshots/<session>`; the adapters additionally block their write
     tools.
  5. Sandbox applies to the whole run, not per role (one workspace).
- **Consequences:** in M9 artifacts are free text; several or structured artifacts per role come
  with M10. Without a sandbox "read-only" is an agreement, not protection: an agent with Bash
  access can write outside the snapshot (like any local agent). The reader container doubles a
  workspace's memory needs while a read-only role runs; it stays until the workspace is stopped or
  discarded. A time limit makes the run fail instead of asking for an approval.

## ADR-019: Loops as new passes, an own expression language and a schema subset

- **Context:** M10 brings structured artifacts, conditions and jumps back to factories
  (doc/factory.md). Earlier passes should stay traceable, dependencies few, and a condition from
  the repository must not be able to do anything but compare values.
- **Decision:**
  1. Every execution of a step is its own row in `run_steps` (`pos` = position in the flow,
     `iteration` = pass). A jump back creates new rows for the positions from the target to the
     jumping step. The API returns the latest execution per position (`steps`, `steps[current]`
     stays the current step) and the older ones under `history`.
  2. Artifacts are stored as the latest version under `<data_dir>/runs/<id>/<name>` and per pass
     under `iterations/<n>/<name>`.
  3. Conditions are an own small language (paths, literals, comparisons, `&& || !`, `len`), not a
     script and not a library like CEL or expr: no access to anything but the artifacts, no loops,
     no functions except `len`.
  4. Schemas are a checked subset of JSON Schema without `$ref`, `oneOf`, `pattern` etc.; unknown
     keywords are an error when loading instead of being silently ignored.
  5. Invalid structured answers go back to the same session as a follow-up instruction (at most
     two corrections) instead of restarting the role.
- **Consequences:** `max_iterations` counts the passes of a step, not of a specific loop; nested
  loops share the count. Whoever needs regular expressions or references in schemas has to do
  without until an extension. The number of rows per run grows with the passes (at most
  `max_iterations` × steps).

## ADR-020: Triggers by polling through gh, state in SQLite, sandbox and budget as default

- **Context:** M10 should start runs from GitHub issues with a label and on a schedule. The daemon
  only listens on `127.0.0.1` and is not reachable from GitHub. Issue texts are untrusted input.
- **Decision:**
  1. GitHub issues are polled with `gh issue list` in the main repository (default every
     5 minutes), not by webhook. Login and repository come from `gh` and the remote, ADE stores no
     token.
  2. The triggers are configured in `.ade/project.yaml`; their state (paused, last poll or last
     date, last error) and the triggered runs (`trigger_runs`, key issue number or date) are in
     SQLite. An issue never starts two runs.
  3. Schedules use an own cron parser (`internal/cron`, 5 fields and short forms), no library.
     After a pause at most the last date is caught up.
  4. Triggered runs use the sandbox by default and only run with `budget.max_cost_usd` in the
     factory; both can be explicitly disabled per trigger (`sandbox: false`,
     `allow_unbudgeted: true`).
- **Consequences:** a run starts up to one interval after labeling. Without `gh auth login` the
  error shows at the trigger in the dashboard. `authors` checks the author of the issue, not who
  set the label (`gh issue list` does not provide that); in public repositories `authors` is
  therefore strongly recommended.

## ADR-021: Background service as a systemd user unit with the PATH taken over

- **Context:** under WSL with systemd the daemon should run without `ade start`. User services do
  not inherit the shell's environment; git, docker, gh, claude and codex are often in
  `~/.local/bin`, `~/.npm-global/bin` or similar. A daemon outside systemd holds the lock of the
  data directory.
- **Decision:**
  1. `ade install-service` writes `~/.config/systemd/user/ade.service` with an absolute binary
     path, a fixed configuration and the current `PATH` (`Environment=`). Further variables,
     especially secrets, do not go into the unit; the configuration is there for that.
  2. stdout/stderr go to the normal daemon log via `StandardOutput=append:`, so troubleshooting and
     the messages of `ade start` stay the same (systemd ≥ 240).
  3. `Restart=on-failure`; ADE does not enable lingering itself (needs root), it only points it
     out.
  4. If a daemon is already running outside systemd, `install-service` aborts with the PID instead
     of sending the service into a restart loop.
  5. `ade start` starts the service via `systemctl --user start` if the unit is installed for the
     same configuration (comment line `# ade-config:` in the unit), otherwise its own background
     process as before.
- **Consequences:** after a new binary path or a changed `PATH`, `install-service` must run again
  (it overwrites the unit and restarts). An SSH agent of the shell is not available to the
  service; pushes need credentials that work without an agent (e.g. `gh auth setup-git`).

## ADR-022: Binary and images in the same release run, same version, attestation

- **Context:** by default the binary uses `ade-agent-base:<version>`. The image workflow only ran
  on changes under `images/` (the `paths` filter also applies to tags), so a release without image
  changes would have referenced an image tag that does not exist. In addition GoReleaser wrote the
  version into the binary without `v`; `DefaultImage` did not recognize it and fell back to
  `:latest`.
- **Decision:**
  1. `release.yml` calls `images.yml` as a reusable workflow (`workflow_call`) in parallel to
     GoReleaser. The release does not wait for the images; a failure turns the run red and can be
     repeated on its own.
  2. Image tag `X.Y.Z` or `X.Y.Z-rc.N` without `v`, binary version with `v` (like
     `git describe`). `DefaultImage` maps `vX.Y.Z[-alpha|beta|rc.N]` to this tag; everything else
     (development, snapshots) uses `:latest`.
  3. Binaries: SPDX SBOM per archive (syft) and build provenance over `checksums.txt`. Images:
     BuildKit SBOM and provenance plus a GitHub attestation in the registry. Verify with
     `gh attestation verify`.
  4. A snapshot build with `install.sh` against the local `dist/` runs in PRs that change the
     release configuration.
- **Consequences:** a release can be published briefly without the matching image until the image
  job is done. There are no signatures with an own key (cosign); the attestation depends on
  GitHub's Sigstore instance and the identity of the workflow.

## ADR-023: Rootless Docker and Podman behind the same Docker CLI

- **Context:** the sandbox calls the Docker CLI (ADR-014) and starts processes with the user's
  UID. Under rootless Docker this UID ends up in the subuid range on the host: files the agent
  creates belong to a foreign UID, and the proxy binary that only the user can read is not
  executable in the container. Podman understands almost the same CLI, but not every Docker
  option. Without delegated cgroup controllers Docker sometimes drops resource limits with only a
  warning (#94).
- **Decision:**
  1. No own backend per runtime: ADE stays with the Docker CLI syntax; `sandbox.docker` may also
     be `podman`. ADE detects the runtime from `info --format '{{json .}}'` (Podman JSON has
     `host`, rootless Docker reports `name=rootless` in `SecurityOptions`).
  2. UID mapping per runtime for agent, reader and proxy containers and `docker exec`: rootful
     unchanged `--user UID:GID`; rootless Docker `--user 0:0` (root in the user namespace = the
     user on the host, without capabilities); rootless Podman `--user UID:GID --userns keep-id`.
  3. Under rootless Docker ADE sets `IS_SANDBOX=1`, so Claude Code allows `bypassPermissions` as
     root – the container is the sandbox.
  4. If a cgroup controller is missing (`memory`, `cpu`, `pids`), `Ensure` aborts with a clear
     message before creating a container. Silently dropping limits would be a weaker sandbox than
     the configuration promises.
  5. Docker-specific options only where they apply: `enable_icc=false` for `ade-egress` only under
     Docker. ADE reads networks via `network inspect` (JSON), because the `ls` formats differ.
  6. A CI job additionally runs the sandbox integration tests under rootless Docker and rootless
     Podman (in the runner's systemd user manager, as on a desktop).
- **Consequences:** under Podman the proxies in `ade-egress` can reach each other; an agent still
  only reaches its own proxy, which rejects internal targets. Under rootless Docker agent CLIs run
  as root in the container. Existing containers do not need to be recreated: `docker exec --user`
  follows the detected runtime.

## ADR-024: Automatic releases from the CHANGELOG

- **Context:** so far the maintainer set the tag by hand after the CHANGELOG PR (ADR-022). The
  section in the CHANGELOG is required anyway (release notes) and names version and date; the tag
  is a second, error-prone step.
- **Decision:**
  1. The top version heading `## [X.Y.Z] – YYYY-MM-DD` in `CHANGELOG.md` counts (also
     `X.Y.Z-rc.N`). `scripts/changelog-version.sh` reads it with a regex and checks the format, the
     date and that the version is newer than the one below (SemVer, `rc` before the full release).
  2. `auto-release.yml` runs on every push to `main` that changes `CHANGELOG.md`. If the tag
     `vX.Y.Z` is missing, the workflow sets it (annotated) on exactly this commit and calls
     `release.yml` as a reusable workflow. If the tag already exists, nothing happens.
  3. The direct call is necessary because a tag pushed with `GITHUB_TOKEN` starts no workflows;
     this way neither a PAT nor a GitHub app is needed. `release.yml` and `images.yml` therefore
     take the version from `inputs.tag` instead of `github.ref`.
  4. The path through a manually pushed tag remains. A manual start of `auto-release.yml` catches
     up on a release if the tag exists but the release is missing.
  5. CI checks the CHANGELOG headings already in the PR.
- **Consequences:** whoever merges a version heading into `main` publishes. That is the approval
  step: the CHANGELOG PR needs the same care as the tag used to. A wrong heading cannot be taken
  back after the merge, only replaced by a new version. Tag protection rules in the repository must
  allow `github-actions[bot]` to create tags.

## ADR-025: English as the project language

- **Context:** until M11 docs, comments, messages and the UI were German. For 1.0 ADE should be
  usable and open to contributions for an international audience (#107).
- **Decision:** docs, code comments, error and log messages, CLI output and the dashboard are in
  English. The changelog section is called `[Unreleased]`; `scripts/changelog-version.sh` skips
  it. The switch happens in several PRs (docs, backend, dashboard).
- **Consequences:** historic issues, PRs and commit messages stay German. Tests that compare
  messages change with them. Links to doc headings changed with the translation.

## ADR-026: Claude subscription token through the egress proxy

- **Context:** in the sandbox Claude Code needed an API key; with a subscription only a `/login`
  per container was possible, whose credentials every agent in it can read (#51, #115).
- **Decision:** `providers.anthropic.oauth_token` takes a token the user creates with the official
  CLI (`claude setup-token`). ADE does not build a login flow of its own (ADR-010). In the sandbox
  the token is handled like an API key: the proxy keeps it, recognizes it by the prefix
  `sk-ant-oat` and sends it as `Authorization: Bearer`; the container only gets
  `CLAUDE_CODE_OAUTH_TOKEN=<placeholder>` and `ANTHROPIC_BASE_URL` to the proxy. Locally Claude
  Code gets the token as `CLAUDE_CODE_OAUTH_TOKEN`. New reference form `file:PATH` (mode 600),
  because the systemd service does not see shell variables.
- **Consequences:** like with an API key, every process in the agent container can use the
  subscription through the proxy; usage limits and terms of use of the subscription apply. Costs
  in the dashboard are estimates at API prices. If Anthropic changes the token format or the
  beta flag, the proxy needs an update.
