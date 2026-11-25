# recordprims: durable-record/v1 store (pure Go + Rust twin)
#
# `make check` is everything CI runs on one host.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
PYTHON ?= $(shell for p in python3 python3.14 python3.13 python3.12; do if command -v $$p >/dev/null 2>&1 && $$p -P -B -E -c 'import yaml' >/dev/null 2>&1; then echo $$p; exit 0; fi; done; echo python3)
export PYTHON

.DEFAULT_GOAL := help
.PHONY: all help check check-platform precommit fmt fmt-check lint test test-go test-rust \
        example-check example-schemas lint-yaml fmt-yaml \
        test-faultpoints faultpoint-exclusion contract-check contract-oracle conformance crosscheck unsafe-check unsafe-check-test deps-check api-scan api-scan-test deny go-standalone surface-scan content-check \
        surface-tests prepush pr-final workflow-pin release-tooling-test checklist-targets doctor doctor-test \
        release-ceremony-test release-export-pin release-validate-pin release-insert-anchors \
        release-github-preflight release-preflight release-prepare-tag-message release-tag release-push-tag release-verify-tag \
        release-verify-remote-tag release-record-anchor release-download release-notes release-checksums \
        release-sign release-verify release-create-draft release-publish release-recover-draft release-verify-published \
        license-check license-check-test version version-check version-sync version-set version-patch \
        version-minor version-major build clean

all: check

help: ## Show available targets
	@awk 'BEGIN{FS=":.*## "} /^[a-z][a-z0-9-]*:.*## /{printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check-platform: fmt-check lint test example-check test-faultpoints faultpoint-exclusion contract-check conformance crosscheck unsafe-check unsafe-check-test deps-check api-scan api-scan-test content-check surface-tests checklist-targets version-check license-check license-check-test ## Platform-native subset of check (no goneat, cargo-deny, or PyYAML required)

check: check-platform contract-oracle workflow-pin release-tooling-test deny lint-yaml ## Everything CI runs on one host

precommit: fmt-check lint ## Fast pre-commit gate

prepush: check doctor-test surface-scan ## Pre-push gate (the surface scan is skipped without a catalog)
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

workflow-pin: ## Every workflow and local action matches its reviewed digest and is read-only
	@$(PYTHON) -P -B -E scripts/release-workflow-pin.py check

checklist-targets: ## Every make target named in RELEASE_CHECKLIST.md exists
	@scripts/checklist-targets.sh

release-tooling-test: ## Controls for the release tooling (synthetic; no keys or credentials)
	@PYTHON=$(PYTHON) scripts/release-workflow-pin.test.sh
	@scripts/release-restore-tag-ref.test.sh

# The doctor reports maintainer host tools; its controls stub them on PATH with
# symlinks, so they run on maintainer hosts (prepush), not on Windows CI.
doctor-test: ## Controls for make doctor (maintainer hosts)
	@scripts/doctor.test.sh

# Release-ceremony controls need gpg and decernor (RECORDPRIMS_DECERNOR_BIN,
# or decernor on PATH for tests). They use throwaway keys only and run on
# maintainer hosts, not in CI.
release-ceremony-test: ## Synthetic release-ceremony controls (throwaway keys; maintainer hosts)
	@scripts/release-github-preflight.test.sh
	@scripts/release-validate-pin.test.sh
	@scripts/release-anchors.test.sh
	@scripts/release-gh.test.sh
	@scripts/release-tag-controls.test.sh
	@scripts/release-verify-tag.test.sh
	@scripts/verify-pinned-tag.test.sh
	@scripts/release-verify-published-tag.test.sh
	@scripts/release-page.test.sh

# Release
# -----------------------------------------------------------------------------
#
# A release is one signed annotated tag, vX.Y.Z, at a commit of main; it
# releases the root Go module (and the Rust crate once that is published). The tag is an
# explicit input (RECORDPRIMS_RELEASE_TAG); it is never derived from VERSION,
# git describe or the branch, and a bare RELEASE_TAG from any origin is refused
# so it cannot stand in for it.
#
# Maintainer inputs (local environment; never in this tree):
#   RECORDPRIMS_RELEASE_TAG             vX.Y.Z
#   RECORDPRIMS_TAG_MESSAGE_DIR         per-release message directory ending in the tag
#   RECORDPRIMS_DECERNOR_BIN            absolute path to decernor >= 0.1.8 (no fallback)
#   RECORDPRIMS_TAGGER_NAME/EMAIL       the committed tagger identity
#   RECORDPRIMS_PGP_KEY_ID              the committed signing subkey followed by !
#   RECORDPRIMS_GPG_HOMEDIR             external GPG home
#   RECORDPRIMS_GPG_SIGNING_FINGERPRINT the approved primary fingerprint
#   RECORDPRIMS_RELEASE_ANCHOR_FILE     external ceremony anchor record
#
# The public pins are committed (docs/security, keys/, config/release) and
# every verify step reads them from the tagged commit. CI holds no signing,
# release or registry authority.

# Release stages must not overlap under `make -j`.
.NOTPARALLEL:

ifneq ($(origin RELEASE_TAG),undefined)
$(error RELEASE_TAG is not accepted; set RECORDPRIMS_RELEASE_TAG=vX.Y.Z)
endif
REQUIRE_RELEASE_TAG = @if [ -z "$(RECORDPRIMS_RELEASE_TAG)" ]; then \
	echo "error: RECORDPRIMS_RELEASE_TAG=vX.Y.Z is required"; exit 1; fi

release-github-preflight: ## Read-only preflight for GitHub-hosted release settings (workflow token, v* tag ruleset, immutable releases)
	@scripts/release-github-preflight.sh

release-preflight: ## Pre-tag gate on a clean main synced with origin
	@source scripts/release-decernor.sh && resolve_release_decernor && echo "[ok] checker $$RELEASE_DECERNOR_BIN"
	@test -z "$$(git status --porcelain)" || { echo "error: working tree not clean"; git status --short; exit 1; }
	@test "$$(git symbolic-ref --quiet --short HEAD)" = main || { echo "error: release from main"; exit 1; }
	git fetch --quiet origin main
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" || { echo "error: HEAD differs from origin/main"; exit 1; }
	$(MAKE) release-github-preflight
	$(MAKE) check doctor-test release-ceremony-test
	RECORDPRIMS_SURFACE_SCAN_REQUIRED=1 RECORDPRIMS_SURFACE_SCOPE=release PYTHON=$(PYTHON) scripts/surface-scan.sh
	@v="$$(cat VERSION)"; grep -q "^## \[$$v\]" CHANGELOG.md || { echo "error: CHANGELOG.md has no [$$v] section"; exit 1; }; \
	  grep -q "^\[$$v\]: " CHANGELOG.md || { echo "error: CHANGELOG.md has no [$$v] compare link"; exit 1; } ; \
	  test -f "docs/releases/v$$v.md" || { echo "error: docs/releases/v$$v.md is required"; exit 1; }; \
	  grep -q "^## v$$v " RELEASE_NOTES.md || { echo "error: RELEASE_NOTES.md has no v$$v entry"; exit 1; }
	@echo "[ok] release preflight; next: make release-prepare-tag-message"

release-prepare-tag-message: ## Prepare the release tag message (outside the repo)
	$(REQUIRE_RELEASE_TAG)
	scripts/release-tag-operator.sh prepare-message

release-tag: ## Create the release tag, signed and local only
	$(REQUIRE_RELEASE_TAG)
	scripts/release-tag-operator.sh local-tag

release-push-tag: ## Push the verified release tag, then verify it
	$(REQUIRE_RELEASE_TAG)
	scripts/release-tag-operator.sh remote-push

release-verify-tag: ## Verify the local release tag against the committed pin
	$(REQUIRE_RELEASE_TAG)
	scripts/release-verify-tag.sh

release-verify-remote-tag: ## Verify the pushed release tag against the committed pin and GitHub
	$(REQUIRE_RELEASE_TAG)
	scripts/release-verify-published-tag.sh

release-record-anchor: ## Record the approved tag object and commit (no overwrite)
	$(REQUIRE_RELEASE_TAG)
	scripts/release-record-anchor.sh

# Release page (after release-record-anchor). Every step reads the tag object
# and commit from RECORDPRIMS_RELEASE_ANCHOR_FILE. Assets stage in
# RECORDPRIMS_RELEASE_DIR (default dist/release/<tag> in this checkout).
RELEASE_DIR = $(abspath $(or $(RECORDPRIMS_RELEASE_DIR),dist/release/$(RECORDPRIMS_RELEASE_TAG)))

release-download: ## Fetch and check the CI source archive for the anchored tag (RECORDPRIMS_RELEASE_BACKFILL=1 for a tag without release.yml)
	$(REQUIRE_RELEASE_TAG)
	scripts/release-stage.sh download "$(RELEASE_DIR)"

release-notes: ## Stage the public files and the page body from the tagged commit
	$(REQUIRE_RELEASE_TAG)
	scripts/release-stage.sh public "$(RELEASE_DIR)"

release-checksums: ## Write SHA256SUMS and SHA512SUMS over the staged files
	$(REQUIRE_RELEASE_TAG)
	scripts/release-stage.sh checksums "$(RELEASE_DIR)"

release-sign: ## Sign both manifests (minisign and the OpenPGP subkey), then verify
	$(REQUIRE_RELEASE_TAG)
	scripts/release-sign.sh "$(RELEASE_DIR)"

release-verify: ## Verify the staged release page against the anchored commit
	$(REQUIRE_RELEASE_TAG)
	scripts/release-verify-assets.sh "$(RELEASE_DIR)"

release-create-draft: ## Create the one draft release with every verified asset
	$(REQUIRE_RELEASE_TAG)
	scripts/release-page.sh draft "$(RELEASE_DIR)"

release-publish: ## Publish the draft if it still holds the verified bytes; require immutable
	$(REQUIRE_RELEASE_TAG)
	scripts/release-page.sh publish "$(RELEASE_DIR)"

release-recover-draft: ## Complete or delete a partial draft (RECORDPRIMS_RELEASE_DRAFT_ID, RECORDPRIMS_RELEASE_DRAFT_ACTION=upload|delete)
	$(REQUIRE_RELEASE_TAG)
	scripts/release-page.sh recover-draft "$(RELEASE_DIR)"

release-verify-published: ## Download the published release and verify it
	$(REQUIRE_RELEASE_TAG)
	scripts/release-page.sh verify-published

release-export-pin: ## One time: write the public signing pins for review (refuses overwrite)
	scripts/release-export-pin.sh

release-validate-pin: ## Validate the committed public pins and anchors
	scripts/release-validate-pin.sh

release-insert-anchors: ## Derive the fingerprint anchors with decernor (refuses overwrite)
	scripts/release-insert-anchors.sh

doctor: ## Report host tool versions against the recommended minimums (DOCTOR_SCOPE=release|dev)
	@scripts/doctor.sh $(or $(DOCTOR_SCOPE),release)

fmt: ## Format Go and Rust
	gofmt -w $$(git ls-files '*.go')
	cargo fmt --all

YAMLFMT ?= yamlfmt
YAMLLINT ?= yamllint
YAML_FILES = .yamlfmt .yamllint .fulmen/repo.yaml '.github/**/*.yml' '.github/**/*.yaml' '.goneat/**/*.yaml'

lint-yaml: ## YAML two-space format and yamllint (.yamlfmt, .yamllint)
	$(YAMLFMT) -dstar -lint $(YAML_FILES)
	$(YAMLLINT) --strict -c .yamllint .yamlfmt .yamllint .fulmen .github .goneat

fmt-yaml: ## Format YAML with .yamlfmt (a workflow change also needs its pin updated)
	$(YAMLFMT) -dstar $(YAML_FILES)

fmt-check: ## Check formatting
	@out="$$(gofmt -l $$(git ls-files '*.go'))"; if [ -n "$$out" ]; then echo "gofmt: $$out" >&2; exit 1; fi
	cargo fmt --all -- --check

lint: ## go vet and clippy
	CGO_ENABLED=0 go vet ./...
	cargo clippy --locked --all-targets -- -D warnings

test: test-go test-rust ## Go and Rust tests

test-go: ## Go tests (cgo-free)
	CGO_ENABLED=0 go test -count=1 ./...

EXAMPLE_DIR := examples/project-record-writer

example-schemas: ## The example's payload schemas admit their valid examples and refuse the others
	CGO_ENABLED=0 go run ./internal/tools/schemacheck $(EXAMPLE_DIR)/registry/record $(EXAMPLE_DIR)/registry/decision $(EXAMPLE_DIR)/registry/project-update $(EXAMPLE_DIR)/registry/risk

example-check: example-schemas ## Build, vet and test the example module (its own go.mod)
	cd $(EXAMPLE_DIR) && CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test -count=1 ./...
	@echo "[ok] example-check"

test-rust: ## Rust tests
	cargo test --locked

contract-check: ## Vendored contract matches contract/PIN.md
	$(PYTHON) -I -B scripts/contract-check.py

contract-oracle: ## Vendored durable-record/v1 interpreter and record-collection/v0 controls pass their own vectors
	cd contract/crucible && $(PYTHON) -I -B scripts/test-durable-record-v1-controls.py > /dev/null
	cd contract/crucible && $(PYTHON) -I -B scripts/test-record-collection-controls.py > /dev/null
	@echo "[ok] contract-oracle"

test-faultpoints: ## Failure-injection tests (builds tagged recordprims_faultpoints)
	CGO_ENABLED=0 go test -count=1 -tags recordprims_faultpoints ./internal/jfile

faultpoint-exclusion: ## Release builds compile no failure-injection seam
	@PYTHON=$(PYTHON) scripts/faultpoint-exclusion.sh

conformance: ## The v1 vectors through the implemented stages; the ledger matches the run
	CGO_ENABLED=0 go vet -tags recordprims_conformance ./internal/conformance
	CGO_ENABLED=0 go test -count=1 -tags recordprims_conformance ./internal/conformance

crosscheck: ## Differential store/body check and cross-open between Go and Rust
	@PYTHON=$(PYTHON) scripts/crosscheck.sh

unsafe-check: ## Only crates/recordprims/src/sys/windows.rs may carry #[allow(unsafe_code)]
	@scripts/unsafe-check.sh

unsafe-check-test: ## Controls for unsafe-check
	@scripts/unsafe-check.test.sh

deps-check: ## Sibling-prim allow-list; cgo-free Go tree
	PYTHON=$(PYTHON) scripts/deps-check.sh
	@echo "[ok] deps-check"

api-scan: ## No sibling prim type in the exported API (Go and Rust)
	@scripts/api-scan.sh

api-scan-test: ## Controls for api-scan: each leak route is refused through the real gate
	@scripts/api-scan.test.sh

license-check: ## License files are the verbatim texts; notices live in NOTICE
	@$(PYTHON) -I -B scripts/license-check.py

license-check-test: ## Controls for license-check: each known drift is refused
	@PYTHON=$(PYTHON) scripts/license-check.test.sh

deny: ## cargo-deny: licences, bans, sources, advisories
	cargo deny --locked check

go-standalone: ## The Go module stands alone: copied outside the repo, no skipped tests
	@scripts/go-standalone.sh


version: ## Print the version
	@cat VERSION

# VERSION is the single source of truth. Change it only through these targets;
# they write every version site (Cargo.toml, Cargo.lock) from it.
version-check: ## Every version site matches VERSION
	@$(PYTHON) -I -B scripts/version-sync.py check

version-sync: ## Write VERSION to every version site
	@$(PYTHON) -I -B scripts/version-sync.py

version-set: ## Set an explicit version (V=X.Y.Z), then sync
	@[[ "$(V)" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$$ ]] || { echo "usage: make version-set V=X.Y.Z" >&2; exit 1; }
	@printf '%s\n' "$(V)" >VERSION
	@$(PYTHON) -I -B scripts/version-sync.py

version-patch: ## Bump the patch version, then sync
	@IFS=. read -r major minor patch <VERSION; printf '%s\n' "$$major.$$minor.$$((patch + 1))" >VERSION
	@$(PYTHON) -I -B scripts/version-sync.py

version-minor: ## Bump the minor version, then sync
	@IFS=. read -r major minor patch <VERSION; printf '%s\n' "$$major.$$((minor + 1)).0" >VERSION
	@$(PYTHON) -I -B scripts/version-sync.py

version-major: ## Bump the major version, then sync
	@IFS=. read -r major minor patch <VERSION; printf '%s\n' "$$((major + 1)).0.0" >VERSION
	@$(PYTHON) -I -B scripts/version-sync.py

build: ## Build both trees
	CGO_ENABLED=0 go build ./...
	cargo build --locked

clean: ## Remove build output
	cargo clean
