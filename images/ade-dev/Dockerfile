# ade-dev: Sandbox-Image für das ADE-Repository selbst (.ade/sandbox.yaml).
#
# Leitet vom Basis-Image ab und ergänzt die Werkzeuge für `make build`, `make test` und
# `make lint`: Go (Version wie in go.mod), gcc für `go test -race` und golangci-lint.
#
#   make image-dev                   # lokal bauen (baut vorher ade-agent-base:dev)

ARG BASE_IMAGE=ghcr.io/davidw475/ade-agent-base:latest
ARG GO_VERSION=1.26
ARG GOLANGCI_LINT_VERSION=v2.15.0

FROM golang:${GO_VERSION}-trixie AS go
FROM golangci/golangci-lint:${GOLANGCI_LINT_VERSION} AS lint

FROM ${BASE_IMAGE}

LABEL org.opencontainers.image.title="ade-dev" \
      org.opencontainers.image.description="ADE-Sandbox für die Entwicklung an ADE: Basis-Image mit Go und golangci-lint" \
      org.opencontainers.image.source="https://github.com/DavidW475/ade" \
      org.opencontainers.image.licenses="Apache-2.0"

USER root
# gcc und libc6-dev: der Race-Detector braucht cgo.
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go
COPY --from=lint /usr/bin/golangci-lint /usr/local/bin/golangci-lint

ENV PATH=/home/ade/go/bin:/usr/local/go/bin:$PATH \
    # Kein Nachladen einer anderen Go-Toolchain aus dem Netz.
    GOTOOLCHAIN=local

USER ade
