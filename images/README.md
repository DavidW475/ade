# Container images

The images for the Docker sandbox are built here (see [doc/sandbox.md](../doc/sandbox.md)).

| Directory | Contents | Milestone |
|---|---|---|
| `agent-base/` | Debian, git, Node, Python, ripgrep, Claude Code, Codex, openvscode-server | M5 |
| `ade-dev/` | ade-agent-base plus Go, gcc and golangci-lint – sandbox for this repository | M5 |

The egress proxy has no image of its own: it runs as `ade proxy` from the daemon's binary,
mounted read-only into the default image (ADR-015).

## ade-agent-base

CI builds the image on changes under `images/` and pushes it from `main` (`:latest`) to
`ghcr.io/davidw475/ade-agent-base`. Every release tag `vX.Y.Z` always builds it, even without
changes, as `:X.Y.Z` (pre-releases `:X.Y.Z-rc.N`) with SBOM and provenance; the binary of the same
version uses this tag as its default image. Locally:

```sh
make image    # → ghcr.io/davidw475/ade-agent-base:dev
```

For a locally built image, set `sandbox.image` in `~/.config/ade/config.yaml`. The
openvscode-server version and its checksums must match `internal/ide/install.go`.

The user `ade` has UID 1000. If ADE starts the container with a different UID, it adds a passwd
entry; `HOME` (`/home/ade`) is writable for everybody.

## ade-dev

Sandbox image for working on ADE itself, configured in `.ade/sandbox.yaml`. It derives from
`ade-agent-base:latest` and adds Go (version as in `go.mod`), gcc for `go test -race` and
golangci-lint, so the setup commands from `.ade/project.yaml` and `make test`/`make lint` run in
the container. CI builds it after `ade-agent-base` and pushes it from `main`. Locally:

```sh
make image-dev    # → ghcr.io/davidw475/ade-dev:latest
```

When changing the Go version in `go.mod`, update `GO_VERSION` in the Dockerfile as well.
