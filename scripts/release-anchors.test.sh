#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Anchor derivation reads the committed exports, preserves decernor's records
# byte-for-byte, never overwrites, and binds the checker by absolute path only.
set -euo pipefail
# shellcheck source=scripts/release-test-fixture.sh
source "$(dirname "$0")/release-test-fixture.sh"
decernor="$(fixture_decernor)"
repo="$fixture_dir/repo"
read -r _ subkey < <(fixture_keys "$fixture_dir/gpg")
fixture_repo "$repo" 1.2.3
gpg --homedir "$fixture_dir/gpg" --batch --armor --export-options export-minimal --export "$subkey!" \
	>"$repo/docs/security/release-signing-keys.asc"
fixture_minisign_pub "$repo/docs/security/recordprims-minisign.pub" 1
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo" pins
insert() { (cd "$repo" && RECORDPRIMS_DECERNOR_BIN="$decernor" ./scripts/release-insert-anchors.sh); }

# Checker binding: required, absolute, a regular executable, not a symlink, and
# really decernor. Neither PATH nor an unprefixed variable stands in for it.
(cd "$repo" && expect_fail_with 'RECORDPRIMS_DECERNOR_BIN is required' env -u RECORDPRIMS_DECERNOR_BIN DECERNOR_BIN="$decernor" ./scripts/release-insert-anchors.sh)
(cd "$repo" && expect_fail_with 'absolute executable' env RECORDPRIMS_DECERNOR_BIN=decernor ./scripts/release-insert-anchors.sh)
ln -s "$decernor" "$fixture_dir/decernor-link"
(cd "$repo" && expect_fail_with 'not a symlink' env RECORDPRIMS_DECERNOR_BIN="$fixture_dir/decernor-link" ./scripts/release-insert-anchors.sh)
printf '#!/bin/sh\necho "notdecernor 9.9.9"\n' >"$fixture_dir/impostor"
chmod +x "$fixture_dir/impostor"
(cd "$repo" && expect_fail_with 'does not identify as decernor' env RECORDPRIMS_DECERNOR_BIN="$fixture_dir/impostor" ./scripts/release-insert-anchors.sh)
# shellcheck disable=SC2016 # The single-quoted text is the impostor script body.
printf '#!/bin/sh\nif [ "$2" = "-e" ]; then printf "Version: 0.1.7\\nCommit: x\\nBuild Date: x\\nGo Version: x\\nGofulmen: x\\nCrucible: x\\n"; else echo "decernor 0.1.7"; fi\n' >"$fixture_dir/old"
chmod +x "$fixture_dir/old"
(cd "$repo" && expect_fail_with '>= 0.1.8' env RECORDPRIMS_DECERNOR_BIN="$fixture_dir/old" ./scripts/release-insert-anchors.sh)
[[ ! -e "$repo/keys/expected-fingerprints.txt" ]]

# The exports come from the committed tree: an uncommitted change is ignored.
fixture_minisign_pub "$repo/docs/security/recordprims-minisign.pub" 2
insert >/dev/null
git -C "$repo" checkout -q -- docs/security/recordprims-minisign.pub
committed_pub="$fixture_dir/committed.pub"
git -C "$repo" cat-file blob HEAD:docs/security/recordprims-minisign.pub >"$committed_pub"
"$decernor" fingerprint verify --anchors "$repo/keys/expected-fingerprints.txt" \
	--anchors-ndjson "$repo/keys/expected-fingerprints.ndjson" \
	--gpg "$repo/docs/security/release-signing-keys.asc" --minisign "$committed_pub" >/dev/null

# The NDJSON is decernor's own record lines, byte-for-byte.
"$decernor" fingerprint "$repo/docs/security/release-signing-keys.asc" --class public --kind gpg \
	--format ndjson --path-mode none --gpg-role primary >"$fixture_dir/expected.ndjson"
"$decernor" fingerprint "$committed_pub" --class public --kind minisign --format ndjson --path-mode none |
	grep -F '"fingerprint_scheme":"minisign-public-blob-sha256-v1"' >>"$fixture_dir/expected.ndjson"
cmp "$fixture_dir/expected.ndjson" "$repo/keys/expected-fingerprints.ndjson"

# Existing anchors are never overwritten.
cp "$repo/keys/expected-fingerprints.txt" "$fixture_dir/before.txt"
expect_fail_with 'never overwritten' insert
cmp "$fixture_dir/before.txt" "$repo/keys/expected-fingerprints.txt"

# A missing committed export fails before any write.
rm -rf "$repo/keys"
git -C "$repo" rm -q docs/security/recordprims-minisign.pub
fixture_commit "$repo" drop-pub
expect_fail_with 'committed regular file' insert
[[ ! -e "$repo/keys/expected-fingerprints.txt" ]]

# Validation: decernor rejects anchors that do not match the exports.
git -C "$repo" checkout -q HEAD~1 -- docs/security/recordprims-minisign.pub
fixture_commit "$repo" restore-pub
insert >/dev/null
fixture_minisign_pub "$fixture_dir/other.pub" 3
(
	RELEASE_DECERNOR_BIN="$decernor"
	export RELEASE_DECERNOR_BIN
	"$repo/scripts/validate-release-anchors.sh" "$repo/keys/expected-fingerprints.txt" \
		"$repo/keys/expected-fingerprints.ndjson" "$repo/docs/security/release-signing-keys.asc" \
		"$repo/docs/security/recordprims-minisign.pub" >/dev/null
	expect_fail_with 'does not verify' "$repo/scripts/validate-release-anchors.sh" \
		"$repo/keys/expected-fingerprints.txt" "$repo/keys/expected-fingerprints.ndjson" \
		"$repo/docs/security/release-signing-keys.asc" "$fixture_dir/other.pub"
	sed 's/^minisign .*/minisign 0000000000000000000000000000000000000000000000000000000000000000/' \
		"$repo/keys/expected-fingerprints.txt" >"$fixture_dir/edited.txt"
	expect_fail_with 'text and records differ' "$repo/scripts/validate-release-anchors.sh" \
		"$fixture_dir/edited.txt" "$repo/keys/expected-fingerprints.ndjson" \
		"$repo/docs/security/release-signing-keys.asc" "$repo/docs/security/recordprims-minisign.pub"
)
echo '[ok] anchor and checker-binding controls passed'
