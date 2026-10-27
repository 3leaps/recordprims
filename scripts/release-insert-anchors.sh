#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer-only: derive the two public fingerprint anchors with decernor from
# the public exports committed at RECORDPRIMS_ANCHOR_COMMIT (default HEAD). The
# exports are read as git blobs, never from the working tree or an environment
# path. decernor's records are kept byte-for-byte; nothing re-serializes them.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
# shellcheck source=scripts/release-decernor.sh
source "$root/scripts/release-decernor.sh"
cd "$root"
resolve_release_decernor
commit="$(git rev-parse --verify "${RECORDPRIMS_ANCHOR_COMMIT:-HEAD}^{commit}")"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
committed() {
	local path="$1" out="$2" mode
	mode="$(git ls-tree "$commit" -- "$path" | awk '{print $1}')"
	[[ "$mode" == 100644 ]] || {
		echo "error: $path must be a committed regular file at $commit" >&2
		exit 1
	}
	git cat-file blob "$commit:$path" >"$out"
	[[ -s "$out" ]] || {
		echo "error: committed $path is empty" >&2
		exit 1
	}
}
committed docs/security/release-signing-keys.asc "$scratch/release-signing-keys.asc"
committed docs/security/recordprims-minisign.pub "$scratch/recordprims-minisign.pub"

derive() {
	local out="$1"
	"$RELEASE_DECERNOR_BIN" fingerprint "$scratch/release-signing-keys.asc" --class public --kind gpg --fail-on-empty \
		--format ndjson --path-mode none --gpg-role primary >"$out/gpg.ndjson"
	"$RELEASE_DECERNOR_BIN" fingerprint "$scratch/recordprims-minisign.pub" --class public --kind minisign --fail-on-empty \
		--format ndjson --path-mode none >"$out/minisign.all.ndjson"
	grep -F '"fingerprint_scheme":"minisign-public-blob-sha256-v1"' "$out/minisign.all.ndjson" \
		>"$out/minisign.ndjson" || true
	python3 - "$out" <<'PY'
import json
import pathlib
import sys
base = pathlib.Path(sys.argv[1])
g_raw = (base / 'gpg.ndjson').read_bytes().splitlines(keepends=True)
m_raw = (base / 'minisign.ndjson').read_bytes().splitlines(keepends=True)
if len(g_raw) != 1 or len(m_raw) != 1 or not g_raw[0].endswith(b'\n') or not m_raw[0].endswith(b'\n'):
    raise SystemExit('error: expected exactly one primary GPG record and one minisign blob record')
g, m = json.loads(g_raw[0]), json.loads(m_raw[0])
if g.get('fingerprint_scheme') != 'openpgp-fingerprint-v1' or g.get('key_role') != 'primary':
    raise SystemExit('error: expected a primary OpenPGP record')
# Records are copied as decernor wrote them; only the text pair is composed.
(base / 'expected-fingerprints.ndjson').write_bytes(g_raw[0] + m_raw[0])
(base / 'expected-fingerprints.txt').write_text('gpg ' + g['fingerprint'] + '\nminisign ' + m['fingerprint'] + '\n')
PY
}
mkdir "$scratch/first" "$scratch/second"
derive "$scratch/first"
derive "$scratch/second"
for name in expected-fingerprints.txt expected-fingerprints.ndjson; do
	cmp "$scratch/first/$name" "$scratch/second/$name" || {
		echo "error: two derivations of $name differ" >&2
		exit 1
	}
done
"$root/scripts/install-release-anchors.sh" "$scratch/first" "$root/keys" \
	"$RELEASE_DECERNOR_BIN" fingerprint verify \
	--anchors "$root/keys/expected-fingerprints.txt" \
	--anchors-ndjson "$root/keys/expected-fingerprints.ndjson" \
	--gpg "$scratch/release-signing-keys.asc" --minisign "$scratch/recordprims-minisign.pub" >/dev/null
echo "[ok] derived public fingerprint anchors from $commit; review and commit keys/"
