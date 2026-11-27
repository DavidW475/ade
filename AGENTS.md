# AGENTS.md

Hinweise für KI-Agenten (und Menschen), die an ADE arbeiten. Gilt für das ganze Repo.

## Projekt in Kürze

ADE ist eine Leitstelle für KI-Agenten unter WSL2: ein Go-Daemon (`ade`) mit eingebettetem
React-Dashboard. Plan und Begriffe stehen in [`doc/`](doc/README.md), der Fahrplan in
[`doc/milestones.md`](doc/milestones.md), Entscheidungen in [`doc/decisions.md`](doc/decisions.md).

## Befehle

```sh
make build   # web/ bauen (→ internal/api/dist) und bin/ade erzeugen
make test    # go test -race ./... und TypeScript-Typprüfung
make lint    # golangci-lint und ESLint
make dev     # Daemon im Vordergrund (Debug-Logging)
```

Vor jedem Commit müssen `make test` und `make lint` grün sein. Zum manuellen Testen eine
eigene Konfiguration mit anderem Port und Datenverzeichnis verwenden (siehe
[CONTRIBUTING.md](CONTRIBUTING.md#lokal-entwickeln)), nie die Daten einer installierten ADE.

## Struktur

| Pfad | Inhalt |
|---|---|
| `cmd/ade/` | CLI-Einstieg und Unterbefehle |
| `internal/api/` | HTTP-Handler, Auth, Host-/Origin-Prüfung, eingebettetes Frontend (`dist/`) |
| `internal/config/` | Globale Konfiguration |
| `internal/daemon/` | Daemon-Start, Lock, Client für die CLI |
| `internal/store/` | SQLite und Migrationen (`migrations/NNNN_name.sql`, lückenlos nummeriert) |
| `internal/browser/` | Browser öffnen (WSL → Windows) |
| `internal/apperr/` | Fehlerarten (NotFound, Invalid, Conflict) → HTTP-Status |
| `internal/git/` | Aufrufe des git-Binarys, Diff über temporären Index, Commit/Fetch/Merge/Push |
| `internal/project/` | Projekte registrieren, `.ade/project.yaml` |
| `internal/workspace/` | Workspaces: Clone, Setup, Diff, Übernahme (Collect, Merge, PR), Verwerfen |
| `internal/agent/` | Agent-Sessions: Prozesse (PTY/Pipes), Event-Hub mit Replay, Neustart-Aufräumen |
| `internal/agent/adapter/` | Adapter-Interface, Event-Typen + JSON-Schema; `claude/`, `pty/`, `fake/` (Tests) |
| `internal/ide/` | openvscode-server je Workspace: Download mit Prüfsumme, Start, Inaktivitäts-Stopp |
| `internal/gateway/` | Reverse-Proxy `ws-<id>.localhost` → IDE, Ticket-Anmeldung, Connection-Token |
| `internal/sandbox/` | Docker-Container, internes Netz und Proxy-Container je Workspace über die Docker-CLI, `.ade/sandbox.yaml` (inkl. `egress`), GC |
| `internal/proxy/` | Egress-Proxy `ade proxy`: Allowlist, Schutz vor internen Zielen, Secret-Injection, Steuer-API, Rückfragen (Hook, `on_blocked: ask`) |
| `internal/approval/` | Freigaben: Warteschlange, Regeln aus `approvals` in `project.yaml`, `ade hook pretooluse`, Egress-Ausnahmen |
| `web/` | Dashboard (Vite + React + TypeScript, Monaco Diff Editor) |
| `examples/`, `images/` | Beispielkonfigurationen, Container-Images (`images/agent-base`) |

Neue Module folgen der Struktur in [architecture.md](doc/architecture.md#10-repo-struktur).

## Konventionen

- **Sprache:** Doku, Kommentare, Fehlermeldungen, Log- und UI-Texte auf Deutsch; Bezeichner auf Englisch.
- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/), z. B. `feat(workspace): …`.
- **Sicherheit zuerst:** Der Daemon führt Code aus. Jede neue Route läuft durch Host-/Origin-Prüfung
  und braucht Auth (außer `/api/health`). Änderungen an Auth, Gateway, Sandbox oder Proxy brauchen Tests.
- **Abhängigkeiten** sparsam, Lizenz kompatibel zu Apache-2.0, kein cgo.
- **Migrationen** nie nachträglich ändern, immer eine neue anlegen.
- **Git** immer über `internal/git` aufrufen. Das Haupt-Repo eines Projekts wird nur gelesen,
  außer bei den Übernahme-Schritten des Daemons (Fetch nach `ade/<id>`, Mergen, Push, Branch
  löschen, ADR-013); Workspaces haben kein Remote zum Haupt-Repo (ADR-005). Befehle im Workspace
  über `git.Worktree` (keine Hooks; bei Sandbox-Workspaces im Container, #41).
- **UI prüfen:** Unter WSL lässt sich das Dashboard mit Edge headless fotografieren
  (`msedge.exe --headless=new --screenshot=… "http://localhost:<port>/auth/login?ticket=…&next=/pfad"`,
  Ticket per `POST /api/auth/tickets` mit dem Daemon-Secret).
- **Architekturentscheidungen** als neues ADR in `doc/decisions.md` anhängen.

## Arbeitsablauf: Meilensteine, Issues und PRs

Jeder Meilenstein (M0, M1, …) ist ein GitHub-Milestone mit Issues. Arbeit wird immer gegen diese
Issues erledigt und über PRs mit ihnen verknüpft.

1. **Issues lesen, bevor du anfängst.** Alle Issues des Meilensteins ansehen, nicht nur
   `doc/milestones.md`; die Issues sind detaillierter:
   ```sh
   gh issue list --milestone "M1 – Projekte & Workspaces" --state open
   gh issue view <nr>
   ```
2. **Branch** von aktuellem `main`: `m<N>-<kurzname>`, z. B. `m1-workspaces`. Nie direkt auf
   `main` committen.
3. **Umsetzen** und am Ende jede Aufgabe und jedes „Fertig, wenn“ der Issues gegen den Code
   prüfen. Was fehlt, wird ergänzt oder im PR ausdrücklich als offen genannt.
4. **PR erstellen** nach [`.github/pull_request_template.md`](.github/pull_request_template.md):
   - Für **jedes vollständig erledigte Issue** eine eigene Zeile `Closes #<nr>` im PR-Text.
     Nur so schließt GitHub das Issue beim Merge.
   - Teilweise erledigte Issues mit `Teil von #<nr>` verlinken; sie bleiben offen.
   - Abweichungen vom Issue oder von `doc/` mit Begründung unter „Abweichungen vom Plan“.
   - Die Akzeptanzkriterien des Meilensteins in `doc/milestones.md` im selben PR abhaken.
5. **CI abwarten** (`gh pr checks <nr> --watch`) und Fehler beheben.
6. **Nach dem Merge prüfen**, ob alle Issues geschlossen sind:
   ```sh
   gh issue list --milestone "<Meilenstein>" --state open
   ```
   - Wurde ein Issue nicht automatisch geschlossen (Schlüsselwort fehlte, PR schon gemergt):
     mit Verweis auf den PR schließen, z. B.
     `gh issue close <nr> --comment "Erledigt in #<pr>."`, und den PR-Text um die Zeile ergänzen.
   - Ist der Meilenstein leer, ihn schließen:
     `gh api -X PATCH repos/DavidW475/ade/milestones/<nummer> -f state=closed`.
7. **Neue Aufgaben**, die beim Arbeiten auffallen, als Issue im passenden Meilenstein anlegen,
   statt sie stillschweigend mitzuerledigen oder zu vergessen.

Merges nach `main` macht der Maintainer; Agenten öffnen PRs, mergen aber nicht selbst.
