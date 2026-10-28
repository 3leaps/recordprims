#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Exported-API scan (repository-classes P7, rule 2): no sibling prim package or
# crate path appears in recordprims' public API. Sibling prims are third-party
# for this rule: wrap them, never re-export them.
#
# Go: internal/tools/apiscan type-checks every non-internal package and
#     walks the types reachable from its exported API.
# Rust: no `pub use` / `pub extern crate` of a sibling crate, and no public
#     item signature naming a sibling crate path, in any crate's sources.
#     (A rustdoc-JSON based check, e.g. cargo public-api, replaces this once
#     the toolchain policy allows a nightly or its stable successor.)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${root}"
fail=0

SIBLING_RS='\b[a-z0-9_]*prims(_[a-z0-9_]+)?::'

# Go: type-check the exported API and fail on any reachable sibling type
# (import aliases and re-exports through internal packages included).
if ! CGO_ENABLED=0 go run ./internal/tools/apiscan; then
  fail=1
fi

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
