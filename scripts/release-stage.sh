#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Stage the release page assets in DIR, bound to the recorded ceremony anchor.
#
#   download DIR   the source archive: from the one successful release.yml run
#                  for the anchored commit, whose release identity must name
#                  the anchored tag object and commit and whose archive must
#                  hold exactly the tar stream of the anchored commit. A tag
#                  whose commit has no release.yml (a release cut before the
#                  workflow existed) is built locally instead, only with
#                  RECORDPRIMS_RELEASE_BACKFILL=1, which is refused for any
#                  other tag.
#   public DIR     LICENSE files, NOTICE, the public keys and fingerprints, and
#                  the page body (docs/releases/<tag>.md), from the tagged commit.
#   checksums DIR  SHA256SUMS and SHA512SUMS over the summed files.
#
# Run make release-verify-remote-tag first; download re-verifies the published
# tag against the anchor before reading anything.
set -euo pipefail
# shellcheck source=scripts/release-assets.sh
source "$(dirname "$0")/release-assets.sh"
mode="${1:-}"
dir="${2:-}"
[[ $# -eq 2 ]] || assets_die 'usage: release-stage.sh download|public|checksums DIR'
release_anchor

case "$mode" in
download)
	[[ "$dir" == /* && ! -L "$dir" ]] || assets_die 'DIR must be an absolute path'
	if [[ -e "$dir" ]]; then
		release_dir "$dir"
		[[ -z "$(ls -A "$dir")" ]] || assets_die "$dir must be empty"
	fi
	mkdir -p "$dir"
	RECORDPRIMS_EXPECTED_TAG_OBJECT="$anchor_object" RECORDPRIMS_EXPECTED_COMMIT="$anchor_commit" \
		"$assets_root/scripts/release-verify-published-tag.sh" >/dev/null
	release_anchor_local
	scratch="$(mktemp -d)"
	trap 'rm -rf "$scratch"' EXIT
	release_source_tar "$scratch/expected.tar"
	source_name="$(release_source_name)"
	if git -C "$assets_root" cat-file -e "$anchor_commit:.github/workflows/release.yml" 2>/dev/null; then
		[[ -z "${RECORDPRIMS_RELEASE_BACKFILL:-}" ]] ||
			assets_die "$anchor_tag has a release workflow; its archive comes from CI, not a local backfill"
		# shellcheck source=scripts/release-gh.sh
		source "$assets_root/scripts/release-gh.sh"
		run="${RECORDPRIMS_RELEASE_RUN_ID:-}"
		if [[ -z "$run" ]]; then
			run="$(release_gh run list --repo "$assets_repo" --workflow release.yml --event push --limit 100 \
				--json databaseId,headSha,headBranch,status,conclusion |
				jq -r --arg sha "$anchor_commit" --arg tag "$anchor_tag" '
					[.[] | select(.headSha == $sha and .headBranch == $tag and .status == "completed" and .conclusion == "success")]
					| if length == 1 then .[0].databaseId else empty end')" ||
				assets_die 'could not list release workflow runs'
			[[ -n "$run" ]] ||
				assets_die "need exactly one successful release.yml run for $anchor_tag at $anchor_commit (or set RECORDPRIMS_RELEASE_RUN_ID)"
		fi
		[[ "$run" =~ ^[0-9]+$ ]] || assets_die 'RECORDPRIMS_RELEASE_RUN_ID must be numeric'
		run_json="$(release_gh run view "$run" --repo "$assets_repo" --json headSha,headBranch,conclusion,workflowName)" ||
			assets_die "release workflow run $run is unavailable"
		jq -e --arg sha "$anchor_commit" --arg tag "$anchor_tag" \
			'.headSha == $sha and .headBranch == $tag and .conclusion == "success" and .workflowName == "Release"' \
			<<<"$run_json" >/dev/null || assets_die "run $run is not a successful release run for the anchored commit"
		release_gh run download "$run" --repo "$assets_repo" -n release-identity -n source -D "$scratch/ci" ||
			assets_die "could not download the artifacts of run $run"
		jq -e --arg tag "$anchor_tag" --arg object "$anchor_object" --arg commit "$anchor_commit" \
			'. == {tag: $tag, object: $object, commit: $commit}' "$scratch/ci/release-identity/release-identity.json" \
			>/dev/null || assets_die 'the CI release identity differs from the ceremony anchor'
		archive="$scratch/ci/source/$source_name"
		[[ -f "$archive" && ! -L "$archive" ]] || assets_die "the CI source artifact has no $source_name"
	else
		[[ "${RECORDPRIMS_RELEASE_BACKFILL:-}" == 1 ]] ||
			assets_die "$anchor_tag has no release workflow; set RECORDPRIMS_RELEASE_BACKFILL=1 to build its source archive locally"
		archive="$scratch/$source_name"
		git -C "$assets_root" archive --format=tar.gz --prefix="recordprims-$anchor_version/" -o "$archive" "$anchor_commit"
	fi
	gzip -dc "$archive" >"$scratch/got.tar" || assets_die "$source_name is not a gzip archive"
	cmp -s "$scratch/got.tar" "$scratch/expected.tar" ||
		assets_die "$source_name does not hold exactly the anchored commit"
	cp "$archive" "$dir/$source_name"
	echo "[ok] $source_name staged for $anchor_tag at $anchor_commit"
	;;
public)
	release_dir "$dir"
	while read -r name path; do
		[[ ! -e "$dir/$name" ]] || assets_die "$name is already staged"
		release_blob "$path" "$dir/$name"
	done < <(release_public_files)
	notes="$(release_notes_name)"
	[[ ! -e "$dir/$notes" ]] || assets_die "$notes is already staged"
	release_blob "docs/releases/$anchor_tag.md" "$dir/$notes"
	echo "[ok] public files and notes staged from $anchor_commit"
	;;
checksums)
	release_dir "$dir"
	for sums in SHA256SUMS SHA512SUMS; do
		[[ ! -e "$dir/$sums" ]] || assets_die "$sums already exists"
	done
	while read -r name; do
		[[ -f "$dir/$name" && ! -L "$dir/$name" ]] || assets_die "$name is not staged"
		printf '%s  %s\n' "$(release_hash sha256 "$dir/$name")" "$name" >>"$dir/SHA256SUMS"
		printf '%s  %s\n' "$(release_hash sha512 "$dir/$name")" "$name" >>"$dir/SHA512SUMS"
	done < <(release_summed)
	echo '[ok] SHA256SUMS and SHA512SUMS written; inspect them, then make release-sign'
	;;
*) assets_die 'usage: release-stage.sh download|public|checksums DIR' ;;
esac
