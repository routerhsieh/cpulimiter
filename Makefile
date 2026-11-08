.DEFAULT_GOAL := help

GO ?= go
BUILD_DIR ?= build
ARCHES ?= amd64 arm64

.PHONY: help build build-all build-linux build-darwin build-windows build-freebsd test test-race clean

help:
	@echo "make build          Build all packages and the hashing example for the host"
	@echo "make build-all      Cross-build Linux, macOS, Windows, and FreeBSD"
	@echo "make build-linux    Cross-build Linux (also: build-darwin, build-windows, build-freebsd)"
	@echo "make test           Run unit tests on the host"
	@echo "make test-race      Run host tests with the race detector (requires CGO and a C compiler)"
	@echo "make clean          Remove the build output directory (BUILD_DIR)"
	@echo "Options: GO=go BUILD_DIR=build ARCHES='amd64 arm64'"

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

clean:
	rm -rf -- "$(BUILD_DIR)"
