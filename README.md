# ADE – AI Developer Environment

Eine Leitstelle für KI-Agenten, gebaut für **WSL2**: Agenten starten und beobachten, sie in
**Docker-Sandboxen** hinter einem **Egress-Proxy** arbeiten lassen, ihre Workspaces in einer
**VS-Code-kompatiblen Browser-IDE** ansehen und Ergebnisse prüfen und freigeben, alles im Browser.

> **Status:** v0.1 plus Docker-Sandbox (M5) und Egress-Proxy (M6) – Agenten arbeiten auf Wunsch
> isoliert im Container, ins Internet nur zu freigegebenen Zielen und ohne Zugriff auf API-Keys;
> das Ergebnis wird im Browser geprüft und übernommen (Mergen, Pull Request, Feedback, Verwerfen).
> Der Plan steht in [`doc/`](doc/README.md),
> der Fortschritt in den [Meilensteinen](https://github.com/DavidW475/ade/milestones).

## Idee

```
ade start  →  http://localhost:7000
```

- **Dashboard:** Projekte, laufende Agenten, Kosten, offene Freigaben
- **Workspaces:** jeder Agent arbeitet in einer eigenen Kopie auf einem eigenen Branch
- **Browser-IDE:** ein Klick öffnet den Workspace in openvscode-server
- **Sandbox:** Docker-Container pro Workspace, sieht nur seinen Workspace; Internet nur über
  einen Egress-Proxy mit Allowlist, API-Keys fügt der Proxy ein (der Container sieht sie nie)
- **Review:** Diff ansehen, mergen, PR erstellen, Feedback geben oder verwerfen
- **Software-Fabriken:** Planer, Entwickler und Reviewer als Pipeline mit Freigaben dazwischen,
  je Rolle eigenes Modell und eigene Rechte (Planer und Reviewer nur lesend)

## Voraussetzungen

- **Windows mit WSL2** (getestet mit Ubuntu); ADE läuft in WSL, das Dashboard im Windows-Browser.
  Unter einem normalen Linux funktioniert es ebenso (Browser über `xdg-open`).
- **Git ≥ 2.38** (`git --version`; für `git merge-tree --write-tree`)
- **[Claude Code](https://docs.claude.com/claude-code)**, installiert und angemeldet – für den
  Adapter `claude`. Alternativ `providers.anthropic.api_key` in der Konfiguration.
- **Docker Engine** in der WSL (oder Docker Desktop mit WSL-Integration) – für die Sandbox.
  Ohne Docker laufen Agenten nur lokal (Schalter „Sandbox“ aus). Das Image
  `ghcr.io/davidw475/ade-agent-base` lädt Docker beim ersten Sandbox-Workspace herunter (~1,5 GB).
- **[GitHub CLI](https://cli.github.com/)** (`gh`), angemeldet mit `gh auth login` – nur für
  „PR erstellen“. Gepusht wird mit deinen normalen Git-Zugangsdaten.
- Repositories am besten im WSL-Dateisystem (z. B. `~/git/…`), nicht unter `/mnt/c`.

## Installation

Das Installationsskript lädt das Binary des [letzten Releases](https://github.com/DavidW475/ade/releases/latest)
für deine Architektur (amd64, arm64), prüft die Prüfsumme aus `checksums.txt`, legt es nach
`~/.local/bin` und meldet fehlende Voraussetzungen:

```sh
curl -fsSL https://raw.githubusercontent.com/DavidW475/ade/main/install.sh | sh
ade version
```

Mit `sh -s -- --service` richtet es ADE zusätzlich als Hintergrunddienst ein (siehe unten).
`ADE_VERSION=v1.0.0` wählt eine bestimmte Version, `ADE_INSTALL_DIR` ein anderes Verzeichnis.
Liegt `~/.local/bin` nicht im `PATH` (Ubuntu nimmt es erst nach einer neuen Anmeldung auf), einmal
`export PATH="$HOME/.local/bin:$PATH"` ausführen bzw. in `~/.bashrc` ergänzen.

Von Hand geht es ebenso (für ARM `ade_linux_arm64.tar.gz`):

```sh
curl -fsSL https://github.com/DavidW475/ade/releases/latest/download/ade_linux_amd64.tar.gz \
  | tar -xz -C ~/.local/bin ade
```

### Als Hintergrunddienst

Ist systemd in der WSL aktiv (`[boot]` `systemd=true` in `/etc/wsl.conf`, danach `wsl --shutdown`),
läuft der Daemon als systemd-User-Dienst und startet mit der WSL:

```sh
ade install-service      # Unit ~/.config/systemd/user/ade.service anlegen und starten
ade uninstall-service    # stoppen und entfernen
```

Die Unit übernimmt den aktuellen `PATH` (git, docker, gh, claude); nach einem Update des Binarys
oder neuen Werkzeugen `ade install-service` erneut ausführen. `ade start` startet dann den Dienst,
statt selbst einen Daemon zu starten. Das Log bleibt `~/.local/share/ade/daemon.log`. Damit der
Dienst auch ohne offene WSL-Shell läuft: `sudo loginctl enable-linger "$USER"`.

## Schnellstart

```sh
ade start                          # Daemon starten und Dashboard im Windows-Browser öffnen
ade project add ~/git/mein-repo    # Repository registrieren (geht auch im Dashboard)
```

1. Im Dashboard das Projekt öffnen und **Agent starten**: Aufgabe eintragen, Adapter `claude`.
   ADE legt einen Workspace an (lokaler Clone, Branch `ade/<id>`) und startet den Agenten darin –
   mit **Sandbox** (Standard, wenn Docker läuft) in einem eigenen Container, der nur diesen
   Workspace sieht. Claude Code braucht dort einen API-Key (`providers.anthropic.api_key`); der
   Egress-Proxy fügt ihn ein, der Container sieht nur einen Platzhalter. Welche Ziele im Internet
   erreichbar sind, regelt `egress` in `.ade/sandbox.yaml`; die Ansicht **Netzwerk** im Workspace
   zeigt erlaubte und geblockte Anfragen.
2. Dem Agenten in der Timeline zusehen; bei Bedarf **IDE öffnen** oder Folgeanweisungen geben.
3. Ist der Agent fertig, committet ADE seine Änderungen und holt den Branch `ade/<id>` in dein
   Repository. Im Workspace zeigt die **Diff-Ansicht** alle Änderungen.
4. Entscheiden:
   - **Mergen** – Merge-Commit in den Basis-Branch deines Repos (bei Konflikten ändert sich nichts),
   - **PR erstellen** – Push nach `origin` und `gh pr create`,
   - **Feedback geben** – eine neue Agent-Session im selben Workspace setzt deinen Kommentar um,
   - **Verwerfen** – Workspace und Branch rückstandsfrei entfernen.

### Software-Fabrik

```sh
cd ~/git/mein-repo
ade run feature --task "Login mit GitHub ergänzen"
```

Die Fabrik `feature` lässt einen Planer (nur lesend) einen Plan schreiben, wartet auf deine
Freigabe, lässt einen Entwickler umsetzen und einen Reviewer (nur lesend) prüfen und erstellt nach
einer zweiten Freigabe einen PR. Fortschritt, Artefakte (Plan, Zusammenfassung, Review) und die
Freigaben stehen im Dashboard unter **Runs**; ein Hinweis beim Freigeben geht an die nächste
Rolle. Eigene Fabriken (andere Rollen, Modelle, Prompts) liegen in `.ade/factories/`, siehe
[doc/factory.md](doc/factory.md) und [`examples/factories/`](examples/factories/).

`ade start --no-browser` gibt die Anmelde-URL aus, statt den Browser zu öffnen.

`ade gc` entfernt verwaiste Sandbox-Container und -Netze (`--dry-run` zeigt sie nur an).

Konfiguration: `~/.config/ade/config.yaml` und pro Repo `.ade/project.yaml` und
`.ade/sandbox.yaml` (alle optional,
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
