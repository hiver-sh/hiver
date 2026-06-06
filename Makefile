CMDS := sandboxd sbxfuse sbxproxy controller sbxvsock sbxguest

# JS/TS subprojects with their own format/lint npm scripts
JS_DIRS := cli client/typescript

.PHONY: help build build-images publish-images up down test test-e2e test-unit gen fmt format lint $(CMDS)

help:
	@awk 'BEGIN {FS = ":.*?## "} /^[0-9a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: $(CMDS) ## Build all cmd binaries into bin/

build-images: ## Build docker images
	docker compose -f docker/compose.yaml --profile build build controller core gateway agent-cli-standalone
	./scripts/bundle-images.sh hiversh/agent-cli-standalone hiversh/agent-cli

publish-images: build-images ## Build and push images to the registry (override tag with TAG=...)
	docker compose -f docker/compose.yaml push controller core gateway
	docker push hiversh/agent-cli:latest

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
	go generate ./internal/api

fmt: ## Format Go sources with gofmt -s
	gofmt -s -w .

format: fmt ## Format Go and all TypeScript subprojects
	@for d in $(JS_DIRS); do \
		echo "==> format $$d"; \
		(cd $$d && npm run format --if-present) || exit 1; \
	done

lint: ## Lint Go (go vet) and all TypeScript subprojects
	go vet ./...
	@for d in $(JS_DIRS); do \
		echo "==> lint $$d"; \
		(cd $$d && npm run lint --if-present) || exit 1; \
	done
