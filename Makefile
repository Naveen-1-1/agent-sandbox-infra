# Build, test and run the agent-sandbox platform locally.
# `make help` lists the targets.

SHELL := /bin/bash
GO    ?= go
PKGS  ?= ./...
# Developer tools are pinned in tools.mod and run through `go tool`.
TOOL  := $(GO) tool -modfile=tools.mod

KIND_CLUSTER ?= sbx
# The cluster's kubeconfig lives in the repo (git-ignored), never in ~/.kube.
export KUBECONFIG ?= $(CURDIR)/bin/kubeconfig

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*## "} \
	  /^##@/ {printf "\n%s\n", substr($$0, 5)} \
	  /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: doctor
doctor: ## Check the local toolchain and Docker resources.
	@hack/doctor.sh

##@ Codegen

CRD_DIR := charts/sbx/crds

GENERATED := api gen $(CRD_DIR)

.PHONY: gen
gen: ## Regenerate deepcopy code, CRDs and gRPC stubs.
	$(TOOL) controller-gen object paths=./api/...
	$(TOOL) controller-gen crd paths=./api/... output:crd:dir=$(CRD_DIR)
	$(TOOL) buf lint
	$(TOOL) buf generate

.PHONY: verify-gen
verify-gen: gen ## Fail if generated files differ from what is committed.
	@if [ -n "$$(git status --porcelain -- $(GENERATED))" ]; then \
	  echo "generated files are out of date; run make gen and commit:"; \
	  git status --porcelain -- $(GENERATED); exit 1; fi

##@ Quality

.PHONY: fmt
fmt: ## Format Go code.
	$(GO) fmt $(PKGS)

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet $(PKGS)

.PHONY: lint
lint: ## Run golangci-lint.
	$(TOOL) golangci-lint run $(PKGS)

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and apply automatic fixes.
	$(TOOL) golangci-lint run --fix $(PKGS)

##@ Test

# Keep in step with the kind node image and internal/testutil/envtestx.
ENVTEST_K8S_VERSION ?= 1.35.0

.PHONY: envtest
envtest: ## Install kube-apiserver and etcd for envtest into bin/envtest.
	@$(TOOL) setup-envtest use -i $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path >/dev/null 2>&1 || \
	  $(TOOL) setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path

.PHONY: test
test: vet envtest extension web ## Run all unit tests (race detector, goroutine-leak checks, envtest), the extension's and the UI's.
	$(GO) test -race -shuffle=on $(PKGS)

EXTENSION := extensions/vscode-activity

$(EXTENSION)/node_modules/.package-lock.json: $(EXTENSION)/package-lock.json
	cd $(EXTENSION) && npm ci --no-audit --no-fund --silent

.PHONY: extension
extension: $(EXTENSION)/node_modules/.package-lock.json ## Type-check, test and build the editor activity extension.
	cd $(EXTENSION) && npm run --silent check && npm test --silent && npm run --silent build

WEB := web

$(WEB)/node_modules/.package-lock.json: $(WEB)/package-lock.json
	cd $(WEB) && npm ci --no-audit --no-fund --silent

.PHONY: web
web: $(WEB)/node_modules/.package-lock.json ## Type-check, test and build the web UI into the gateway (internal/gateway/webui/dist).
	cd $(WEB) && npm run --silent check && npm test --silent && npm run --silent build -- --logLevel warn

.PHONY: test-short
test-short: ## Run fast unit tests only (no race detector, no envtest).
	$(GO) test -short $(PKGS)

.PHONY: test-e2e
test-e2e: ## Run end-to-end tests on the kind cluster after make deploy (RUN=<regex> to filter).
	$(GO) test -tags e2e -count=1 -p 1 -timeout 20m ./test/e2e/... $(if $(RUN),-run '$(RUN)') -v

##@ Upstream

.PHONY: vendor-upstream
vendor-upstream: ## Refresh the vendored agent-sandbox Helm chart (pinned tag + commit).
	hack/vendor-agent-sandbox.sh

.PHONY: verify-vendor
verify-vendor: ## Fail if charts/agent-sandbox differs from the pinned upstream chart.
	hack/vendor-agent-sandbox.sh --check

##@ Cluster

.PHONY: kind-up
kind-up: ## Create or reuse the 3-node kind cluster and local registry (localhost:5001).
	KIND_CLUSTER=$(KIND_CLUSTER) hack/kind-up.sh

.PHONY: kind-down
kind-down: ## Delete the kind cluster (REGISTRY_TOO=1 also removes the registry).
	KIND_CLUSTER=$(KIND_CLUSTER) hack/kind-down.sh

##@ Build

ARCH     ?= $(shell $(GO) env GOARCH)
REGISTRY ?= localhost:5001
BINARIES := sbx-agent sbx-sidecar sbx-controller sbx-gateway
IMAGES   ?= sbx-runtime sbx-controller sbx-gateway sbx-fixtures
# Content-derived tag; computed after the binaries are built.
TAG = $(shell hack/image-tag.sh $(ARCH))

.PHONY: build-linux
build-linux: web ## Cross-compile the in-cluster binaries into bin/linux_$(ARCH) (the gateway embeds the UI).
	@for b in $(BINARIES); do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -buildvcs=false -ldflags "-s -w" \
	    -o bin/linux_$(ARCH)/$$b ./cmd/$$b || exit 1; \
	done

.PHONY: images
images: build-linux extension ## Build container images tagged with a content hash.
	@tag=$(TAG); for img in $(IMAGES); do \
	  docker build -q --platform linux/$(ARCH) --provenance=false -f images/$${img#sbx-}.Dockerfile \
	    -t $(REGISTRY)/$$img:$$tag . >/dev/null || exit 1; \
	  echo "built $(REGISTRY)/$$img:$$tag"; \
	done

.PHONY: push
push: images ## Push images to the local registry.
	@tag=$(TAG); for img in $(IMAGES); do docker push -q $(REGISTRY)/$$img:$$tag >/dev/null || exit 1; done; \
	  echo "pushed tag $$tag"

##@ Deploy

NAMESPACE ?= sbx-system
VALUES    ?= deploy/values/kind.yaml

# The umbrella chart's dependencies, rebuilt only when the lock or a local
# subchart changes (kube-prometheus-stack is downloaded once).
CHART_DEPS := charts/sbx/Chart.yaml charts/sbx/Chart.lock $(shell find charts/agent-sandbox charts/s3-local -type f)

charts/sbx/charts/.deps: $(CHART_DEPS)
	@helm repo list 2>/dev/null | grep -q '^prometheus-community' || \
	  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
	helm dependency build --skip-refresh charts/sbx >/dev/null
	@touch $@

.PHONY: deploy
deploy: push charts/sbx/charts/.deps ## Build, push and install the platform (VALUES=deploy/values/kind.yaml).
	@# Helm installs CRDs only on a first install; apply every chart's on each deploy.
	helm show crds charts/sbx | kubectl apply --server-side --force-conflicts -f - >/dev/null
	helm upgrade --install sbx charts/sbx --namespace $(NAMESPACE) --create-namespace \
	  -f $(VALUES) --set images.registry=$(REGISTRY) --set images.tag=$(TAG) --wait --timeout 8m

.PHONY: grafana
grafana: ## Open Grafana with the sbx dashboards on http://127.0.0.1:3000 (anonymous viewer on kind).
	@echo "Grafana: http://127.0.0.1:3000/dashboards?tag=sbx  (admin password: kubectl -n $(NAMESPACE) get secret sbx-grafana -o jsonpath='{.data.admin-password}' | base64 -d)"
	kubectl -n $(NAMESPACE) port-forward svc/sbx-grafana 3000:80

.PHONY: undeploy
undeploy: ## Uninstall the platform (CRDs and the signing key are kept).
	helm uninstall sbx --namespace $(NAMESPACE)

##@ Demo

DEMO_IMAGE ?= mcr.microsoft.com/devcontainers/javascript-node:22
DEMO_ARGS  ?=

.PHONY: demo
demo: ## Play an agent session that sleeps and wakes (run `make kind-up deploy` first).
	@runtime=$$(kubectl -n $(NAMESPACE) get deploy sbx-controller \
	  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SBX_RUNTIME_IMAGE")].value}'); \
	  NAMESPACE=$(NAMESPACE) hack/prepull.sh "$$runtime" "$(DEMO_IMAGE)"
	$(GO) build -o bin/sbxctl ./cmd/sbxctl
	@SBX_TOKEN=$$(kubectl -n $(NAMESPACE) get secret sbx-gateway-admin -o jsonpath='{.data.token}' | base64 -d) \
	  bin/sbxctl demo --image $(DEMO_IMAGE) $(DEMO_ARGS)

BENCH_ARGS ?=

.PHONY: bench
bench: ## Measure the platform on the real repositories in test/bench/repos.json; writes notes/bench-kind-<date>.json (BENCH_ARGS for sbx-bench flags).
	$(GO) build -o bin/sbx-bench ./cmd/sbx-bench
	NAMESPACE=$(NAMESPACE) hack/bench.sh --env kind $(BENCH_ARGS)

.PHONY: ui
ui: ## Print a link that opens the web UI signed in as admin (run `make kind-up deploy` first).
	@$(GO) build -o bin/sbxctl ./cmd/sbxctl
	@SBX_TOKEN=$$(kubectl -n $(NAMESPACE) get secret sbx-gateway-admin -o jsonpath='{.data.token}' | base64 -d) \
	  bin/sbxctl ui
