# Threat-Model

Stand: M11 (vor 1.0). Ergänzt [architecture.md § 7](architecture.md#7-sicherheit) und
[sandbox.md](sandbox.md). Jeder Angriffsweg hat eine Gegenmaßnahme oder ein benanntes Restrisiko.
Das Review der sicherheitskritischen Teile (Issue #95) steht in [Abschnitt 6](#6-review-m11).

## 1. Was geschützt wird

| Wert | Wo | Warum wertvoll |
|---|---|---|
| **Host** | WSL-Distribution des Nutzers, Windows dahinter | Codeausführung auf dem Host = Zugriff auf alles Folgende |
| **Code** | Haupt-Repos der Projekte, Branches, Remotes | Manipulierte Commits landen beim Merge oder Push im echten Projekt |
| **Zugangsdaten** | API-Keys (`providers.*.api_key`), `gh`-Login, SSH-Keys, Git-Credentials, Abo-Logins der Agent-CLIs | Missbrauch, Kosten, Zugriff auf weitere Repos und CI-Secrets |
| **Daemon-Zugang** | `daemon.secret`, Browser-Session (`ade_session`), IDE-Sessions (`ade_ide`), Proxy-Token, Hook-Token | Wer den Daemon steuert, startet Agenten und mergt |
| **Daten des Daemons** | `ade.db` (Aufgaben, Ausgaben, Egress-Log), Logs, Artefakte | Enthalten Repo-Inhalte und Prompts |
| **Budget** | Kosten der Anbieter-APIs | Endlosschleifen oder Missbrauch verursachen echte Kosten |

## 2. Angreifer

| Angreifer | Fähigkeit | Annahme |
|---|---|---|
| **A1 – Bösartiges Repo oder Issue** | Kontrolliert Dateien im Repo, Issue-Texte, Abhängigkeiten, die das Setup lädt | Kann den Agenten über Prompt-Injection steuern |
| **A2 – Prompt-injizierter oder fehlerhafter Agent** | Führt beliebigen Code dort aus, wo der Agent läuft: in der Sandbox (Container) oder, ohne Sandbox, auf dem Host | Ziel: aus der Sandbox ausbrechen, Secrets holen, Daten herausschleusen, Code ins Haupt-Repo schmuggeln |
| **A3 – Fremde Webseite im Browser** | JavaScript im Windows-Browser des Nutzers, kann `localhost` ansprechen, DNS-Rebinding | Kennt kein Secret und kein Cookie |
| **A4 – Anderer lokaler Benutzer** | Eigenes Konto in derselben WSL-Distribution, erreicht `127.0.0.1` | Kein root, nicht in der `docker`-Gruppe |
| **A5 – Netzwerk außerhalb des Rechners** | Erreicht Ports des Rechners im LAN | – |

**Nicht im Modell:** root auf dem Host oder Mitglieder der `docker`-Gruppe (rootful Docker ist
root-gleich), ein kompromittierter Windows-Host, kompromittierte Anbieter-APIs, GitHub oder
Image-Registries, Angriffe auf den Kernel oder die Container-Laufzeit selbst (Zero-Days).

## 3. Vertrauensgrenzen

```
 Windows-Browser ──(1)── Daemon 127.0.0.1:7000 ──(4)── Haupt-Repo, gh, Secrets
   │                        │      │
   │ ws-<id>.localhost      │(2)   │(3) Docker-CLI
   └──── Gateway ───────────┘      ▼
                         ┌─ Agent-Container ──(5)── Proxy-Container ──(6)── Internet
                         │   /workspace (bind)        Allowlist, Secret-Injection
                         └─ Lese-Container (read-only)
```

1. **Browser → Daemon:** Host-/Origin-Prüfung, Session-Cookie, Tickets.
2. **IDE-Subdomain → Gateway:** eigene Session je Workspace; der Inhalt der IDE gilt als nicht
   vertrauenswürdig (er stammt aus dem Workspace).
3. **Daemon → Container:** Der Daemon steuert, der Container liefert nur Daten (Events, Dateien,
   git-Ausgaben). Was aus dem Workspace kommt, ist Eingabe von A2.
4. **Workspace → Haupt-Repo:** Nur der Daemon überträgt, und nur auf Aktion des Nutzers oder eines
   Fabrik-Schritts (Fetch nach `ade/<id>`, Merge, Push; ADR-005, ADR-013).
5. **Agent → Proxy:** einziges Tor nach außen; Steuer-API nur mit Token.
6. **Proxy → Internet:** Allowlist, keine internen Ziele.

## 4. Angriffswege

### 4.1 Browser (A3)

| Angriff | Gegenmaßnahme | Restrisiko |
|---|---|---|
| CSRF auf die API | `Sec-Fetch-Site: cross-site` und fremder `Origin` werden bei allen nicht sicheren Methoden und WebSocket-Upgrades abgelehnt; Cookie `SameSite=Strict`; GET-Routen ändern nichts | – |
| DNS-Rebinding (fremde Domain → 127.0.0.1) | `Host` muss `localhost`, `127.0.0.1`, `::1` oder `*.localhost` mit dem Daemon-Port sein | – |
| Cross-Site-WebSocket-Hijacking (`/ws/…`) | Origin-Prüfung auch bei GET mit `Upgrade: websocket` | – |
| Lesen von API-Antworten | Kein CORS; `no-store` auf `/api/` und `/ws/` | – |
| Clickjacking, XSS im Dashboard | `X-Frame-Options: DENY`, CSP `default-src 'self'` ohne `unsafe-eval`/Inline-Skripte, `nosniff`; React escapt Ausgaben, Diffs laufen durch Monaco | `style-src 'unsafe-inline'` (für Monaco) |
| Offener Redirect nach der Anmeldung | `next` nur als lokaler Pfad (`safeNext`) | – |
| Ticket aus Verlauf oder Referrer | Einmalig, 2 min gültig, Redirect entfernt es aus der Adresszeile, `Referrer-Policy: no-referrer` | – |
| Login-CSRF | Tickets gibt es nur mit dem Daemon-Secret | – |

### 4.2 IDE und Gateway (A2 über die IDE, A3)

| Angriff | Gegenmaßnahme | Restrisiko |
|---|---|---|
| IDE-Inhalt (Erweiterungen, Webviews, Server im Container) ruft die Dashboard-API auf | Eigene Origin `ws-<id>.localhost`; Dashboard-Cookie ist host-only für `localhost` und wird nicht mitgeschickt; Origin-Prüfung lehnt Subdomains für das Dashboard ab | – |
| Eine IDE greift auf eine andere zu | Cookie `ade_ide` host-only je Subdomain und an genau einen Workspace gebunden; Origin muss die eigene Subdomain sein | – |
| Connection-Token der IDE stehlen | Das Gateway setzt den Token selbst ein und entfernt `vscode-tkn` und `ade_ide` aus der Anfrage; der Browser sieht ihn nie | Code im Container kann den Token lesen (er gehört dort ohnehin dem Agenten) |
| Cookie-Tossing (`Set-Cookie; Domain=localhost` aus der IDE) | Browser lehnen `Domain=localhost` als Public Suffix ab; ein untergeschobenes Cookie wäre nur eine ungültige Session | Abmeldung (Denial of Service) |
| IDE-Session überlebt Abmeldung | IDE-Sessions hängen an der Dashboard-Session und enden mit ihr | – |

### 4.3 Sandbox: Ausbruch und Host-Zugriff (A2)

| Angriff | Gegenmaßnahme | Restrisiko |
|---|---|---|
| Rechte im Container ausweiten | `--cap-drop ALL`, `no-new-privileges`, kein privilegierter Modus, kein Docker-Socket, Non-Root (rootful/Podman) bzw. root ohne Capabilities im User-Namespace (rootless Docker, ADR-023) | Kernel- oder Laufzeit-Lücken |
| Host-Dateien lesen oder schreiben | Nur der Workspace ist gemountet (`/workspace`), das Haupt-Repo nie; `checkMountPath` lehnt ungeeignete Pfade ab | – |
| Ressourcen erschöpfen (Fork-Bombe, Speicher) | `--memory`, `--cpus`, `--pids-limit` (Standard 4g/2/512; ohne delegierte cgroup-Controller startet die Sandbox nicht) | Plattenplatz im Workspace ist unbegrenzt |
| git-Konfiguration des Workspaces (`core.fsmonitor`, Filter, Hooks) führt Code beim Daemon aus | git-Befehle in Sandbox-Workspaces laufen im Container (`git.Worktree` mit Runner, #41); immer `core.hooksPath=/dev/null`, `core.fsmonitor=false` | – |
| Fetch aus dem Workspace ins Haupt-Repo | Läuft auf dem Host, startet `upload-pack` im Workspace; git beachtet `uploadpack.packObjectsHook` nur aus geschützter Konfiguration, nicht aus der des Repos; `--no-recurse-submodules`, `--no-tags` | Mit `objects/info/alternates` oder `.git`-Datei kann der Agent Objekte anderer Repos auf dem Host in `ade/<id>` holen; sie erscheinen im Diff vor dem Merge |
| Symlinks im Workspace zeigen auf Host-Dateien | Diff und Dateiansicht lesen über git (Symlink = Linkziel als Text) | – |
| Lese-Rollen schreiben doch | Lese-Container mountet den Workspace `readonly` | – |
| Dateien mit fremdem Eigentümer im Workspace | UID-Abbildung je Laufzeit (ADR-023), getestet unter allen drei Laufzeiten | – |
| Proxy-Binary manipulieren | Nur lesend gemountet, Proxy-Container `--read-only`, gehört nicht zum Workspace | – |

### 4.4 Netzwerk und Secrets (A2)

| Angriff | Gegenmaßnahme | Restrisiko |
|---|---|---|
| Direkter Internetzugang | Agent hängt nur im internen Netz (`--internal`), kein DNS (`--dns` auf eine Sackgasse); einziger Weg ist der Proxy | – |
| Interne Ziele (Host, andere Container, LAN, Cloud-Metadaten) über den Proxy | Sperrliste aller privaten, Loopback-, Link-Local-, Multicast- und Tunnel-Netze (IPv4, IPv6, IPv4-mapped, IPv4-kompatibel, 6to4, Teredo, NAT64); geprüft wird die tatsächlich verbundene IP im `Control` des Dialers, also nach der Namensauflösung (DNS-Rebinding) | Öffentliche IPs des Hosts selbst (selten unter WSL) |
| Exfiltration über erlaubte Hosts | Allowlist so eng wie möglich; Egress-Log mit Bytes je Ziel im Dashboard; `on_blocked: ask` | Daten können über jedes erlaubte Ziel abfließen (z. B. ein Gist über `api.github.com`); ohne TLS-Interception sieht der Proxy nur Host und Port |
| API-Keys auslesen | Keys liegen nur im Proxy; der Agent schickt einen Platzhalter an `/p/<anbieter>/`, der Proxy ersetzt `Authorization`/`X-Api-Key` und schickt den Key nur an den festen Upstream des Anbieters; Klartext-HTTP und CONNECT bekommen nie einen Key | Der Agent kann den Key über den Proxy *benutzen* (begrenzt durch Budgets) |
| Steuer-API des Proxys (Policy, Secrets, Antworten auf Fragen) | Bearer-Token (konstanter Vergleich), nur der Daemon kennt ihn; ohne Token gesperrt; Port nur auf `127.0.0.1` veröffentlicht | – |
| Proxys anderer Workspaces | Docker: `enable_icc=false` im Egress-Netz; Agenten erreichen nur ihr eigenes internes Netz | Podman: Proxys erreichen sich gegenseitig (Steuer-API trotzdem per Token geschützt, doc/sandbox.md § 6) |
| Freigaben fälschen (Hook, Egress-Fragen) | Hook-Token je Session; Antworten nur über die authentifizierte Daemon-API | Viele Fragen nerven (Denial of Service) |
| Abo-Logins der CLIs | Ohne Sandbox liegen sie auf dem Host; in der Sandbox nur, was ausdrücklich hineingereicht wird (#51) | – |

### 4.5 Code ins Projekt schmuggeln (A1, A2)

| Angriff | Gegenmaßnahme | Restrisiko |
|---|---|---|
| Manipulation des Haupt-Repos (Hooks, Branches, Konfiguration) | Workspace ist ein eigener Clone ohne Remote zum Haupt-Repo (ADR-005); nur der Daemon überträgt | – |
| Schadcode im Ergebnis | Merge und PR nur nach Review im Dashboard (Diff zur Basis); Fabriken mit `gate: human` vor `pull_request` | Ein Review übersieht etwas |
| `.ade/*.yaml` im Ergebnis weitet Sandbox, Freigaben oder Fabriken aus | Konfiguration wird aus dem Haupt-Repo gelesen, nie aus dem Workspace; wirkt erst nach dem Merge | Unaufmerksames Review von `.ade/` |
| Workflows im PR-Branch (`.github/workflows/`) laufen mit den Secrets des Repos | PRs entstehen über einen Push in dasselbe Repo; Vorlagen haben ein Gate davor | Eine Fabrik ohne Gate vor `pull_request` pusht ungeprüften Code (#103) |
| Prompt-Injection über Issue-Texte (Auslöser) | Nur Issues mit Label (setzen nur Nutzer mit Triage-Recht); Run in der Sandbox; Gate vor dem PR | Der Issue-Text selbst stammt von beliebigen Nutzern |
| Bösartige Projektkonfiguration (`setup`, Fabriken) | Ein Projekt zu registrieren heißt, seiner `.ade/`-Konfiguration zu vertrauen; ohne Sandbox laufen Setup und Agent auf dem Host | Ohne Sandbox ist A2 = Codeausführung auf dem Host (bewusste Wahl, Hinweis im Dashboard) |
| Checkout-Filter beim Merge | Merge im Haupt-Repo nutzt die Filter aus der Konfiguration des *Nutzers* (z. B. LFS); `.gitattributes` aus dem Ergebnis kann nur diese auswählen | – |

### 4.6 Lokaler Zugriff (A4, A5)

| Angriff | Gegenmaßnahme | Restrisiko |
|---|---|---|
| Daemon aus dem LAN erreichen | Lauscht nur auf `127.0.0.1`; veröffentlichte Container-Ports nur auf `127.0.0.1` | WSL-Spiegelmodus (`networkingMode=mirrored`) ändert daran nichts, solange auf 127.0.0.1 gebunden wird |
| Daemon-Secret, Datenbank, Logs lesen | Datenverzeichnis `0700` (wird beim Start erzwungen), `daemon.secret`, Datenbank (inkl. `-wal`/`-shm`), Logs, Tokens `0600`; Workspaces-Verzeichnis `0700` | – |
| API ohne Secret ansprechen | Jede Route außer `/api/health` braucht Session oder Secret; Hook nur mit Hook-Token | – |
| Session-Cookie über einen anderen Port abgreifen | Cookies gelten in Browsern für alle Ports eines Hosts; `HttpOnly` | Ein lokaler Dienst unter `localhost:<anderer Port>`, den der Nutzer im Browser öffnet, bekommt `ade_session` mitgeschickt. Wer solche Dienste betreibt, hat meist ohnehin Zugriff auf das Konto |
| IDE- und Steuer-Ports auf 127.0.0.1 | Connection-Token (IDE) bzw. Bearer-Token (Proxy) | – |

### 4.7 Lieferkette

| Angriff | Gegenmaßnahme | Restrisiko |
|---|---|---|
| Manipuliertes openvscode-server | Download mit fester SHA-256-Prüfsumme | – |
| Manipuliertes Release oder Image | Prüfsummen, SBOM und Build-Provenance (Attestierung) für Binary und Images (#97); `install.sh` prüft die Prüfsumme | Prüfung der Attestierung bleibt dem Nutzer überlassen (`gh attestation verify`) |
| Bekannte Lücken in Abhängigkeiten | `govulncheck` und `npm audit` in der CI; wenige Abhängigkeiten, kein cgo | Neue Lücken zwischen zwei CI-Läufen |

## 5. Restrisiken im Überblick

1. **Ohne Sandbox** hat der Agent die Rechte des Nutzers. Die Sandbox ist die eigentliche Grenze.
2. **Exfiltration über erlaubte Ziele** ist ohne TLS-Interception nicht zu verhindern (Backlog).
3. **Menschliches Review** ist die letzte Linie gegen Schadcode im Ergebnis, auch in `.ade/` und
   `.github/workflows/`.
4. **Kernel- und Laufzeit-Lücken** liegen außerhalb dessen, was ADE leisten kann; rootless Docker
   oder Podman verkleinern den Schaden (ADR-023).
5. **Cookies sind nicht an Ports gebunden.** Lokale Webdienste anderer Herkunft sehen das
   Session-Cookie.
6. **Podman** isoliert die Proxy-Container untereinander nicht.

## 6. Review M11

Geprüft im Rahmen von #95. Befunde sind behoben (mit Test) oder als Issue angelegt.

**Gateway und Auth** (`internal/api`, `internal/gateway`)
- [x] Host-Prüfung vor allen Routen, Origin-Prüfung für nicht sichere Methoden und WebSocket-Upgrades
- [x] Alle Routen außer `/api/health` mit `requireAuth`/`requireSecret`; Hook mit eigenem Token
- [x] Cookies `HttpOnly`, `SameSite=Strict` (Dashboard) bzw. `Lax` (IDE, Cross-Site-Navigation), host-only, Pfad `/`
- [x] Tickets einmalig, 2 min, 256 Bit; IDE-Tickets an Workspace und Dashboard-Session gebunden
- [x] Connection-Token der IDE nur im Gateway; Secrets konstant verglichen
- [x] `ReadHeaderTimeout` gegen hängende Verbindungen

**Proxy** (`internal/proxy`)
- [x] Sperrliste inkl. IPv6, IPv4-mapped, 6to4, Teredo, NAT64; Prüfung im `Control` des Dialers
- [x] **Behoben:** IPv4-kompatible Adressen (`::/96`, z. B. `::127.0.0.1`) und Site-Local (`fec0::/10`) fehlten
- [x] CONNECT-Ziele gegen Allowlist und Sperrliste; Secret-Injection nur an den festen Upstream, eingehende Zugangs-Header werden ersetzt
- [x] Steuer-API nur mit Token, Port nur auf 127.0.0.1

**Container-Härtung** (`internal/sandbox`)
- [x] Mounts: nur Workspace (Agent), Workspace read-only (Lese-Container), Binary read-only (Proxy)
- [x] `cap-drop ALL`, `no-new-privileges`, `--init`, Limits, kein Docker-Socket, Proxy `--read-only`
- [x] **Behoben:** `ade.db` (mit `-wal`/`-shm`) entstand mit den Rechten der umask (meist `0644`), jetzt `0600`; ein schon vorhandenes, für andere lesbares Datenverzeichnis wird beim Start auf `0700` gesetzt

**Abhängigkeiten**
- [x] **Behoben:** `monaco-editor` 0.57 brachte DOMPurify ≤ 3.4.16 mit (5 Advisories, niedrig); jetzt 0.58
- [x] `govulncheck` (ohne Befund) und `npm audit` als CI-Job

**Als Issue angelegt**
- #103: Fabrik ohne Gate vor `pull_request` pusht ungeprüften Agent-Code (inkl. Workflows)
