#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Tag inputs: the explicit release tag, the derived tag set, the per-tag message
# files, the subkey selector and the Makefile's refusal of a bare RELEASE_TAG.
# shellcheck disable=SC2016
set -euo pipefail
# shellcheck source=scripts/release-test-fixture.sh
source "$(dirname "$0")/release-test-fixture.sh"
version=1.2.3
repo="$fixture_dir/repo"
fixture_repo "$repo" "$version"
printf '%s\n' 0123456789ABCDEF0123456789ABCDEF01234567 >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo"
lib="$repo/scripts/release-tag-common.sh"
in_repo() { (cd "$repo" && env "$@"); }
call() { in_repo "${@:2}" bash -c "source '$lib'; $1"; }

# Every checklist `make` target exists (also part of make check).
"$fixture_root/scripts/checklist-targets.sh" >/dev/null

# The Makefile refuses a bare RELEASE_TAG from any origin and derives no default.
expect_fail_with 'RELEASE_TAG is not accepted' env RELEASE_TAG=v9.9.9 make -s -C "$fixture_root" -n help
expect_fail_with 'RELEASE_TAG is not accepted' env -u RELEASE_TAG make -s -C "$fixture_root" -n help RELEASE_TAG=v9.9.9
expect_fail_with 'RELEASE_TAG is not accepted' env RELEASE_TAG= make -s -C "$fixture_root" -n help
expect_fail_with 'RECORDPRIMS_RELEASE_TAG=vX.Y.Z is required' env -u RELEASE_TAG -u RECORDPRIMS_RELEASE_TAG make -s -C "$fixture_root" release-tag
if grep -En 'RECORDPRIMS_RELEASE_TAG[[:space:]]*\?=|v\$\(VERSION\)' "$fixture_root/Makefile"; then
	echo 'error: the Makefile derives a release tag' >&2
	exit 1
fi

# The release tag is required, canonical, and equal to VERSION.
expect_fail_with 'RECORDPRIMS_RELEASE_TAG is required' call tag_version -u RECORDPRIMS_RELEASE_TAG
expect_fail_with 'canonical vX.Y.Z' call tag_version RECORDPRIMS_RELEASE_TAG=1.2.3
expect_fail_with 'does not match VERSION' call tag_version RECORDPRIMS_RELEASE_TAG=v1.2.4
call tag_version RECORDPRIMS_RELEASE_TAG="v$version"

# The tag set is fixed: the one vX.Y.Z tag, and no file in the checkout
# widens or renames it.
[[ "$(call 'tag_load_set && printf "%s," "${tag_names[@]}"' RECORDPRIMS_RELEASE_TAG="v$version")" == "v$version," ]]
[[ ! -e "$fixture_root/config/release/release-tags.txt" ]]
printf 'v\ngo/canon/v\n' >"$repo/config/release/release-tags.txt"
[[ "$(call 'tag_load_set && printf "%s," "${tag_names[@]}"' RECORDPRIMS_RELEASE_TAG="v$version")" == "v$version," ]]
rm -f "$repo/config/release/release-tags.txt"
rm -f "$repo/go.mod"
expect_fail_with 'go.mod is required at the repository root' call tag_load_set RECORDPRIMS_RELEASE_TAG="v$version"
# A module in the old nested place does not stand in for the root module.
mkdir -p "$repo/go/canon"
printf 'module github.com/3leaps/recordprims/go/canon\n' >"$repo/go/canon/go.mod"
expect_fail_with 'go.mod is required at the repository root' call tag_load_set RECORDPRIMS_RELEASE_TAG="v$version"
rm -rf "$repo/go"
expect_fail_with 'does not match VERSION' call tag_load_set RECORDPRIMS_RELEASE_TAG=v9.9.9
git -C "$repo" checkout -q -- .

# Message files: one per tag, outside the repo, under a directory named for the
# tag, holding exactly the generated message.
messages="$fixture_dir/messages/v$version"
fixture_messages "$messages" "$version" "v$version"
msg() { call "tag_message_file '$1'" RECORDPRIMS_RELEASE_TAG="v$version" RECORDPRIMS_TAG_MESSAGE_DIR="${2:-$messages}"; }
[[ "$(msg "v$version")" == "$messages/v$version.txt" ]]
expect_fail_with 'must end in the release tag' msg "v$version" "$fixture_dir/messages"
# A symlinked ancestor is allowed (macOS /tmp and /var are symlinks); the
# directory itself must not be a symlink, and its resolved path must still be
# outside the repository and named for the tag.
mkdir -p "$fixture_dir/real/v$version"
cp "$messages/v$version.txt" "$fixture_dir/real/v$version/"
ln -s "$fixture_dir/real" "$fixture_dir/link"
[[ "$(msg "v$version" "$fixture_dir/link/v$version")" == "$fixture_dir/link/v$version/v$version.txt" ]]
mkdir -p "$fixture_dir/aliases"
ln -s "$fixture_dir/real/v$version" "$fixture_dir/aliases/v$version"
expect_fail_with 'must name an existing directory' msg "v$version" "$fixture_dir/aliases/v$version"
mkdir -p "$repo/inside/v$version"
ln -s "$repo/inside" "$fixture_dir/into-repo"
expect_fail_with 'outside the repository' msg "v$version" "$fixture_dir/into-repo/v$version"
# A raw macOS temporary path (an unresolved /var/folders or /tmp) is accepted.
raw_tmp="$(mktemp -d)/v$version"
mkdir "$raw_tmp"
cp "$messages/v$version.txt" "$raw_tmp/"
msg "v$version" "$raw_tmp" >/dev/null
rm -rf "$(dirname "$raw_tmp")"
mkdir -p "$repo/inside/v$version"
expect_fail_with 'outside the repository' msg "v$version" "$repo/inside/v$version"
cp "$messages/v$version.txt" "$fixture_dir/good.txt"
# The message is exactly "recordprims X.Y.Z" and a newline; anything else fails.
for bad in 'recordprims 9.9.9' 'Release 1.2.3' 'recordprims 1.2.3 ' 'recordprims  1.2.3'; do
	printf '%s\n' "$bad" >"$messages/v$version.txt"
	expect_fail_with 'must be exactly' msg "v$version"
done
printf 'recordprims 1.2.3' >"$messages/v$version.txt"
expect_fail_with 'must be exactly' msg "v$version"
printf 'recordprims 1.2.3\n\nExtra text.\n' >"$messages/v$version.txt"
expect_fail_with 'must be exactly' msg "v$version"
cp "$fixture_dir/good.txt" "$messages/v$version.txt"
msg "v$version" >/dev/null

# The preparer writes the complete message, ready to use, and never clobbers:
# an existing file is kept, and must already be the generated message.
prep="$fixture_dir/prep/v$version"
in_repo RECORDPRIMS_RELEASE_TAG="v$version" RECORDPRIMS_TAG_MESSAGE_DIR="$prep" ./scripts/release-prepare-tag-message.py >/dev/null
[[ -f "$prep/v$version.txt" && ! -e "$prep/go__canon__v$version.txt" ]]
[[ "$(find "$prep" -mindepth 1 | wc -l | tr -d ' ')" == 1 ]]
cmp "$fixture_dir/good.txt" "$prep/v$version.txt"
in_repo RECORDPRIMS_RELEASE_TAG="v$version" RECORDPRIMS_TAG_MESSAGE_DIR="$prep" ./scripts/release-prepare-tag-message.py >/dev/null
cmp "$fixture_dir/good.txt" "$prep/v$version.txt"
printf 'hand edited\n' >"$prep/v$version.txt"
expect_fail_with 'must be exactly' in_repo RECORDPRIMS_RELEASE_TAG="v$version" RECORDPRIMS_TAG_MESSAGE_DIR="$prep" ./scripts/release-prepare-tag-message.py
[[ "$(cat "$prep/v$version.txt")" == 'hand edited' ]]
expect_fail_with 'must match VERSION' in_repo RECORDPRIMS_RELEASE_TAG=v0.0.1 RECORDPRIMS_TAG_MESSAGE_DIR="$fixture_dir/prep/v0.0.1" ./scripts/release-prepare-tag-message.py

# The subkey selector must be the committed subkey, exact and with "!".
call tag_selector RECORDPRIMS_PGP_KEY_ID='0123456789ABCDEF0123456789ABCDEF01234567!'
expect_fail_with 'followed by !' call tag_selector RECORDPRIMS_PGP_KEY_ID=0123456789ABCDEF0123456789ABCDEF01234567
expect_fail_with 'followed by !' call tag_selector RECORDPRIMS_PGP_KEY_ID='0123456789abcdef0123456789abcdef01234567!'
expect_fail_with 'differs from config/release/signing-subkey.txt' call tag_selector RECORDPRIMS_PGP_KEY_ID='FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF!'

# The approved primary must be set and equal the committed anchor at HEAD.
printf 'gpg %s\nminisign %064d\n' 0123456789ABCDEF0123456789ABCDEF01234567 0 >"$repo/keys/expected-fingerprints.txt"
fixture_commit "$repo" anchors
call tag_approved_primary RECORDPRIMS_GPG_SIGNING_FINGERPRINT=0123456789ABCDEF0123456789ABCDEF01234567
expect_fail_with 'approved primary RECORDPRIMS_GPG_SIGNING_FINGERPRINT required' call tag_approved_primary -u RECORDPRIMS_GPG_SIGNING_FINGERPRINT
expect_fail_with 'approved primary RECORDPRIMS_GPG_SIGNING_FINGERPRINT required' call tag_approved_primary RECORDPRIMS_GPG_SIGNING_FINGERPRINT=0123456789abcdef0123456789abcdef01234567
expect_fail_with 'not the approved primary' call tag_approved_primary RECORDPRIMS_GPG_SIGNING_FINGERPRINT=FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF
# The working tree is not consulted: an uncommitted edit does not satisfy it.
printf 'gpg %s\nminisign %064d\n' FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF 0 >"$repo/keys/expected-fingerprints.txt"
expect_fail_with 'not the approved primary' call tag_approved_primary RECORDPRIMS_GPG_SIGNING_FINGERPRINT=FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF
git -C "$repo" checkout -q -- keys

# The release requires the root module path.
printf 'module example.com/other\n' >"$repo/go.mod"
expect_fail_with 'must declare module github.com/3leaps/recordprims$' call tag_load_set RECORDPRIMS_RELEASE_TAG="v$version"
printf 'module github.com/3leaps/recordprims/go/canon\n' >"$repo/go.mod"
expect_fail_with 'must declare module github.com/3leaps/recordprims$' call tag_load_set RECORDPRIMS_RELEASE_TAG="v$version"
git -C "$repo" checkout -q -- go.mod

# Tagger identity must be the committed one, and free of header injection.
call tag_identity RECORDPRIMS_TAGGER_NAME='3 Leaps Infosec Team' RECORDPRIMS_TAGGER_EMAIL=infosec@3leaps.net
expect_fail_with 'committed tagger identity' call tag_identity RECORDPRIMS_TAGGER_NAME=Someone RECORDPRIMS_TAGGER_EMAIL=infosec@3leaps.net

# Origin must be the GitHub repository.
call tag_origin
git -C "$repo" remote set-url origin https://example.invalid/3leaps/recordprims.git
expect_fail_with 'origin must be' call tag_origin
echo '[ok] tag input controls passed'
