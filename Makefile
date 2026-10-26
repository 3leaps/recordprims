# recordprims: durable-record/v1 store (pure Go + Rust twin)
#
# `make check` is everything CI runs on one host.

SHELL := bash
PYTHON ?= python3

.DEFAULT_GOAL := help
.PHONY: all help check precommit fmt fmt-check lint test test-go test-rust \
        contract-check contract-oracle deps-check api-scan deny go-standalone surface-scan content-check \
        surface-tests prepush pr-final \
        bootstrap release-check version version-check build clean

all: check

help: ## Show available targets
	@awk 'BEGIN{FS=":.*## "} /^[a-z][a-z0-9-]*:.*## /{printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check: fmt-check lint test contract-check contract-oracle deps-check api-scan content-check surface-tests deny version-check ## Everything CI runs on one host

precommit: fmt-check lint ## Fast pre-commit gate

prepush: check surface-scan ## Pre-push gate (the surface scan is skipped without a catalog)
	@echo "[ok] prepush"

# Before opening or updating a PR: everything CI runs, plus the maintainer's
# public-surface scan, which is required here, including the PR title and body
# in the file named by RECORDPRIMS_SURFACE_PR_TEXT.
pr-final: check ## Before opening or updating a PR: check plus the required surface scan
	@RECORDPRIMS_SURFACE_SCAN_REQUIRED=1 PYTHON=$(PYTHON) scripts/surface-scan.sh
	@echo "[ok] pr-final"

# The public-surface scan uses limensafe with a maintainer catalog named by
# LIMENSAFE_CATALOG_PATH, outside this repository. It never runs in CI.
surface-scan: ## Public-surface scan (tree, commit messages, PR text, branch) when LIMENSAFE_CATALOG_PATH is set
	@PYTHON=$(PYTHON) scripts/surface-scan.sh

# Vocabulary-free shapes only (local paths, keys, tokens, commit markers); runs
# in CI as part of check.
content-check: ## Refuse local paths, private keys, tokens and commit markers in tracked files
	@$(PYTHON) -I -B scripts/content-check.py

surface-tests: ## Controls for content-check and the surface scan (the scan suite skips without limensafe)
	@PYTHON=$(PYTHON) scripts/content-check.test.sh
	@PYTHON=$(PYTHON) scripts/surface-scan.test.sh

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

api-scan: ## No sibling prim type in the exported API (Go and Rust)
	@scripts/api-scan.sh

deny: ## cargo-deny: licences, bans, sources, advisories
	cargo deny --locked check

go-standalone: ## The Go module stands alone: copied outside the repo, no skipped tests
	@scripts/go-standalone.sh

bootstrap: ## Install the pinned CI tools (goneat, cargo-deny) into ./.tools and print the PATH to use
	@dest="$$(scripts/ci-install-tools.sh "$$(rustc -vV | awk '/^host:/{print $$2}')" .tools)"; \
	  echo "[ok] bootstrap: add $$dest to PATH"

release-check: ## Release readiness (pre-release: no release machinery yet)
	@echo "release-check: recordprims is pre-release; release machinery (signing, provenance, publish) lands before the first tag" >&2
	@exit 1

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
