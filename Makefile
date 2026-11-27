VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/ade
DIST    := internal/api/dist
GOLANGCI_LINT ?= golangci-lint

.PHONY: all build web go test lint fmt dev clean image image-dev

all: build

## build: Frontend bauen und in ein einzelnes Binary einbetten (bin/ade)
build: web go

## web: Dashboard bauen (web/ → internal/api/dist)
web: web/node_modules
	cd web && npm run build
	@touch $(DIST)/.gitkeep

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci
	@touch $@

## go: Go-Binary bauen (bettet den aktuellen Inhalt von internal/api/dist ein)
go:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/ade

## test: Go-Tests (mit Race-Detector) und Frontend-Typprüfung
test: web/node_modules
	go test -race ./...
	cd web && npm run typecheck

## lint: golangci-lint und ESLint
lint: web/node_modules
	$(GOLANGCI_LINT) run ./...
	cd web && npm run lint

## fmt: Go-Code formatieren
fmt:
	gofmt -w .

## image: Sandbox-Image ade-agent-base lokal bauen (Tag :dev)
image:
	docker build -t ghcr.io/davidw475/ade-agent-base:dev images/agent-base

## image-dev: Sandbox-Image ade-dev für dieses Repo lokal bauen (Tag :latest, siehe .ade/sandbox.yaml)
image-dev:
	docker build -t ghcr.io/davidw475/ade-dev:latest images/ade-dev

## dev: Daemon im Vordergrund mit go run (Frontend separat: cd web && npm run dev)
dev:
	CGO_ENABLED=0 go run ./cmd/ade serve --debug

## clean: Build-Artefakte entfernen
clean:
	rm -rf bin $(DIST)/assets $(DIST)/index.html $(DIST)/favicon.svg

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
