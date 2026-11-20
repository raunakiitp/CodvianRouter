# Makefile — thin entry point for Codvian Router (GAP-TRACK PKG-5).
#
# Every target delegates to the real build/test logic; nothing here
# duplicates it. The CI gate is scripts/ci.sh (gofmt + build + vet +
# full -race test suite). Run `make help` for the target list.
#
# Go toolchain: `go` on PATH if present, else ~/go/bin/go (the pinned
# toolchain location this repo uses; the release workflow pins Go
# 1.27.4). Override per-invocation: `make GO=/path/to/go <target>`.

GO := $(if $(shell command -v go 2>/dev/null),go,$(HOME)/go/bin/go)
BIN := bin/codvian-router
PKG := ./cmd/codvian-router

.PHONY: help build test vet ci clean docker release-check

help: ## Show this help.
	@grep -E '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

build: ## Build the router binary into bin/codvian-router.
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -o $(BIN) $(PKG)

test: ## Run the full unit test suite (no race detector; `make ci` adds -race).
	$(GO) test -count=1 ./...

vet: ## Run go vet over the whole module.
	$(GO) vet ./...

ci: ## Run the full CI gate (gofmt + build + vet + -race tests).
	./scripts/ci.sh

clean: ## Remove build outputs (bin/codvian-router, dist/).
	rm -f bin/codvian-router
	rm -rf dist

docker: ## Build the Docker image (needs a Docker daemon).
	docker build -t codvian-router:dev -f Dockerfile .

release-check: ## Validate .goreleaser.yml (goreleaser check if installed, else YAML parse).
	@if command -v goreleaser >/dev/null 2>&1; then \
		goreleaser check; \
	else \
		echo "goreleaser not installed — falling back to YAML parse"; \
		python3 -c "import yaml; yaml.safe_load(open('.goreleaser.yml')); print('YAML OK: .goreleaser.yml parses')"; \
	fi
