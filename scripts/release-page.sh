#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer only: the GitHub Release page for the anchored tag.
#
#   draft DIR           verify DIR, refuse if any release exists for the tag,
#                       then create one draft with every asset at once and the
#                       page body from the tagged release notes, and check the
#                       draft's assets byte for byte.
#   publish DIR         verify DIR and the published tag, check the one draft
#                       still holds exactly DIR's bytes and body, publish it,
#                       and require GitHub to report it immutable.
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
draft | publish) [[ $# -eq 2 ]] || assets_die "usage: release-page.sh $mode DIR" ;;
verify-published) [[ $# -eq 1 ]] || assets_die 'usage: release-page.sh verify-published' ;;
*) assets_die 'usage: release-page.sh draft DIR | publish DIR | verify-published' ;;
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

# remote_matches ID DIR: the release holds exactly DIR's assets, byte for byte.
remote_matches() {
	local id="$1" local_dir="$2" json
	json="$(release_gh api "repos/$assets_repo/releases/$id")" || assets_die "release $id is unavailable"
	jq -r '.assets[].name' <<<"$json" | LC_ALL=C sort >"$scratch/remote-names"
	release_uploaded | LC_ALL=C sort >"$scratch/local-names"
	cmp -s "$scratch/remote-names" "$scratch/local-names" || assets_die 'the release assets differ from the staged set'
	rm -rf "$scratch/remote"
	release_gh release download "$anchor_tag" --repo "$assets_repo" --dir "$scratch/remote" >/dev/null ||
		assets_die 'could not download the release assets'
	while read -r name; do
		cmp -s "$scratch/remote/$name" "$local_dir/$name" || assets_die "remote $name differs from the staged bytes"
	done < <(release_uploaded)
	jq -j '.body // ""' <<<"$json" >"$scratch/body"
	cmp -s <(normalized "$scratch/body") <(normalized "$local_dir/$(release_notes_name)") ||
		assets_die 'the release body differs from the tagged release notes'
}

case "$mode" in
draft)
	"$assets_root/scripts/release-verify-assets.sh" "$dir" >/dev/null
	[[ "$(releases_for_tag | jq length)" == 0 ]] || assets_die "a release for $anchor_tag already exists"
	assets=()
	while read -r name; do assets+=("$dir/$name"); done < <(release_uploaded)
	release_gh release create "$anchor_tag" --repo "$assets_repo" --verify-tag --draft --target "$anchor_commit" \
		--title "recordprims $anchor_version" --notes-file "$dir/$(release_notes_name)" "${assets[@]}" >/dev/null
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
	release_gh api -X PATCH "repos/$assets_repo/releases/$id" -F draft=false >/dev/null || assets_die 'publishing failed'
	published="$(release_gh api "repos/$assets_repo/releases/tags/$anchor_tag")" || assets_die 'the published release is unavailable'
	jq -e --arg tag "$anchor_tag" '.tag_name == $tag and .draft == false and .immutable == true' <<<"$published" >/dev/null ||
		assets_die "the published release for $anchor_tag is not reported immutable"
	echo "[ok] $anchor_tag published and immutable; run make release-verify-published"
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
