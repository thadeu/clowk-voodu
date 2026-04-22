.PHONY: help build build-cli build-controller test lint fmt vet clean install tidy check

BINARY_CLI        := voodu
BINARY_CONTROLLER := voodu-controller
VERSION           ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
COMMIT            := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE              := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS           := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

tidy: ## Download and tidy dependencies
	go mod download
	go mod tidy

fmt: ## Format Go code
	go fmt ./...

vet: ## Run go vet
	go vet ./cmd/... ./internal/... ./pkg/...

lint: ## Run golangci-lint
	golangci-lint run ./cmd/... ./internal/... ./pkg/...

test: ## Run tests (excludes legacy)
	go test -race -coverprofile=coverage.out ./cmd/... ./internal/... ./pkg/...

build-cli: ## Build voodu CLI
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY_CLI) ./cmd/cli

build-controller: ## Build voodu-controller daemon
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY_CONTROLLER) ./cmd/controller

build: build-cli build-controller ## Build all binaries

install: build-cli ## Install voodu to /usr/local/bin (with vd symlink)
	sudo cp bin/$(BINARY_CLI) /usr/local/bin/
	sudo ln -sf /usr/local/bin/$(BINARY_CLI) /usr/local/bin/vd

check: fmt vet lint test ## Run all checks
	@echo "All checks passed"

clean: ## Clean build artifacts
	rm -rf bin/ coverage.out
	go clean
