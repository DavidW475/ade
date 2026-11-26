# Container-Images

Hier entstehen die Images für die Docker-Sandbox (ab Meilenstein M5, siehe
[doc/sandbox.md](../doc/sandbox.md)).

| Verzeichnis | Inhalt | Meilenstein |
|---|---|---|
| `agent-base/` | Debian, git, Node, Python, ripgrep, Claude Code, openvscode-server | M5 |
| `proxy/` | Egress-Proxy (`ade proxy`) | M6 |

## ade-agent-base

CI baut das Image bei Änderungen unter `images/` und pusht es von `main` (`:latest`) und von
Release-Tags (`:<version>`) nach `ghcr.io/davidw475/ade-agent-base`. Lokal:

```sh
make image    # → ghcr.io/davidw475/ade-agent-base:dev
```

Für ein lokal gebautes Image in `~/.config/ade/config.yaml` `sandbox.image` setzen. Die
openvscode-server-Version und ihre Prüfsummen müssen zu `internal/ide/install.go` passen.

Der User `ade` hat die UID 1000. Startet ADE den Container mit einer anderen UID, legt es einen
passwd-Eintrag an; `HOME` (`/home/ade`) ist für alle beschreibbar.
