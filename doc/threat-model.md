# Threat model

Status: M11 (before 1.0). Complements [architecture.md § 7](architecture.md#7-security) and
[sandbox.md](sandbox.md). Every attack path has a countermeasure or a named residual risk. The
review of the security-critical parts (issue #95) is in [section 6](#6-review-m11).

## 1. What is protected

| Asset | Where | Why valuable |
|---|---|---|
| **Host** | The user's WSL distribution, Windows behind it | Code execution on the host = access to everything below |
| **Code** | Main repositories of the projects, branches, remotes | Manipulated commits end up in the real project on merge or push |
| **Credentials** | API keys (`providers.*.api_key`), `gh` login, SSH keys, Git credentials, subscription logins of the agent CLIs | Abuse, costs, access to further repositories and CI secrets |
| **Daemon access** | `daemon.secret`, browser session (`ade_session`), IDE sessions (`ade_ide`), proxy token, hook token | Whoever controls the daemon starts agents and merges |
| **Daemon data** | `ade.db` (tasks, outputs, egress log), logs, artifacts | Contain repository contents and prompts |
| **Budget** | Costs of the provider APIs | Endless loops or abuse cause real costs |

## 2. Attackers

| Attacker | Capability | Assumption |
|---|---|---|
| **A1 – Malicious repository or issue** | Controls files in the repository, issue texts, dependencies the setup downloads | Can steer the agent through prompt injection |
| **A2 – Prompt-injected or faulty agent** | Runs arbitrary code where the agent runs: in the sandbox (container) or, without a sandbox, on the host | Goal: escape the sandbox, get secrets, exfiltrate data, smuggle code into the main repository |
| **A3 – Foreign website in the browser** | JavaScript in the user's Windows browser, can address `localhost`, DNS rebinding | Knows no secret and no cookie |
| **A4 – Other local user** | Own account in the same WSL distribution, reaches `127.0.0.1` | No root, not in the `docker` group |
| **A5 – Network outside the machine** | Reaches ports of the machine in the LAN | – |

**Not in the model:** root on the host or members of the `docker` group (rootful Docker is
equivalent to root), a compromised Windows host, compromised provider APIs, GitHub or image
registries, attacks on the kernel or the container runtime itself (zero-days).

## 3. Trust boundaries

```
 Windows browser ──(1)── daemon 127.0.0.1:7000 ──(4)── main repo, gh, secrets
   │                        │      │
   │ ws-<id>.localhost      │(2)   │(3) Docker CLI
   └──── gateway ───────────┘      ▼
                         ┌─ agent container ──(5)── proxy container ──(6)── internet
                         │   /workspace (bind)        allowlist, secret injection
                         └─ reader container (read-only)
```

1. **Browser → daemon:** host/origin check, session cookie, tickets.
2. **IDE subdomain → gateway:** own session per workspace; the IDE's content is considered
   untrusted (it comes from the workspace).
3. **Daemon → container:** the daemon controls, the container only delivers data (events, files,
   git output). Whatever comes from the workspace is input from A2.
4. **Workspace → main repository:** only the daemon transfers, and only on an action of the user
   or a factory step (fetch to `ade/<id>`, merge, push; ADR-005, ADR-013).
5. **Agent → proxy:** the only gate to the outside; control API only with a token.
6. **Proxy → internet:** allowlist, no internal targets.

## 4. Attack paths

### 4.1 Browser (A3)

| Attack | Countermeasure | Residual risk |
|---|---|---|
| CSRF on the API | `Sec-Fetch-Site: cross-site` and a foreign `Origin` are rejected for all unsafe methods and WebSocket upgrades; cookie `SameSite=Strict`; GET routes change nothing | – |
| DNS rebinding (foreign domain → 127.0.0.1) | `Host` must be `localhost`, `127.0.0.1`, `::1` or `*.localhost` with the daemon port | – |
| Cross-site WebSocket hijacking (`/ws/…`) | Origin check also for GET with `Upgrade: websocket` | – |
| Reading API responses | No CORS; `no-store` on `/api/` and `/ws/` | – |
| Clickjacking, XSS in the dashboard | `X-Frame-Options: DENY`, CSP `default-src 'self'` without `unsafe-eval`/inline scripts, `nosniff`; React escapes output, diffs go through Monaco | `style-src 'unsafe-inline'` (for Monaco) |
| Open redirect after login | `next` only as a local path (`safeNext`) | – |
| Ticket from history or referrer | One-time, valid for 2 min, the redirect removes it from the address bar, `Referrer-Policy: no-referrer` | – |
| Login CSRF | Tickets are only issued with the daemon secret | – |

### 4.2 IDE and gateway (A2 via the IDE, A3)

| Attack | Countermeasure | Residual risk |
|---|---|---|
| IDE content (extensions, webviews, servers in the container) calls the dashboard API | Own origin `ws-<id>.localhost`; the dashboard cookie is host-only for `localhost` and is not sent; the origin check rejects subdomains for the dashboard | – |
| One IDE accesses another | Cookie `ade_ide` host-only per subdomain and bound to exactly one workspace; the origin must be the own subdomain | – |
| Stealing the IDE's connection token | The gateway inserts the token itself and removes `vscode-tkn` and `ade_ide` from the request; the browser never sees it | Code in the container can read the token (it belongs to the agent there anyway) |
| Cookie tossing (`Set-Cookie; Domain=localhost` from the IDE) | Browsers reject `Domain=localhost` as a public suffix; a planted cookie would only be an invalid session | Logout (denial of service) |
| IDE session survives logout | IDE sessions depend on the dashboard session and end with it | – |

### 4.3 Sandbox: escape and host access (A2)

| Attack | Countermeasure | Residual risk |
|---|---|---|
| Escalating privileges in the container | `--cap-drop ALL`, `no-new-privileges`, no privileged mode, no Docker socket, non-root (rootful/Podman) or root without capabilities in the user namespace (rootless Docker, ADR-023) | Kernel or runtime vulnerabilities |
| Reading or writing host files | Only the workspace is mounted (`/workspace`), never the main repository; `checkMountPath` rejects unsuitable paths | – |
| Exhausting resources (fork bomb, memory) | `--memory`, `--cpus`, `--pids-limit` (default 4g/2/512; without delegated cgroup controllers the sandbox does not start) | Disk space in the workspace is unlimited |
| The workspace's git configuration (`core.fsmonitor`, filters, hooks) runs code in the daemon | git commands in sandbox workspaces run in the container (`git.Worktree` with runner, #41); always `core.hooksPath=/dev/null`, `core.fsmonitor=false` | – |
| Fetch from the workspace into the main repository | Runs on the host, starts `upload-pack` in the workspace; git only honors `uploadpack.packObjectsHook` from protected configuration, not from the repository's; `--no-recurse-submodules`, `--no-tags` | With `objects/info/alternates` or a `.git` file the agent can pull objects of other repositories on the host into `ade/<id>`; they show up in the diff before the merge |
| Symlinks in the workspace point to host files | Diff and file view read through git (symlink = link target as text) | – |
| Read-only roles write after all | The reader container mounts the workspace `readonly` | – |
| Files with a foreign owner in the workspace | UID mapping per runtime (ADR-023), tested under all three runtimes | – |
| Manipulating the proxy binary | Mounted read-only, proxy container `--read-only`, not part of the workspace | – |

### 4.4 Network and secrets (A2)

| Attack | Countermeasure | Residual risk |
|---|---|---|
| Direct internet access | The agent is only attached to the internal network (`--internal`), no DNS (`--dns` to a dead end); the only way is the proxy | – |
| Internal targets (host, other containers, LAN, cloud metadata) through the proxy | Block list of all private, loopback, link-local, multicast and tunnel networks (IPv4, IPv6, IPv4-mapped, IPv4-compatible, 6to4, Teredo, NAT64); the IP actually connected to is checked in the dialer's `Control`, i.e. after name resolution (DNS rebinding) | Public IPs of the host itself (rare under WSL) |
| Exfiltration through allowed hosts | Allowlist as narrow as possible; egress log with bytes per target in the dashboard; `on_blocked: ask` | Data can flow out through any allowed target (e.g. a gist via `api.github.com`); without TLS interception the proxy only sees host and port |
| Reading API keys | Keys only live in the proxy; the agent sends a placeholder to `/p/<provider>/`, the proxy replaces `Authorization`/`X-Api-Key` and only sends the key to the provider's fixed upstream; plain HTTP and CONNECT never get a key | The agent can *use* the key through the proxy (limited by budgets) |
| The proxy's control API (policy, secrets, answers to questions) | Bearer token (constant-time comparison), only the daemon knows it; locked without a token; port only published on `127.0.0.1` | – |
| Proxies of other workspaces | Docker: `enable_icc=false` in the egress network; agents only reach their own internal network | Podman: proxies reach each other (the control API is still protected by its token, doc/sandbox.md § 6) |
| Forging approvals (hook, egress questions) | Hook token per session; answers only through the authenticated daemon API | Many questions are annoying (denial of service) |
| Subscription logins of the CLIs | Without a sandbox they are on the host; in the sandbox only what is explicitly passed in (#51) | – |

### 4.5 Smuggling code into the project (A1, A2)

| Attack | Countermeasure | Residual risk |
|---|---|---|
| Manipulating the main repository (hooks, branches, configuration) | The workspace is a separate clone without a remote to the main repository (ADR-005); only the daemon transfers | – |
| Malicious code in the result | Merge and PR only after review in the dashboard (diff against the base); factories with `gate: human` before `pull_request` | A review overlooks something |
| `.ade/*.yaml` in the result widens sandbox, approvals or factories | The configuration is read from the main repository, never from the workspace; it only applies after the merge | Inattentive review of `.ade/` |
| Workflows in the PR branch (`.github/workflows/`) run with the repository's secrets | PRs are created by a push into the same repository; `pull_request` needs a gate before it, after which no writing role runs (#103). Without a gate (`unattended: true`) the run waits for an approval as soon as the diff changes `.github/workflows/`, `.github/actions/` or `.ade/`; gate and review highlight these paths | Unattended runs push other changes unreviewed, such as build scripts that an existing workflow calls |
| Prompt injection through issue texts (triggers) | Only issues with a label (only users with triage permission set them); run in the sandbox; gate before the PR | The issue text itself comes from arbitrary users |
| Malicious project configuration (`setup`, factories) | Registering a project means trusting its `.ade/` configuration; without a sandbox, setup and agent run on the host | Without a sandbox A2 = code execution on the host (a deliberate choice, notice in the dashboard) |
| Checkout filters during the merge | The merge in the main repository uses the filters from the *user's* configuration (e.g. LFS); `.gitattributes` from the result can only select among them | – |

### 4.6 Local access (A4, A5)

| Attack | Countermeasure | Residual risk |
|---|---|---|
| Reaching the daemon from the LAN | Listens only on `127.0.0.1`; published container ports only on `127.0.0.1` | WSL mirrored mode (`networkingMode=mirrored`) does not change this as long as it binds to 127.0.0.1 |
| Reading the daemon secret, database, logs | Data directory `0700` (enforced at start), `daemon.secret`, database (incl. `-wal`/`-shm`), logs, tokens `0600`; workspaces directory `0700` | – |
| Calling the API without the secret | Every route except `/api/health` needs a session or the secret; the hook only with the hook token | – |
| Grabbing the session cookie through another port | In browsers cookies apply to all ports of a host; `HttpOnly` | A local service at `localhost:<other port>` that the user opens in the browser receives `ade_session`. Whoever runs such services usually has access to the account anyway |
| IDE and control ports on 127.0.0.1 | Connection token (IDE) or bearer token (proxy) | – |

### 4.7 Supply chain

| Attack | Countermeasure | Residual risk |
|---|---|---|
| Manipulated openvscode-server | Download with a fixed SHA-256 checksum | – |
| Manipulated release or image | Checksums, SBOM and build provenance (attestation) for binary and images (#97); `install.sh` verifies the checksum | Verifying the attestation is up to the user (`gh attestation verify`) |
| Known vulnerabilities in dependencies and Go | Toolchain pinned in `go.mod` (`toolchain`); `govulncheck` and `npm audit` on every PR and weekly; few dependencies, no cgo | Published binaries stay vulnerable until the next release |

## 5. Residual risks at a glance

1. **Without a sandbox** the agent has the user's permissions. The sandbox is the actual boundary.
2. **Exfiltration through allowed targets** cannot be prevented without TLS interception (backlog).
3. **Human review** is the last line against malicious code in the result, also in `.ade/` and
   `.github/workflows/`.
4. **Kernel and runtime vulnerabilities** are beyond what ADE can do; rootless Docker or Podman
   reduce the damage (ADR-023).
5. **Cookies are not bound to ports.** Local web services of another origin see the session
   cookie.
6. **Podman** does not isolate the proxy containers from each other.

## 6. Review M11

Checked as part of #95. Findings are fixed (with a test) or filed as an issue.

**Gateway and auth** (`internal/api`, `internal/gateway`)
- [x] Host check before all routes, origin check for unsafe methods and WebSocket upgrades
- [x] All routes except `/api/health` with `requireAuth`/`requireSecret`; hook with its own token
- [x] Cookies `HttpOnly`, `SameSite=Strict` (dashboard) or `Lax` (IDE, cross-site navigation), host-only, path `/`
- [x] Tickets one-time, 2 min, 256 bits; IDE tickets bound to workspace and dashboard session
- [x] The IDE's connection token only in the gateway; secrets compared in constant time
- [x] `ReadHeaderTimeout` against hanging connections

**Proxy** (`internal/proxy`)
- [x] Block list incl. IPv6, IPv4-mapped, 6to4, Teredo, NAT64; check in the dialer's `Control`
- [x] **Fixed:** IPv4-compatible addresses (`::/96`, e.g. `::127.0.0.1`) and site-local (`fec0::/10`) were missing
- [x] CONNECT targets checked against allowlist and block list; secret injection only to the fixed upstream, incoming credential headers are replaced
- [x] Control API only with a token, port only on 127.0.0.1

**Container hardening** (`internal/sandbox`)
- [x] Mounts: only the workspace (agent), workspace read-only (reader container), binary read-only (proxy)
- [x] `cap-drop ALL`, `no-new-privileges`, `--init`, limits, no Docker socket, proxy `--read-only`
- [x] **Fixed:** `ade.db` (with `-wal`/`-shm`) was created with the umask's permissions (usually `0644`), now `0600`; an existing data directory readable by others is set to `0700` at start

**Dependencies**
- [x] **Fixed:** `monaco-editor` 0.57 brought DOMPurify ≤ 3.4.16 (5 advisories, low); now 0.58
- [x] **Fixed:** CI and release built with Go 1.26.0 (`go-version-file` without `toolchain`), the standard library had 34 reachable vulnerabilities (among others `crypto/x509`, `net/http`); now `toolchain go1.26.11` in `go.mod`
- [x] `govulncheck` and `npm audit` as workflow `audit.yml` on PRs, on `main` and weekly

**Filed as an issue**
- [x] **Fixed (#103):** a factory without a gate before `pull_request` pushed unreviewed agent code (incl. workflows)
