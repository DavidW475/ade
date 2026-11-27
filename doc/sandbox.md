# Sandbox & Egress-Proxy

Ziel: Ein Agent darf in seinem Workspace **alles** tun – aber **nur** dort, und ins Netz
**nur dorthin**, wo die Policy es erlaubt. Er kann weder Secrets lesen noch etwas veröffentlichen.

> **Stand M7:** Alles in diesem Dokument ist umgesetzt, außer TLS-Interception (später).
> Umsetzung: ADR-014 (Container), ADR-015 (Netz, Proxy, Secrets), ADR-016 (Freigaben).

## 1. Aufbau pro Workspace

```mermaid
flowchart LR
    subgraph NI["Docker-Netz ade-ws-42 (--internal)"]
        A["ade-ws-42 (Alias agent)<br/>Agent-CLI + openvscode-server<br/>/workspace (Mount)"]
    end
    P["ade-proxy-42<br/>ade proxy<br/>(in ade-ws-42 und ade-egress)"]
    D[ade daemon]
    I((Internet))

    A -- "HTTP(S)_PROXY / BASE_URLs" --> P
    P -- "ade-egress: nur Allowlist" --> I
    D -- "Policy, Secrets, Logs" --- P
    D -- "Steuer-API :8081" --> P
    D -- "Gateway → :3000" --> P
    P -- "IDE-Weiterleitung" --> A
```

- **`ade-ws-<id>`** ist ein `--internal`-Netz: Container darin haben **keine Route nach draußen**.
- Der **Proxy-Container** hängt in beiden Netzen und ist der einzige Weg ins Internet.
- `HTTP_PROXY`/`HTTPS_PROXY` im Agent-Container sind nur Bequemlichkeit – **erzwungen** wird die
  Isolation durch das interne Netz. Ein Programm, das den Proxy ignoriert, erreicht schlicht nichts.
- **`ade-egress`** ist ein gemeinsames Netz aller Proxys ohne Verkehr zwischen den Containern
  (`enable_icc=false`).
- Docker veröffentlicht keine Ports von Containern, die nur in internen Netzen hängen. Der Daemon
  erreicht deshalb Steuer-API (`:8081`) und IDE (`:3000`) über veröffentlichte Ports des
  **Proxy-Containers** auf `127.0.0.1`; der Proxy leitet die IDE per TCP an `agent:3000` weiter.
- Der Proxy-Container führt das `ade`-Binary des Daemons aus (nur lesend gemountet, eine Kopie im
  Datenverzeichnis: `<data_dir>/sandbox/ade-proxy`; statisch gebaut), im Standard-Image, als
  Nutzer, ohne Capabilities, mit nur lesendem Dateisystem und kleinen Grenzen.

## 2. Agent-Container

| Einstellung | Wert |
|---|---|
| Image | `ghcr.io/davidw475/ade-agent-base:<version>` (oder Projekt-Image aus `.ade/sandbox.yaml`) |
| User | non-root: UID/GID des Daemons (`--user`); fehlt sie im Image, legt ADE einen passwd-Eintrag an |
| Mounts | nur `~/.ade/workspaces/<id>` → `/workspace` (rw); optional Caches (npm, go) als Volumes |
| Capabilities | `--cap-drop ALL`, `--security-opt no-new-privileges` |
| Ressourcen | `--memory`, `--cpus`, `--pids-limit` aus Policy |
| Verboten | Docker-Socket, Host-Netz, privilegierter Modus, Mount des Haupt-Repos |
| Prozesse | `sleep infinity` unter `--init`; `openvscode-server` (Port 3000, Token, auf `127.0.0.1` veröffentlicht), Agent-CLI, Setup-Befehle und git des Daemons über `docker exec` |
| Workspace-Clone | ohne Hardlinks (keine geteilten Objektdateien mit dem Haupt-Repo) |

Basis-Image enthält: Debian slim, git, curl, Node.js LTS, Python 3, ripgrep, gängige
Agent-CLIs, openvscode-server, `ade`-Hook-Binary. Projekte können ein eigenes Image ableiten
(`FROM ade-agent-base`) oder Setup-Befehle in `.ade/project.yaml` angeben.

## 3. Egress-Proxy (`ade proxy`)

Ein kleiner HTTP-Forward-Proxy in Go, Teil des `ade`-Binaries.

**Version 1 (ohne TLS-Aufbrechen):**

- `CONNECT host:port` → erlaubt, wenn `host` auf der Allowlist steht (Einträge ohne Port gelten
  für 80 und 443); sonst `403` mit Begründung.
- Klartext-HTTP (absolute URI) → Prüfung über den Host der URI.
- **Interne Ziele sind immer gesperrt:** Loopback, private Netze (auch Docker-Netze), Link-Local,
  CGNAT, Multicast u. a. Geprüft wird die tatsächlich verwendete IP direkt vor dem `connect`
  (kein DNS-Rebinding), auch wenn ein Name auf der Allowlist steht.
- Jede Anfrage wird als Egress-Event gemeldet (erlaubt/geblockt, Grund, Host, Port, Bytes, Dauer;
  CONNECT-Tunnel beim Schließen). Der Daemon holt sie per Long-Polling ab, speichert die letzten
  2000 je Workspace und zeigt sie in der Ansicht **Netzwerk**.
- Ohne Policy (direkt nach dem Start) ist alles gesperrt.
- **Steuer-API** (`:8081`, nur mit Token aus `ADE_PROXY_TOKEN`; der Agent kennt es nicht):
  `PUT /policy` (Allow/Deny, Keys, Rückfragen), `GET /events`, `GET /questions`,
  `POST /questions/{id}`, `GET /healthz`.
- **Freigaben** (`on_blocked: ask`): Eine Anfrage an ein nicht freigegebenes Ziel wartet (bis zu
  2 min) auf eine Entscheidung im Dashboard („Agent will `pypi.org:443` erreichen“): **Einmal**
  (nur die wartenden Anfragen), **Für diesen Workspace**, **Immer (Projekt)** oder **Ablehnen**
  (bis zum Neustart des Proxys nicht erneut fragen). Ziele auf der deny-Liste und interne
  Adressen fragen nie. Gleichzeitige Anfragen an dasselbe Ziel teilen sich eine Freigabe.
- **Ausnahmen:** „Für diesen Workspace“ und „Immer (Projekt)“ speichert der Daemon in SQLite und
  ergänzt damit die Allowlist – `.ade/sandbox.yaml` im Repo bleibt unverändert. Auch mit
  `on_blocked: deny` lässt sich ein geblockter Host in der Netzwerk-Ansicht mit einem Klick
  erlauben; dort stehen alle Ausnahmen und lassen sich entfernen.
- **Freigabe-Hook:** Der Proxy nimmt unter `/ade/hook/pretooluse` die Fragen von
  `ade hook pretooluse` aus dem Agent-Container entgegen und reicht sie über die Steuer-API an
  den Daemon weiter (doc/architecture.md#43-freigaben). Das ade-Binary ist dafür nur lesend unter
  `/opt/ade/ade` im Agent-Container gemountet.

**Später (optional):** TLS-Interception mit eigener CA im Container für Regeln auf
Pfad-/Methodenebene (z. B. GitHub nur lesend). Bewusst nicht in v1 – mehr Komplexität,
mehr Angriffsfläche.

### DNS

Auch im internen Netz beantwortet Docker DNS-Anfragen (eingebetteter DNS-Server `127.0.0.11`).
Geprüft mit Docker 29: Für `--internal`-Netze leitet er externe Namen nicht weiter, nur
Container-Namen (`proxy`) lösen auf. Unabhängig von der Docker-Version setzt ADE zusätzlich
`--dns 192.0.2.1` (TEST-NET-1, im internen Netz unerreichbar) als Upstream – keine
Loopback-Adresse, denn die würde Docker im Netz-Namespace des Hosts befragen. Der Agent braucht
keine externe Auflösung, der Proxy löst für ihn auf. Ein Docker-Test prüft beides.

### Policy-Format (`.ade/sandbox.yaml`)

```yaml
image: ghcr.io/davidw475/ade-agent-base:0.1
resources:
  memory: 4g
  cpus: 2
  pids: 512
egress:
  presets: [anthropic, github-readonly, npm, pypi, go-proxy]
  allow:
    - api.example.com
    - "*.githubusercontent.com"
  deny:
    - "*.pastebin.com"
  on_blocked: ask          # deny (Standard) oder ask: Freigabe im Dashboard
```

Presets sind im Binary hinterlegte Domain-Listen (`internal/sandbox/egress.go`):

| Preset | Hosts |
|---|---|
| `anthropic` | api.anthropic.com, console.anthropic.com, platform.claude.com, claude.ai, statsig.anthropic.com |
| `github-readonly` | github.com, api.github.com, codeload.github.com, raw/objects/release-assets.githubusercontent.com, ghcr.io, pkg-containers.githubusercontent.com |
| `npm` | registry.npmjs.org, registry.yarnpkg.com |
| `pypi` | pypi.org, files.pythonhosted.org |
| `go-proxy` | proxy.golang.org, sum.golang.org |

- Ohne `egress`-Abschnitt gelten `anthropic`, `github-readonly`, `npm`, `pypi`, `go-proxy`.
  Mit Abschnitt gilt nur, was dort steht; `egress: {}` sperrt alles.
- `github-readonly` heißt ohne TLS-Aufbrechen nur: Der Container erreicht GitHub, hat aber keine
  Zugangsdaten dafür. Schreibzugriffe scheitern an der Anmeldung, nicht am Proxy.
- `deny` geht vor `allow`; ein Eintrag ohne Port sperrt alle Ports.
- Der Daemon liest die Datei aus dem Haupt-Repo und schickt Änderungen alle paar Sekunden an
  laufende Proxys – ohne Neustart. Ist die Datei ungültig, behalten laufende Proxys die zuletzt
  gültige Policy, und neue Container starten erst nach der Korrektur (kein stilles Zurückfallen
  auf die Voreinstellung).

## 4. Secrets

Grundsatz: **Kein API-Key, kein Git-Token im Agent-Container.**

API-Zugriffe der Agent-CLIs laufen über **Basis-URLs, die auf den Proxy zeigen**:

```
ANTHROPIC_BASE_URL=http://proxy:8080/p/anthropic
```

Der Proxy leitet `…/p/anthropic/*` an `https://api.anthropic.com/*` weiter und **fügt den Key
selbst ein**. Der Agent sieht nur einen Platzhalter. Vorteile:

- Ein kompromittierter Agent kann den Key nicht auslesen oder weitergeben.
- Der Proxy kann pro Workspace **Tokens zählen und Budgets erzwingen**.
- Kein TLS-Aufbrechen nötig (Agent → Proxy im internen Netz per HTTP, Proxy → Anbieter per HTTPS).

Ob eine CLI eine konfigurierbare Basis-URL unterstützt, ist pro Adapter zu prüfen
(Claude Code: `ANTHROPIC_BASE_URL`). Für CLIs ohne diese Option bleibt nur Key-im-Container
mit Warnung im Dashboard oder TLS-Interception (später).

Umsetzung (M6, zuerst Anthropic):

- Der Daemon schickt die aufgelösten Keys (`providers.<name>.api_key`) über die Steuer-API an den
  Proxy. Sie stehen nie in Container-Konfiguration, Umgebung oder Dateien des Agent-Containers.
- Agent-Sessions in der Sandbox bekommen `ANTHROPIC_BASE_URL=http://proxy:8080/p/anthropic`,
  `ANTHROPIC_API_KEY=<Platzhalter>` und `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`. Der Proxy
  entfernt `Authorization`/`X-Api-Key` des Clients und setzt den echten Key; Antworten (SSE)
  reicht er ungepuffert durch.
- `proxy` steht in `NO_PROXY`. Clients, die die Basis-URL trotzdem über `HTTP_PROXY` ansprechen,
  bedient der Proxy genauso (Anfragen an sich selbst unter `/p/`).
- Ohne konfigurierten Key bekommt die CLI keine Basis-URL und nutzt ihre eigene Anmeldung über
  die Allowlist (Preset `anthropic`).

**Git:** Der Agent committet lokal im Workspace. `fetch`/`push` zum Remote macht ausschließlich
der Daemon auf dem Host nach Freigabe. Lesender Zugriff auf GitHub (z. B. Abhängigkeiten) geht
über die Allowlist.

**Anmeldung per Abo:** Falls ein Nutzer eine CLI mit eigenem Abo-Login in der Sandbox nutzt,
liegen dessen Credentials im Container (Volume). Das ist bewusst eine Nutzerentscheidung, ADE
zeigt dann einen Hinweis. ADE selbst implementiert keine Abo-Logins von Anbietern
(siehe [decisions.md](decisions.md#adr-010-anbieter-zugang-nur-über-api-keys-oder-offizielle-clis)).

## 5. Lebenszyklus

| Ereignis | Aktion |
|---|---|
| Workspace mit Sandbox angelegt | Netz `ade-ws-<id>`, Proxy `ade-proxy-<id>` und Container `ade-ws-<id>` erstellen und starten (ggf. Image laden), Policy an den Proxy, Setup-Befehle im Container |
| Session startet | `docker exec -it ade-ws-<id> <adapter-cmd>` (startet den Container bei Bedarf) |
| Session stoppt | SIGTERM an alle Prozesse mit `ADE_SESSION_ID=<id>` im Container, nach Timeout SIGKILL |
| IDE öffnen | `docker exec ade-ws-<id> openvscode-server …`, Gateway → veröffentlichter Port des Proxys → `agent:3000` |
| Daemon wird beendet | Container und Proxys stoppen (Dateien bleiben im Workspace-Verzeichnis) |
| Workspace verworfen | Container, Proxy, Netz und Workspace-Verzeichnis löschen |
| Daemon-Start | Container per Label finden: verwaiste (samt Netzen) entfernen, übrige stoppen; Start bei Bedarf. Container aus M5 (Label `ade.net` fehlt) werden beim nächsten Start neu angelegt |

Alle Docker-Objekte tragen Labels (`ade.managed=true`, `ade.workspace=<id>`,
`ade.data_dir=<Datenverzeichnis>`), damit `ade gc` verwaiste Ressourcen sicher aufräumen kann –
auch die einer ADE-Instanz, deren Datenverzeichnis nicht mehr existiert, aber nie die einer
anderen laufenden Instanz.
