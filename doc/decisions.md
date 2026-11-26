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
