#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Push the verified local release tag set in one atomic push, then verify the
# remote tags.
set -euo pipefail
# shellcheck source=scripts/release-tag-common.sh
source "$(dirname "$0")/release-tag-common.sh"
cd "$tag_root"
tag_version
tag_origin
tag_approved_primary
"$tag_root/scripts/release-verify-tag.sh"
tag_load_set
tags=("${tag_names[@]}")
refs=()
for tag in "${tags[@]}"; do
	[[ -z "$(git ls-remote --tags origin "refs/tags/$tag")" ]] || tag_die "remote tag $tag already exists"
	refs+=("refs/tags/$tag")
done
git push --atomic origin "${refs[@]}"
"$tag_root/scripts/release-verify-remote-tag.sh"
