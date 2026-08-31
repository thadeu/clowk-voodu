.PHONY: help build build-cli build-controller build-linux build-linux-arm64 build-linux-amd64 test lint fmt vet clean install-cli force-install-cli tidy check

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

build-linux-arm64: ## Cross-compile both binaries for linux/arm64 into bin/linux-arm64/
	mkdir -p bin/linux-arm64
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/linux-arm64/$(BINARY_CLI) ./cmd/cli
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/linux-arm64/$(BINARY_CONTROLLER) ./cmd/controller

build-linux-amd64: ## Cross-compile both binaries for linux/amd64 into bin/linux-amd64/
	mkdir -p bin/linux-amd64
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/linux-amd64/$(BINARY_CLI) ./cmd/cli
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/linux-amd64/$(BINARY_CONTROLLER) ./cmd/controller

build-linux: build-linux-arm64 build-linux-amd64 ## Cross-compile both binaries for linux (arm64 + amd64)

install-cli: build-cli ## Install voodu to /usr/local/bin (with vd symlink)
	sudo rm -f /usr/local/bin/vd /usr/local/bin/$(BINARY_CLI) 2>/dev/null || true
	sudo install -m 0755 bin/$(BINARY_CLI) /usr/local/bin/$(BINARY_CLI)
	@if [ "$$(uname -s)" = "Darwin" ]; then \
		sudo codesign --force --sign - /usr/local/bin/$(BINARY_CLI); \
	fi
	sudo ln -sf /usr/local/bin/$(BINARY_CLI) /usr/local/bin/vd

force-install-cli: ## Force-rebuild from scratch + reinstall (use when stale binary suspected)
	@echo "==> Force-rebuild bin/$(BINARY_CLI)"
	go build -a -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY_CLI) ./cmd/cli
	@echo "==> Reinstall to /usr/local/bin/$(BINARY_CLI)"
	sudo rm -f /usr/local/bin/vd /usr/local/bin/$(BINARY_CLI) 2>/dev/null || true
	sudo install -m 0755 bin/$(BINARY_CLI) /usr/local/bin/$(BINARY_CLI)
	@if [ "$$(uname -s)" = "Darwin" ]; then \
		sudo codesign --force --sign - /usr/local/bin/$(BINARY_CLI); \
	fi
	sudo ln -sf /usr/local/bin/$(BINARY_CLI) /usr/local/bin/vd
	@echo ""
	@echo "✓ Installed. Verify with:"
	@echo "   /usr/local/bin/$(BINARY_CLI) --version"
	@echo "   should show:  $(VERSION)"
	@echo ""
	@echo "⚠  If 'vd' still acts stale, your shell cached the old path."
	@echo "   Clear it with:   hash -r       (bash/zsh)"
	@echo "   Or open a new terminal session."

# install-controller asks the box what it is before building for it.
#
# This used to hardcode arm64, which is right for a lima VM on Apple Silicon
# and wrong for every x86 VPS — and the failure is silent at the point of the
# mistake: scp and install both succeed, then systemd cannot exec the binary
# and the service just does not come up. One `uname -m` is cheaper than that
# debugging session.
#
# Override with ARCH=amd64 / ARCH=arm64 when the box cannot be reached the
# usual way (a bastion, a paused VM) and you already know what it is.
install-controller: ## Ship to HOST (auto-detects arch; override with ARCH=amd64|arm64)
	@test -n "$(HOST)" || { echo "HOST is required: make install-controller HOST=user@box"; exit 1; }
	@set -e; \
	arch="$(ARCH)"; \
	if [ -z "$$arch" ]; then \
		remote="$$(ssh $(HOST) uname -m)"; \
		case "$$remote" in \
			x86_64|amd64)   arch=amd64 ;; \
			aarch64|arm64)  arch=arm64 ;; \
			*) echo "unsupported architecture on $(HOST): $$remote"; exit 1 ;; \
		esac; \
		echo "==> $(HOST) is $$remote → building linux/$$arch  (override: ARCH=amd64|arm64)"; \
	else \
		echo "==> building linux/$$arch (ARCH override)"; \
	fi; \
	$(MAKE) build-linux-$$arch; \
	scp bin/linux-$$arch/$(BINARY_CLI) $(HOST):/tmp/$(BINARY_CLI); \
	scp bin/linux-$$arch/$(BINARY_CONTROLLER) $(HOST):/tmp/$(BINARY_CONTROLLER); \
	ssh $(HOST) 'sudo install -m 0755 /tmp/$(BINARY_CLI) /usr/local/bin/$(BINARY_CLI) \
		&& sudo install -m 0755 /tmp/$(BINARY_CONTROLLER) /usr/local/bin/$(BINARY_CONTROLLER) \
		&& sudo systemctl restart $(BINARY_CONTROLLER)'; \
	echo "==> installed:"; \
	ssh $(HOST) '$(BINARY_CONTROLLER) --version 2>/dev/null || true; systemctl is-active $(BINARY_CONTROLLER)'

check: fmt vet lint test ## Run all checks
	@echo "All checks passed"

clean: ## Clean build artifacts
	rm -rf bin/ coverage.out
	go clean
