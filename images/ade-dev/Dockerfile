# ade-dev: sandbox image for the ADE repository itself (.ade/sandbox.yaml).
#
# Derives from the base image and adds the tools for `make build`, `make test` and
# `make lint`: Go (version as in go.mod), gcc for `go test -race` and golangci-lint.
#
#   make image-dev                   # build locally (builds ade-agent-base:dev first)

ARG BASE_IMAGE=ghcr.io/davidw475/ade-agent-base:latest
ARG GO_VERSION=1.26
ARG GOLANGCI_LINT_VERSION=v2.15.0

FROM golang:${GO_VERSION}-trixie AS go
FROM golangci/golangci-lint:${GOLANGCI_LINT_VERSION} AS lint

FROM ${BASE_IMAGE}

LABEL org.opencontainers.image.title="ade-dev" \
      org.opencontainers.image.description="ADE sandbox for developing ADE: base image with Go and golangci-lint" \
      org.opencontainers.image.source="https://github.com/DavidW475/ade" \
      org.opencontainers.image.licenses="Apache-2.0"

USER root
# gcc and libc6-dev: the race detector needs cgo.
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go
COPY --from=lint /usr/bin/golangci-lint /usr/local/bin/golangci-lint

ENV PATH=/home/ade/go/bin:/usr/local/go/bin:$PATH \
    # Do not download another Go toolchain from the network.
    GOTOOLCHAIN=local

USER ade
