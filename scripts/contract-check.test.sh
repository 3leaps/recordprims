#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# contract-check refuses each kind of drift between contract/PIN.md, the
# inventory and the vendored bytes. Every case runs the real check against a
# scratch copy of the tracked tree with one planted change.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
PYTHON_BIN="${PYTHON:-python3}"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
base="$scratch/base"
mkdir "$base"
git -C "$root" ls-files -z -- contract scripts/contract-check.py | tar -C "$root" --null -T - -cf - | tar -xf - -C "$base"

check() { (cd "$repo" && "$PYTHON_BIN" -I -B scripts/contract-check.py) >"$scratch/out" 2>&1; }
fresh() {
	repo="$scratch/repo"
	rm -rf "$repo"
	cp -R "$base" "$repo"
	pin="$repo/contract/PIN.md"
	vendor="$repo/contract/crucible"
}
# edit PYTHON_EXPR: rewrite PIN.md with a Python expression over s.
edit() { "$PYTHON_BIN" -I -B -c "import sys; p=sys.argv[1]; s=open(p).read(); s=$1; open(p,'w').write(s)" "$pin"; }
refused() {
	if check; then echo "error: accepted: $1" >&2; exit 1; fi
	grep -q -- "$2" "$scratch/out" || { echo "error: $1: refused for another reason" >&2; cat "$scratch/out" >&2; exit 1; }
}

fresh
check || { echo "error: the tracked contract does not pass" >&2; cat "$scratch/out" >&2; exit 1; }

row='| `scripts/test-record-collection-controls.py` | `d3b56849c459cb6f8062de56d958aa55941068b5` |'
tree_row='| `schemas/record-collection/v0/` | tree `47bd13abbc630616ba105e7b927e5b1cb0002c3c` |'
grep -qF -- "$row" "$pin" && grep -qF -- "$tree_row" "$pin" || { echo "error: control rows not found in PIN.md" >&2; exit 1; }

fresh; edit "s.replace('d3b56849c459cb6f8062de56d958aa55941068b5', 'd3b56849c459cb6f8062de56d958aa55941068b')"
refused 'shortened object id' 'malformed vendored-set row'

fresh; edit "s.replace('\`d3b56849c459cb6f8062de56d958aa55941068b5\` |', '\`d3b56849c459cb6f8062de56d958aa55941068b5\` | extra |')"
refused 'extra column' 'malformed vendored-set row'

fresh; edit "s.replace('''$row\n''', '')"
refused 'removed blob row' 'vendored file not under a pinned path: scripts/test-record-collection-controls.py'

fresh; edit "s.replace('''$tree_row\n''', '')"
refused 'removed tree row' 'vendored file not under a pinned path: schemas/record-collection/v0/'

fresh; edit "s.replace('''$row\n''', '').replace('''$tree_row\n''', '')"
refused 'emptied section' 'has no pinned rows'

fresh; edit "s.replace('## Vendored set: record-collection/v0', ' ## Vendored set: record-collection/v0')"
refused 'indented heading' 'indented heading'

fresh; edit "s.replace('''$row\n''', '''$row\n$row\n''')"
refused 'duplicate path' 'pinned twice'

fresh; edit "s.replace('## Vendored set: ', '## Pinned: ')"
refused 'no vendored-set section' 'no vendored-set section'

fresh; printf ' ' >>"$vendor/schemas/record-collection/v0/README.md"
refused 'changed byte' 'object id mismatch: schemas/record-collection/v0'

fresh; rm "$vendor/schemas/record-collection/v0/README.md"
refused 'removed file' 'inventory lists a missing file: schemas/record-collection/v0/README.md'

fresh; printf 'x\n' >"$vendor/scripts/stray.py"
refused 'extra file outside pinned paths' 'vendored file not under a pinned path: scripts/stray.py'

echo "[ok] contract-check controls: malformed row, extra column, removed blob and tree rows, empty section, indented heading, duplicate path, no section, changed byte, removed file, stray file"
