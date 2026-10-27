#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Record the independently approved original tag identity without overwriting.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$root"
file="${RECORDPRIMS_RELEASE_ANCHOR_FILE:?approved external anchor destination required}"
[[ "$file" == /* && ! -e "$file" && ! -L "$file" ]] || {
    echo 'error: absent absolute ceremony anchor destination required' >&2
    exit 1
}
destination="$(cd "$(dirname "$file")" && pwd -P)/$(basename "$file")"
case "$destination" in "$root" | "$root"/*)
    echo 'error: ceremony anchor must be outside trusted checkout' >&2
    exit 1
    ;;
esac
[[ "${RECORDPRIMS_EXPECTED_TAG_OBJECT:-}" =~ ^[0-9a-f]{40}$ && "${RECORDPRIMS_EXPECTED_COMMIT:-}" =~ ^[0-9a-f]{40}$ ]] || {
    echo 'error: original approved tag object and commit required' >&2
    exit 1
}
scratch="$(mktemp)"
trap 'rm -f "$scratch"' EXIT
RECORDPRIMS_ANCHOR_OUT="$scratch" "$root/scripts/release-verify-published-tag.sh"
set -C
cat "$scratch" >"$file"
echo '[ok] original approved ceremony anchor recorded; retain it across operations'
