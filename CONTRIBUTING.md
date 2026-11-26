# Mitwirken an ADE

Danke für dein Interesse! ADE ist in einer frühen Phase; der Plan steht in [`doc/`](doc/README.md).

## Voraussetzungen

- Go (Version laut `go.mod`, aktuelle stabile empfohlen)
- Node.js 22+ und npm
- [golangci-lint](https://golangci-lint.run/) v2 für `make lint`
- Entwicklung am besten in WSL2 oder Linux, Repo im Linux-Dateisystem (nicht unter `/mnt/c`)

## Bauen und testen

```sh
make build   # Frontend bauen (web/ → internal/api/dist) und bin/ade erzeugen
make test    # Go-Tests mit Race-Detector + TypeScript-Typprüfung
make lint    # golangci-lint und ESLint
```

`go build` und `go test` funktionieren auch ohne Frontend-Build; dann zeigt das Dashboard
nur einen Hinweis.

Tests gegen einen echten Docker-Daemon (Sandbox) laufen nur mit `ADE_TEST_DOCKER=1`; sie bauen
ein kleines Debian-Image mit git (`ade-test-git:1`) oder nehmen `ADE_TEST_IMAGE`:

```sh
ADE_TEST_DOCKER=1 go test ./internal/sandbox/ ./internal/agent/
make image   # Sandbox-Image ade-agent-base lokal bauen (Tag :dev)
```

## Lokal entwickeln

```sh
make dev                # Daemon im Vordergrund (go run, Debug-Logging)
cd web && npm run dev   # Vite-Dev-Server auf http://localhost:5173 (leitet /api an :7000 weiter)
bin/ade start --no-browser   # gibt die Anmelde-URL aus
```

Die Anmelde-URL setzt das Session-Cookie für `localhost`; danach funktioniert auch der
Vite-Dev-Server.

Eine eigene Konfiguration (anderer Port, eigenes Datenverzeichnis) hält die Entwicklung von
einer installierten ADE getrennt:

```sh
cat > /tmp/ade-dev.yaml <<'YAML'
listen: 127.0.0.1:7099
data_dir: /tmp/ade-dev/data
workspaces_dir: /tmp/ade-dev/workspaces
YAML
go run ./cmd/ade serve --config /tmp/ade-dev.yaml --debug
```

## Issues und Pull Requests

Der Ablauf (Branch pro Meilenstein, `Closes #N` pro erledigtem Issue im PR, Prüfen nach dem
Merge) ist in [AGENTS.md](AGENTS.md#arbeitsablauf-meilensteine-issues-und-prs) beschrieben und
gilt für Menschen genauso.

## Konventionen

- **Sprache:** Doku, Kommentare, Fehlermeldungen und UI-Texte auf Deutsch; Bezeichner auf Englisch.
- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:` …).
- **Architekturentscheidungen** als ADR in [`doc/decisions.md`](doc/decisions.md) anhängen.
- **Sicherheit:** Der Daemon kann Code ausführen. Änderungen an Auth, Host-/Origin-Prüfung,
  Sandbox oder Proxy brauchen Tests.
- Neue Abhängigkeiten sparsam und nur mit Apache-2.0-kompatibler Lizenz.

## Lizenz

Beiträge stehen unter der [Apache-2.0-Lizenz](LICENSE).
