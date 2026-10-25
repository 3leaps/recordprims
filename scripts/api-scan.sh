#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Exported-API scan (repository-classes P7, rule 2): no sibling prim package or
# crate path appears in recordprims' public API. Sibling prims are third-party
# for this rule: wrap them, never re-export them.
#
# Go: the full exported surface from `go doc -all` for every non-internal
#     package must not mention a sibling module path.
# Rust: no `pub use` / `pub extern crate` of a sibling crate, and no public
#     item signature naming a sibling crate path, in any crate's sources.
#     (A rustdoc-JSON based check, e.g. cargo public-api, replaces this once
#     the toolchain policy allows a nightly or its stable successor.)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${root}"
fail=0

SIBLING_GO='github\.com/3leaps/[a-z0-9-]*prims'
SIBLING_RS='\b[a-z0-9_]*prims(_[a-z0-9_]+)?::'

# Go: every non-internal package's exported API. `go doc` prints sibling types
# by package name (e.g. canonprims.Digest), so resolve each sibling import of
# the package to its package name and look for "<name>." in the exported API.
for pkg in $(CGO_ENABLED=0 go list ./... | tr -d '\r' | grep -v '/internal'); do
  api="$(CGO_ENABLED=0 go doc -all "$pkg" 2>/dev/null || true)"
  for imp in $(CGO_ENABLED=0 go list -f '{{join .Imports "\n"}}' "$pkg" | tr -d '\r' | grep -E "^${SIBLING_GO}" | grep -v '^github.com/3leaps/recordprims' || true); do
    name="$(CGO_ENABLED=0 go list -f '{{.Name}}' "$imp" | tr -d '\r')"
    hits="$(printf '%s\n' "$api" | grep -nE "(^|[^A-Za-z0-9_])${name}\." || true)"
    if [ -n "$hits" ]; then
      echo "api-scan: Go package $pkg exposes sibling $imp in its exported API:" >&2
      printf '  %s\n' "$hits" >&2
      fail=1
    fi
  done
done

# Rust: re-exports and public signatures that name a sibling crate.
while IFS= read -r -d '' f; do
  hits="$(grep -nE "^\s*pub(\([a-z]+\))?\s+(use|extern crate)\s+" "$f" | grep -E "$SIBLING_RS|extern crate [a-z0-9_]*prims" | grep -v "recordprims" || true)"
  sig="$(grep -nE "^\s*pub(\([a-z]+\))?\s+(fn|struct|enum|trait|type|const|static)\b" "$f" | grep -E "$SIBLING_RS" | grep -v "recordprims::" || true)"
  for h in "$hits" "$sig"; do
    if [ -n "$h" ]; then
      echo "api-scan: Rust public API names a sibling crate in $f:" >&2
      printf '  %s\n' "$h" >&2
      fail=1
    fi
  done
done < <(find crates -name '*.rs' -not -path '*/target/*' -print0)

[ "$fail" -eq 0 ] && echo "[ok] api-scan"
exit "$fail"
