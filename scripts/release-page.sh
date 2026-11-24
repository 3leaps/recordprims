#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer only: the GitHub Release page for the anchored tag.
#
#   draft DIR           verify DIR, refuse if any release exists for the tag,
#                       then create one draft with every asset at once and the
#                       page body from the tagged release notes, and check the
#                       draft's assets byte for byte.
#   publish DIR         verify DIR and the published tag, check the one draft
#                       still holds exactly DIR's bytes and body, check that
#                       immutable releases are enabled, publish it, and require
#                       GitHub to report it immutable.
#   recover-draft DIR   for a draft a failed create left partial: with
#                       RECORDPRIMS_RELEASE_DRAFT_ID naming it and
#                       RECORDPRIMS_RELEASE_DRAFT_ACTION=delete or upload,
#                       confirm it is the only release for the tag, a draft at
#                       the anchored commit whose uploaded assets are a
#                       byte-identical subset of DIR, then delete it (so draft
#                       can run again) or delete any zero-byte `starter` asset
#                       a failed upload left (by its exact asset id), upload
#                       the missing assets and check the draft is complete.
#   verify-published    download the published release into a new directory
#                       and verify it like a staged one; it must be immutable.
#
# Every step re-verifies the published tag against the ceremony anchor first.
set -euo pipefail
# shellcheck source=scripts/release-assets.sh
source "$(dirname "$0")/release-assets.sh"
# shellcheck source=scripts/release-gh.sh
source "$assets_root/scripts/release-gh.sh"
mode="${1:-}"
case "$mode" in
draft | publish | recover-draft) [[ $# -eq 2 ]] || assets_die "usage: release-page.sh $mode DIR" ;;
verify-published) [[ $# -eq 1 ]] || assets_die 'usage: release-page.sh verify-published' ;;
*) assets_die 'usage: release-page.sh draft DIR | publish DIR | recover-draft DIR | verify-published' ;;
esac
dir="${2:-}"
release_anchor
RECORDPRIMS_EXPECTED_TAG_OBJECT="$anchor_object" RECORDPRIMS_EXPECTED_COMMIT="$anchor_commit" \
	"$assets_root/scripts/release-verify-published-tag.sh" >/dev/null
release_anchor_local
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

# releases_for_tag: every release (drafts included) whose tag is the anchored tag.
releases_for_tag() {
	release_gh api --paginate "repos/$assets_repo/releases?per_page=100" |
		jq -s --arg tag "$anchor_tag" '[add // [] | .[] | select(.tag_name == $tag)]'
}

# normalized FILE: LF line endings and a final newline, the only changes
# GitHub makes to a release body.
normalized() { awk '{sub(/\r$/, "")} 1' "$1"; }

# remote_subset ID DIR: every asset on the release is named in DIR's set; every
# uploaded asset has DIR's bytes. Assets in GitHub's `starter` state (an upload
# that failed, zero bytes) are listed, not compared, in scratch/starters as
# "ID NAME"; any other state is refused. Leaves the sorted local and uploaded
# names and the release JSON in scratch.
remote_subset() {
	local id="$1" local_dir="$2" json name patterns=()
	json="$(release_gh api "repos/$assets_repo/releases/$id")" || assets_die "release $id is unavailable"
	printf '%s\n' "$json" >"$scratch/release.json"
	release_uploaded | LC_ALL=C sort >"$scratch/local-names"
	[[ -z "$(jq -r '.assets[].name' <<<"$json" | LC_ALL=C sort | LC_ALL=C comm -13 "$scratch/local-names" -)" ]] ||
		assets_die 'the release holds assets outside the staged set'
	[[ "$(jq '[.assets[] | select((.state // "uploaded") | IN("uploaded", "starter") | not)] | length' <<<"$json")" == 0 ]] ||
		assets_die 'the release holds an asset in an unexpected state'
	jq -r '.assets[] | select((.state // "uploaded") == "uploaded") | .name' <<<"$json" | LC_ALL=C sort >"$scratch/remote-names"
	jq -r '.assets[] | select(.state == "starter") | "\(.id) \(.name)"' <<<"$json" >"$scratch/starters"
	rm -rf "$scratch/remote"
	mkdir "$scratch/remote"
	while read -r name; do patterns+=(-p "$name"); done <"$scratch/remote-names"
	if [[ "${#patterns[@]}" -gt 0 ]]; then
		release_gh release download "$anchor_tag" --repo "$assets_repo" --dir "$scratch/remote" "${patterns[@]}" >/dev/null ||
			assets_die 'could not download the release assets'
	fi
	while read -r name; do
		cmp -s "$scratch/remote/$name" "$local_dir/$name" || assets_die "remote $name differs from the staged bytes"
	done <"$scratch/remote-names"
}

# remote_matches ID DIR: the release holds exactly DIR's assets, all uploaded,
# byte for byte, and the tagged notes as its body.
remote_matches() {
	remote_subset "$1" "$2"
	[[ ! -s "$scratch/starters" ]] || assets_die 'the release holds an incomplete (starter) asset'
	cmp -s "$scratch/remote-names" "$scratch/local-names" || assets_die 'the release assets differ from the staged set'
	jq -j '.body // ""' "$scratch/release.json" >"$scratch/body"
	cmp -s <(normalized "$scratch/body") <(normalized "$2/$(release_notes_name)") ||
		assets_die 'the release body differs from the tagged release notes'
}

case "$mode" in
draft)
	"$assets_root/scripts/release-verify-assets.sh" "$dir" >/dev/null
	[[ "$(releases_for_tag | jq length)" == 0 ]] || assets_die "a release for $anchor_tag already exists"
	assets=()
	while read -r name; do assets+=("$dir/$name"); done < <(release_uploaded)
	release_gh release create "$anchor_tag" --repo "$assets_repo" --verify-tag --draft --target "$anchor_commit" \
		--title "recordprims $anchor_version" --notes-file "$dir/$(release_notes_name)" "${assets[@]}" >/dev/null ||
		assets_die 'creating the draft failed; if a partial draft exists, run make release-recover-draft'
	drafts="$(releases_for_tag)"
	[[ "$(jq '[.[] | select(.draft == true)] | length' <<<"$drafts")" == 1 && "$(jq length <<<"$drafts")" == 1 ]] ||
		assets_die "expected exactly one draft for $anchor_tag"
	remote_matches "$(jq -r '.[0].id' <<<"$drafts")" "$dir"
	echo "[ok] draft release for $anchor_tag holds the verified assets; inspect it, then make release-publish"
	;;
publish)
	"$assets_root/scripts/release-verify-assets.sh" "$dir" >/dev/null
	drafts="$(releases_for_tag)"
	[[ "$(jq length <<<"$drafts")" == 1 && "$(jq -r '.[0].draft' <<<"$drafts")" == true ]] ||
		assets_die "expected exactly one draft release for $anchor_tag"
	id="$(jq -r '.[0].id' <<<"$drafts")"
	remote_matches "$id" "$dir"
	# Publish only where GitHub will make the release immutable.
	status=0
	release_immutable_releases "$assets_repo" || status=$?
	[[ "$status" == 0 ]] ||
		assets_die "immutable releases must be enabled for $assets_repo before publishing; the draft stays unpublished"
	release_gh api -X PATCH "repos/$assets_repo/releases/$id" -F draft=false >/dev/null || assets_die 'publishing failed'
	published="$(release_gh api "repos/$assets_repo/releases/tags/$anchor_tag")" || assets_die 'the published release is unavailable'
	jq -e --arg tag "$anchor_tag" '.tag_name == $tag and .draft == false and .immutable == true' <<<"$published" >/dev/null ||
		assets_die "the published release for $anchor_tag is not reported immutable"
	echo "[ok] $anchor_tag published and immutable; run make release-verify-published"
	;;
recover-draft)
	"$assets_root/scripts/release-verify-assets.sh" "$dir" >/dev/null
	id="${RECORDPRIMS_RELEASE_DRAFT_ID:-}"
	action="${RECORDPRIMS_RELEASE_DRAFT_ACTION:-}"
	[[ "$id" =~ ^[0-9]+$ ]] || assets_die 'RECORDPRIMS_RELEASE_DRAFT_ID must name the draft release id'
	[[ "$action" == delete || "$action" == upload ]] || assets_die 'RECORDPRIMS_RELEASE_DRAFT_ACTION must be delete or upload'
	drafts="$(releases_for_tag)"
	jq -e --arg id "$id" --arg commit "$anchor_commit" \
		'length == 1 and (.[0].id | tostring) == $id and .[0].draft == true and .[0].target_commitish == $commit' \
		<<<"$drafts" >/dev/null ||
		assets_die "release $id is not the only release for $anchor_tag as a draft at $anchor_commit"
	remote_subset "$id" "$dir"
	if [[ "$action" == delete ]]; then
		release_gh api -X DELETE "repos/$assets_repo/releases/$id" >/dev/null || assets_die "deleting draft $id failed"
		[[ "$(releases_for_tag | jq length)" == 0 ]] || assets_die "a release for $anchor_tag still exists"
		echo "[ok] partial draft $id deleted; run make release-create-draft again"
	else
		# A starter asset on this confirmed draft is removed by its exact id,
		# then uploaded again with the other missing assets.
		while read -r asset name; do
			[[ "$asset" =~ ^[0-9]+$ ]] || assets_die "starter asset $name has no valid id"
			release_gh api -X DELETE "repos/$assets_repo/releases/assets/$asset" >/dev/null ||
				assets_die "deleting starter asset $name ($asset) failed"
		done <"$scratch/starters"
		missing=()
		while read -r name; do
			missing+=("$dir/$name")
		done < <(LC_ALL=C comm -23 "$scratch/local-names" "$scratch/remote-names")
		if [[ "${#missing[@]}" -gt 0 ]]; then
			release_gh release upload "$anchor_tag" --repo "$assets_repo" "${missing[@]}" >/dev/null ||
				assets_die 'uploading the missing assets failed; run recover-draft again'
		fi
		remote_matches "$id" "$dir"
		echo "[ok] draft $id now holds every verified asset; inspect it, then make release-publish"
	fi
	;;
verify-published)
	published="$(release_gh api "repos/$assets_repo/releases/tags/$anchor_tag")" || assets_die "no published release for $anchor_tag"
	jq -e --arg tag "$anchor_tag" '.tag_name == $tag and .draft == false and .immutable == true' <<<"$published" >/dev/null ||
		assets_die "the release for $anchor_tag is not published and immutable"
	mkdir "$scratch/page"
	release_gh release download "$anchor_tag" --repo "$assets_repo" --dir "$scratch/page" >/dev/null ||
		assets_die 'could not download the published assets'
	jq -j '.body // ""' <<<"$published" >"$scratch/body"
	# The body matches the tagged notes up to GitHub's normalization; the
	# tagged notes then stand in as the page body for the asset check.
	release_blob "docs/releases/$anchor_tag.md" "$scratch/notes"
	cmp -s <(normalized "$scratch/body") <(normalized "$scratch/notes") ||
		assets_die 'the published body differs from the tagged release notes'
	cp "$scratch/notes" "$scratch/page/$(release_notes_name)"
	"$assets_root/scripts/release-verify-assets.sh" "$scratch/page" >/dev/null
	echo "[ok] published $anchor_tag: immutable, body and every asset verified against $anchor_commit"
	;;
esac
