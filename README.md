# ADE – AI Developer Environment

Eine Leitstelle für KI-Agenten, gebaut für **WSL2**: Agenten starten und beobachten, sie in
**Docker-Sandboxen** hinter einem **Egress-Proxy** arbeiten lassen, ihre Workspaces in einer
**VS-Code-kompatiblen Browser-IDE** ansehen und Ergebnisse prüfen und freigeben, alles im Browser.

> **Status:** frühe Planungsphase. Es gibt noch keinen lauffähigen Code.
> Der Plan steht in [`doc/`](doc/README.md), der Fortschritt in den
> [Meilensteinen](https://github.com/DavidW475/ade/milestones).

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

## Dokumentation

- [Überblick und Begriffe](doc/README.md)
- [Architektur](doc/architecture.md)
- [Sandbox und Egress-Proxy](doc/sandbox.md)
- [Software-Fabriken](doc/factory.md)
- [Meilensteine](doc/milestones.md)
- [Architekturentscheidungen](doc/decisions.md)

## Lizenz

[Apache-2.0](LICENSE)
