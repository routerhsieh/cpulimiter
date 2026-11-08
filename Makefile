.DEFAULT_GOAL := help

GO ?= go
BUILD_DIR ?= build
ARCHES ?= amd64 arm64
E2E_ARGS ?=
E2E_OUTPUT ?= $(BUILD_DIR)/e2e

.PHONY: help build build-all build-linux build-darwin build-windows build-freebsd test test-race test-e2e test-e2e-smoke test-e2e-long clean

help:
	@echo "make build          Build all packages and the hashing example for the host"
	@echo "make build-all      Cross-build Linux, macOS, Windows, and FreeBSD"
	@echo "make build-linux    Cross-build Linux (also: build-darwin, build-windows, build-freebsd)"
	@echo "make test           Run unit tests on the host"
	@echo "make test-race      Run host tests with the race detector (requires CGO and a C compiler)"
	@echo "make test-e2e       Run macOS/Linux CPU workload tests (several minutes)"
	@echo "make test-e2e-smoke Run a smaller CPU workload matrix"
	@echo "make test-e2e-long  Include five-minute windows (about an hour)"
	@echo "make clean          Remove the build output directory (BUILD_DIR)"
	@echo "Options: GO=go BUILD_DIR=build ARCHES='amd64 arm64'"
	@echo "E2E options: E2E_ARGS='' E2E_OUTPUT=$(E2E_OUTPUT)"
	@echo "Each E2E run creates a unique timestamped subdirectory under E2E_OUTPUT"

build:
	$(GO) build ./...
	mkdir -p "$(BUILD_DIR)/native"
	$(GO) build -o "$(BUILD_DIR)/native/" ./examples/hashing

build-all: build-linux build-darwin build-windows build-freebsd

# Cross-builds do not execute tests or require a cross-platform C compiler.
# Go selects the hashing executable's extension for each target OS.
build-linux build-darwin build-windows build-freebsd: build-%:
	@set -e; for arch in $(ARCHES); do \
		echo "Building $*/$$arch"; \
		CGO_ENABLED=0 GOOS=$* GOARCH=$$arch $(GO) build ./...; \
		mkdir -p "$(BUILD_DIR)/$*/$$arch"; \
		CGO_ENABLED=0 GOOS=$* GOARCH=$$arch $(GO) build -o "$(BUILD_DIR)/$*/$$arch/" ./examples/hashing; \
	done

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-e2e: E2E_TIMEOUT = 15m
test-e2e: E2E_MODE =

test-e2e-smoke: E2E_TIMEOUT = 5m
test-e2e-smoke: E2E_MODE = -e2e.smoke

test-e2e-long: E2E_TIMEOUT = 75m
test-e2e-long: E2E_MODE = -e2e.long

test-e2e test-e2e-smoke test-e2e-long:
	@set -e; \
		mkdir -p "$(E2E_OUTPUT)"; \
		e2e_output_dir=$$(cd "$(E2E_OUTPUT)" && pwd -P); \
		e2e_run_dir=$$(mktemp -d "$$e2e_output_dir/$$(date -u +%Y%m%dT%H%M%SZ)-$@-XXXXXX"); \
		echo "E2E reports: $$e2e_run_dir"; \
		$(GO) test -tags=e2e -count=1 -v -timeout=$(E2E_TIMEOUT) ./e2e $(E2E_MODE) -e2e.output="$$e2e_run_dir" $(E2E_ARGS)

clean:
	rm -rf -- "$(BUILD_DIR)"
