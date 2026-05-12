CMDS := sandboxd sbxfuse sbxproxy mcpserver

.PHONY: help build test-e2e test-unit gen fmt $(CMDS)

help:
	@awk 'BEGIN {FS = ":.*?## "} /^[0-9a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: $(CMDS) ## Build all cmd binaries into bin/.

test-e2e: ## Run e2e tests
	go test -count=1 ./test/e2e/... 2>&1

test-unit: ## Run unit tests
	go test -count=1 $$(go list ./... | grep -v '/test/e2e') 2>&1

$(CMDS):
	go build -o bin/$@ ./cmd/$@

gen: ## Run go generate on the API package.
	go generate ./internal/api ./internal/mcp

fmt: ## Format Go sources with gofmt -s.
	gofmt -s -w .
