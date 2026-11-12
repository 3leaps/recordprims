#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer entry point: prepare the tag messages, create the signed tags
# locally, or push them. Each mode re-checks its inputs before any side effect.
set -euo pipefail

mode="${1:-}"
case "$mode" in
prepare-message | local-tag | remote-push) ;;
*)
	echo 'error: expected prepare-message, local-tag or remote-push' >&2
	exit 1
	;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"
# Check the sole tag input before an optional loader can use it to select a cut.
: "${RECORDPRIMS_RELEASE_TAG:?set RECORDPRIMS_RELEASE_TAG=vX.Y.Z before loading the environment}"
[[ "$RECORDPRIMS_RELEASE_TAG" == "v$(tr -d '\r\n' <VERSION)" ]] || {
	echo 'error: RECORDPRIMS_RELEASE_TAG must match VERSION' >&2
	exit 1
}
intended="$RECORDPRIMS_RELEASE_TAG"

if [[ "${RECORDPRIMS_APPROVED_ENV_LOADER+set}" == set ]]; then
	loader="$RECORDPRIMS_APPROVED_ENV_LOADER"
	[[ "$loader" == /* && -f "$loader" && -r "$loader" && ! -L "$loader" ]] || {
		echo 'error: the approved environment loader must be a readable absolute regular file' >&2
		exit 1
	}
	loader_dir="$(cd "$(dirname "$loader")" && pwd -P)"
	case "$loader_dir/" in "$root/" | "$root/"*)
		echo 'error: the approved environment loader must be outside the repository' >&2
		exit 1
		;;
	esac
	# The loader may set shell options; hide its output, check its status, and
	# restore strict mode before any later guard.
	# shellcheck disable=SC1090
	if source "$loader" >/dev/null 2>&1; then loader_ok=1; else loader_ok=0; fi
	set -euo pipefail
	[[ "$loader_ok" == 1 ]] || {
		echo 'error: the approved environment loader failed' >&2
		exit 1
	}
fi

# A loader may change directory or the tag; bind every guard to this cut.
cd "$root" || exit 1
[[ "$(pwd -P)" == "$root" && "${RECORDPRIMS_RELEASE_TAG:-}" == "$intended" ]] || {
	echo 'error: the loader changed the repository root or the release tag' >&2
	exit 1
}
export RECORDPRIMS_RELEASE_TAG RECORDPRIMS_TAG_MESSAGE_DIR
if [[ "$mode" == prepare-message ]]; then
	exec "${PYTHON:-python3}" -P -B -E "$root/scripts/release-prepare-tag-message.py"
fi
export RECORDPRIMS_TAGGER_NAME RECORDPRIMS_TAGGER_EMAIL RECORDPRIMS_PGP_KEY_ID RECORDPRIMS_GPG_HOMEDIR RECORDPRIMS_DECERNOR_BIN

# shellcheck source=scripts/release-tag-common.sh
source "$root/scripts/release-tag-common.sh"
tag_version
tag_identity
tag_selector
# shellcheck source=scripts/release-decernor.sh
source "$root/scripts/release-decernor.sh"
resolve_release_decernor || {
	echo 'error: the checker is required before any release side effect' >&2
	exit 1
}
[[ "$(git symbolic-ref --quiet --short HEAD)" == main ]] || {
	echo 'error: release tags are created from main' >&2
	exit 1
}
tag_checkout
tag_load_set
for tag in "${tag_names[@]}"; do
	tag_message_file "$tag" >/dev/null
done

case "$mode" in
local-tag) "$root/scripts/release-tag.sh" ;;
remote-push) "$root/scripts/release-push-tag.sh" ;;
esac
