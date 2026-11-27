# Architekturentscheidungen (ADRs)

Kurzform: Kontext → Entscheidung → Konsequenzen. Neue Entscheidungen unten anhängen;
überholte als *ersetzt durch ADR-xxx* markieren, nicht löschen.

## ADR-001: Daemon + Web-UI statt Desktop-App

- **Kontext:** Agenten laufen lange; die UI soll unter Windows angenehm sein. WSLg-Fenster haben
  Eigenheiten (Skalierung, Zwischenablage, Fensterrahmen).
- **Entscheidung:** Ein Daemon in der WSL, UI im Windows-Browser über `localhost`.
- **Konsequenzen:** Kein WSLg nötig; Agenten überleben das Schließen der UI; ein Desktop-Wrapper
  (z. B. Wails) bleibt später optional möglich. Sicherheit des localhost-Servers muss ernst
  genommen werden (ADR-008).

## ADR-002: Go für das Backend

- **Kontext:** Viele Prozesse, PTYs, Docker, Proxy, Nebenläufigkeit; einfache Verteilung gewünscht.
- **Entscheidung:** Go, ein statisches Binary mit eingebettetem Frontend.
- **Konsequenzen:** Docker- und Proxy-Ökosystem in Go ist ausgereift; Proxy kann im selben Binary
  leben (`ade proxy`).

## ADR-003: openvscode-server als Browser-IDE

- **Kontext:** VS-Code-Erlebnis im Browser, Open Source, ohne einen eigenen Fork zu pflegen.
- **Entscheidung:** openvscode-server (MIT) pro Workspace; Extensions aus Open VSX.
- **Konsequenzen:** Kein Microsoft-Marketplace, keine proprietären Microsoft-Extensions
  (z. B. Pylance, Remote-Extensions). Alternative code-server bleibt austauschbar.

## ADR-004: Subdomain-Routing für IDEs

- **Kontext:** VS Code unter einem Unterpfad ist fehleranfällig; mehrere IDEs sollen sich nicht
  Cookies/Storage teilen.
- **Entscheidung:** `ws-<id>.localhost:7000` statt `/ide/<id>/`.
- **Konsequenzen:** Eigener Origin pro IDE; Auth-Cookie muss für die Subdomain gesetzt werden
  (Token-Übergabe beim ersten Aufruf).

## ADR-005: Workspace = lokaler Git-Clone statt Worktree

- **Kontext:** Ein `git worktree` verweist per `.git`-Datei auf das Haupt-Repo. Im Container
  funktioniert das nur, wenn das Haupt-Repo mitgemountet wird – dann könnte der Agent dessen
  Hooks, Konfiguration und andere Branches verändern.
- **Entscheidung:** `git clone --local` pro Workspace (Objekte per Hardlink, daher günstig);
  Ergebnisse holt der Daemon per `git fetch` ins Haupt-Repo.
- **Konsequenzen:** Ein Mechanismus für lokal und Sandbox; volle Isolation; etwas mehr
  Speicher für Arbeitsdateien.

## ADR-006: Netzisolation über internes Docker-Netz + Proxy-Container

- **Kontext:** `HTTP_PROXY` ist nur eine Bitte an Programme.
- **Entscheidung:** Agent-Container nur im `--internal`-Netz; einziger Ausgang ist der
  Proxy-Container mit Allowlist.
- **Konsequenzen:** Isolation ist erzwungen; Programme ohne Proxy-Unterstützung haben kein Netz
  (gewollt). DNS-Verhalten muss geprüft werden.

## ADR-007: Secrets per Proxy-Injection

- **Kontext:** Agenten können durch Prompt-Injection manipuliert werden.
- **Entscheidung:** API-Keys bleiben im Daemon/Proxy; Agent-CLIs sprechen über Basis-URLs mit dem
  Proxy, der den Key einsetzt. Git-Push nur durch den Daemon.
- **Konsequenzen:** Keys sind aus dem Container nicht auslesbar; Token-Zählung und Budgets am
  Proxy möglich. CLIs ohne Basis-URL-Option brauchen Sonderbehandlung.

## ADR-008: localhost-Härtung

- **Kontext:** Jede Webseite im Browser kann Anfragen an `localhost` schicken (CSRF, DNS-Rebinding).
- **Entscheidung:** Nur `127.0.0.1` binden, `Host`- und `Origin`-Prüfung, Session-Cookie über
  Einmal-Token aus `ade start`.
- **Konsequenzen:** Etwas mehr Aufwand beim Start; Grundvoraussetzung, weil der Daemon Code
  ausführen kann.

## ADR-009: Lizenz Apache-2.0

- **Kontext:** Open Source auf GitHub; möglichst breite Nutzung; Abhängigkeiten sind MIT/Apache.
- **Entscheidung:** Apache-2.0 (Patentklausel, kompatibel mit allen geplanten Abhängigkeiten).
- **Konsequenzen:** Kommerzielle Nutzung und geschlossene Forks erlaubt. Lizenzhinweise der
  Abhängigkeiten (openvscode-server, Monaco, xterm.js …) werden mitgeliefert (`NOTICE`/
  Image-Labels). Markennamen (VS Code, Claude, Docker) nicht im Produktnamen.

## ADR-010: Anbieter-Zugang nur über API-Keys oder offizielle CLIs

- **Kontext:** Anbieter erlauben Drittanwendungen meist nicht, Abo-Logins (z. B. Claude Pro/Max,
  ChatGPT Plus) für eigene Zwecke zu verwenden.
- **Entscheidung:** ADE nutzt API-Keys des Nutzers oder startet offizielle CLIs, die der Nutzer
  selbst installiert und angemeldet hat. ADE baut keine eigenen Abo-Login-Flows.
- **Konsequenzen:** Vor jedem Release die aktuellen Nutzungsbedingungen der unterstützten
  Anbieter prüfen.

## ADR-011: Claude Code als Prozess mit stream-json-Ein- und -Ausgabe

- **Kontext:** Für eine Timeline braucht ADE strukturierte Events; Folgeanweisungen sollen ohne
  Neustart möglich sein, und nach einem Daemon-Neustart soll sich eine Session fortsetzen lassen.
- **Entscheidung:** Der `claude`-Adapter startet `claude -p --input-format stream-json
  --output-format stream-json --verbose` mit Pipes (kein PTY). Anweisungen gehen als JSON-Zeilen
  über stdin; der Prozess bleibt zwischen Durchgängen am Leben (Status `idle`). Die `session_id`
  aus der Ausgabe wird gespeichert; nach Prozessende setzt `--resume <id>` die Unterhaltung fort.
  Beendet sich der Daemon, gelten Sessions als `interrupted` und werden nicht weiter ausgeführt.
- **Konsequenzen:** Eine Folgeanweisung während eines Durchgangs wird danach bearbeitet. Kein
  Terminal-Stream für Claude-Sessions; wer interaktiv arbeiten will, öffnet ein `pty`-Terminal im
  selben Workspace. Berechtigungen laufen bis M7 über `--permission-mode` (Standard im Dashboard:
  `acceptEdits`); Werkzeuge, die eine Bestätigung bräuchten, lehnt die CLI im Print-Modus ab.
  Das stream-json-Format ist nicht formal spezifiziert: Der Parser ignoriert unbekannte Zeilen,
  Tests nutzen aufgezeichnete Ausgaben (`internal/agent/adapter/claude/testdata`).

## ADR-012: Browser-IDE lokal mit festgelegter openvscode-server-Version und eigener Subdomain-Session

- **Kontext:** Bis M5 läuft die IDE ohne Container. openvscode-server ist nicht installiert und
  wird nicht über einen Paketmanager verteilt. Die IDE führt Code aus (Terminal, Erweiterungen)
  und darf das Dashboard nicht steuern können.
- **Entscheidung:** ADE lädt eine festgelegte Version (`ide.Version`, Prüfsumme je Architektur im
  Code) beim ersten Start nach `<data_dir>/ide/` und bricht bei falscher Prüfsumme ab;
  `ide.binary` in der Konfiguration ersetzt den Download. Pro Workspace läuft ein Prozess auf
  `127.0.0.1` mit freiem Port und zufälligem Connection-Token, der nur in einer Datei steht
  (`--connection-token-file`). Das Gateway setzt das Token-Cookie `vscode-tkn` selbst. Anmeldung an
  `ws-<id>.localhost` über ein Einmal-Ticket aus `POST /api/workspaces/{id}/ide/ticket`, das an
  den Workspace und die Dashboard-Session gebunden ist; es wird gegen ein Host-only-Cookie
  `ade_ide` (HttpOnly, `SameSite=Lax`) getauscht. Logout im Dashboard beendet auch die
  IDE-Sessions. Erweiterungen teilen sich alle IDEs, Einstellungen und Serverdaten liegen je
  Workspace. Ohne offene Verbindung stoppt ADE eine IDE nach `ide.idle_timeout` (Standard 15 min).
- **Konsequenzen:** Lax statt Strict, weil `localhost` und `ws-<id>.localhost` verschiedene Sites
  sind und der Sprung aus dem Dashboard eine Cross-Site-Navigation ist. Origin-Prüfung: Auf einer
  IDE-Subdomain ist nur deren eigener Origin erlaubt, auf dem Dashboard keine IDE. Die
  Dashboard-CSP gilt nicht für IDE-Subdomains. Die IDE erbt dieselbe gefilterte Umgebung wie
  Agenten (kein `GH_TOKEN`, kein `SSH_AUTH_SOCK`). Webviews lädt VS Code von `vscode-cdn.net`, dafür
  braucht es Internet. Beim Anheben der Version müssen beide Prüfsummen neu berechnet werden.

## ADR-013: Übernahme ins Projekt-Repo durch den Daemon

- **Kontext:** Bis M3 hat ADE das Projekt-Repo nur gelesen (ADR-005). Für Review und Übernahme
  (M4) muss das Ergebnis eines Agenten dorthin, ohne dass der Agent Zugriff auf das Repo oder
  Push-Rechte bekommt, und ohne den Arbeitsbaum des Nutzers ungefragt zu verändern.
- **Entscheidung:** Der Daemon schreibt ins Projekt-Repo nur über `internal/git` und nur so:
  1. ohne Nutzeraktion ausschließlich `refs/heads/ade/<id>` (WIP-Commit im Workspace, dann
     `fetch` mit Force-Refspec – der Namensraum `ade/*` gehört ADE);
  2. „Mergen“: immer ein Merge-Commit (`--no-ff`). Konflikte werden vorher mit
     `git merge-tree --write-tree` erkannt; dann ändert sich nichts. Ist der Basis-Branch
     ausgecheckt, läuft `git merge` in diesem Arbeitsbaum (git selbst verweigert den Merge bei
     berührten lokalen Änderungen), sonst `commit-tree` + `update-ref` mit Vergleichswert;
  3. „PR erstellen“: `git push origin ade/<id>` und `gh pr create` mit den Zugangsdaten des
     Nutzers auf dem Host;
  4. „Verwerfen“: `git branch -D ade/<id>` (nicht, wenn der Branch ausgecheckt ist).
  Befehle im Workspace laufen mit `core.hooksPath=/dev/null` und `core.fsmonitor=false`, Commits
  ohne Signatur. Fehlt eine Git-Identität, committet ADE als `ADE <ade@localhost>`.
- **Konsequenzen:** Git ≥ 2.38 ist Voraussetzung. Merge-Commits im Haupt-Repo durchlaufen die
  Hooks und die Signatur-Einstellung des Nutzers wie ein normales `git merge`. Ein Agent kann in
  der Repo-Konfiguration des Workspaces weitere Befehle hinterlegen (z. B. Clean-Filter), die beim
  WIP-Commit des Daemons laufen; bis zur Sandbox (M5) laufen Agenten ohnehin mit Nutzerrechten,
  danach muss der Daemon diese Konfiguration ignorieren oder im Container committen (#41).
  Der Monaco-Editor wird lokal gebündelt (kein CDN, Dashboard-CSP nur `'self'`) und als eigener
  Chunk erst auf der Workspace-Seite geladen.


## ADR-014: Docker-Sandbox über die Docker-CLI, git des Daemons im Container

- **Kontext:** M5 isoliert Agent, Setup und IDE in einem Container pro Workspace. Dafür braucht
  ADE Container-Verwaltung, `docker exec` mit PTY und Signale an Prozesse im Container. Außerdem
  führt der Daemon git-Befehle im Workspace aus, dessen Repo-Konfiguration dem Agenten gehört (#41).
- **Entscheidung:**
  1. ADE ruft die **Docker-CLI** auf (wie das git-Binary), nicht den Go-Client. `docker exec -it`
     läuft im selben PTY-Code wie lokale Prozesse, Größenänderungen reicht die CLI weiter, und es
     kommt keine große Abhängigkeit hinzu.
  2. Container `ade-ws-<id>`: einziger Mount ist der Workspace als `/workspace`, User = UID/GID
     des Nutzers, `--cap-drop ALL`, `no-new-privileges`, `--init`, Grenzen aus `.ade/sandbox.yaml`
     (gelesen aus dem Projekt-Repo), Hauptprozess `sleep infinity`. Labels `ade.managed`,
     `ade.workspace`, `ade.data_dir`; über `ade.data_dir` trennen sich mehrere ADE-Instanzen.
  3. Da die CLI Signale nicht an `docker exec` weiterreicht, beendet ADE Prozesse im Container
     über ihre Umgebung (`ADE_SESSION_ID=<id>` bzw. `ADE_IDE_OWNER`): ein kleines Skript im
     Container signalisiert alle Prozesse mit dieser Variable – wie eine Prozessgruppe.
  4. Umgebungsvariablen gehen ohne Wert an `docker exec --env KEY` und mit Wert in die Umgebung
     der CLI, damit z. B. API-Keys nicht in der Prozessliste stehen.
  5. Sandbox ist eine Eigenschaft des Workspaces (festgelegt beim Anlegen). Sandbox-Workspaces
     klonen ohne Hardlinks: Geteilte Inodes würden dem Container erlauben, Objektdateien des
     Haupt-Repos zu überschreiben.
  6. **#41:** Bei Sandbox-Workspaces laufen alle git-Befehle des Daemons im Workspace (WIP-Commit,
     Diff, Datei-Inhalte, temporärer Index) per `docker exec` im Container (`git.Worktree` mit
     Runner). Filter, fsmonitor oder Hooks aus der Workspace-Konfiguration wirken damit nur in der
     Sandbox. Das `fetch` ins Haupt-Repo läuft weiter auf dem Host; `upload-pack` führt keine
     Befehle aus der Repo-Konfiguration aus.
  7. Beim Daemon-Start werden verwaiste Container entfernt und die übrigen gestoppt (beendet
     Prozesse des letzten Laufs); sie starten bei Bedarf wieder. Beim Beenden stoppt der Daemon
     seine Container. `ade gc` entfernt verwaiste Container auch ohne laufenden Daemon.
  8. Die IDE läuft im Container auf Port 3000, Docker veröffentlicht ihn nur auf `127.0.0.1`.
- **Konsequenzen:** Jede git-Abfrage eines Sandbox-Workspaces kostet einige `docker exec`
  (~50–100 ms). Die Workspace-Liste startet keine Container und zeigt für gestoppte keine
  Änderungszahlen. Der Container hat bis M6 normales Netz; ein API-Key aus der Konfiguration ist
  im Container lesbar. Rootless Docker und abweichende UIDs funktionieren, die Datei-Eigentümer im
  Workspace stimmen aber nur bei klassischem Docker mit dem Nutzer überein.

## ADR-015: Egress-Proxy als eigener Container, Secret-Injection über die Steuer-API

- **Kontext:** M6 beschränkt das Netz der Sandbox auf freigegebene Ziele und hält API-Keys aus dem
  Container heraus (doc/sandbox.md). Docker veröffentlicht für Container in `--internal`-Netzen
  keine Ports, und der Daemon lauscht nur auf `127.0.0.1` des Hosts.
- **Entscheidung:**
  1. Pro Workspace ein internes Netz `ade-ws-<id>` und ein Proxy-Container `ade-proxy-<id>` im
     gemeinsamen Netz `ade-egress` (ohne Verkehr zwischen Containern) und im internen Netz
     (Alias `proxy`). Erzwungen wird die Isolation durch das Netz; `HTTP(S)_PROXY` ist Bequemlichkeit.
  2. Der Proxy ist `ade proxy` aus demselben Binary: Der Daemon kopiert sein (statisches)
     Binary nach `<data_dir>/sandbox/ade-proxy` und mountet es nur lesend ins Standard-Image.
     Proxy und Daemon haben so immer dieselbe Version, ohne eigenes Image.
  3. Der Daemon steuert den Proxy über eine HTTP-Steuer-API auf einem veröffentlichten Port
     (`127.0.0.1`), geschützt durch ein zufälliges Token je Container (über die Umgebung der
     Docker-CLI übergeben, nicht in der Prozessliste). Policy und Keys gehen nur darüber; der
     Proxy hält sie im Speicher. Events holt der Daemon per Long-Polling ab.
  4. Die IDE erreicht der Daemon über eine TCP-Weiterleitung des Proxys an `agent:3000`.
  5. Der Proxy verbindet nie zu internen Adressen; geprüft wird die IP beim `connect`.
  6. Externe DNS-Auflösung im Agent-Container ist zusätzlich per `--dns 192.0.2.1` abgeschaltet.
  7. Container aus M5 (ohne Label `ade.net`) werden beim nächsten Start neu angelegt.
  8. Bei ungültiger `sandbox.yaml` gilt nicht die Voreinstellung: laufende Proxys behalten die
     letzte gültige Policy, neue Container starten nicht.
- **Konsequenzen:** Ein zusätzlicher Container (~10 MB RAM) pro laufendem Workspace. Der Daemon
  muss ohne cgo gebaut sein (`make build`, `make dev`); ein dynamisch gelinktes Binary meldet er
  beim Start. Ohne TLS-Interception gilt die Allowlist nur auf Host-Ebene. Hängen andere
  Programme als Agent-Sessions (IDE-Terminal) an der Anthropic-API, nutzen sie die eigene
  Anmeldung der CLI über die Allowlist.
