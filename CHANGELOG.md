# Changelog

Alle nennenswerten Änderungen an ADE. Format nach [Keep a Changelog](https://keepachangelog.com/de/1.1.0/),
Versionen nach [Semantic Versioning](https://semver.org/lang/de/). Der Release-Workflow übernimmt den
Abschnitt der jeweiligen Version als Release-Notizen.

## [Unveröffentlicht]

### Neu

- **Installationsskript** (M11): `curl -fsSL https://raw.githubusercontent.com/DavidW475/ade/main/install.sh | sh`
  lädt das passende Release-Binary, prüft die Prüfsumme, installiert nach `~/.local/bin` und warnt
  bei fehlenden Voraussetzungen (git ≥ 2.38, docker/podman, gh, claude/codex). `--service` richtet
  zusätzlich den Dienst ein. Das Skript liegt auch jedem Release bei.
- **`ade install-service`** und **`ade uninstall-service`**: Daemon als systemd-User-Dienst unter
  WSL (`systemd=true`). Die Unit übernimmt den `PATH`, schreibt ins Daemon-Log und startet bei
  Fehlern neu; `ade start` nutzt den Dienst, wenn er für dieselbe Konfiguration installiert ist.
- **Releases mit Attestierung** (M11): Jeder Tag baut Binaries und die Images `ade-agent-base`
  und `ade-dev` mit gleicher Version (`:X.Y.Z`), mit SBOM und Build-Provenance
  (`gh attestation verify …`). Ein Snapshot-Build samt `install.sh` läuft in der CI, wenn sich
  die Release-Konfiguration ändert. Release-Ablauf in CONTRIBUTING.md.
- **Automatische Releases** (M11): Steht nach einem Merge auf `main` oben im CHANGELOG eine neue
  Version `## [X.Y.Z] – JJJJ-MM-TT`, setzt `auto-release.yml` den Tag und startet den
  Release-Lauf. `scripts/changelog-version.sh` liest und prüft die Überschrift, auch schon im PR.
- **Fabrik-Vorlagen** (M10): `bugfix` (Fehler per Test reproduzieren, beheben, Tests grün),
  `review-only` (zwei Reviewer parallel, Zusammenfassung, kein PR) und `refactor` (schrittweise
  mit Test nach jedem Schritt); `feature` mit Review und Test parallel und Rücksprung bei
  Änderungswünschen. Neuer Eingabetyp `branch`: Der Workspace des Runs zweigt davon ab.
- **Docker-Sandbox** (M5): Workspaces lassen sich mit Sandbox anlegen (Schalter beim Agent-Start
  und beim Anlegen, Standard `defaults.sandbox`). Ein Container pro Workspace sieht nur das
  Workspace-Verzeichnis (`/workspace`), läuft mit deiner UID, ohne Capabilities und mit Grenzen
  für Speicher, CPU und Prozesse. Agent-Sessions, Terminals, Setup-Befehle, die Browser-IDE und
  die git-Befehle des Daemons im Workspace laufen darin.
- `.ade/sandbox.yaml` (Image, Ressourcen) und `sandbox.image` in der globalen Konfiguration.
- Image `ghcr.io/davidw475/ade-agent-base` (Debian, git, Node.js, Python, ripgrep, Claude Code,
  openvscode-server); `make image` baut es lokal.
- `ade gc` entfernt verwaiste Sandbox-Container; der Daemon gleicht Container beim Start ab.
- **Egress-Proxy** (M6): Der Sandbox-Container hängt in einem internen Docker-Netz ohne Route nach
  draußen. Einziger Weg ins Internet ist ein Proxy-Container pro Workspace (`ade proxy`), der nur
  Ziele aus `egress` in `.ade/sandbox.yaml` durchlässt (Presets `anthropic`, `github-readonly`,
  `npm`, `pypi`, `go-proxy`, eigene `allow`-/`deny`-Einträge). Änderungen wirken ohne Neustart.
- **Secret-Injection:** Ist `providers.anthropic.api_key` gesetzt, spricht Claude Code in der
  Sandbox die API über `http://proxy:8080/p/anthropic` an; der Proxy fügt den Key ein.
- Ansicht **Netzwerk** im Workspace: Policy, Ziele mit erlaubten und geblockten Anfragen, letzte
  Anfragen.
- **Freigaben** (M7): Claude Code fragt vor jedem Werkzeug-Aufruf über den Hook
  `ade hook pretooluse` bei ADE nach – lokal direkt beim Daemon, in der Sandbox über den
  Egress-Proxy. Was die Regeln in `approvals` (`.ade/project.yaml`, getrennt für `sandbox` und
  `local`) nicht erlauben oder sperren, wartet auf eine Entscheidung im Dashboard (Seite
  **Freigaben** mit Badge, Browser-Benachrichtigung, Anzeige in der Session). Ohne Entscheidung
  gilt die Anfrage nach `approvals.timeout` (Standard 10m) als abgelehnt.
- `egress.on_blocked: ask`: Anfragen an nicht freigegebene Ziele warten auf eine Freigabe
  („Einmal“, „Für diesen Workspace“, „Immer (Projekt)“, „Ablehnen“). Geblockte Hosts lassen sich
  in der Netzwerk-Ansicht mit einem Klick dauerhaft erlauben; die Ausnahmen speichert der Daemon,
  das Repo bleibt unverändert.
- **Adapter `codex`** (M8): Codex CLI lokal und in der Sandbox, mit Timeline (Nachrichten,
  Befehle, Dateiänderungen) und Fortsetzen über `codex exec resume`. Codex ist im Image
  `ade-agent-base` enthalten; neues Egress-Preset `openai` (auch in der Voreinstellung).
- **Secret-Injection für OpenAI:** Mit `providers.openai.api_key` spricht Codex in der Sandbox
  die API über `http://proxy:8080/p/openai/v1` an; lokal bekommt es den Key als `CODEX_API_KEY`.
- **Kosten:** Der Egress-Proxy liest die Token-Nutzung aus den API-Antworten (Anthropic, OpenAI;
  JSON und Streaming). Kosten und Tokens erscheinen live in der Session, im Workspace, im Projekt
  (Monat und gesamt) und in der Projektübersicht. Ohne Messung am Proxy gilt die Meldung der CLI;
  Codex-Kosten berechnet ADE aus Tokens. `GET /api/usage`.
- Eingebaute Preistabelle (Schätzwerte, Stand 2026-11) und `pricing` in der globalen
  Konfiguration zum Ergänzen.
- **Modell-Aliase:** `models` in der globalen Konfiguration (z. B. `opus` → Anbieter und
  Modell-ID); Auswahl im Startdialog, der Anbieter muss zum Adapter passen.
- **Budgets:** `budget.session_usd` und `budget.monthly_usd` in `.ade/project.yaml`, je Session
  auch beim Start. Bei Überschreitung pausiert der Agent (Status „pausiert“) und wartet auf eine
  Budget-Freigabe; „Budget erhöhen“ setzt ihn fort, „Ablehnen“ beendet ihn.

- **Software-Fabrik** (M9): Fabriken in `.ade/factories/<name>.yaml` verketten Rollen-Agenten
  (Adapter, Modell-Alias, Rechte, Prompt) zu einem Run: lineare Schritte, Gates
  (`gate: human`) als Freigabe im Dashboard und `action: pull_request` am Ende. Die letzte
  Antwort einer Rolle wird ihr Artefakt (z. B. `plan.md`) und geht an die folgenden Rollen.
  Mitgeliefert ist die Vorlage `feature` (Planer → Freigabe → Entwickler → Reviewer → Freigabe
  → PR). Runs überstehen einen Neustart des Daemons.
- **Lese-Rollen** (`permissions: read`) können den Workspace nicht ändern: in der Sandbox ein
  eigener Container mit `/workspace` nur lesend, ohne Sandbox ein Schnappschuss.
- `ade run <fabrik> --task "…"` (mit `--wait`), Seiten **Runs** und Run-Ansicht mit Pipeline,
  Gates, Artefakten und Kosten; Startdialog im Projekt. `budget.max_cost_usd` und
  `budget.max_duration` je Fabrik.
- API: `GET /api/factories`, `GET/POST /api/runs`, `GET /api/runs/{id}`,
  `POST /api/runs/{id}/cancel`, `GET /api/runs/{id}/artifacts/{name}`.
- **Fabrik v2** (M10): strukturierte Artefakte (JSON, optional mit Schema; ungültige Antworten
  korrigiert die Rolle bis zu zweimal), Bedingungen (`when`), Entscheidungen mit Rücksprung
  (`goto`, `max_iterations`), Gates mit `on_deny` (Ablehnen mit Begründung springt zurück) und
  parallele Schritte (`parallel`). Frühere Durchläufe bleiben in `history` sichtbar.
- **Auslöser** (M10): `triggers` in `.ade/project.yaml` starten Runs automatisch – je offenem
  GitHub-Issue mit Label (Polling über `gh`, Kommentar bei Start und Ende, der PR schließt das
  Issue) oder nach Zeitplan (Cron). Ausgelöste Runs laufen standardmäßig in der Sandbox und
  brauchen ein Fabrik-Budget. API: `GET /api/projects/{id}/triggers`,
  `POST /api/projects/{id}/triggers/{name}/pause|resume`.

### Behoben

- Release-Binaries nutzen das gleich versionierte Sandbox-Image (`ade-agent-base:X.Y.Z`, auch für
  Vorabversionen `-rc.N`) statt `:latest`; GoReleaser schreibt die Version jetzt mit `v` ins Binary.
- `ade gc` und der Container-Abgleich scheitern nicht mehr, wenn ein Container zwischen
  Auflisten und Inspizieren verschwindet (#66).

### Sicherheit

- Ein Agent kann in der Repo-Konfiguration seines Workspaces keine Befehle mehr hinterlegen, die
  der Daemon auf dem Host ausführt (Filter, fsmonitor): Bei Sandbox-Workspaces laufen WIP-Commit,
  Diff und Datei-Inhalte im Container (#41).
- Sandbox-Workspaces klonen ohne Hardlinks, damit der Container keine Objektdateien des
  Haupt-Repos verändern kann.
- Kein API-Key im Sandbox-Container (weder Umgebung noch `docker inspect`). Programme, die den
  Proxy umgehen, erreichen nichts; externe Namen lösen im Container nicht auf. Der Proxy verbindet
  nie zu internen Adressen (Host, Container, private Netze).

### Geändert

- Sandbox-Container bekommen das ade-Binary nur lesend für den Freigabe-Hook und werden dafür beim
  nächsten Start einmal neu angelegt.
- Der Berechtigungsmodus von Claude Code gilt nur noch, wenn der Freigabe-Hook nicht verfügbar ist.
- Sandbox-Container aus M5 (normales Netz) legt ADE beim nächsten Start neu an; Dateien im
  Workspace bleiben, Änderungen außerhalb von `/workspace` gehen verloren.
- Ungültige `.ade/sandbox.yaml`: Neue Sandbox-Container starten erst nach der Korrektur, laufende
  behalten die zuletzt gültige Netzwerk-Policy.
- `make dev` baut ohne cgo; der Daemon braucht ein statisches Binary für den Proxy-Container.
- Anfragen an `/p/<anbieter>/` fordert der Proxy unkomprimiert an (für die Token-Zählung).
- `/api/adapters` liefert zusätzlich Modi je Adapter, Anbieter und Modell-Aliase.

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
