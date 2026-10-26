#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Public-surface scan with limensafe and a maintainer catalog kept outside
# this repository. It never runs in CI: the catalog is not given to any
# workflow, so nothing private can reach a GitHub runner.
#
# Scans the tracked tree at HEAD, each commit message in BASE..HEAD, the PR
# title and body when RECORDPRIMS_SURFACE_PR_TEXT names a file, and the branch
# name. Before trusting the catalog, it must block its own canary (the alias
# of its RecordprimsCanary entity, read from the catalog at run time).
#
#   LIMENSAFE_CATALOG_PATH       the catalog (catalog_id recordprims-release); an
#                                absolute path to a regular file outside this
#                                repository and any worktree of it. Unset: warn
#                                and skip, unless RECORDPRIMS_SURFACE_SCAN_REQUIRED=1.
#                                Set but empty, relative, missing or inside: fail.
#   RECORDPRIMS_LIMENSAFE_BIN     optional absolute path to limensafe.
#   RECORDPRIMS_SURFACE_BASE      base for the commit-message scan (default
#                                origin/main); scope=release scans all of HEAD.
#   RECORDPRIMS_SURFACE_PR_TEXT   a file with the PR title and body to scan;
#                                required when the scan is required in PR scope.
#   RECORDPRIMS_SURFACE_SCAN_REQUIRED=1  require the catalog (pr-final, preflight).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$root"
catalog_id=recordprims-release
canary_id=RecordprimsCanary

# fd 3 is the caller's stderr: errors stay visible even where a scan's own
# finding lines are redirected away.
exec 3>&2
die() {
	echo "error: $*" >&3
	exit 1
}

if [[ -z "${LIMENSAFE_CATALOG_PATH+set}" ]]; then
	[[ "${RECORDPRIMS_SURFACE_SCAN_REQUIRED:-0}" != 1 ]] || die 'LIMENSAFE_CATALOG_PATH is required for this gate'
	echo '[skip] surface scan: LIMENSAFE_CATALOG_PATH is not set'
	exit 0
fi
catalog="$LIMENSAFE_CATALOG_PATH"
[[ -n "$catalog" ]] || die 'LIMENSAFE_CATALOG_PATH is set but empty'
[[ "$catalog" == /* ]] || die 'LIMENSAFE_CATALOG_PATH must be an absolute path'
[[ -f "$catalog" && -r "$catalog" ]] || die 'LIMENSAFE_CATALOG_PATH does not name a readable file'
# The catalog must be outside this repository and every worktree of it, by
# path (as given and resolved, letter case folded on a case-insensitive
# volume) and by file identity (no hard link to it under any of them).
common="$(git rev-parse --path-format=absolute --git-common-dir)"
worktrees="$(git --git-dir="$common" worktree list --porcelain)" || die 'cannot enumerate worktrees'
roots=("$root")
while IFS= read -r line; do
	[[ "$line" == worktree\ * ]] || continue
	roots+=("${line#worktree }")
done <<<"$worktrees"
"${PYTHON:-python3}" -I "$root/scripts/surface-catalog-path.py" "$catalog" "${roots[@]}" 2>&3 ||
	die 'the catalog location was refused'

bin="${RECORDPRIMS_LIMENSAFE_BIN:-limensafe}"
if [[ "$bin" == */* ]]; then
	[[ "$bin" == /* && -x "$bin" ]] || die 'RECORDPRIMS_LIMENSAFE_BIN must be an absolute path to an executable'
else
	bin="$(command -v "$bin")" || die 'limensafe not found on PATH'
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/recordprims-surface.XXXXXX")"
trap 'rm -rf "$work"' EXIT

# scan LABEL ARGS...: one limensafe scan. A fresh report is required each time,
# the exit status is read (0 clean, 1 findings, 2+ error), and the catalog must
# have loaded. Returns 0 clean, 1 findings; any error exits.
scan() {
	local label="$1" rc=0
	shift
	# The redirect below truncates the report too; removing it first keeps a
	# stale report from surviving any change to how the scan is invoked.
	rm -f "$work/report.json"
	# Every committed file is public once pushed, so no file is skipped for being
	# listed in a .gitignore or .limensafeignore, or for its size.
	"$bin" scan "$@" --catalog "$catalog" --mode release --include-ignored --max-file-size 4294967296 \
		>"$work/report.json" 2>"$work/scan.err" || rc=$?
	[[ "$rc" -le 1 ]] || {
		grep -v '^catalog warning: ' "$work/scan.err" | head -3 >&3 || true
		die "$label: limensafe exited $rc"
	}
	"${PYTHON:-python3}" -I - "$work/report.json" "$catalog_id" "$label" "$rc" <<'PY'
import json, os, sys
path, catalog_id, label, rc = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
def fail(message):
    # fd 3 is the caller's real stderr; errors are never redirected away.
    os.write(3, (message + "\n").encode())
    sys.exit(1)
try:
    d = json.load(open(path))
    loaded = {c["catalog_id"]: c["load_status"] for c in d["scan_metadata"]["catalogs_loaded"]}
    total = d["summary"]["findings_total"]
except (OSError, ValueError, KeyError, TypeError):
    fail(f"error: {label}: limensafe report is missing or invalid")
if loaded.get(catalog_id) != "ok":
    fail(f"error: {label}: catalog {catalog_id} did not load")
if (total > 0) != (rc == 1):
    fail(f"error: {label}: exit status {rc} disagrees with {total} finding(s)")
# Completeness: a skipped file or directory was not scanned. Only binary files
# may be skipped; any other reason, including one a later limensafe adds, fails.
meta = d.get("scan_metadata", {})
skipped = meta.get("files_skipped_by_reason")
if not isinstance(skipped, dict):
    fail(f"error: {label}: limensafe report does not account for skipped files")
other = {k: v for k, v in skipped.items() if k != "binary_detected" and v}
if other:
    fail(f"error: {label}: files were skipped, not scanned: " + ", ".join(f"{k}={v}" for k, v in sorted(other.items())))
if meta.get("directories_skipped"):
    fail(f"error: {label}: directories were skipped, not scanned: {meta.get('directories_skipped')}")
for f in d.get("findings", []):
    loc = f.get("location", {})
    print(f"  {f.get('severity', '?')} {f.get('entity_class', '?')} {loc.get('path', '-')}:{loc.get('line', '-')}", file=sys.stderr)
if total:
    print(f"[block] {label}: {total} finding(s)", file=sys.stderr)
    sys.exit(3)
PY
	local status=$?
	[[ "$status" -ne 3 ]] || return 1
	[[ "$status" -eq 0 ]] || exit 1
	echo "[ok] $label"
}

# The catalog must block its own canary before any result is trusted.
canary="$("${PYTHON:-python3}" -I "$root/scripts/surface-canary.py" "$catalog" "$canary_id")" ||
	die 'the catalog has no usable canary'
mkdir "$work/canary"
printf 'tripwire %s\n' "$canary" >"$work/canary/canary.txt"
unset canary
# A scan error exits from scan itself; only "clean" (canary not caught) and
# "findings" return here. The block lines it prints go to a scratch file.
if scan canary "$work/canary" 2>"$work/canary.err"; then
	die 'the catalog did not block its canary'
fi
"${PYTHON:-python3}" -I -c '
import json, sys
d = json.load(open(sys.argv[1]))
ok = any(f.get("decision") == "block" and f.get("replacement_id") == sys.argv[2] for f in d.get("findings", []))
sys.exit(0 if ok else "error: the canary was not blocked as " + sys.argv[2])' "$work/report.json" "$canary_id"
rm -rf "$work/canary"
echo '[ok] catalog blocks its canary'

failed=0
# Every committed blob at HEAD, symlink targets included. Not git archive,
# which drops export-ignore paths that are still pushed.
mkdir "$work/tree"
"${PYTHON:-python3}" -I -B "$root/scripts/export-committed-tree.py" "$root" HEAD "$work/tree" >/dev/null 2>&3 ||
	die 'cannot export the committed tree'
scan tree "$work/tree" || failed=1
rm -rf "$work/tree"

if [[ "${RECORDPRIMS_SURFACE_SCOPE:-pr}" == release ]]; then
	commits="$(git rev-list HEAD)"
else
	base="${RECORDPRIMS_SURFACE_BASE:-origin/main}"
	git rev-parse --verify --quiet "$base^{commit}" >/dev/null || die "base $base not found; fetch it or set RECORDPRIMS_SURFACE_BASE"
	commits="$(git rev-list "$base..HEAD")"
fi
for sha in $commits; do
	git log -1 --format=%B "$sha" >"$work/message"
	scan "commit ${sha:0:12}" --commit-msg - <"$work/message" || failed=1
done

# The PR title and body are a public surface. pr-final (required, PR scope)
# must scan them; the release scope covers a tagged commit, not a PR.
if [[ -n "${RECORDPRIMS_SURFACE_PR_TEXT:-}" ]]; then
	[[ -f "${RECORDPRIMS_SURFACE_PR_TEXT}" && -s "${RECORDPRIMS_SURFACE_PR_TEXT}" ]] ||
		die 'RECORDPRIMS_SURFACE_PR_TEXT does not name a non-empty file'
	scan 'pr text' --commit-msg - <"$RECORDPRIMS_SURFACE_PR_TEXT" || failed=1
elif [[ "${RECORDPRIMS_SURFACE_SCAN_REQUIRED:-0}" == 1 && "${RECORDPRIMS_SURFACE_SCOPE:-pr}" != release ]]; then
	die 'RECORDPRIMS_SURFACE_PR_TEXT is required here: write the PR title and body to a file and pass it'
fi

if branch="$(git symbolic-ref --quiet --short HEAD)"; then
	printf '%s\n' "$branch" | scan 'branch name' --branch-name - || failed=1
else
	echo '[skip] branch name: detached HEAD'
fi

[[ "$failed" -eq 0 ]] || die 'public-surface findings (run limensafe directly for locations)'
echo "[ok] surface scan: tree, $(wc -w <<<"$commits" | tr -d ' ') commit message(s)$([[ -n "${RECORDPRIMS_SURFACE_PR_TEXT:-}" ]] && echo ', pr text')"
