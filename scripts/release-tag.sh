#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Create every tag in the release set, signed and local only, at HEAD.
# Publication is a separate command (release-push-tag.sh).
set -euo pipefail
# shellcheck source=scripts/release-tag-common.sh
source "$(dirname "$0")/release-tag-common.sh"
cd "$tag_root"
tag_version
tag_identity
tag_selector
tag_approved_primary
tag_key_home
# The checker is bound before any side effect (the fetch in tag_checkout
# included); the committed pins are validated before signing.
# shellcheck source=scripts/release-decernor.sh
source "$tag_root/scripts/release-decernor.sh"
resolve_release_decernor || tag_die 'the checker is required before any release side effect'
tag_checkout
"$tag_root/scripts/release-validate-pin.sh" >/dev/null || tag_die 'the committed pins do not validate'
[[ "$(git ls-tree HEAD -- keys/expected-fingerprints.txt | awk '{print $1}')" == 100644 ]] ||
	tag_die 'committed fingerprint anchors are required before tagging'
tag_load_set
tags=("${tag_names[@]}")
for tag in "${tags[@]}"; do
	[[ -z "$(git ls-remote --tags origin "refs/tags/$tag")" ]] || tag_die "remote tag $tag already exists"
	[[ -z "$(git show-ref --tags "refs/tags/$tag" || true)" ]] || tag_die "local tag $tag already exists"
	tag_message_file "$tag" >/dev/null
done
export GIT_COMMITTER_NAME="$RECORDPRIMS_TAGGER_NAME" GIT_COMMITTER_EMAIL="$RECORDPRIMS_TAGGER_EMAIL"
for tag in "${tags[@]}"; do
	git tag -s -a --cleanup=verbatim -u "$RECORDPRIMS_PGP_KEY_ID" -F "$(tag_message_file "$tag")" "$tag" HEAD
done
"$tag_root/scripts/release-verify-tag.sh"
echo "[ok] ${#tags[@]} signed tag(s) created locally; nothing was pushed"
