#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# The Go module must stand alone: copied outside the repository (only its own
# files: go.mod, go.sum and .go sources, no Rust tree, no contract scripts),
# built and tested with GOWORK off and cgo disabled, with no skipped tests.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$(mktemp -d)"
trap 'rm -rf "$dest"' EXIT

cd "$root"
# Copy the Go module's own files only (tracked .go sources plus go.mod/go.sum),
# preserving directories.
while IFS= read -r f; do
  mkdir -p "$dest/mod/$(dirname "$f")"
  cp "$f" "$dest/mod/$f"
done < <(git ls-files 'go.mod' 'go.sum' '*.go' | grep -v '^crates/' | grep -v '^contract/')

cd "$dest/mod"
log="$dest/go-test.log"
GOWORK=off CGO_ENABLED=0 go build ./...
GOWORK=off CGO_ENABLED=0 go test -count=1 -v ./... | tee "$log" > /dev/null
if grep -q -- '--- SKIP' "$log"; then
  echo "go-standalone: standalone Go tests must not skip" >&2
  exit 1
fi
echo "[ok] go-standalone"
