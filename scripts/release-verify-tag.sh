#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Verify the local release tag set: messages, tagger and common target from the
# prepared files, then signatures against the pin committed in the tagged commit.
set -euo pipefail
# shellcheck source=scripts/release-tag-common.sh
source "$(dirname "$0")/release-tag-common.sh"
# shellcheck source=scripts/release-decernor.sh
source "$(dirname "$0")/release-decernor.sh"
cd "$tag_root"
tag_version
tag_selector
resolve_release_decernor
export RELEASE_DECERNOR_BIN
target="$(git rev-parse HEAD)"
tag_load_set
for tag in "${tag_names[@]}"; do
	tag_verify_object "$tag" "$target"
done
"$tag_root/scripts/verify-pinned-tag.sh"
