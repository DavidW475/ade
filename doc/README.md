# ADE – AI Developer Environment

> Eine Leitstelle für KI-Agenten: Agenten starten, beobachten, in sicheren Sandboxen arbeiten lassen,
> Ergebnisse prüfen und freigeben – alles im Browser, optimiert für WSL.

## Vision

Eine klassische IDE ist für Menschen gebaut, die Code schreiben. In einer **ADE** schreiben
**Agenten** den Code – der Mensch **plant, beobachtet, prüft und gibt frei**.

```
ade start  →  http://localhost:7000
```

1. Im **Dashboard** siehst du alle Projekte und Agenten mit Status, Kosten und offenen Freigaben.
2. Ein Klick auf einen Agenten öffnet seinen **Workspace in einer VS-Code-kompatiblen Browser-IDE**
   (openvscode-server) – inklusive Terminal, Dateien und Diff.
3. Agenten laufen optional in einer **Docker-Sandbox** hinter einem **Egress-Proxy**, der nur
   freigegebene Ziele erlaubt.
4. Mehrere Agenten mit Rollen (Planer, Entwickler, Reviewer, Tester) und unterschiedlichen
   Modellen lassen sich zu einer **Software-Fabrik** verketten.

## Dokumente

| Dokument | Inhalt |
|---|---|
| [architecture.md](architecture.md) | Gesamtarchitektur, Komponenten, Datenmodell, API, Sicherheit, Tech-Stack, Repo-Struktur |
| [sandbox.md](sandbox.md) | Docker-Sandbox, Netzwerk-Isolation, Egress-Proxy, Secret-Handling |
| [threat-model.md](threat-model.md) | Bedrohungsmodell: Werte, Angreifer, Vertrauensgrenzen, Angriffswege, Restrisiken |
| [factory.md](factory.md) | Software-Fabriken: Rollen, Abläufe, Gates, Modelle |
| [milestones.md](milestones.md) | Meilensteine M0–M11 mit Umfang und Akzeptanzkriterien |
| [decisions.md](decisions.md) | Architekturentscheidungen (ADRs) mit Begründung |

## Begriffe

| Begriff | Bedeutung |
|---|---|
| **Projekt** | Ein registriertes Git-Repository (liegt im Linux-Dateisystem der WSL). |
| **Workspace** | Eine isolierte Arbeitskopie eines Projekts für genau einen Agenten (lokaler Git-Clone auf eigenem Branch `ade/<id>`). |
| **Agent / Session** | Ein laufender Agent-Prozess (z. B. Claude Code CLI) mit einer Aufgabe in einem Workspace. |
| **Adapter** | Übersetzt eine konkrete Agent-CLI (claude, codex, generisches Terminal) in ADE-Events. |
| **Sandbox** | Docker-Container, in dem Agent und IDE eines Workspaces laufen. |
| **Egress-Policy** | Regeln, wohin eine Sandbox ins Netz darf (Domain-Allowlist). |
| **Freigabe (Approval)** | Eine Aktion, die ein Mensch bestätigen muss (Tool-Aufruf, Merge, Policy-Ausnahme). |
| **Fabrik / Run** | Eine definierte Pipeline aus Rollen-Agenten; ein Run ist eine Ausführung davon. |

## Zielplattform

- **Primär:** WSL2 (Ubuntu/Debian) unter Windows 11, Browser unter Windows über `localhost`.
- **Nebenbei:** Jedes Linux mit Docker funktioniert genauso – WSL-Spezifika sind optional.

## Lizenz

Geplant: **Apache-2.0** (siehe [decisions.md](decisions.md#adr-009-lizenz-apache-20)).
