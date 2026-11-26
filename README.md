# ADE – AI Developer Environment

Eine Leitstelle für KI-Agenten, gebaut für **WSL2**: Agenten starten und beobachten, sie in
**Docker-Sandboxen** hinter einem **Egress-Proxy** arbeiten lassen, ihre Workspaces in einer
**VS-Code-kompatiblen Browser-IDE** ansehen und Ergebnisse prüfen und freigeben, alles im Browser.

> **Status:** v0.1 – Agenten lokal arbeiten lassen, Ergebnis im Browser prüfen und übernehmen
> (Mergen, Pull Request, Feedback, Verwerfen). **Noch ohne Sandbox:** Agenten laufen mit deinen
> Rechten, nur in vertrauenswürdigen Repositories verwenden. Der Plan steht in [`doc/`](doc/README.md),
> der Fortschritt in den [Meilensteinen](https://github.com/DavidW475/ade/milestones).

## Idee

```
ade start  →  http://localhost:7000
```

- **Dashboard:** Projekte, laufende Agenten, Kosten, offene Freigaben
- **Workspaces:** jeder Agent arbeitet in einer eigenen Kopie auf einem eigenen Branch
- **Browser-IDE:** ein Klick öffnet den Workspace in openvscode-server
- **Sandbox:** Docker-Container mit erzwungener Netz-Allowlist; API-Keys bleiben außerhalb (ab M5)
- **Review:** Diff ansehen, mergen, PR erstellen, Feedback geben oder verwerfen
- **Software-Fabriken:** Planer, Entwickler, Reviewer und Tester als Pipeline, mit unterschiedlichen Modellen (ab M9)

## Voraussetzungen

- **Windows mit WSL2** (getestet mit Ubuntu); ADE läuft in WSL, das Dashboard im Windows-Browser.
  Unter einem normalen Linux funktioniert es ebenso (Browser über `xdg-open`).
- **Git ≥ 2.38** (`git --version`; für `git merge-tree --write-tree`)
- **[Claude Code](https://docs.claude.com/claude-code)**, installiert und angemeldet – für den
  Adapter `claude`. Alternativ `providers.anthropic.api_key` in der Konfiguration.
- **[GitHub CLI](https://cli.github.com/)** (`gh`), angemeldet mit `gh auth login` – nur für
  „PR erstellen“. Gepusht wird mit deinen normalen Git-Zugangsdaten.
- Repositories am besten im WSL-Dateisystem (z. B. `~/git/…`), nicht unter `/mnt/c`.

## Installation

Binary des [letzten Releases](https://github.com/DavidW475/ade/releases/latest) nach `~/.local/bin`
(für ARM: `ade_linux_arm64.tar.gz`):

```sh
mkdir -p ~/.local/bin
curl -fsSL https://github.com/DavidW475/ade/releases/latest/download/ade_linux_amd64.tar.gz \
  | tar -xz -C ~/.local/bin ade
ade version
```

Liegt `~/.local/bin` nicht im `PATH` (Ubuntu nimmt es erst nach einer neuen Anmeldung auf), einmal
`export PATH="$HOME/.local/bin:$PATH"` ausführen bzw. in `~/.bashrc` ergänzen. Prüfsummen stehen in
`checksums.txt` beim Release.

## Schnellstart

```sh
ade start                          # Daemon starten und Dashboard im Windows-Browser öffnen
ade project add ~/git/mein-repo    # Repository registrieren (geht auch im Dashboard)
```

1. Im Dashboard das Projekt öffnen und **Agent starten**: Aufgabe eintragen, Adapter `claude`.
   ADE legt einen Workspace an (lokaler Clone, Branch `ade/<id>`) und startet den Agenten darin.
2. Dem Agenten in der Timeline zusehen; bei Bedarf **IDE öffnen** oder Folgeanweisungen geben.
3. Ist der Agent fertig, committet ADE seine Änderungen und holt den Branch `ade/<id>` in dein
   Repository. Im Workspace zeigt die **Diff-Ansicht** alle Änderungen.
4. Entscheiden:
   - **Mergen** – Merge-Commit in den Basis-Branch deines Repos (bei Konflikten ändert sich nichts),
   - **PR erstellen** – Push nach `origin` und `gh pr create`,
   - **Feedback geben** – eine neue Agent-Session im selben Workspace setzt deinen Kommentar um,
   - **Verwerfen** – Workspace und Branch rückstandsfrei entfernen.

`ade start --no-browser` gibt die Anmelde-URL aus, statt den Browser zu öffnen.

Konfiguration: `~/.config/ade/config.yaml` und pro Repo `.ade/project.yaml` (beide optional,
Beispiele in [`examples/`](examples/)). Daten liegen in `~/.local/share/ade/`, Workspaces in
`~/.ade/workspaces/`.

## Aus dem Quellcode bauen

Voraussetzungen: Go und Node.js 22+ (siehe [CONTRIBUTING.md](CONTRIBUTING.md)).

```sh
make build        # Dashboard bauen und in bin/ade einbetten
bin/ade start
```

Änderungen je Version stehen im [CHANGELOG](CHANGELOG.md).

## Dokumentation

- [Überblick und Begriffe](doc/README.md)
- [Architektur](doc/architecture.md)
- [Sandbox und Egress-Proxy](doc/sandbox.md)
- [Software-Fabriken](doc/factory.md)
- [Meilensteine](doc/milestones.md)
- [Architekturentscheidungen](doc/decisions.md)
- [Mitwirken](CONTRIBUTING.md)

## Lizenz

[Apache-2.0](LICENSE)
