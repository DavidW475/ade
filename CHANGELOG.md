# Changelog

Alle nennenswerten Änderungen an ADE. Format nach [Keep a Changelog](https://keepachangelog.com/de/1.1.0/),
Versionen nach [Semantic Versioning](https://semver.org/lang/de/). Der Release-Workflow übernimmt den
Abschnitt der jeweiligen Version als Release-Notizen.

## [Unveröffentlicht]

## [0.1.0] – 2026-11-26

Erste benutzbare Version: Agenten lokal unter WSL2 arbeiten lassen, ihr Ergebnis prüfen und
übernehmen. **Noch ohne Sandbox** – Agenten laufen mit deinen Rechten; nur in vertrauenswürdigen
Repositories verwenden (Sandbox folgt mit M5).

### Neu

- **Daemon und Dashboard** (M0): `ade start` startet den Daemon auf `127.0.0.1:7000` und öffnet das
  Dashboard im Windows-Browser; Anmeldung per Einmal-Ticket, Host-/Origin-Prüfung, SQLite-Store,
  ein einzelnes Binary mit eingebettetem Frontend.
- **Projekte und Workspaces** (M1): Repositories registrieren (`ade project add`), `.ade/project.yaml`
  mit Basis-Branch und Setup-Befehlen; jeder Workspace ist ein lokaler Clone mit eigenem Branch
  `ade/<id>` ohne Remote zum Haupt-Repo.
- **Agent-Sessions** (M2): Adapter `claude` (Claude Code mit `stream-json`) und `pty` (beliebiger
  Befehl, z. B. eine Shell); Live-Timeline mit Tool-Aufrufen und Kosten, Terminal im Browser,
  Folgeanweisungen, Fortsetzen nach einem Daemon-Neustart.
- **Browser-IDE** (M3): openvscode-server je Workspace unter `ws-<id>.localhost:7000`, startet bei
  Bedarf und stoppt bei Inaktivität.
- **Review und Übernahme** (M4):
  - Am Ende jeder Agent-Runde committet der Daemon offene Änderungen (`ade: wip <session>`) und holt
    den Branch `ade/<id>` per `fetch` ins Projekt-Repo.
  - Diff-Ansicht mit Monaco Diff Editor (nebeneinander oder inline), Dateiliste mit Status und
    Zeilenzahlen; Dateiinhalte werden erst bei Auswahl geladen.
  - **Mergen** in den Basis-Branch mit Merge-Commit; Konflikte werden vorher erkannt und gemeldet,
    ohne etwas zu ändern.
  - **PR erstellen**: Push vom Host und `gh pr create` mit generierter Beschreibung; spätere Pushes
    aktualisieren den PR. Der Agent selbst hat keine Push-Rechte.
  - **Feedback geben**: neue Session im selben Workspace mit Kommentar und aktuellem Diff; ihr
    Ergebnis wird ein weiterer Commit im selben Branch.
  - **Verwerfen** entfernt Workspace, Branch im Projekt-Repo, Sessions, Logs und IDE-Daten.
- Release-Binaries für linux/amd64 und linux/arm64.

### Behoben

- Workspaces lassen sich auch anlegen, wenn `workspaces_dir` auf einem anderen Dateisystem liegt
  als das Repository (#39).

[Unveröffentlicht]: https://github.com/DavidW475/ade/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/DavidW475/ade/releases/tag/v0.1.0
