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
	@echo "make test-e2e-long  Include single/multi-core five-minute windows (about 2h 45m)"
	@echo "make clean          Remove the build output directory (BUILD_DIR)"
	@echo "Options: GO=go BUILD_DIR=build ARCHES='amd64 arm64'"
	@echo "E2E options: E2E_ARGS='' E2E_OUTPUT=$(E2E_OUTPUT)"
	@echo "Each E2E run creates a unique timestamped subdirectory under E2E_OUTPUT"
	@echo "Logs, PASS/FAIL/SKIP summaries, CPU analysis, and attempt diagnostics are saved automatically"
	@echo "Each run is also archived as .zip using Go (no external tar/zip required)"

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

test-e2e-long: E2E_TIMEOUT = 3h30m
test-e2e-long: E2E_MODE = -e2e.long

test-e2e test-e2e-smoke test-e2e-long:
	@set -e; \
		mkdir -p "$(E2E_OUTPUT)"; \
		e2e_output_dir=$$(cd "$(E2E_OUTPUT)" && pwd -P); \
		e2e_run_dir=$$(mktemp -d "$$e2e_output_dir/$$(date -u +%Y%m%dT%H%M%SZ)-$@-XXXXXX"); \
		echo "E2E reports: $$e2e_run_dir"; \
		printf '%s\n' '{"schema_version":1,"status":"BOOTSTRAPPING","tests":[]}' >"$$e2e_run_dir/summary.json"; \
		e2e_finish() { \
			e2e_status=$$?; \
			trap - 0; \
			if [ -x "$$e2e_run_dir/.runner" ] && \
				"$$e2e_run_dir/.runner" -archive-only -output "$$e2e_run_dir"; then \
				:; \
			else \
				echo "E2E ZIP unavailable; original reports remain in $$e2e_run_dir (see bootstrap.log or archive-error.log)" >&2; \
				if [ "$$e2e_status" -eq 0 ]; then e2e_status=1; fi; \
			fi; \
			rm -f "$$e2e_run_dir/.runner"; \
			exit "$$e2e_status"; \
		}; \
		trap e2e_finish 0; \
		if $(GO) build -o "$$e2e_run_dir/.runner" ./e2e/cmd/run >"$$e2e_run_dir/bootstrap.log" 2>&1 && \
			"$$e2e_run_dir/.runner" -go "$(GO)" -output "$$e2e_run_dir" -timeout $(E2E_TIMEOUT) -- $(E2E_MODE) $(E2E_ARGS) 2>>"$$e2e_run_dir/bootstrap.log"; then \
			:; \
		else \
			e2e_status=$$?; \
			cat "$$e2e_run_dir/bootstrap.log"; \
			if [ ! -f "$$e2e_run_dir/summary.md" ]; then \
				cat "$$e2e_run_dir/bootstrap.log" >>"$$e2e_run_dir/run.log"; \
				printf '%s\n' '{"schema_version":1,"status":"ERROR","exit_code":1,"tests":[],"errors":["report runner could not build, start or finalize; see bootstrap.log, run.log and any events.jsonl"]}' >"$$e2e_run_dir/summary.json"; \
				printf '%s\n' '# E2E ERROR' '' 'Report runner could not build, start or finalize. See bootstrap.log, run.log and any events.jsonl.' >"$$e2e_run_dir/summary.md"; \
			fi; \
			exit "$$e2e_status"; \
		fi

clean:
	rm -rf -- "$(BUILD_DIR)"
