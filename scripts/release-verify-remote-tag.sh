#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# After a push: each remote tag must be the same annotated object as the local
# verified tag. Signatures are then re-checked against the committed pin.
set -euo pipefail
# shellcheck source=scripts/release-tag-common.sh
source "$(dirname "$0")/release-tag-common.sh"
# shellcheck source=scripts/release-decernor.sh
source "$(dirname "$0")/release-decernor.sh"
cd "$tag_root"
tag_version
resolve_release_decernor
export RELEASE_DECERNOR_BIN
tag_load_set
for tag in "${tag_names[@]}"; do
	local_object="$(git rev-parse --verify --quiet "refs/tags/$tag")" || tag_die "local tag $tag is absent"
	remote_object="$(git ls-remote --tags origin "refs/tags/$tag" | awk -v ref="refs/tags/$tag" '$2==ref {print $1}')"
	[[ -n "$remote_object" ]] || tag_die "remote tag $tag is absent"
	[[ "$remote_object" == "$local_object" ]] || tag_die "remote tag $tag differs from the local verified object"
done
"$tag_root/scripts/verify-pinned-tag.sh" --published
echo '[ok] remote tags match the verified local tags'
