SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# Pinned tool versions. CI reads these too, so bump them here only.
GOLANGCI_LINT_VERSION ?= v2.5.0
KUSTOMIZE_VERSION     ?= v5.8.3
KUBECONFORM_VERSION   ?= v0.8.0
K8S_SCHEMA_VERSION    ?= v1.31.0
KIND_VERSION          ?= v0.30.0

BIN         := $(CURDIR)/bin
CACHE       := $(CURDIR)/.cache
SCHEMA_DIR  := $(CACHE)/schemas/$(K8S_SCHEMA_VERSION)-standalone-strict

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/rbalukja15/k8s-doctor/internal/version.Version=$(VERSION)

GOLANGCI_LINT ?= $(or $(shell command -v golangci-lint 2>/dev/null),$(BIN)/golangci-lint)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: dev
dev: ## Run the CLI from source (pass ARGS="...")
	go run ./cmd/k8s-doctor $(ARGS)

.PHONY: build
build: ## Build the k8s-doctor binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/k8s-doctor ./cmd/k8s-doctor

.PHONY: test
test: ## Run unit tests and the fixture suite
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests with a coverage report
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

.PHONY: lint
lint: | $(GOLANGCI_LINT) ## Run golangci-lint (includes gofumpt and goimports checks)
	$(GOLANGCI_LINT) run ./...

.PHONY: fmt
fmt: | $(GOLANGCI_LINT) ## Format the code with gofumpt and goimports
	$(GOLANGCI_LINT) fmt ./...

.PHONY: fmt-check
fmt-check: | $(GOLANGCI_LINT) ## Fail if any file needs formatting
	$(GOLANGCI_LINT) fmt --diff ./...

.PHONY: test-kind
test-kind: build $(BIN)/kind ## Run the cluster collector against a kind cluster (needs docker, kubectl, jq)
	KIND=$(BIN)/kind K8S_DOCTOR=$(BIN)/k8s-doctor test/kind/run.sh

.PHONY: eval
eval: ## Measure detection precision/recall over fixtures (stub until #21)
	@echo "make eval: not implemented yet (tracked in #21). 'make test' already runs every fixture."

.PHONY: demo
demo: ## Diagnose a kind cluster with seeded problems (stub until #23)
	@echo "make demo: not implemented yet (tracked in #23)."

.PHONY: tools
tools: $(BIN)/kustomize $(BIN)/kubeconform ## Install pinned kustomize and kubeconform into ./bin

.PHONY: schemas
schemas: $(SCHEMA_DIR) ## Fetch Kubernetes JSON schemas for offline kubeconform runs

.PHONY: clean
clean: ## Remove build output and caches
	rm -rf $(BIN) $(CACHE) coverage.out

$(BIN)/golangci-lint:
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(BIN)/kustomize:
	GOBIN=$(BIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

$(BIN)/kind:
	GOBIN=$(BIN) go install sigs.k8s.io/kind@$(KIND_VERSION)

$(BIN)/kubeconform:
	GOBIN=$(BIN) go install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)

$(SCHEMA_DIR):
	rm -rf $(CACHE)/schemas.tmp
	git clone --quiet --depth 1 --filter=blob:none --sparse \
		https://github.com/yannh/kubernetes-json-schema $(CACHE)/schemas.tmp
	git -C $(CACHE)/schemas.tmp sparse-checkout set $(K8S_SCHEMA_VERSION)-standalone-strict
	mkdir -p $(CACHE)/schemas
	mv $(CACHE)/schemas.tmp/$(K8S_SCHEMA_VERSION)-standalone-strict $(SCHEMA_DIR)
	rm -rf $(CACHE)/schemas.tmp
