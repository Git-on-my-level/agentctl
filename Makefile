SHELL := /bin/sh

GO ?= go
PYTHON ?= python3
BINARY_NAME ?= agentctl
MAIN_PKG ?= ./cmd/agentctl
BUILD_DIR ?= build
DIST_DIR ?= dist
SCRIPTS_DIR := scripts
BIN := $(BUILD_DIR)/$(BINARY_NAME)

# These flags keep paths, VCS stamping, and linker symbol tables out of a
# release binary.  A caller may add flags through GOFLAGS/LDFLAGS.
GOFLAGS ?= -trimpath -buildvcs=false
LDFLAGS ?= -s -w -X main.version=$(VERSION)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf '%s' dev)
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || printf '%s' unknown)

.PHONY: all ci fmt fmt-check vet test build check-ids check-schemas check-links check-scripts check-portable-assets check-distribution release \
	install uninstall clean

all: ci

ci: fmt-check vet test check-ids check-schemas check-links check-scripts check-portable-assets check-distribution build

fmt:
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './.git/*')"; \
	if [ -n "$$files" ]; then gofmt -w $$files; fi

fmt-check:
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './.git/*')"; \
	if [ -n "$$files" ]; then \
		bad="$$(gofmt -l $$files)"; \
		if [ -n "$$bad" ]; then printf '%s\n' "Go files need gofmt:" $$bad >&2; exit 1; fi; \
	fi

vet:
	@test -f go.mod || { echo 'go.mod is required for vet' >&2; exit 1; }
	GOFLAGS='$(GOFLAGS)' $(GO) vet ./...

test:
	@test -f go.mod || { echo 'go.mod is required for tests' >&2; exit 1; }
	GOFLAGS='$(GOFLAGS)' $(GO) test ./...

check-ids:
	$(PYTHON) $(SCRIPTS_DIR)/id-reference.py

build:
	@test -f go.mod || { echo 'go.mod is required for build' >&2; exit 1; }
	@mkdir -p '$(BUILD_DIR)'
	GOFLAGS='$(GOFLAGS)' $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o '$(BIN)' '$(MAIN_PKG)'

check-schemas:
	$(PYTHON) $(SCRIPTS_DIR)/check-schemas.py

check-links:
	$(PYTHON) $(SCRIPTS_DIR)/check-links.py

check-scripts:
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck $(SCRIPTS_DIR)/*.sh; else bash -n $(SCRIPTS_DIR)/*.sh; fi

check-portable-assets:
	@test -f go.mod || { echo 'go.mod is required for portable asset checks' >&2; exit 1; }
	GOFLAGS='$(GOFLAGS)' $(GO) test ./internal/portableasset -run TestEmbeddedDistributionMatchesCanonicalSources -count=1

check-distribution:
	tests/validators/test_distributions.sh
	tests/validators/test_install_scripts.sh
	@if [ -x tests/validators/test_supervisor_scripts.sh ]; then tests/validators/test_supervisor_scripts.sh; fi

release:
	VERSION='$(VERSION)' COMMIT='$(COMMIT)' MAIN_PKG='$(MAIN_PKG)' \
		$(SCRIPTS_DIR)/build-release.sh --output-dir '$(DIST_DIR)'

install: build
	$(SCRIPTS_DIR)/install.sh --binary '$(BIN)' --force

uninstall:
	$(SCRIPTS_DIR)/uninstall.sh $(if $(FORCE),--force,)

clean:
	@rm -rf '$(BUILD_DIR)'
