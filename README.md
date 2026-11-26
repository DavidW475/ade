# ADE – AI Developer Environment

Eine Leitstelle für KI-Agenten, gebaut für **WSL2**: Agenten starten und beobachten, sie in
**Docker-Sandboxen** hinter einem **Egress-Proxy** arbeiten lassen, ihre Workspaces in einer
**VS-Code-kompatiblen Browser-IDE** ansehen und Ergebnisse prüfen und freigeben, alles im Browser.

> **Status:** in Entwicklung (M2 – Agent-Sessions). Repositories lassen sich registrieren,
> Workspaces anlegen, Diffs ansehen; Claude Code oder ein beliebiges Terminal laufen im Workspace,
> live im Browser (Timeline bzw. xterm.js). Noch ohne Sandbox. Der Plan steht in [`doc/`](doc/README.md), der
> Fortschritt in den [Meilensteinen](https://github.com/DavidW475/ade/milestones).

## Idee

```
ade start  →  http://localhost:7000
```

- **Dashboard:** Projekte, laufende Agenten, Kosten, offene Freigaben
- **Workspaces:** jeder Agent arbeitet in einer eigenen Kopie auf einem eigenen Branch
- **Browser-IDE:** ein Klick öffnet den Workspace in openvscode-server
- **Sandbox:** Docker-Container mit erzwungener Netz-Allowlist; API-Keys bleiben außerhalb
- **Review:** Diff ansehen, mergen, PR erstellen, Feedback geben oder verwerfen
- **Software-Fabriken:** Planer, Entwickler, Reviewer und Tester als Pipeline, mit unterschiedlichen Modellen

## Schnellstart (aus dem Quellcode)

Voraussetzungen: Go und Node.js 22+ (siehe [CONTRIBUTING.md](CONTRIBUTING.md)).

```sh
make build
bin/ade start            # startet den Daemon und öffnet das Dashboard im Windows-Browser
bin/ade start --no-browser   # gibt stattdessen die Anmelde-URL aus
bin/ade project add ~/git/mein-repo   # Repository registrieren (oder im Dashboard)
```

Agenten startest du im Dashboard über **Agent starten** im Projekt. Für den Adapter `claude`
muss [Claude Code](https://docs.claude.com/claude-code) installiert und angemeldet sein (oder
`providers.anthropic.api_key` in der Konfiguration gesetzt). Bis zur Sandbox (M5) laufen
Agenten mit deinen Rechten – nur in vertrauenswürdigen Repositories verwenden.

Konfiguration: `~/.config/ade/config.yaml` und pro Repo `.ade/project.yaml` (beide optional,
Beispiele in [`examples/`](examples/)). Daten liegen in `~/.local/share/ade/`, Workspaces in
`~/.ade/workspaces/`.

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
