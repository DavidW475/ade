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

## Releases

Releases macht der Maintainer, und zwar über `CHANGELOG.md` (ADR-024): Steht nach einem Merge auf
`main` oben eine Version `## [X.Y.Z] – JJJJ-MM-TT` (oder `## [X.Y.Z-rc.N] – …` als Vorabversion),
zu der es noch keinen Tag gibt, setzt
[`auto-release.yml`](.github/workflows/auto-release.yml) den Tag `vX.Y.Z` auf diesen Commit und
startet [`release.yml`](.github/workflows/release.yml):

- **GoReleaser** baut `ade_linux_amd64.tar.gz` und `ade_linux_arm64.tar.gz` (Version `vX.Y.Z` im
  Binary), `checksums.txt` und je Archiv eine SPDX-SBOM, lädt `install.sh` mit hoch und übernimmt
  den passenden Abschnitt aus `CHANGELOG.md` als Release-Notizen. Für alle Dateien in
  `checksums.txt` entsteht eine Build-Provenance-Attestierung.
- **Images** (`images.yml`, parallel): `ghcr.io/davidw475/ade-agent-base` und `ade-dev` mit dem Tag
  `X.Y.Z` (ohne `v`; bei Vollversionen zusätzlich `X.Y`), SBOM und Provenance. Das Binary nutzt
  standardmäßig genau `ade-agent-base:X.Y.Z` (`sandbox.DefaultImage`), deshalb werden die Images
  bei jedem Release gebaut, auch ohne Änderung unter `images/`.

Die Version liest [`scripts/changelog-version.sh`](scripts/changelog-version.sh). Es prüft Format,
Datum und Reihenfolge; dieselbe Prüfung läuft in jedem PR (Job „Shell-Skripte“).

Ablauf:

1. In `CHANGELOG.md` den Abschnitt `[Unveröffentlicht]` in `## [X.Y.Z] – JJJJ-MM-TT` umbenennen,
   einen neuen leeren `[Unveröffentlicht]` anlegen und die Vergleichslinks am Ende ergänzen; per PR
   nach `main`. **Der Merge veröffentlicht.** Lokal prüfen: `scripts/changelog-version.sh` gibt die
   neue Version aus.
2. Optional vorher (braucht [GoReleaser](https://goreleaser.com/install/)): `goreleaser check` und
   `goreleaser release --snapshot --clean --skip=sbom`. In PRs, die die Release-Konfiguration
   ändern, läuft derselbe Snapshot samt `install.sh` in der CI
   ([`release-snapshot.yml`](.github/workflows/release-snapshot.yml)).
3. Nach dem Merge den Lauf verfolgen: `gh run watch` (Workflow „Auto-Release“). Danach prüfen:
   ```sh
   gh release view vX.Y.Z
   gh release download vX.Y.Z -p 'ade_linux_amd64.tar.gz' -D /tmp/ade-rel
   gh attestation verify /tmp/ade-rel/ade_linux_amd64.tar.gz --repo DavidW475/ade
   gh attestation verify oci://ghcr.io/davidw475/ade-agent-base:X.Y.Z --repo DavidW475/ade
   ADE_VERSION=vX.Y.Z sh install.sh && ade version
   ```
   Schlägt nur der Image-Build fehl, ist das Release schon veröffentlicht: Fehler beheben und den
   Lauf über „Re-run failed jobs“ wiederholen. Scheitert der Lauf, bevor das Release entsteht, den
   Fehler beheben und „Auto-Release“ von Hand starten (`gh workflow run auto-release.yml`): Gibt
   es den Tag schon, aber kein Release, holt der Workflow es nach.

Ein von Hand gepushter Tag (`git tag -a vX.Y.Z -m "ADE vX.Y.Z" && git push origin vX.Y.Z`)
startet `release.yml` weiterhin direkt; der CHANGELOG-Abschnitt muss trotzdem existieren.

## Konventionen

- **Sprache:** Doku, Kommentare, Fehlermeldungen und UI-Texte auf Deutsch; Bezeichner auf Englisch.
- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:` …).
- **Architekturentscheidungen** als ADR in [`doc/decisions.md`](doc/decisions.md) anhängen.
- **Sicherheit:** Der Daemon kann Code ausführen. Änderungen an Auth, Host-/Origin-Prüfung,
  Sandbox oder Proxy brauchen Tests.
- Neue Abhängigkeiten sparsam und nur mit Apache-2.0-kompatibler Lizenz.

## Lizenz

Beiträge stehen unter der [Apache-2.0-Lizenz](LICENSE).
