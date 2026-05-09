CMDS := sandboxd sbxfuse sbxproxy

.PHONY: help build gen fmt $(CMDS)

help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@printf "  \033[36m%-10s\033[0m build a single binary (\033[33m%s\033[0m)\n" "<cmd>" "$(CMDS)"

build: $(CMDS) ## Build all cmd binaries into bin/.

$(CMDS):
	go build -o bin/$@ ./cmd/$@

gen: ## Run go generate on the API package.
	go generate ./internal/api

fmt: ## Format Go sources with gofmt -s.
	gofmt -s -w .
