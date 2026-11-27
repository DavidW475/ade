# Container-Images

Hier entstehen die Images für die Docker-Sandbox (ab Meilenstein M5, siehe
[doc/sandbox.md](../doc/sandbox.md)).

| Verzeichnis | Inhalt | Meilenstein |
|---|---|---|
| `agent-base/` | Debian, git, Node, Python, ripgrep, Claude Code, openvscode-server | M5 |
| `ade-dev/` | ade-agent-base plus Go, gcc und golangci-lint – Sandbox für dieses Repo | M5 |
| `proxy/` | Egress-Proxy (`ade proxy`) | M6 |

## ade-agent-base

CI baut das Image bei Änderungen unter `images/` und pusht es von `main` (`:latest`) nach
`ghcr.io/davidw475/ade-agent-base`. Jeder Release-Tag `vX.Y.Z` baut es immer, auch ohne
Änderung, als `:X.Y.Z` (Vorabversionen `:X.Y.Z-rc.N`) mit SBOM und Provenance; diesen Tag nutzt
das gleich versionierte Binary als Standard-Image. Lokal:

```sh
make image    # → ghcr.io/davidw475/ade-agent-base:dev
```

Für ein lokal gebautes Image in `~/.config/ade/config.yaml` `sandbox.image` setzen. Die
openvscode-server-Version und ihre Prüfsummen müssen zu `internal/ide/install.go` passen.

Der User `ade` hat die UID 1000. Startet ADE den Container mit einer anderen UID, legt es einen
passwd-Eintrag an; `HOME` (`/home/ade`) ist für alle beschreibbar.

## ade-dev

Sandbox-Image für die Arbeit an ADE selbst, eingetragen in `.ade/sandbox.yaml`. Es leitet von
`ade-agent-base:latest` ab und ergänzt Go (Version wie in `go.mod`), gcc für `go test -race` und
golangci-lint, damit die Setup-Befehle aus `.ade/project.yaml` und `make test`/`make lint` im
Container laufen. CI baut es nach `ade-agent-base` und pusht es von `main`. Lokal:

```sh
make image-dev    # → ghcr.io/davidw475/ade-dev:latest
```

Beim Wechsel der Go-Version in `go.mod` `GO_VERSION` im Dockerfile mitziehen.
