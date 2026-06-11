CMDS := sandboxd sbxfuse sbxproxy controller sbxvsock sbxguest

# JS/TS subprojects with their own format/lint npm scripts
JS_DIRS := cli client/typescript

.PHONY: help build build-images bundle-sandbox-images publish-images publish-sandbox-images buildx-builder up down test e2e test-e2e test-unit gen fmt format lint $(CMDS)

help:
	@awk 'BEGIN {FS = ":.*?## "} /^[0-9a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: $(CMDS) ## Build all cmd binaries into bin/

build-images: ## Build docker images
	docker compose -f docker/compose.yaml --profile build build controller core gateway

# Platforms to build multi-arch images for. The deployment servers are amd64,
# so amd64 must be included or `docker pull` fails with "no matching manifest".
PLATFORMS ?= linux/amd64,linux/arm64

# Build and push the default sandbox images as multi-arch manifest lists.
# `hiver bundle --platform` pushes directly, so there's no separate push step.
# The inputs (hiversh/core, hiversh/agent-cli-standalone) are pulled per-arch
# from the registry, so run `make publish-images` first.
bundle-sandbox-images: ## Bundle and push the default sandbox images (multi-arch)
	hiver bundle ./docker/agent-cli --tag hiversh/agent-cli --platform $(PLATFORMS)
	hiver bundle python:3.13-alpine --tag hiversh/python:3.13-alpine --platform $(PLATFORMS)
	hiver bundle node:alpine --tag hiversh/node:alpine --platform $(PLATFORMS)


# Multi-arch builds need a docker-container driver builder; the default `docker`
# driver can't build+push a manifest list. Create one if it's missing.
buildx-builder:
	@docker buildx inspect hiver-multiarch >/dev/null 2>&1 || \
		docker buildx create --name hiver-multiarch --driver docker-container --bootstrap

# agent-cli-standalone is included so `bundle-sandbox-images` can pull it per-arch.
# Run from docker/ so bake resolves the compose file's relative build contexts
# (e.g. `gateway`, `..`) against that dir rather than the repo root.
publish-images: buildx-builder ## Build and push multi-arch images to the registry
	cd docker && docker buildx bake -f compose.yaml \
		--builder hiver-multiarch \
		--set "*.platform=$(PLATFORMS)" \
		--push \
		controller core gateway

# `bundle-sandbox-images` already builds and pushes multi-arch manifests; a plain
# `docker push` here would clobber them with a single-arch image, so this is just
# an alias kept for the old name.
publish-sandbox-images: bundle-sandbox-images ## Build and push sandbox images (multi-arch)

sync-client-version: ## Sync @hiver.sh/client version to match cli/package.json, update cli dep and lockfile
	./scripts/sync-client-version.sh

link-cli: ## Builds the local CLI and makes it available as hiver in the PATH
	cd cli && npm run build && npm link

unlink-cli: ## Unlinks the local CLI
	npm unlink -g @hiver.sh/cli

up: ## Start services
	docker compose -f docker/compose.yaml up -d

down: ## Stop services
	docker compose -f docker/compose.yaml down

test: test-unit ## Run tests

e2e: test-e2e ## Run e2e tests (alias for test-e2e)

test-e2e: ## Run e2e tests
	go test -v -count=1 -timeout 30m ./test/e2e/... 2>&1

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
