.PHONY: help build test clean install deps fmt vet lint run build-binaries release

# Default target
help: ## Show this help message
	@echo "Available commands:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

# Development commands
deps: ## Download dependencies
	go mod download
	go mod tidy

fmt: ## Format code
	go fmt ./...

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint
	golangci-lint run

test:
	gotestsum --format=short-verbose

# Build commands
build: ## Build the binary
	go build -o bin/gokku ./cmd/cli

bench:
	go test ./... -bench=. -benchmem -v

check: fmt lint vet test
	@echo "All checks passed"

build-binaries: ## Build binaries for all platforms
	chmod +x scripts/build-binaries.sh
	./scripts/build-binaries.sh

# Installation
install: build ## Install gokku binary
	sudo cp bin/gokku /usr/local/bin/

# Cleanup
clean: ## Clean build artifacts
	rm -rf bin/
	go clean

# Release
release: ## Create release
	chmod +x scripts/create-release.sh
	./scripts/create-release.sh -y

