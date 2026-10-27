#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# The release tag verifies only against the pin committed in the tagged commit,
# and only when it is annotated, signed by the committed signing subkey and
# points at the release commit.
set -euo pipefail
# shellcheck disable=SC2119 # verify is called with and without --published.
# shellcheck source=scripts/release-test-fixture.sh
source "$(dirname "$0")/release-test-fixture.sh"
decernor="$(fixture_decernor)"
version=1.2.3
repo="$fixture_dir/repo"
messages="$fixture_dir/messages/v$version"
read -r primary subkey < <(fixture_keys "$fixture_dir/gpg")
read -r other_primary other_subkey < <(fixture_keys "$fixture_dir/gpg-other")

fixture_repo "$repo" "$version"
gpg --homedir "$fixture_dir/gpg" --batch --armor --export-options export-minimal --export "$subkey!" \
	>"$repo/docs/security/release-signing-keys.asc"
fixture_minisign_pub "$repo/docs/security/recordprims-minisign.pub" 1
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo" pins
RECORDPRIMS_DECERNOR_BIN="$decernor" "$repo/scripts/release-insert-anchors.sh" >/dev/null
[[ "$(awk '$1=="gpg" {print $2}' "$repo/keys/expected-fingerprints.txt")" == "$primary" ]]
fixture_commit "$repo" anchors

tags=("v$version")
fixture_messages "$messages" "$version" "${tags[@]}"
sign() {
	local home="$1" key="$2" tag="$3" target="${4:-HEAD}"
	GNUPGHOME="$home" GIT_COMMITTER_NAME='3 Leaps Infosec Team' GIT_COMMITTER_EMAIL=infosec@3leaps.net \
		git -C "$repo" tag -s -a --cleanup=verbatim -u "$key!" -F "$messages/${tag//\//__}.txt" "$tag" "$target"
}
for tag in "${tags[@]}"; do sign "$fixture_dir/gpg" "$subkey" "$tag"; done

# shellcheck disable=SC2120 # --published is passed by some calls only.
verify() {
	(cd "$repo" && RECORDPRIMS_RELEASE_TAG="v$version" RELEASE_DECERNOR_BIN="$decernor" ./scripts/verify-pinned-tag.sh "$@")
}
out="$(verify)"
[[ "$out" == *"1 signed tag(s) verified"* ]]

retag() { git -C "$repo" tag -d "$1" >/dev/null; }

# A tag that points at a different commit fails, even though it is validly signed.
release_commit="$(git -C "$repo" rev-parse HEAD)"
git -C "$repo" commit -q --allow-empty -m later
expect_fail_with 'tag must point at HEAD' verify
git -C "$repo" reset -q --hard "$release_commit"
verify >/dev/null

# Only refs/tags is read by default: preserved good tags in a private
# namespace, selected through the environment, cannot stand in for an
# unsigned replacement at refs/tags.
for tag in "${tags[@]}"; do git -C "$repo" update-ref "refs/recordprims-verify/1.1/tags/$tag" "refs/tags/$tag"; done
retag "v$version"
GIT_COMMITTER_NAME='3 Leaps Infosec Team' GIT_COMMITTER_EMAIL=infosec@3leaps.net \
	git -C "$repo" tag -a --cleanup=verbatim -F "$messages/v$version.txt" "v$version" HEAD
(cd "$repo" && expect_fail_with "signature is invalid under the pin|not signed by the committed signing subkey" \
	env RECORDPRIMS_VERIFY_REF_PREFIX=refs/recordprims-verify/1.1/tags/ RECORDPRIMS_RELEASE_TAG="v$version" \
	RELEASE_DECERNOR_BIN="$decernor" ./scripts/verify-pinned-tag.sh)
(cd "$repo" && RECORDPRIMS_RELEASE_TAG="v$version" RELEASE_DECERNOR_BIN="$decernor" \
	./scripts/verify-pinned-tag.sh --staged-prefix refs/recordprims-verify/1.1/tags/ >/dev/null)
(cd "$repo" && expect_fail_with 'invalid --staged-prefix' env RECORDPRIMS_RELEASE_TAG="v$version" \
	RELEASE_DECERNOR_BIN="$decernor" ./scripts/verify-pinned-tag.sh --staged-prefix refs/heads/)
retag "v$version"
sign "$fixture_dir/gpg" "$subkey" "v$version"
for tag in "${tags[@]}"; do git -C "$repo" update-ref -d "refs/recordprims-verify/1.1/tags/$tag"; done
verify >/dev/null

# A publishing workflow in the tagged commit that differs from its pin fails,
# even though every tag is validly signed.
git -C "$repo" tag -d "${tags[@]}" >/dev/null
printf '# unreviewed\n' >>"$repo/.github/workflows/release.yml"
fixture_commit "$repo" unreviewed-workflow
for tag in "${tags[@]}"; do sign "$fixture_dir/gpg" "$subkey" "$tag"; done
expect_fail_with 'publishing workflow in the tagged commit differs' verify
git -C "$repo" tag -d "${tags[@]}" >/dev/null
git -C "$repo" reset -q --hard HEAD~1
for tag in "${tags[@]}"; do sign "$fixture_dir/gpg" "$subkey" "$tag"; done
verify >/dev/null

# A lightweight tag fails.
retag "v$version"
git -C "$repo" tag "v$version"
expect_fail_with 'must be an annotated tag' verify
retag "v$version"
sign "$fixture_dir/gpg" "$subkey" "v$version"
verify >/dev/null

# A tag signed by a key that is not the committed subkey fails.
retag "v$version"
sign "$fixture_dir/gpg-other" "$other_subkey" "v$version"
expect_fail_with 'signature is invalid under the pin|not signed by the committed signing subkey' verify
retag "v$version"
sign "$fixture_dir/gpg" "$subkey" "v$version"
verify >/dev/null

# A leftover path-prefixed module tag from the two-tag layout is not part of
# the release: it is neither required nor counted.
fixture_messages "$messages" "$version" "go/canon/v$version"
sign "$fixture_dir/gpg" "$subkey" "go/canon/v$version"
out="$(verify)"
[[ "$out" == *"1 signed tag(s) verified"* ]]
retag "go/canon/v$version"

# A wrong tagger fails.
retag "v$version"
GNUPGHOME="$fixture_dir/gpg" GIT_COMMITTER_NAME='Someone Else' GIT_COMMITTER_EMAIL=else@example.invalid \
	git -C "$repo" tag -s -a --cleanup=verbatim -u "$subkey!" -F "$messages/v$version.txt" "v$version" HEAD
expect_fail_with 'tagger differs' verify
retag "v$version"
sign "$fixture_dir/gpg" "$subkey" "v$version"
verify >/dev/null

# A missing release tag fails.
retag "v$version"
expect_fail_with "v$version is absent" verify
sign "$fixture_dir/gpg" "$subkey" "v$version"
verify >/dev/null

# Working-tree edits to the pins are never consulted: replacing them with the
# other key leaves verification bound to the tagged commit.
gpg --homedir "$fixture_dir/gpg-other" --batch --armor --export-options export-minimal --export "$other_subkey!" \
	>"$repo/docs/security/release-signing-keys.asc"
printf '%s\n' "$other_subkey" >"$repo/config/release/signing-subkey.txt"
verify >/dev/null
git -C "$repo" checkout -q -- docs/security config/release

# A subkey file that names a different subkey fails at the committed source.
git -C "$repo" tag -d "${tags[@]}" >/dev/null
printf '%s\n' "$other_subkey" >"$repo/config/release/signing-subkey.txt"
fixture_commit "$repo" wrong-subkey
for tag in "${tags[@]}"; do sign "$fixture_dir/gpg" "$subkey" "$tag"; done
expect_fail_with 'not signed by the committed signing subkey' verify

# A tagged root go.mod declaring another module path is not a release.
git -C "$repo" tag -d "${tags[@]}" >/dev/null
git -C "$repo" checkout -q -- .
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
printf 'module example.com/other\n' >"$repo/go.mod"
fixture_commit "$repo" wrong-module
for tag in "${tags[@]}"; do sign "$fixture_dir/gpg" "$subkey" "$tag"; done
expect_fail_with 'must declare module github.com/3leaps/recordprims$' verify
# The old nested module path is refused too: the module must be at the root.
git -C "$repo" tag -d "${tags[@]}" >/dev/null
printf 'module github.com/3leaps/recordprims/go/canon\n' >"$repo/go.mod"
fixture_commit "$repo" nested-module-path
for tag in "${tags[@]}"; do sign "$fixture_dir/gpg" "$subkey" "$tag"; done
expect_fail_with 'must declare module github.com/3leaps/recordprims$' verify
printf 'module github.com/3leaps/recordprims\n' >"$repo/go.mod"
fixture_commit "$repo" right-module

# A tagged tree without the root Go module is not a release, even if it
# carries the module in the old nested place.
git -C "$repo" tag -d "${tags[@]}" >/dev/null
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
git -C "$repo" rm -q go.mod
mkdir -p "$repo/go/canon"
printf 'module github.com/3leaps/recordprims/go/canon\n' >"$repo/go/canon/go.mod"
fixture_commit "$repo" no-go-module
for tag in "${tags[@]}"; do sign "$fixture_dir/gpg" "$subkey" "$tag"; done
expect_fail_with 'go.mod is required at the root' verify
[[ "$other_primary" != "$primary" ]]
echo '[ok] verify-pinned-tag controls passed'
