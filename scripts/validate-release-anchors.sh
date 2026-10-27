#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Check that the committed anchor pair is well-formed and that decernor, the
# one designated checker, agrees with it for the given public exports.
# Usage: validate-release-anchors.sh <anchors.txt> <anchors.ndjson> <keys.asc> <minisign.pub>
# Callers resolve the checker first (scripts/release-decernor.sh).
set -euo pipefail
txt="${1:?anchor text required}"
ndjson="${2:?anchor NDJSON required}"
asc="${3:?OpenPGP public export required}"
pub="${4:?minisign public pin required}"
: "${RELEASE_DECERNOR_BIN:?resolve the checker before validating anchors}"
for file in "$txt" "$ndjson" "$asc" "$pub"; do
	[[ -s "$file" && ! -L "$file" ]] || {
		echo 'error: anchor validation input missing, empty or a symlink' >&2
		exit 1
	}
done
if grep -q -e 'PRIVATE KEY BLOCK' -e 'secret key' "$asc" "$pub"; then
	echo 'error: private material in a public export' >&2
	exit 1
fi
# Shape only: two lines and two records naming the same values. The values
# themselves are checked by decernor below, never re-derived here.
python3 - "$txt" "$ndjson" <<'PY'
import json
import pathlib
import re
import sys
txt, ndjson = (pathlib.Path(p) for p in sys.argv[1:])
lines = txt.read_text().splitlines()
raw = ndjson.read_text().splitlines()
if len(lines) != 2 or len(raw) != 2 or any(not line for line in raw):
    raise SystemExit('error: anchors must be exactly two lines and two records')
g, m = (json.loads(line) for line in raw)
if g.get('fingerprint_scheme') != 'openpgp-fingerprint-v1' or g.get('key_role') != 'primary':
    raise SystemExit('error: first record must be the primary OpenPGP fingerprint')
if m.get('fingerprint_scheme') != 'minisign-public-blob-sha256-v1':
    raise SystemExit('error: second record must be the minisign public-blob fingerprint')
if not re.fullmatch('[0-9A-F]{40}', g.get('fingerprint', '')) or not re.fullmatch('[0-9a-f]{64}', m.get('fingerprint', '')):
    raise SystemExit('error: malformed fingerprint')
if lines != ['gpg ' + g['fingerprint'], 'minisign ' + m['fingerprint']]:
    raise SystemExit('error: anchor text and records differ')
PY
"$RELEASE_DECERNOR_BIN" fingerprint verify --anchors "$txt" --anchors-ndjson "$ndjson" \
	--gpg "$asc" --minisign "$pub" >/dev/null || {
	echo 'error: decernor does not verify the public exports against the anchors' >&2
	exit 1
}
echo '[ok] public exports verify against the committed anchors'
