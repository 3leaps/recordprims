#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Local tag verification (release-verify-tag.sh) binds the tag to the prepared
# message bytes, the committed tagger identity and HEAD, and then to the
# signature under the committed pin. Each check is shown to refuse.
set -euo pipefail
# shellcheck source=scripts/release-test-fixture.sh
source "$(dirname "$0")/release-test-fixture.sh"
decernor="$(fixture_decernor)"
version=1.2.3
tag="v$version"
repo="$fixture_dir/repo"
messages="$fixture_dir/messages/$tag"
read -r _ subkey < <(fixture_keys "$fixture_dir/gpg")

fixture_repo "$repo" "$version"
cp "$fixture_root/scripts/release-verify-tag.sh" "$repo/scripts/"
gpg --homedir "$fixture_dir/gpg" --batch --armor --export-options export-minimal --export "$subkey!" \
	>"$repo/docs/security/release-signing-keys.asc"
fixture_minisign_pub "$repo/docs/security/recordprims-minisign.pub" 1
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo" pins
RECORDPRIMS_DECERNOR_BIN="$decernor" "$repo/scripts/release-insert-anchors.sh" >/dev/null
fixture_commit "$repo" anchors
fixture_messages "$messages" "$version" "$tag"

sign() {
	local name="${1:-3 Leaps Infosec Team}" message="${2:-$messages/$tag.txt}"
	git -C "$repo" tag -d "$tag" >/dev/null 2>&1 || true
	GNUPGHOME="$fixture_dir/gpg" GIT_COMMITTER_NAME="$name" GIT_COMMITTER_EMAIL=infosec@3leaps.net \
		git -C "$repo" \
		-c gpg.format=openpgp \
		-c gpg.program=gpg \
		-c gpg.openpgp.program=gpg \
		tag -s -a --cleanup=verbatim -u "$subkey!" -F "$message" "$tag" HEAD
}
verify() {
	(cd "$repo" && RECORDPRIMS_RELEASE_TAG="$tag" RECORDPRIMS_TAG_MESSAGE_DIR="$messages" \
		RECORDPRIMS_PGP_KEY_ID="$subkey!" RECORDPRIMS_DECERNOR_BIN="$decernor" ./scripts/release-verify-tag.sh)
}

sign
verify >/dev/null

# A tag whose message differs from the prepared message is refused, even though
# it is validly signed.
printf 'recordprims %s altered\n' "$version" >"$fixture_dir/altered.txt"
sign '3 Leaps Infosec Team' "$fixture_dir/altered.txt"
expect_fail_with 'differs from the prepared message' verify

# A tagger other than the committed identity is refused.
sign 'Someone Else'
expect_fail_with 'tagger differs from the committed identity' verify

# A tag that no longer points at HEAD is refused.
sign
git -C "$repo" commit -q --allow-empty -m later
expect_fail_with 'does not point at' verify
git -C "$repo" reset -q --hard HEAD~1

# A lightweight tag is refused.
git -C "$repo" tag -d "$tag" >/dev/null
git -C "$repo" tag "$tag" HEAD
expect_fail_with 'must be an annotated tag' verify

# The subkey selector must name the committed signing subkey.
sign
(cd "$repo" && expect_fail_with 'differs from config/release/signing-subkey.txt' env RECORDPRIMS_RELEASE_TAG="$tag" \
	RECORDPRIMS_TAG_MESSAGE_DIR="$messages" RECORDPRIMS_PGP_KEY_ID="$(printf '%040d' 0)!" \
	RECORDPRIMS_DECERNOR_BIN="$decernor" ./scripts/release-verify-tag.sh)
verify >/dev/null
echo '[ok] local tag verification controls passed'
