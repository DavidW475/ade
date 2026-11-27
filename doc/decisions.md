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

## ADR-016: Freigaben über einen PreToolUse-Hook, in der Sandbox über den Egress-Proxy

- **Kontext:** M7 lässt einen Menschen über kritische Werkzeug-Aufrufe und Netzwerk-Ausnahmen
  entscheiden, asynchron im Dashboard. Claude Code läuft im Print-Modus (ADR-011) und lehnt
  Werkzeuge, die eine Rückfrage bräuchten, ohne Hook ab. Der Proxy-Container erreicht den Daemon
  nicht (er lauscht nur auf `127.0.0.1` des Hosts), der Daemon erreicht den Proxy aber über die
  Steuer-API (ADR-015). Das Haupt-Repo eines Projekts ändert der Daemon nur bei der Übernahme.
- **Entscheidung:**
  1. Der Claude-Adapter bindet `ade hook pretooluse` per `--settings` als `PreToolUse`-Hook für
     alle Werkzeuge ein. Der Hook antwortet immer mit `allow` oder `deny`, bei jedem Fehler `deny`
     (fail closed). Sein Timeout ist `approvals.timeout` plus Spielraum.
  2. Der Daemon entscheidet: zuerst die Regeln aus `approvals` in `.ade/project.yaml`
     (deny → ask → allow, getrennt für Sandbox und lokal, Voreinstellung: Sandbox alles, lokal
     Lesen und Dateiänderungen im Workspace), sonst eine Freigabe in der Warteschlange, die bis
     zur Entscheidung oder zum Timeout blockiert. Bash-Allow-Regeln passen nie auf
     zusammengesetzte Befehle; Pfadmuster gelten relativ zum Workspace, lokal nach Auflösen von
     Symlinks. Bei ungültiger `project.yaml` erlaubt keine Regel etwas.
  3. Lokal ruft der Hook `POST /api/hook/pretooluse` mit einem Token je Session auf (nur für
     Fragen dieser Session gültig, endet mit dem Prozess).
  4. In der Sandbox wird das ade-Binary nur lesend nach `/opt/ade/ade` gemountet; der Hook fragt
     `http://proxy:8080/ade/hook/pretooluse`. Der Proxy hält die Frage fest, der Daemon holt
     offene Fragen per Long-Polling über die Steuer-API ab (`GET /questions`) und beantwortet sie
     (`POST /questions/{id}`). Dieselbe Warteschlange trägt Egress-Fragen bei
     `egress.on_blocked: ask`: Die Anfrage wartet (Standard 2 min), gleiche Ziele teilen sich eine
     Frage, nach „Ablehnen“ fragt der Proxy bis zu seinem Neustart nicht erneut.
  5. Egress-Freigaben „für diesen Workspace“ und „immer (Projekt)“ speichert der Daemon als
     Ausnahmen in SQLite und fügt sie der Policy hinzu, statt `.ade/sandbox.yaml` im Haupt-Repo
     zu ändern. Platzhalter gehören weiter in die Datei.
  6. Offene Freigaben eines früheren Daemon-Laufs gelten beim Start als abgelaufen.
- **Konsequenzen:** Jeder Werkzeug-Aufruf kostet einen Prozessstart und eine lokale HTTP-Anfrage.
  Container ohne Hook-Binary werden einmal neu angelegt (`ade.net=proxy-2`); dabei geht der
  Zustand im Container-Dateisystem verloren (z. B. ein Abo-Login unter `/home/ade`). Der
  Berechtigungsmodus der CLI gilt nur noch, wenn kein Hook verfügbar ist. Terminal-Sessions
  (`pty`) haben keinen Hook. Ausnahmen aus dem Dashboard stehen nicht im Repo; die Netzwerk-
  Ansicht zeigt sie an und kann sie entfernen.

## ADR-017: Kosten aus einer Quelle je Session, Budgets über Pause und Freigabe

- **Kontext:** M8 soll Kosten pro Session zeigen und Budgets durchsetzen, auch für CLIs, die
  keine USD melden (Codex). Der Egress-Proxy sieht in der Sandbox jede API-Antwort, wenn er den
  Key einfügt (ADR-007); Claude Code meldet seine Kosten selbst am Ende jedes Durchgangs. Beide
  zusammen würden doppelt zählen. Codex beachtet `OPENAI_BASE_URL` nicht, und `codex exec`
  bearbeitet eine Anweisung je Prozess mit kumulierten Token-Zahlen pro Unterhaltung.
- **Entscheidung:**
  1. Jede Nutzung ist ein Eintrag in `usage` (am Projekt, nicht am Workspace, damit Monatsbudgets
     verworfene Workspaces mitzählen) und wird zur Session addiert. Je Session gilt eine Quelle:
     Misst der Proxy (Sandbox mit Secret-Injection für den Anbieter des Adapters), zählt nur er,
     sonst die Meldung der CLI. Proxy-Nutzung ohne messende Session zählt nur für den Workspace.
  2. Der Proxy liest die Antworten von `/p/<anbieter>/` beim Durchreichen mit (JSON bis 16 MiB,
     SSE zeilenweise) und fordert sie dafür unkomprimiert an. Er meldet nur Tokens und Modell;
     USD rechnet der Daemon mit einer eingebauten, überschreibbaren Preistabelle (Präfix der
     Modell-ID, längster Treffer).
  3. Codex bekommt die Proxy-Basis-URL als eigenen Provider per `-c`; seine kumulierten Zahlen
     verrechnet der Parser mit den bis zum Prozessstart gezählten Tokens der Session.
  4. Budgets (Session, Kalendermonat des Projekts) prüft der Daemon nach jeder Nutzung. Bei
     Überschreitung beendet er den Prozess (Status `paused`) und legt eine Freigabe der Art
     `budget` an (24 h). Erlauben erhöht das Budget um einen Schritt und setzt die Session mit
     Resume fort; Ablehnen oder Ablauf beendet sie (`stopped`).
- **Konsequenzen:** Kosten aus der Preistabelle sind Schätzwerte; unbekannte Modelle zählen nur
  Tokens und sind markiert. Ein Budget greift erst nach der Antwort, die es überschreitet (eine
  laufende Anfrage wird nicht abgebrochen); ohne Proxy erst am Ende eines Durchgangs. Was am Proxy
  vorbei läuft (Abo-Login über CONNECT), kann er nicht zählen. Pausieren beendet den Prozess;
  fortsetzen können nur Adapter mit Resume.

## ADR-018: Fabrik-Runs aus normalen Sessions, Artefakte außerhalb des Workspaces, Lese-Rollen über eigenen Container

- **Kontext:** M9 verkettet Rollen-Agenten zu einem Run (doc/factory.md). Jede Rolle soll eine
  normale Session bleiben (Timeline, Freigaben, Kosten, Sandbox), der Run einen Neustart des
  Daemons überstehen, und Lese-Rollen dürfen den gemeinsamen Workspace nicht ändern. Ein
  laufender Container lässt sich nicht nachträglich anders mounten; `docker exec` kann keine
  Mounts hinzufügen.
- **Entscheidung:**
  1. Der Runner (`internal/factory`) hält den Zustand in `runs` und `run_steps` und schreibt ihn
     unter einem Lock je Run fort. Auslöser sind Statuswechsel der Sessions (Listener am
     Agent-Manager), Entscheidungen an Gates, ein Takt und der Start. Die Definition wird beim
     Start als JSON gespeichert.
  2. Eine Rolle ist fertig, wenn ihre Session `idle` oder `done` wird. Ihre letzte Antwort ist
     das Artefakt; ADE speichert es unter `<data_dir>/runs/<id>/` (nicht `.ade/runs/` im
     Workspace wie zuvor geplant) und gibt es den folgenden Rollen im Prompt mit. Ein wartender
     Agent wird beendet, sobald der Schritt fertig ist; der Workspace wird dabei gesichert.
  3. Gates sind Freigaben der Art `gate` (7 Tage). Nach einem Neustart legt der Runner offene
     Gates neu an und setzt unterbrochene Sessions über Resume fort.
  4. Lese-Rollen laufen in der Sandbox in einem zweiten Container je Workspace
     (`ade-ws-<id>-ro`, Rolle `reader`) mit `/workspace` als Read-only-Bind-Mount, im selben
     internen Netz hinter demselben Proxy. Ohne Sandbox arbeiten sie in einem Schnappschuss
     (`git clone --shared` des Workspaces) unter `<data_dir>/snapshots/<session>`; die Adapter
     sperren zusätzlich ihre Schreibwerkzeuge.
  5. Sandbox gilt für den ganzen Run, nicht je Rolle (ein Workspace).
- **Konsequenzen:** Artefakte sind in M9 Freitext; mehrere oder strukturierte Artefakte je Rolle
  kommen mit M10. Ohne Sandbox ist „nur lesen“ eine Absprache, kein Schutz: Ein Agent mit
  Bash-Zugriff kann außerhalb des Schnappschusses schreiben (wie jeder lokale Agent). Der
  Lese-Container verdoppelt den Speicherbedarf eines Workspaces, solange eine Lese-Rolle läuft;
  er bleibt bis zum Stoppen oder Verwerfen des Workspaces bestehen. Ein Zeitlimit lässt den Run
  scheitern, statt nach einer Freigabe zu fragen.

## ADR-019: Schleifen als neue Durchläufe, eigene Ausdruckssprache und Schema-Teilmenge

- **Kontext:** M10 bringt strukturierte Artefakte, Bedingungen und Rücksprünge in Fabriken
  (doc/factory.md). Frühere Durchläufe sollen nachvollziehbar bleiben, Abhängigkeiten sparsam
  sein, und eine Bedingung aus dem Repo darf nichts anderes können als Werte vergleichen.
- **Entscheidung:**
  1. Jede Ausführung eines Schritts ist eine eigene Zeile in `run_steps` (`pos` = Position im
     Ablauf, `iteration` = Durchlauf). Ein Rücksprung legt für die Positionen vom Ziel bis zur
     springenden Stelle neue Zeilen an. Die API liefert je Position die jüngste Ausführung
     (`steps`, `steps[current]` bleibt der aktuelle Schritt) und die älteren unter `history`.
  2. Artefakte liegen als jüngster Stand unter `<data_dir>/runs/<id>/<name>` und je Durchlauf
     unter `iterations/<n>/<name>`.
  3. Bedingungen sind eine eigene kleine Sprache (Pfade, Literale, Vergleiche, `&& || !`,
     `len`), kein Skript und keine Bibliothek wie CEL oder expr: kein Zugriff auf anderes als
     die Artefakte, keine Schleifen, keine Funktionen außer `len`.
  4. Schemas sind eine geprüfte Teilmenge von JSON Schema ohne `$ref`, `oneOf`, `pattern` usw.;
     unbekannte Schlüsselwörter sind ein Fehler beim Laden statt still ignoriert.
  5. Ungültige strukturierte Antworten gehen als Folgeanweisung an dieselbe Session zurück
     (höchstens zwei Korrekturen), statt die Rolle neu zu starten.
- **Konsequenzen:** `max_iterations` zählt die Durchläufe eines Schritts, nicht einer
  bestimmten Schleife; verschachtelte Schleifen teilen sich die Zählung. Wer reguläre
  Ausdrücke oder Referenzen in Schemas braucht, muss bis zu einer Erweiterung darauf
  verzichten. Die Zahl der Zeilen je Run wächst mit den Durchläufen (höchstens
  `max_iterations` × Schritte).

## ADR-020: Auslöser per Polling über gh, Zustand in SQLite, Sandbox und Budget als Standard

- **Kontext:** M10 soll Runs aus GitHub-Issues mit Label und nach Zeitplan starten. Der Daemon
  lauscht nur auf `127.0.0.1` und ist von GitHub aus nicht erreichbar. Issue-Texte sind fremde
  Eingabe.
- **Entscheidung:**
  1. GitHub-Issues werden über `gh issue list` im Haupt-Repo abgefragt (Standard alle 5 Minuten),
     nicht per Webhook. Anmeldung und Repo kommen von `gh` und dem Remote, ADE speichert kein
     Token.
  2. Die Auslöser stehen in `.ade/project.yaml`; ihr Zustand (pausiert, letzte Abfrage bzw.
     letzter Termin, letzter Fehler) und die ausgelösten Runs (`trigger_runs`, Schlüssel
     Issue-Nummer bzw. Termin) in SQLite. Ein Issue startet nie zwei Runs.
  3. Zeitpläne nutzen einen eigenen Cron-Parser (`internal/cron`, 5 Felder und Kurzformen),
     keine Bibliothek. Nach einer Pause wird höchstens der letzte Termin nachgeholt.
  4. Ausgelöste Runs laufen standardmäßig in der Sandbox und nur mit `budget.max_cost_usd` in der
     Fabrik; beides lässt sich je Auslöser ausdrücklich abschalten (`sandbox: false`,
     `allow_unbudgeted: true`).
- **Konsequenzen:** Ein Run startet bis zu einem Intervall nach dem Labeln. Ohne `gh auth login`
  steht der Fehler am Auslöser im Dashboard. `authors` prüft den Verfasser des Issues, nicht wer
  das Label gesetzt hat (das liefert `gh issue list` nicht); in öffentlichen Repos ist `authors`
  deshalb dringend empfohlen.

## ADR-021: Hintergrunddienst als systemd-User-Unit mit übernommenem PATH

- **Kontext:** Unter WSL mit systemd soll der Daemon ohne `ade start` laufen. User-Dienste erben
  die Umgebung der Shell nicht; git, docker, gh, claude und codex liegen oft in `~/.local/bin`,
  `~/.npm-global/bin` o. ä. Ein Daemon außerhalb von systemd hält den Lock des Datenverzeichnisses.
- **Entscheidung:**
  1. `ade install-service` schreibt `~/.config/systemd/user/ade.service` mit absolutem Binary-Pfad,
     fester Konfiguration und dem aktuellen `PATH` (`Environment=`). Weitere Variablen, insbesondere
     Secrets, kommen nicht in die Unit; dafür gibt es die Konfiguration.
  2. stdout/stderr gehen per `StandardOutput=append:` ins normale Daemon-Log, damit Fehlersuche
     und Meldungen von `ade start` gleich bleiben (systemd ≥ 240).
  3. `Restart=on-failure`; Lingering setzt ADE nicht selbst (braucht root), es weist nur darauf hin.
  4. Läuft schon ein Daemon außerhalb von systemd, bricht `install-service` mit PID ab, statt den
     Dienst in eine Neustart-Schleife zu schicken.
  5. `ade start` startet den Dienst über `systemctl --user start`, wenn die Unit für dieselbe
     Konfiguration installiert ist (Kommentarzeile `# ade-config:` in der Unit), sonst wie bisher
     einen eigenen Hintergrundprozess.
- **Konsequenzen:** Nach neuem Binary-Pfad oder geändertem `PATH` muss `install-service` erneut
  laufen (es überschreibt die Unit und startet neu). Ein SSH-Agent der Shell steht dem Dienst
  nicht zur Verfügung; Pushes brauchen Zugangsdaten, die ohne Agent funktionieren (z. B. `gh auth
  setup-git`).

## ADR-022: Binary und Images im selben Release-Lauf, gleiche Version, Attestierung

- **Kontext:** Das Binary nutzt standardmäßig `ade-agent-base:<version>`. Der Image-Workflow lief
  nur bei Änderungen unter `images/` (der `paths`-Filter gilt auch für Tags), ein Release ohne
  Image-Änderung hätte also ein Image-Tag referenziert, das es nicht gibt. GoReleaser schrieb die
  Version zudem ohne `v` ins Binary, `DefaultImage` erkannte sie nicht und fiel auf `:latest` zurück.
- **Entscheidung:**
  1. `release.yml` ruft `images.yml` als wiederverwendbaren Workflow (`workflow_call`) parallel zu
     GoReleaser auf. Das Release wartet nicht auf die Images; ein Fehlschlag macht den Lauf rot und
     lässt sich einzeln wiederholen.
  2. Image-Tag `X.Y.Z` bzw. `X.Y.Z-rc.N` ohne `v`, Binary-Version mit `v` (wie `git describe`).
     `DefaultImage` bildet `vX.Y.Z[-alpha|beta|rc.N]` auf dieses Tag ab; alles andere (Entwicklung,
     Snapshots) nutzt `:latest`.
  3. Binaries: SPDX-SBOM je Archiv (syft) und Build-Provenance über `checksums.txt`. Images:
     BuildKit-SBOM und -Provenance plus GitHub-Attestierung in der Registry. Prüfen mit
     `gh attestation verify`.
  4. Ein Snapshot-Build mit `install.sh` gegen das lokale `dist/` läuft in PRs, die die
     Release-Konfiguration ändern.
- **Konsequenzen:** Ein Release kann kurz ohne zugehöriges Image veröffentlicht sein, bis der
  Image-Job fertig ist. Signaturen mit eigenem Schlüssel (cosign) gibt es nicht; die Attestierung
  hängt an GitHubs Sigstore-Instanz und der Identität des Workflows.

## ADR-023: Rootless Docker und Podman hinter derselben Docker-CLI

- **Kontext:** Die Sandbox ruft die Docker-CLI auf (ADR-014) und startet Prozesse mit der UID des
  Nutzers. Unter rootless Docker landet diese UID auf dem Host im subuid-Bereich: Dateien, die der
  Agent anlegt, gehören einer fremden UID, und das nur für den Nutzer lesbare Proxy-Binary ist im
  Container nicht ausführbar. Podman versteht fast dieselbe CLI, aber nicht jede Docker-Option.
  Ressourcen-Grenzen verwirft Docker ohne delegierte cgroup-Controller teils nur mit Warnung (#94).
- **Entscheidung:**
  1. Kein eigenes Backend je Laufzeit: ADE bleibt bei der Docker-CLI-Syntax; `sandbox.docker` darf
     auch `podman` sein. Die Laufzeit erkennt ADE aus `info --format '{{json .}}'` (Podman-JSON hat
     `host`, rootless Docker meldet `name=rootless` in `SecurityOptions`).
  2. UID-Abbildung je Laufzeit für Agent-, Lese- und Proxy-Container und `docker exec`: rootful
     unverändert `--user UID:GID`; rootless Docker `--user 0:0` (root im User-Namespace = Nutzer
     auf dem Host, ohne Capabilities); rootless Podman `--user UID:GID --userns keep-id`.
  3. Unter rootless Docker setzt ADE `IS_SANDBOX=1`, damit Claude Code `bypassPermissions` als root
     zulässt – der Container ist die Sandbox.
  4. Fehlt ein cgroup-Controller (`memory`, `cpu`, `pids`), bricht `Ensure` vor dem Anlegen eines
     Containers mit klarer Meldung ab. Grenzen stillschweigend wegzulassen wäre eine schwächere
     Sandbox, als die Konfiguration verspricht.
  5. Docker-spezifische Optionen nur, wo sie gelten: `enable_icc=false` für `ade-egress` nur unter
     Docker. Netze liest ADE per `network inspect` (JSON), weil sich die `ls`-Formate unterscheiden.
  6. Ein CI-Job führt die Sandbox-Integrationstests zusätzlich unter rootless Docker und rootless
     Podman aus (im systemd-User-Manager des Runners, wie auf einem Desktop).
- **Konsequenzen:** Unter Podman können sich die Proxys in `ade-egress` gegenseitig erreichen; ein
  Agent erreicht trotzdem nur seinen eigenen Proxy, der interne Ziele ablehnt. Unter rootless
  Docker laufen Agent-CLIs als root im Container. Bestehende Container müssen nicht neu angelegt
  werden: `docker exec --user` folgt der erkannten Laufzeit.
