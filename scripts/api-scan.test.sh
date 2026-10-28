#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# api-scan refuses each leak of a canonprims type through recordprims' exported
# Go API and accepts internal-only use. Every case runs the real gate against a
# scratch copy of the tracked tree with one planted file.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
repo="$scratch/repo"
mkdir "$repo"
git -C "$root" ls-files -z | tar -C "$root" --null -T - -cf - | tar -xf - -C "$repo"
seam="$repo/internal/canon/canon.go"
cp "$seam" "$scratch/seam.go"

scan() { (cd "$repo" && scripts/api-scan.sh) >"$scratch/out" 2>&1; }
plant() {
	cp "$scratch/seam.go" "$seam"
	if [ -n "$2" ]; then printf '\n%s\n' "$2" >>"$seam"; fi
	printf 'package recordprims\n\nimport (\n\tcp "github.com/3leaps/canonprims/canon"\n\t"github.com/3leaps/recordprims/internal/canon"\n)\n\nvar _ = cp.JCS\nvar _ = canon.Digest\n\n%s\n' "$1" >"$repo/zz_probe.go"
	(cd "$repo" && CGO_ENABLED=0 go build ./...) || { echo "error: probe does not compile: $1" >&2; exit 1; }
}
refused() {
	plant "$2" "${3:-}"
	if scan; then echo "error: accepted: $1" >&2; exit 1; fi
	grep -q 'exposes sibling type github.com/3leaps/canonprims' "$scratch/out" || {
		echo "error: $1: refused for another reason" >&2; cat "$scratch/out" >&2; exit 1
	}
}
accepted() {
	plant "$2" "${3:-}"
	scan || { echo "error: refused: $1" >&2; cat "$scratch/out" >&2; exit 1; }
}

refused 'aliased import' 'func F() cp.Limits { return cp.DefaultLimits() }'
refused 'internal re-export' 'func F() canon.L { return canon.L{} }' 'type L = cp.Limits'
refused 'unused type param' 'type Box[T cp.Scheme] struct{}'
refused 'private field param' 'type Box[T cp.Scheme] struct{ value T }'
refused 'alias type param' 'type A[T cp.Scheme] = []int'
refused 'generic func constraint' 'func F[T cp.Scheme]() {}'
refused 'exported field' 'type S struct{ R cp.Refusal }'
accepted 'internal use' 'func F() (string, error) { return canon.Digest([]byte("{}")) }'
accepted 'private generic' 'type box[T cp.Scheme] struct{ value T }'
accepted 'clean generic' 'type Box[T any] struct{ value T }'

# A package whose `go list` record exceeds 64 KiB is still scanned: the leak
# sits in a package listed with enough files to pass that size.
plant 'func Leaked(cp.Scheme) {}' ''
for i in $(seq -w 1 2400); do
	printf 'package recordprims\n' >"$repo/zz_pad_with_a_long_file_name_$i.go"
done
# Size by the shortest form of the record, the bare file list, so the control
# holds for any reader format.
size="$(cd "$repo" && CGO_ENABLED=0 go list -f '{{join .GoFiles ""}}' . | wc -c | tr -d ' ')"
[ "$size" -gt 65536 ] || { echo "error: file list is $size bytes, want over 64 KiB" >&2; exit 1; }
if scan; then echo "error: accepted: leak in a package with a long listing" >&2; exit 1; fi
grep -q 'exposes sibling type github.com/3leaps/canonprims' "$scratch/out" || {
	echo "error: long listing: refused for another reason" >&2; cat "$scratch/out" >&2; exit 1
}
rm -f "$repo"/zz_pad_with_a_long_file_name_*.go

# A tree that does not compile is a failure, never [ok].
cp "$scratch/seam.go" "$seam"
printf 'package recordprims\n\nfunc Broken() { undefined() }\n' >"$repo/zz_probe.go"
if scan; then echo "error: accepted a tree that does not compile" >&2; exit 1; fi

echo "[ok] api-scan controls: aliased import, internal re-export, generic constraints, fields, a leak behind a >64 KiB listing; internal and clean generic use pass; broken build fails"
