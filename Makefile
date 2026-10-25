# recordprims: durable-record/v1 store (pure Go + Rust twin)
#
# `make check` is everything CI runs on one host.

SHELL := bash
PYTHON ?= python3

.DEFAULT_GOAL := help
.PHONY: all help check precommit fmt fmt-check lint test test-go test-rust \
        contract-check contract-oracle deps-check surface-scan pr-final version version-check build clean

all: check

help: ## Show available targets
	@awk 'BEGIN{FS=":.*## "} /^[a-z][a-z0-9-]*:.*## /{printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check: fmt-check lint test contract-check contract-oracle deps-check version-check ## Everything CI runs on one host

precommit: fmt-check lint ## Fast pre-commit gate

pr-final: check surface-scan ## Before opening or updating a PR: check plus the maintainer surface scan
	@echo "[ok] pr-final"

# The maintainer's public-surface scan: LIMENSAFE_CATALOG_PATH names a catalog
# (catalog_id recordprims-release) outside the repository. Unset, it is skipped
# with a warning; RECORDPRIMS_SURFACE_SCAN_REQUIRED=1 makes it mandatory.
surface-scan: ## Public-surface scan (tree + proposed commit messages), when LIMENSAFE_CATALOG_PATH is set
	@PYTHON=$(PYTHON) scripts/surface-scan.sh

fmt: ## Format Go and Rust
	gofmt -w $$(git ls-files '*.go')
	cargo fmt --all

fmt-check: ## Check formatting
	@out="$$(gofmt -l $$(git ls-files '*.go'))"; if [ -n "$$out" ]; then echo "gofmt: $$out" >&2; exit 1; fi
	cargo fmt --all -- --check

lint: ## go vet and clippy
	CGO_ENABLED=0 go vet ./...
	cargo clippy --locked --all-targets -- -D warnings

test: test-go test-rust ## Go and Rust tests

test-go: ## Go tests (cgo-free)
	CGO_ENABLED=0 go test -count=1 ./...

test-rust: ## Rust tests
	cargo test --locked

contract-check: ## Vendored contract matches contract/PIN.md
	$(PYTHON) -I -B scripts/contract-check.py

contract-oracle: ## Vendored durable-record/v1 interpreter passes its own vectors
	cd contract/crucible && $(PYTHON) -I -B scripts/test-durable-record-v1-controls.py > /dev/null
	@echo "[ok] contract-oracle"

deps-check: ## Sibling-prim allow-list; cgo-free Go tree
	PYTHON=$(PYTHON) scripts/deps-check.sh
	@echo "[ok] deps-check"

version: ## Print the version
	@cat VERSION

version-check: ## VERSION matches Cargo.toml
	@v="$$(cat VERSION)"; c="$$(awk -F'"' '/^\[workspace.package\]/{f=1} f&&/^version/{print $$2; exit}' Cargo.toml)"; \
	  if [ "$$v" != "$$c" ]; then echo "version-check: VERSION=$$v Cargo.toml=$$c" >&2; exit 1; fi; echo "[ok] version-check $$v"

build: ## Build both trees
	CGO_ENABLED=0 go build ./...
	cargo build --locked

clean: ## Remove build output
	cargo clean
