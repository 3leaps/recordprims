#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# crate-copies-check refuses each kind of drift between the crate's copies and
# their sources. Every case runs the real check against a scratch copy of the
# tracked tree with one planted change.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
PYTHON_BIN="${PYTHON:-python3}"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
base="$scratch/base"
mkdir "$base"
git -C "$root" ls-files -z -- internal/shape/schemas crates/recordprims/schemas crates/recordprims/LICENSE-MIT crates/recordprims/LICENSE-APACHE crates/recordprims/NOTICE LICENSE-MIT LICENSE-APACHE NOTICE scripts/crate-copies-check.py |
	tar -C "$root" --null -T - -cf - | tar -xf - -C "$base"

check() { (cd "$repo" && "$PYTHON_BIN" -I -B scripts/crate-copies-check.py) >"$scratch/out" 2>&1; }
fresh() {
	repo="$scratch/repo"
	rm -rf "$repo"
	cp -R "$base" "$repo"
	copy="$repo/crates/recordprims"
}
refused() {
	if check; then echo "error: accepted: $1" >&2; exit 1; fi
	grep -qF -- "$2" "$scratch/out" || { echo "error: $1: refused for another reason" >&2; cat "$scratch/out" >&2; exit 1; }
}

fresh
check || { echo "error: the tracked copies do not pass" >&2; cat "$scratch/out" >&2; exit 1; }

fresh; printf ' ' >>"$copy/schemas/v1/record.schema.json"
refused 'changed schema byte' 'copy differs from source: crates/recordprims/schemas/v1/record.schema.json'

fresh; printf ' ' >>"$repo/internal/shape/schemas/v0/record.schema.json"
refused 'changed source schema' 'copy differs from source: crates/recordprims/schemas/v0/record.schema.json'

fresh; rm "$copy/schemas/v1/stream-seal.schema.json"
refused 'removed schema copy' 'missing copy: crates/recordprims/schemas/v1/stream-seal.schema.json'

fresh; printf '{}\n' >"$copy/schemas/v1/stray.schema.json"
refused 'extra schema in copy' 'extra file in copy: crates/recordprims/schemas/v1/stray.schema.json'

fresh; rm -r "$copy/schemas"
refused 'removed schema directory' 'missing copy: crates/recordprims/schemas/'

fresh; printf 'x\n' >>"$copy/LICENSE-MIT"
refused 'changed license copy' 'copy differs from source: crates/recordprims/LICENSE-MIT'

fresh; rm "$copy/LICENSE-APACHE"
refused 'removed license copy' 'missing copy: crates/recordprims/LICENSE-APACHE'

fresh; printf 'x\n' >>"$repo/NOTICE"
refused 'changed notice source' 'copy differs from source: crates/recordprims/NOTICE'

echo "[ok] crate-copies-check controls: changed copy, changed source, removed copy, extra file, removed directory, changed and removed license, changed notice"
