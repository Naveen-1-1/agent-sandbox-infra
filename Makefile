# Build, test and run the agent-sandbox platform locally.
# `make help` lists the targets.

SHELL := /bin/bash
GO    ?= go
PKGS  ?= ./...

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*## "} \
	  /^##@/ {printf "\n%s\n", substr($$0, 5)} \
	  /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Quality

.PHONY: fmt
fmt: ## Format Go code.
	$(GO) fmt $(PKGS)

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet $(PKGS)

##@ Test

.PHONY: test
test: vet ## Run all unit tests with the race detector and goroutine-leak checks.
	$(GO) test -race -shuffle=on $(PKGS)

.PHONY: test-short
test-short: ## Run fast unit tests only (no race detector, no envtest).
	$(GO) test -short $(PKGS)
