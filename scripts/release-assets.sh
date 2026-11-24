#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Source this file. Shared rules for the release page: the recorded ceremony
# anchor binds every step to one tag object and commit, and the asset set is
# fixed here, not configured. Public files are copied from git blobs at the
# anchored commit, never from the working tree.
set -euo pipefail

assets_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck disable=SC2034 # Consumed by scripts sourcing this helper.
assets_repo=3leaps/recordprims

assets_die() {
	echo "error: $*" >&2
	exit 1
}

# release_anchor: read RECORDPRIMS_RELEASE_ANCHOR_FILE (written once by
# make release-record-anchor) and set anchor_tag, anchor_object,
# anchor_commit and anchor_version. The tag must be RECORDPRIMS_RELEASE_TAG.
release_anchor() {
	local file="${RECORDPRIMS_RELEASE_ANCHOR_FILE:-}" content
	[[ "${RECORDPRIMS_RELEASE_TAG:-}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
		assets_die 'canonical RECORDPRIMS_RELEASE_TAG required'
	[[ "$file" == /* && -f "$file" && ! -L "$file" ]] ||
		assets_die 'RECORDPRIMS_RELEASE_ANCHOR_FILE must name the recorded ceremony anchor (absolute regular file)'
	content="$(cat "$file")"
	local pattern='^tag=(v[0-9]+\.[0-9]+\.[0-9]+)'$'\n''object=([0-9a-f]{40})'$'\n''commit=([0-9a-f]{40})$'
	[[ "$content" =~ $pattern ]] ||
		assets_die 'the ceremony anchor is malformed'
	anchor_tag="${BASH_REMATCH[1]}"
	anchor_object="${BASH_REMATCH[2]}"
	anchor_commit="${BASH_REMATCH[3]}"
	[[ "$anchor_tag" == "$RECORDPRIMS_RELEASE_TAG" ]] || assets_die 'the ceremony anchor names another tag'
	anchor_version="${anchor_tag#v}"
}

# release_anchor_local: the local tag must still be the anchored object at
# the anchored commit.
release_anchor_local() {
	[[ "$(git -C "$assets_root" rev-parse --verify --quiet "refs/tags/$anchor_tag")" == "$anchor_object" &&
		"$(git -C "$assets_root" rev-parse "refs/tags/$anchor_tag^{commit}")" == "$anchor_commit" ]] ||
		assets_die "local $anchor_tag is not the anchored tag object and commit"
}

# Public files: asset name, then its path in the tagged commit.
release_public_files() {
	cat <<'EOF'
LICENSE-APACHE LICENSE-APACHE
LICENSE-MIT LICENSE-MIT
NOTICE NOTICE
release-signing-keys.asc docs/security/release-signing-keys.asc
recordprims-minisign.pub docs/security/recordprims-minisign.pub
expected-fingerprints.txt keys/expected-fingerprints.txt
EOF
}

release_source_name() { printf 'recordprims-%s-src.tar.gz\n' "$anchor_version"; }

# The files the checksum manifests cover, in manifest order.
release_summed() {
	release_source_name
	release_public_files | awk '{print $1}'
}

# Every asset on the page: the summed files, both manifests and their
# minisign and OpenPGP signatures.
release_uploaded() {
	release_summed
	local sums
	for sums in SHA256SUMS SHA512SUMS; do
		printf '%s\n%s.minisig\n%s.asc\n' "$sums" "$sums" "$sums"
	done
}

# The page body, kept beside the assets but not uploaded as one.
release_notes_name() { printf 'release-notes.md\n'; }

# release_blob PATH OUT: a regular file from the anchored commit.
release_blob() {
	local path="$1" out="$2"
	[[ "$(git -C "$assets_root" ls-tree "$anchor_commit" -- "$path" | awk '{print $1}')" == 100644 ]] ||
		assets_die "$path is required as a regular file in the tagged commit"
	git -C "$assets_root" cat-file blob "$anchor_commit:$path" >"$out"
}

# release_dir DIR: an absolute, non-symlink directory.
release_dir() {
	[[ "$1" == /* && -d "$1" && ! -L "$1" ]] || assets_die "$1 must be an existing absolute directory"
}

# release_source_tar OUT: the uncompressed archive of the anchored commit, to
# compare with any source archive by content (gzip bytes vary with the
# compressor; the tar stream from git archive does not).
release_source_tar() {
	git -C "$assets_root" archive --format=tar --prefix="recordprims-$anchor_version/" "$anchor_commit" >"$1"
}

release_hash() {
	python3 -I -B -c 'import hashlib, sys
h = hashlib.new(sys.argv[1])
with open(sys.argv[2], "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
print(h.hexdigest())' "$1" "$2"
}
