# Image URL to use all building/pushing image targets
IMG ?= somaz940/git-bridge:v0.3.0
APP_NAME := git-bridge
MODULE := git-bridge

GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "\
	-X $(MODULE)/internal/version.Version=$(shell echo ${IMG} | cut -d: -f2) \
	-X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE) \
	-s -w"

# Container tool (docker or podman)
CONTAINER_TOOL ?= docker

# Docker build args
DOCKER_BUILD_ARGS = \
	--build-arg VERSION=$(shell echo ${IMG} | cut -d: -f2) \
	--build-arg GIT_COMMIT=$(GIT_COMMIT) \
	--build-arg BUILD_DATE=$(BUILD_DATE)

# Platforms for multi-arch builds
PLATFORMS ?= linux/amd64,linux/arm64

# Get the currently used golang install path
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Versions
GOLANGCI_LINT_VERSION ?= v2.1.6

## Tool Binaries
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

# Deploy
DEPLOY_NAME ?= $(APP_NAME)
DEPLOY_PORT ?= 8080
# Prefer examples/config.local.yaml (gitignored) when present, else fall back to
# examples/config.yaml. The local override lets `make deploy` avoid unresolved
# ${ENV_VAR} placeholders for SQS/credentials during smoke tests.
DEPLOY_CONFIG ?= $(shell [ -f $(CURDIR)/examples/config.local.yaml ] && echo $(CURDIR)/examples/config.local.yaml || echo $(CURDIR)/examples/config.yaml)
K8S_NAMESPACE ?= git-bridge

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: build
build: ## Build binary to bin/
	go build $(LDFLAGS) -o bin/$(APP_NAME) ./cmd/$(APP_NAME)

.PHONY: run
run: build ## Build and run locally using examples/config.yaml
	CONFIG_PATH=$(DEPLOY_CONFIG) ./bin/$(APP_NAME)

.PHONY: fmt
fmt: ## Run go fmt against code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

##@ Testing

.PHONY: test
test: ## Run all tests with race detection and coverage
	go test ./... -race -cover

.PHONY: test-unit
test-unit: ## Run unit tests only (internal packages)
	go test ./internal/... -race -cover

.PHONY: test-integration
test-integration: ## Run integration tests only
	go test -race -run TestIntegration ./...

.PHONY: test-helm
test-helm: ## Run Helm chart tests (lint, template render)
	@bash hack/test-helm.sh

.PHONY: cover
cover: ## Generate HTML coverage report
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

##@ Build

.PHONY: install
install: build ## Install binary to /usr/local/bin
	cp bin/$(APP_NAME) /usr/local/bin/$(APP_NAME)

.PHONY: uninstall
uninstall: ## Remove binary from /usr/local/bin
	rm -f /usr/local/bin/$(APP_NAME)

.PHONY: cross-build
cross-build: ## Build for multiple OS/arch (output: dist/)
	@mkdir -p dist
	@for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		output=dist/$(APP_NAME)-$${os}-$${arch}; \
		echo "Building $${os}/$${arch}..."; \
		GOOS=$${os} GOARCH=$${arch} go build $(LDFLAGS) -o $${output} ./cmd/$(APP_NAME); \
	done
	@echo "Cross-build complete. Binaries in dist/"

##@ Docker

.PHONY: docker-build
docker-build: ## Build docker image
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-buildx-tag
docker-buildx-tag: ## Build and push multi-arch image with version tag
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name $(APP_NAME)-builder
	$(CONTAINER_TOOL) buildx use $(APP_NAME)-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) \
		$(DOCKER_BUILD_ARGS) \
		--tag ${IMG} \
		-f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm $(APP_NAME)-builder
	rm Dockerfile.cross

.PHONY: docker-buildx-latest
docker-buildx-latest: ## Build and push multi-arch image with latest tag
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name $(APP_NAME)-builder
	$(CONTAINER_TOOL) buildx use $(APP_NAME)-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) \
		$(DOCKER_BUILD_ARGS) \
		--tag $(shell echo ${IMG} | cut -f1 -d:):latest \
		-f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm $(APP_NAME)-builder
	rm Dockerfile.cross

.PHONY: docker-buildx
docker-buildx: ## Build and push both version and latest tags
docker-buildx: docker-buildx-tag docker-buildx-latest

##@ Version

.PHONY: version
version: ## Show current version across all files
	@./hack/bump-version.sh --current

VERSION ?=
.PHONY: bump-version
bump-version: ## Bump version across all files. Usage: make bump-version VERSION=v0.2.0
	@if [ -z "$(VERSION)" ]; then echo "Usage: make bump-version VERSION=vX.Y.Z"; exit 1; fi
	@./hack/bump-version.sh $(VERSION)

##@ Workflow

.PHONY: check-gh
check-gh: ## Check if gh CLI is installed and authenticated
	@command -v gh >/dev/null 2>&1 || { echo "\033[31m✗ gh CLI not installed. Run: brew install gh\033[0m"; exit 1; }
	@gh auth status >/dev/null 2>&1 || { echo "\033[31m✗ gh CLI not authenticated. Run: gh auth login\033[0m"; exit 1; }
	@echo "\033[32m✓ gh CLI ready\033[0m"

.PHONY: branch
branch: ## Create feature branch (usage: make branch name=watch-mode)
	@if [ -z "$(name)" ]; then echo "Usage: make branch name=<feature-name>"; exit 1; fi
	git checkout main
	git pull origin main
	git checkout -b feat/$(name)
	@echo "\033[32m✓ Branch feat/$(name) created\033[0m"

.PHONY: pr
pr: check-gh ## Run tests, push, and create PR (usage: make pr title="Add feature")
	@if [ -z "$(title)" ]; then echo "Usage: make pr title=\"PR title\""; exit 1; fi
	go test ./... -race -cover
	go vet ./...
	git push -u origin $$(git branch --show-current)
	@./scripts/create-pr.sh "$(title)"
	@echo "\033[32m✓ PR created\033[0m"

##@ Deploy

.PHONY: deploy
deploy: build ## Build binary + run local server with examples/config.yaml
	@echo "Stopping existing process (if any)..."
	-@pkill -f "bin/$(APP_NAME)" 2>/dev/null || true
	@sleep 0.5
	@echo "Starting $(APP_NAME) on port $(DEPLOY_PORT)..."
	@CONFIG_PATH=$(DEPLOY_CONFIG) ./bin/$(APP_NAME) &
	@sleep 1
	@echo "Server running at http://localhost:$(DEPLOY_PORT) (PID: $$(pgrep -f 'bin/$(APP_NAME)'))"

.PHONY: undeploy
undeploy: ## Stop local server
	@echo "Stopping $(APP_NAME)..."
	-@pkill -f "bin/$(APP_NAME)" 2>/dev/null || true
	@echo "Server stopped."

.PHONY: deploy-docker
deploy-docker: ## Deploy as Docker container (pulls image if not local)
	@if ! $(CONTAINER_TOOL) image inspect ${IMG} >/dev/null 2>&1; then \
		echo "\033[33m⚠ Image ${IMG} not found locally. Pulling from registry...\033[0m"; \
		$(CONTAINER_TOOL) pull ${IMG} || { echo "\033[31m✗ Pull failed. Run 'make docker-build' to build locally.\033[0m"; exit 1; }; \
	fi
	@echo "Stopping existing container (if any)..."
	-@$(CONTAINER_TOOL) rm -f $(DEPLOY_NAME) 2>/dev/null
	@echo "Starting $(DEPLOY_NAME) on port $(DEPLOY_PORT)..."
	$(CONTAINER_TOOL) run -d \
		--name $(DEPLOY_NAME) \
		-p $(DEPLOY_PORT):8080 \
		-v $(DEPLOY_CONFIG):/etc/git-bridge/config.yaml:ro \
		${IMG}
	@echo "Container $(DEPLOY_NAME) running at http://localhost:$(DEPLOY_PORT)"

.PHONY: undeploy-docker
undeploy-docker: ## Stop and remove Docker container
	@echo "Stopping $(DEPLOY_NAME)..."
	-$(CONTAINER_TOOL) rm -f $(DEPLOY_NAME) 2>/dev/null
	@echo "Container $(DEPLOY_NAME) removed."

.PHONY: deploy-smoke
deploy-smoke: ## Smoke test against running server
	@bash hack/test-deploy.sh $(DEPLOY_PORT)

.PHONY: deploy-all
deploy-all: deploy deploy-smoke ## Build + run + smoke test (all-in-one)

.PHONY: deploy-k8s
deploy-k8s: ## Deploy to Kubernetes cluster using k8s/ manifests
	kubectl apply -f k8s/namespace.yaml
	kubectl apply -f k8s/secret.yaml
	kubectl apply -f k8s/configmap.yaml
	kubectl apply -f k8s/pvc.yaml
	kubectl apply -f k8s/deployment.yaml
	kubectl rollout status deployment/$(DEPLOY_NAME) -n $(K8S_NAMESPACE) --timeout=120s

.PHONY: undeploy-k8s
undeploy-k8s: ## Remove from Kubernetes cluster
	kubectl delete -f k8s/deployment.yaml --ignore-not-found
	kubectl delete -f k8s/pvc.yaml --ignore-not-found
	kubectl delete -f k8s/configmap.yaml --ignore-not-found
	kubectl delete -f k8s/secret.yaml --ignore-not-found
	kubectl delete -f k8s/namespace.yaml --ignore-not-found

.PHONY: restart
restart: ## Restart Kubernetes deployment
	kubectl rollout restart -n $(K8S_NAMESPACE) deployment/$(DEPLOY_NAME)

.PHONY: logs
logs: ## Tail Kubernetes pod logs
	kubectl logs -n $(K8S_NAMESPACE) -l app=$(DEPLOY_NAME) -f

##@ Cleanup

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/ coverage.out coverage.html

##@ Dependencies

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f $(1) ] || { \
set -e ;\
TMP_DIR=$$(mktemp -d) ;\
cd $$TMP_DIR ;\
go mod init tmp ;\
echo "Downloading $(2)@$(3)" ;\
GOBIN=$(LOCALBIN) CGO_ENABLED=0 GOOS=$$(go env GOOS) GOARCH=$$(go env GOARCH) go install $(2)@$(3) ;\
rm -rf $$TMP_DIR ;\
}
endef
