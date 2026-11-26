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
