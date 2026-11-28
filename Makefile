VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/ade
DIST    := internal/api/dist
GOLANGCI_LINT ?= golangci-lint

.PHONY: all build web go test lint fmt dev clean image image-dev

all: build

## build: build the frontend and embed it into a single binary (bin/ade)
build: web go

## web: build the dashboard (web/ → internal/api/dist)
web: web/node_modules
	cd web && npm run build
	@touch $(DIST)/.gitkeep

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci
	@touch $@

## go: build the Go binary (embeds the current contents of internal/api/dist)
go:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/ade

## test: Go tests (with race detector) and frontend type check
test: web/node_modules
	go test -race ./...
	cd web && npm run typecheck

## lint: golangci-lint and ESLint
lint: web/node_modules
	$(GOLANGCI_LINT) run ./...
	cd web && npm run lint

## fmt: format Go code
fmt:
	gofmt -w .

## image: build the sandbox image ade-agent-base locally (tag :dev)
image:
	docker build -t ghcr.io/davidw475/ade-agent-base:dev images/agent-base

## image-dev: build the sandbox image ade-dev for this repository locally (tag :latest, see .ade/sandbox.yaml)
image-dev:
	docker build -t ghcr.io/davidw475/ade-dev:latest images/ade-dev

## dev: daemon in the foreground with go run (frontend separately: cd web && npm run dev)
dev:
	CGO_ENABLED=0 go run ./cmd/ade serve --debug

## clean: remove build artifacts
clean:
	rm -rf bin $(DIST)/assets $(DIST)/index.html $(DIST)/favicon.svg

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
