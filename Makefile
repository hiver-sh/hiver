CMDS := sandboxd sbxfuse sbxproxy controller sbxvsock sbxguest

.PHONY: help build build-images publish-images up down test test-e2e test-unit gen fmt $(CMDS)

help:
	@awk 'BEGIN {FS = ":.*?## "} /^[0-9a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: $(CMDS) ## Build all cmd binaries into bin/

build-images: ## Build docker images
	docker compose -f docker/compose.yaml --profile build build controller core agent-cli-standalone
	./scripts/bundle-images.sh hiveruntime/agent-cli-standalone hiveruntime/agent-cli

publish-images: build-images ## Build and push images to the registry (override tag with TAG=...)
	docker compose -f docker/compose.yaml push controller core
	docker push hiveruntime/agent-cli-standalone:latest

up: ## Start services
	docker compose -f docker/compose.yaml up -d

down: ## Stop services
	docker compose -f docker/compose.yaml down

test: test-unit ## Run tests

test-e2e: ## Run e2e tests
	go test -v -count=1 ./test/e2e/... 2>&1

test-unit: ## Run unit tests
	go test -v -count=1 $$(go list ./... | grep -v '/test/e2e') 2>&1

$(CMDS):
	go build -o bin/$@ ./cmd/$@

gen: ## Run go generate on the API package
	go generate ./internal/api ./internal/mcp

fmt: ## Format Go sources with gofmt -s
	gofmt -s -w .
