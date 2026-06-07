CMDS := sandboxd sbxfuse sbxproxy controller sbxvsock sbxguest

# JS/TS subprojects with their own format/lint npm scripts
JS_DIRS := cli client/typescript

.PHONY: help build build-images publish-images up down test test-e2e test-unit gen fmt format lint $(CMDS)

help:
	@awk 'BEGIN {FS = ":.*?## "} /^[0-9a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: $(CMDS) ## Build all cmd binaries into bin/

build-images: ## Build docker images
	docker compose -f docker/compose.yaml --profile build build controller core gateway agent-cli-standalone

bundle-sandbox-images: ## Bundle the default sandbox images
	hiver bundle hiversh/agent-cli-standalone --tag hiversh/agent-cli
	hiver bundle python:3.13-alpine --tag hiversh/python:3.13-alpine
	hiver bundle node:alpine --tag hiversh/node:alpine

publish-images: ## Push images to the registry
	docker compose -f docker/compose.yaml push controller core gateway

publish-sandbox-images: build-images ## Push sandbox images to the registry
	docker push hiversh/agent-cli:latest
	docker push hiversh/python:3.13-alpine
	docker push hiversh/node:alpine

link-cli: ## Builds the local CLI and makes it available as hiver in the PATH
	cd cli && npm run build && npm link

unlink-cli: ## Unlinks the local CLI
	npm unlink -g @hiver.sh/cli

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
