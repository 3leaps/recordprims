#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Pin validation reads the committed pins (or, only with --staged DIR, the
# one-time export's scratch output) and requires exactly the committed signing
# subkey. The one-time export writes nothing until its output validates.
set -euo pipefail
# shellcheck source=scripts/release-test-fixture.sh
source "$(dirname "$0")/release-test-fixture.sh"
decernor="$(fixture_decernor)"
repo="$fixture_dir/repo"
read -r _ subkey < <(fixture_keys "$fixture_dir/gpg")
read -r _ other_subkey < <(fixture_keys "$fixture_dir/gpg-other")
fixture_repo "$repo" 1.2.3
fixture_commit "$repo" base
gpg --homedir "$fixture_dir/gpg" --batch --armor --export-options export-minimal --export "$subkey!" \
	>"$repo/docs/security/release-signing-keys.asc"
fixture_minisign_pub "$repo/docs/security/recordprims-minisign.pub" 1
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
validate() { (cd "$repo" && RECORDPRIMS_DECERNOR_BIN="$decernor" ./scripts/release-validate-pin.sh "$@"); }

# Before the first commit only --staged can see the new pins.
expect_fail_with 'committed regular file' validate
validate --staged "$repo" >/dev/null
expect_fail_with 'usage' validate --other
expect_fail_with 'usage' validate --staged
expect_fail_with 'absolute directory' validate --staged relative

# A minisign secret key is never a public pin, whatever its comment says.
cp "$repo/docs/security/recordprims-minisign.pub" "$fixture_dir/good.pub"
python3 - "$repo/docs/security/recordprims-minisign.pub" <<'PY2'
import base64, pathlib, sys
blob = b'Ed' + b'B2' + b'\x00' * 154
pathlib.Path(sys.argv[1]).write_text('untrusted comment: release key\n' + base64.b64encode(blob).decode() + '\n')
PY2
expect_fail_with 'not exactly one public key' validate --staged "$repo"
printf 'untrusted comment: release key\n' >"$repo/docs/security/recordprims-minisign.pub"
expect_fail_with 'not exactly one public key' validate --staged "$repo"
cat "$fixture_dir/good.pub" "$fixture_dir/good.pub" >"$repo/docs/security/recordprims-minisign.pub"
expect_fail_with 'not exactly one public key' validate --staged "$repo"
cp "$fixture_dir/good.pub" "$repo/docs/security/recordprims-minisign.pub"
validate --staged "$repo" >/dev/null

# The one-time export: the checker first, then validation in scratch, then the
# no-clobber write. Every refusal leaves the pins absent.
export_repo="$fixture_dir/export"
fixture_repo "$export_repo" 1.2.3
cp "$fixture_root/scripts/release-export-pin.sh" "$export_repo/scripts/"
fixture_commit "$export_repo" base
gpg_home="$(cd "$fixture_dir/gpg" && pwd -P)"
export_pins() {
	(cd "$export_repo" && env "$@" RECORDPRIMS_GPG_HOMEDIR="$gpg_home" RECORDPRIMS_PGP_KEY_ID="$subkey!" ./scripts/release-export-pin.sh)
}
no_pins() {
	[[ ! -e "$export_repo/docs/security/release-signing-keys.asc" && ! -e "$export_repo/docs/security/recordprims-minisign.pub" &&
		! -e "$export_repo/config/release/signing-subkey.txt" ]] || {
		echo 'error: a refused export wrote a pin' >&2
		exit 1
	}
}
cp "$repo/docs/security/recordprims-minisign.pub" "$fixture_dir/export.pub"
python3 - "$fixture_dir/disguised.pub" <<'PY2'
import base64, pathlib, sys
blob = b'Ed' + b'B2' + b'\x00' * 154
pathlib.Path(sys.argv[1]).write_text('untrusted comment: release key\n' + base64.b64encode(blob).decode() + '\n')
PY2
expect_fail_with 'RECORDPRIMS_DECERNOR_BIN is required' export_pins -u RECORDPRIMS_DECERNOR_BIN RECORDPRIMS_MINISIGN_PUB_EXPORT="$fixture_dir/export.pub"
no_pins
expect_fail_with 'did not validate; nothing was written' export_pins RECORDPRIMS_DECERNOR_BIN="$decernor" RECORDPRIMS_MINISIGN_PUB_EXPORT="$fixture_dir/disguised.pub"
no_pins
export_pins RECORDPRIMS_DECERNOR_BIN="$decernor" RECORDPRIMS_MINISIGN_PUB_EXPORT="$fixture_dir/export.pub" >/dev/null
cmp "$fixture_dir/export.pub" "$export_repo/docs/security/recordprims-minisign.pub"
[[ "$(cat "$export_repo/config/release/signing-subkey.txt")" == "$subkey" ]]
expect_fail_with 'already exists' export_pins RECORDPRIMS_DECERNOR_BIN="$decernor" RECORDPRIMS_MINISIGN_PUB_EXPORT="$fixture_dir/export.pub"

fixture_commit "$repo" pins
expect_fail_with 'must be committed' validate
validate --pins-only >/dev/null

# Anchors, once committed, must verify against the pins.
RECORDPRIMS_DECERNOR_BIN="$decernor" "$repo/scripts/release-insert-anchors.sh" >/dev/null
fixture_commit "$repo" anchors
validate >/dev/null
fixture_minisign_pub "$repo/docs/security/recordprims-minisign.pub" 2
fixture_commit "$repo" rotated-pub-without-anchors
expect_fail_with 'does not verify' validate
git -C "$repo" checkout -q HEAD~1 -- docs/security/recordprims-minisign.pub
fixture_commit "$repo" restore

# The committed subkey file must name the pin's signing subkey.
printf '%s\n' "$other_subkey" >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo" wrong-subkey
expect_fail_with 'differs from config/release/signing-subkey.txt' validate
printf 'not-a-fingerprint\n' >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo" bad-subkey
expect_fail_with 'one uppercase 40-hex fingerprint' validate
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo" good-subkey
validate >/dev/null

# An export with two subkeys, or private material, is refused.
gpg --homedir "$fixture_dir/gpg" --batch --armor --export >"$repo/docs/security/release-signing-keys.asc"
gpg --homedir "$fixture_dir/gpg-other" --batch --armor --export >>"$repo/docs/security/release-signing-keys.asc"
fixture_commit "$repo" two-keys
expect_fail validate
# Split so this file never holds the whole header (content-check refuses it).
printf -- '-----BEGIN PGP PRIVATE %s-----\n' 'KEY BLOCK' >"$repo/docs/security/release-signing-keys.asc"
fixture_commit "$repo" private
expect_fail_with 'private material' validate
echo '[ok] pin validation controls passed'
