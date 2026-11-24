#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# The release page: staging binds to the ceremony anchor; the source archive
# comes from the one successful release run (or, for a tag cut before the
# workflow existed, a local backfill that is refused for any other tag); the
# public files and notes are the tagged blobs; signatures must come from the
# committed keys; the draft is created once with every asset and published
# only while it still holds the verified bytes, and must then be immutable.
# GitHub is an offline stub; the published-tag check is stubbed to the
# anchor comparison (it has its own controls).
set -euo pipefail
# shellcheck source=scripts/release-test-fixture.sh
source "$(dirname "$0")/release-test-fixture.sh"
decernor="$(fixture_decernor)"
repo="$fixture_dir/repo"
read -r primary subkey < <(fixture_keys "$fixture_dir/gpg")
read -r _ other_subkey < <(fixture_keys "$fixture_dir/gpg-other")
minisign -G -W -p "$fixture_dir/minisign.pub" -s "$fixture_dir/minisign.key" >/dev/null 2>&1
minisign -G -W -p "$fixture_dir/other.pub" -s "$fixture_dir/other.key" >/dev/null 2>&1

fixture_repo "$repo" 0.1.0
for script in release-assets.sh release-stage.sh release-sign.sh release-verify-assets.sh release-page.sh; do
	cp "$fixture_root/scripts/$script" "$repo/scripts/"
done
cat >"$repo/scripts/release-verify-published-tag.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(git rev-parse "refs/tags/$RECORDPRIMS_RELEASE_TAG")" == "$RECORDPRIMS_EXPECTED_TAG_OBJECT" ]]
[[ "$(git rev-parse "refs/tags/$RECORDPRIMS_RELEASE_TAG^{commit}")" == "$RECORDPRIMS_EXPECTED_COMMIT" ]]
SH
chmod +x "$repo/scripts/release-verify-published-tag.sh"
gpg --homedir "$fixture_dir/gpg" --batch --armor --export-options export-minimal --export "$subkey!" \
	>"$repo/docs/security/release-signing-keys.asc"
cp "$fixture_dir/minisign.pub" "$repo/docs/security/recordprims-minisign.pub"
printf '%s\n' "$subkey" >"$repo/config/release/signing-subkey.txt"
printf 'apache\n' >"$repo/LICENSE-APACHE"
printf 'mit\n' >"$repo/LICENSE-MIT"
printf 'notice\n' >"$repo/NOTICE"
mkdir -p "$repo/docs/releases"
printf '# v0.1.0\n\nFirst.\n' >"$repo/docs/releases/v0.1.0.md"
# The first release predates the release workflow.
rm -rf "$repo/.github"
fixture_commit "$repo" pins
RECORDPRIMS_DECERNOR_BIN="$decernor" RECORDPRIMS_ANCHOR_COMMIT=HEAD "$repo/scripts/release-insert-anchors.sh" >/dev/null
fixture_commit "$repo" anchors
GIT_COMMITTER_NAME=fixture git -C "$repo" tag -a -m 'recordprims 0.1.0' v0.1.0

anchor() {
	local tag="$1"
	printf 'tag=%s\nobject=%s\ncommit=%s\n' "$tag" "$(git -C "$repo" rev-parse "refs/tags/$tag")" \
		"$(git -C "$repo" rev-parse "refs/tags/$tag^{commit}")" >"$fixture_dir/$tag.anchor"
}
anchor v0.1.0

# Offline GitHub.
store="$fixture_dir/github"
mkdir -p "$store/releases" "$fixture_dir/bin"
printf '[]\n' >"$store/runs.json"
printf '#!/bin/sh\nexec python3 -I -B "%s" "$@"\n' "$fixture_root/scripts/release-page-gh-stub.py" >"$fixture_dir/bin/gh"
chmod +x "$fixture_dir/bin/gh"
export PATH="$fixture_dir/bin:$PATH" GH_STUB_STORE="$store"
unset GH_HOST GH_REPO GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN

run() {
	local tag="$1"
	shift
	(cd "$repo" && env RECORDPRIMS_RELEASE_TAG="$tag" RECORDPRIMS_RELEASE_ANCHOR_FILE="$fixture_dir/$tag.anchor" "$@")
}
stage() { run "$1" ./scripts/release-stage.sh "$2" "$3"; }
sign() {
	run "$1" env RECORDPRIMS_MINISIGN_KEY="${4:-$fixture_dir/minisign.key}" RECORDPRIMS_PGP_KEY_ID="${3:-$subkey}!" \
		RECORDPRIMS_GPG_HOMEDIR="$fixture_dir/gpg" ./scripts/release-sign.sh "$2"
}
verify() { run "$1" ./scripts/release-verify-assets.sh "$2"; }
page() { run "$@"; }

# Anchor binding: required, well formed, for this tag, and matching the local tag.
dist="$fixture_dir/dist-v0.1.0"
expect_fail_with 'recorded ceremony anchor' run v0.1.0 env RECORDPRIMS_RELEASE_ANCHOR_FILE=relative ./scripts/release-stage.sh public "$dist"
printf 'tag=v0.1.0\n' >"$fixture_dir/bad.anchor"
expect_fail_with 'anchor is malformed' run v0.1.0 env RECORDPRIMS_RELEASE_ANCHOR_FILE="$fixture_dir/bad.anchor" ./scripts/release-stage.sh public "$dist"
expect_fail_with 'names another tag' run v0.2.0 env RECORDPRIMS_RELEASE_ANCHOR_FILE="$fixture_dir/v0.1.0.anchor" ./scripts/release-stage.sh public "$dist"

# A release cut before the workflow existed is built locally, only on request.
expect_fail_with 'set RECORDPRIMS_RELEASE_BACKFILL=1' stage v0.1.0 download "$dist"
run v0.1.0 env RECORDPRIMS_RELEASE_BACKFILL=1 ./scripts/release-stage.sh download "$dist" >/dev/null
expect_fail_with 'must be empty' run v0.1.0 env RECORDPRIMS_RELEASE_BACKFILL=1 ./scripts/release-stage.sh download "$dist"
stage v0.1.0 public "$dist" >/dev/null
expect_fail_with 'already staged' stage v0.1.0 public "$dist"
stage v0.1.0 checksums "$dist" >/dev/null
expect_fail_with 'already exists' stage v0.1.0 checksums "$dist"
grep -q '  recordprims-0.1.0-src.tar.gz$' "$dist/SHA256SUMS"
[[ "$(wc -l <"$dist/SHA512SUMS")" -eq 7 ]]

# Signing needs the committed subkey; the result verifies, and is never overwritten.
expect_fail_with 'committed signing subkey followed by !' sign v0.1.0 "$dist" "$other_subkey"
sign v0.1.0 "$dist" >/dev/null 2>&1
verify v0.1.0 "$dist" >/dev/null
expect_fail_with 'already signed' sign v0.1.0 "$dist"
run v0.1.0 env RECORDPRIMS_GPG_SIGNING_FINGERPRINT="$primary" ./scripts/release-verify-assets.sh "$dist" >/dev/null
expect_fail_with 'not the approved primary' run v0.1.0 env RECORDPRIMS_GPG_SIGNING_FINGERPRINT="${primary//?/A}" \
	./scripts/release-verify-assets.sh "$dist"

# Tampering of any kind is refused.
tamper() {
	local file="$1" pattern="$2" saved="$fixture_dir/saved"
	cp "$dist/$file" "$saved"
	printf 'x' >>"$dist/$file"
	expect_fail_with "$pattern" verify v0.1.0 "$dist"
	cp "$saved" "$dist/$file"
}
tamper NOTICE 'NOTICE differs'
tamper release-notes.md 'page body differs'
tamper SHA256SUMS 'SHA256SUMS does not match'
cp "$dist/recordprims-0.1.0-src.tar.gz" "$fixture_dir/saved"
git -C "$repo" archive --format=tar.gz --prefix=recordprims-0.1.0/ -o "$dist/recordprims-0.1.0-src.tar.gz" HEAD~1
expect_fail_with 'does not hold exactly the anchored commit' verify v0.1.0 "$dist"
cp "$fixture_dir/saved" "$dist/recordprims-0.1.0-src.tar.gz"
touch "$dist/extra"
expect_fail_with 'does not hold exactly the release assets' verify v0.1.0 "$dist"
rm "$dist/extra"
# Signatures by other keys: a valid OpenPGP signature by another key, and a
# valid minisign signature by another key.
mv "$dist/SHA256SUMS.asc" "$fixture_dir/saved"
gpg --homedir "$fixture_dir/gpg-other" --batch --local-user "$other_subkey!" --armor --detach-sign \
	--output "$dist/SHA256SUMS.asc" "$dist/SHA256SUMS" 2>/dev/null
expect_fail_with 'asc does not verify|not signed by the committed signing subkey' verify v0.1.0 "$dist"
mv "$fixture_dir/saved" "$dist/SHA256SUMS.asc"
mv "$dist/SHA512SUMS.minisig" "$fixture_dir/saved"
minisign -S -s "$fixture_dir/other.key" -m "$dist/SHA512SUMS" -x "$dist/SHA512SUMS.minisig" >/dev/null 2>&1
expect_fail_with 'minisig does not verify' verify v0.1.0 "$dist"
mv "$fixture_dir/saved" "$dist/SHA512SUMS.minisig"
verify v0.1.0 "$dist" >/dev/null

# Draft once with every asset; publish only the verified bytes; require immutable.
page v0.1.0 ./scripts/release-page.sh draft "$dist" >/dev/null
expect_fail_with 'already exists' page v0.1.0 ./scripts/release-page.sh draft "$dist"
[[ "$(grep -c '^release create' "$store/calls.log")" -eq 1 ]]
expect_fail_with 'no published release' page v0.1.0 ./scripts/release-page.sh verify-published
remote_asset="$store/releases/1000/assets/NOTICE"
cp "$remote_asset" "$fixture_dir/saved"
printf 'swap' >"$remote_asset"
expect_fail_with 'remote NOTICE differs' page v0.1.0 ./scripts/release-page.sh publish "$dist"
cp "$fixture_dir/saved" "$remote_asset"
page v0.1.0 ./scripts/release-page.sh publish "$dist" >/dev/null
page v0.1.0 ./scripts/release-page.sh verify-published >/dev/null
expect_fail_with 'exactly one draft' page v0.1.0 ./scripts/release-page.sh publish "$dist"

# A release that carries the workflow takes its archive from the one
# successful run, whose identity must be the anchor; a backfill is refused.
printf 'on:\n  push:\n    tags: [v*]\npermissions:\n  contents: read\njobs: {}\n' >"$repo/release.yml.tmp"
mkdir -p "$repo/.github/workflows"
mv "$repo/release.yml.tmp" "$repo/.github/workflows/release.yml"
printf '# v0.2.0\n' >"$repo/docs/releases/v0.2.0.md"
fixture_commit "$repo" workflow
GIT_COMMITTER_NAME=fixture git -C "$repo" tag -a -m 'recordprims 0.2.0' v0.2.0
anchor v0.2.0
commit="$(git -C "$repo" rev-parse HEAD)"
object="$(git -C "$repo" rev-parse refs/tags/v0.2.0)"
artifacts="$store/artifacts/77"
mkdir -p "$artifacts/release-identity" "$artifacts/source"
printf '{"tag":"v0.2.0","object":"%s","commit":"%s"}\n' "$object" "$commit" >"$artifacts/release-identity/release-identity.json"
git -C "$repo" archive --format=tar.gz --prefix=recordprims-0.2.0/ -o "$artifacts/source/recordprims-0.2.0-src.tar.gz" "$commit"
dist2="$fixture_dir/dist-v0.2.0"
expect_fail_with 'exactly one successful release.yml run' stage v0.2.0 download "$dist2"
printf '[{"databaseId":77,"headSha":"%s","headBranch":"v0.2.0","status":"completed","conclusion":"success"},{"databaseId":76,"headSha":"%s","headBranch":"v0.2.0","status":"completed","conclusion":"failure"}]\n' \
	"$commit" "$commit" >"$store/runs.json"
rm -rf "$dist2"
expect_fail_with 'comes from CI, not a local backfill' run v0.2.0 env RECORDPRIMS_RELEASE_BACKFILL=1 ./scripts/release-stage.sh download "$dist2"
rm -rf "$dist2"
expect_fail_with 'not a successful release run' run v0.2.0 env RECORDPRIMS_RELEASE_RUN_ID=76 ./scripts/release-stage.sh download "$dist2"
rm -rf "$dist2"
printf '{"tag":"v0.2.0","object":"%s","commit":"%s"}\n' "$object" "$(git -C "$repo" rev-parse HEAD~1)" \
	>"$artifacts/release-identity/release-identity.json"
expect_fail_with 'CI release identity differs' stage v0.2.0 download "$dist2"
rm -rf "$dist2"
printf '{"tag":"v0.2.0","object":"%s","commit":"%s"}\n' "$object" "$commit" >"$artifacts/release-identity/release-identity.json"
git -C "$repo" archive --format=tar.gz --prefix=recordprims-0.2.0/ -o "$artifacts/source/recordprims-0.2.0-src.tar.gz" HEAD~1
expect_fail_with 'does not hold exactly the anchored commit' stage v0.2.0 download "$dist2"
rm -rf "$dist2"
git -C "$repo" archive --format=tar.gz --prefix=recordprims-0.2.0/ -o "$artifacts/source/recordprims-0.2.0-src.tar.gz" "$commit"
stage v0.2.0 download "$dist2" >/dev/null
cmp -s "$dist2/recordprims-0.2.0-src.tar.gz" "$artifacts/source/recordprims-0.2.0-src.tar.gz"
stage v0.2.0 public "$dist2" >/dev/null
stage v0.2.0 checksums "$dist2" >/dev/null
sign v0.2.0 "$dist2" >/dev/null 2>&1

# A release that GitHub does not report immutable after publishing is refused.
page v0.2.0 ./scripts/release-page.sh draft "$dist2" >/dev/null
expect_fail_with 'not reported immutable' page v0.2.0 env GH_STUB_MUTABLE=1 ./scripts/release-page.sh publish "$dist2"
expect_fail_with 'not published and immutable' page v0.2.0 ./scripts/release-page.sh verify-published

# A local tag that moved away from the anchor stops every step.
GIT_COMMITTER_NAME=fixture git -C "$repo" tag -f -a -m 'moved' v0.1.0 HEAD >/dev/null
expect_fail_with 'local v0.1.0 is not the anchored tag' sign v0.1.0 "$dist"
echo '[ok] release page: anchor binding, CI archive or one-time backfill, tagged public files and notes, committed-key signatures, tamper refusals, one draft with every asset, publish of verified bytes only, immutable required'
