# ade-agent-base: base image for the ADE sandbox (doc/sandbox.md#2-agent-container).
#
# Contains agent CLIs and openvscode-server; ADE starts one container per workspace with
# /workspace as the only mount. Projects can derive from it (FROM ade-agent-base).
#
#   make image                       # build locally (tag :dev)
#   docker run --rm -it ghcr.io/davidw475/ade-agent-base:latest bash

FROM node:24-trixie-slim

# openvscode-server: the same version and checksums as internal/ide/install.go.
ARG OPENVSCODE_VERSION=1.109.5
ARG OPENVSCODE_SHA256_AMD64=b433bf4f0227321a7014d8460d10a8f958adc0f45aa79bd889e84e65e8f88363
ARG OPENVSCODE_SHA256_ARM64=36d9c14036489b63de84ebace837fcacf7e60e669a0dc715802c5443684ea4dc
# Claude Code (npm package @anthropic-ai/claude-code); "latest" or a fixed version.
ARG CLAUDE_CODE_VERSION=latest
# Codex CLI (npm package @openai/codex); "latest" or a fixed version.
ARG CODEX_VERSION=latest
ARG TARGETARCH

LABEL org.opencontainers.image.title="ade-agent-base" \
      org.opencontainers.image.description="ADE sandbox: agent CLIs and openvscode-server" \
      org.opencontainers.image.source="https://github.com/DavidW475/ade" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git less openssh-client procps python3 python3-venv \
      ripgrep jq unzip xz-utils make bash-completion \
 && rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
      amd64) arch=x64;   sum="$OPENVSCODE_SHA256_AMD64" ;; \
      arm64) arch=arm64; sum="$OPENVSCODE_SHA256_ARM64" ;; \
      *) echo "architecture ${TARGETARCH} is not supported" >&2; exit 1 ;; \
    esac; \
    name="openvscode-server-v${OPENVSCODE_VERSION}-linux-${arch}"; \
    curl -fsSL -o /tmp/ovs.tar.gz \
      "https://github.com/gitpod-io/openvscode-server/releases/download/openvscode-server-v${OPENVSCODE_VERSION}/${name}.tar.gz"; \
    echo "${sum}  /tmp/ovs.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/ovs.tar.gz -C /opt; \
    mv "/opt/${name}" /opt/openvscode-server; \
    ln -s /opt/openvscode-server/bin/openvscode-server /usr/local/bin/openvscode-server; \
    rm /tmp/ovs.tar.gz

RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
 && npm cache clean --force

# The node user (UID 1000) becomes ade. ADE starts the container with the user's UID; if it
# differs, ADE adds a passwd entry at start. HOME is therefore writable for everybody (the
# container belongs to exactly one workspace).
RUN usermod --login ade --home /home/ade --move-home node \
 && groupmod --new-name ade node \
 && chmod 0777 /home/ade \
 && git config --system --add safe.directory /workspace \
 && git config --system init.defaultBranch main

ENV HOME=/home/ade \
    SHELL=/bin/bash \
    LANG=C.UTF-8 \
    # Claude Code: no automatic updates in the container.
    DISABLE_AUTOUPDATER=1

USER ade
WORKDIR /workspace
CMD ["sleep", "infinity"]
