# Beispiele

Beispielkonfigurationen für ADE.

| Datei | Zweck |
|---|---|
| [`config.yaml`](config.yaml) | Globale Konfiguration, gehört nach `~/.config/ade/config.yaml` |
| [`.ade/project.yaml`](.ade/project.yaml) | Projekt-Konfiguration, gehört ins Repo nach `.ade/project.yaml` |
| [`.ade/sandbox.yaml`](.ade/sandbox.yaml) | Sandbox-Container (Image, Ressourcen, Netzwerk-Regeln), gehört ins Repo nach `.ade/sandbox.yaml` |
| [`factories/feature.yaml`](factories/feature.yaml) | Fabrik `feature` mit [Prompts](factories/prompts/): Planer → Freigabe → Entwickler → Reviewer → Freigabe → PR. Eingebaut; zum Anpassen nach `.ade/factories/` kopieren |

Die Vorlagen unter `factories/` sind ins ade-Binary eingebettet (`embed.go`): `ade run feature`
funktioniert ohne eigene Datei. Eine gleichnamige Fabrik in `.ade/factories/` hat Vorrang.
