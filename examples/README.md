# Examples

Example configurations for ADE. Every option is described in the
[configuration reference](../doc/configuration.md).

| File | Purpose |
|---|---|
| [`config.yaml`](config.yaml) | Global configuration, goes to `~/.config/ade/config.yaml` |
| [`.ade/project.yaml`](.ade/project.yaml) | Project configuration, goes into the repository as `.ade/project.yaml` |
| [`.ade/sandbox.yaml`](.ade/sandbox.yaml) | Sandbox container (image, resources, network rules), goes into the repository as `.ade/sandbox.yaml` |
| [`factories/`](factories/) | Factory templates `feature`, `bugfix`, `refactor` and `review-only` with [prompts](factories/prompts/) and [schemas](factories/schemas/). Built in; copy them to `.ade/factories/` to customize them |

The templates under `factories/` are embedded into the ade binary (`embed.go`): `ade run feature`
works without an own file. A factory with the same name in `.ade/factories/` takes precedence.
