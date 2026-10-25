#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Sibling-prim dependency rule (crucible repository-classes standard, section 4.1):
# recordprims may depend only on the sibling prims allow-listed here, per tree.
# The Go tree must also stay cgo-free and depend only on allow-listed modules.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${root}"
fail=0

# Allow-lists. Add an entry only with the standard's rules met: a pinned
# version (a tag once the sibling is public; an exact commit during
# pre-release) and no sibling type in recordprims' public API.
RUST_ALLOWED_PRIMS="canonprims canonprims-core canonprims-json storageprims"
GO_ALLOWED_MODULES="github.com/3leaps/canonprims"

# Rust: no *prims crate outside the allow-list anywhere in the resolved graph.
if ! metadata="$(cargo metadata --locked --format-version 1 2>/dev/null)"; then
  echo "deps-check: cargo metadata failed; the Rust dependency graph was not checked" >&2
  fail=1
elif ! printf '%s' "$metadata" | ALLOWED="$RUST_ALLOWED_PRIMS" "${PYTHON:-python3}" -I -c '
import json, os, re, sys
allowed = set(os.environ["ALLOWED"].split())
prim = re.compile(r"^[a-z0-9_-]*prims(?:[-_][a-z0-9_-]*)?$")
names = {p["name"].lower() for p in json.load(sys.stdin)["packages"]}
bad = sorted(n for n in names if prim.match(n) and n != "recordprims" and not n.startswith("recordprims-") and n not in allowed)
print("\n".join(bad))
sys.exit(1 if bad else 0)'; then
  echo "deps-check: a non-allow-listed prim appears in the Rust dependency graph" >&2
  fail=1
fi

# Go: every non-standard dependency must be allow-listed, and the build is cgo-free.
go_deps="$(GOWORK=off CGO_ENABLED=0 go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | tr -d '\r' | grep -v '^github.com/3leaps/recordprims' || true)"
for dep in $go_deps; do
  ok=0
  for allowed in $GO_ALLOWED_MODULES; do
    case "$dep" in "$allowed"|"$allowed"/*) ok=1 ;; esac
  done
  if [ "$ok" -ne 1 ]; then
    echo "deps-check: Go dependency not allow-listed: $dep" >&2
    fail=1
  fi
done
if grep -rlE '^\s*import\s+"C"' --include='*.go' --exclude-dir=target --exclude-dir=.git . >/dev/null 2>&1; then
  echo "deps-check: the Go tree must be cgo-free (found import \"C\")" >&2
  fail=1
fi

exit "$fail"
