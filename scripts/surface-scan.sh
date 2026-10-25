#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Public-surface scan of the tracked tree and of the commit messages being
# proposed, using limensafe with a maintainer catalog kept outside the repo.
#
#   LIMENSAFE_CATALOG_PATH     absolute path to a catalog whose catalog_id is
#                              recordprims-release; it must resolve outside this
#                              repository. Unset: warn and skip (the scan is the
#                              maintainer's gate, not available in public CI),
#                              unless RECORDPRIMS_SURFACE_SCAN_REQUIRED=1.
#                              Set but empty: fail.
#   RECORDPRIMS_LIMENSAFE_BIN  optional absolute path to the limensafe binary
#                              (default: limensafe on PATH).
#   RECORDPRIMS_SURFACE_BASE   base ref for the commit-message scan
#                              (default: origin/main).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$root"

if [[ -z "${LIMENSAFE_CATALOG_PATH+set}" ]]; then
  if [[ "${RECORDPRIMS_SURFACE_SCAN_REQUIRED:-0}" == 1 ]]; then
    echo "error: LIMENSAFE_CATALOG_PATH is required for this gate" >&2
    exit 1
  fi
  echo "[skip] surface scan: LIMENSAFE_CATALOG_PATH is not set"
  exit 0
fi
catalog="$LIMENSAFE_CATALOG_PATH"
[[ -n "$catalog" ]] || { echo "error: LIMENSAFE_CATALOG_PATH is set but empty" >&2; exit 1; }
[[ "$catalog" == /* && -f "$catalog" ]] ||
  { echo "error: LIMENSAFE_CATALOG_PATH must be an absolute path to a file" >&2; exit 1; }
resolved="$(cd "$(dirname "$catalog")" && pwd -P)/$(basename "$catalog")"
if [[ -L "$catalog" ]]; then
  target="$(readlink "$catalog")"
  case "$target" in /*) resolved="$target" ;; *) resolved="$(cd "$(dirname "$catalog")" && pwd -P)/$target" ;; esac
fi
# The catalog and its resolved target must be outside this repository and any
# linked worktree of it.
common="$(git rev-parse --path-format=absolute --git-common-dir)"
for path in "$catalog" "$resolved"; do
  case "$path" in "$root"/*) echo "error: the catalog must be outside the repository" >&2; exit 1 ;; esac
  while read -r wt; do
    case "$path" in "$wt"/*) echo "error: the catalog must be outside any worktree of this repository" >&2; exit 1 ;; esac
  done < <(git --git-dir="$common" worktree list --porcelain | sed -n 's/^worktree //p')
done

bin="${RECORDPRIMS_LIMENSAFE_BIN:-limensafe}"
if [[ "$bin" == */* ]]; then
  [[ "$bin" == /* && -x "$bin" ]] || { echo "error: RECORDPRIMS_LIMENSAFE_BIN must be an absolute path to an executable" >&2; exit 1; }
else
  command -v "$bin" >/dev/null || { echo "error: limensafe not found on PATH" >&2; exit 1; }
fi
base="${RECORDPRIMS_SURFACE_BASE:-origin/main}"

report="$(mktemp)"
trap 'rm -f "$report"' EXIT
check_report() {
  local surface="$1"
  "${PYTHON:-python3}" -I -c '
import json, sys
d = json.load(open(sys.argv[1]))
loaded = {c["catalog_id"]: c["load_status"] for c in d["scan_metadata"]["catalogs_loaded"]}
if loaded.get("recordprims-release") != "ok":
    sys.exit("surface scan: catalog recordprims-release did not load")
n = d["summary"]["findings_total"]
for f in d["findings"]:
    loc = f.get("location", {})
    print("  %s %s %s:%s" % (f["severity"], f["entity_class"], loc.get("path", "-"), loc.get("line", "-")), file=sys.stderr)
sys.exit(f"surface scan ({sys.argv[2]}): {n} finding(s)" if n else 0)
' "$report" "$surface"
}

# Tracked tree (what the next commit would publish).
"$bin" scan . --git-archive=HEAD --catalog "$catalog" > "$report" 2>/dev/null || true
check_report tree

# Commit messages introduced relative to the base, one at a time.
if git rev-parse --verify --quiet "$base" >/dev/null; then
  for sha in $(git rev-list "$base"..HEAD); do
    git log -1 --format=%B "$sha" | "$bin" scan --commit-msg --catalog "$catalog" - > "$report" 2>/dev/null || true
    check_report "commit ${sha:0:12}"
  done
else
  echo "[warn] surface scan: base $base not found; commit messages not scanned" >&2
fi
echo "[ok] surface scan"
