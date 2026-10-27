#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Install a reviewed public anchor pair. Existing anchors are never
# overwritten; a rotation is a reviewed edit, not a re-run of this script.
# Usage: install-release-anchors.sh <staging-dir> <dest-dir> [verify command...]
set -euo pipefail
staging="${1:?staging directory required}"
dest="${2:?destination required}"
shift 2
names=(expected-fingerprints.txt expected-fingerprints.ndjson)
for name in "${names[@]}"; do
	[[ -s "$staging/$name" && ! -L "$staging/$name" ]] || {
		echo 'error: incomplete anchor pair' >&2
		exit 1
	}
	[[ ! -e "$dest/$name" && ! -L "$dest/$name" ]] || {
		echo "error: $dest/$name exists; anchors are never overwritten" >&2
		exit 1
	}
done
[[ ! -L "$dest" && (! -e "$dest" || -d "$dest") ]] || {
	echo 'error: unsafe anchor directory' >&2
	exit 1
}
mkdir -p "$dest"
rollback() {
	trap - EXIT INT TERM HUP
	for name in "${names[@]}"; do rm -f "$dest/$name" "$dest/$name.new"; done
}
trap rollback EXIT
trap 'rollback; exit 130' INT
trap 'rollback; exit 143' TERM
trap 'rollback; exit 129' HUP
for name in "${names[@]}"; do
	(set -C && cat "$staging/$name" >"$dest/$name.new")
done
for name in "${names[@]}"; do
	mv -n "$dest/$name.new" "$dest/$name"
	[[ ! -e "$dest/$name.new" ]] || {
		echo "error: $dest/$name appeared during installation" >&2
		exit 1
	}
done
if [[ "$#" -gt 0 ]]; then
	"$@"
fi
trap - EXIT INT TERM HUP
