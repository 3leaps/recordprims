#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer only: sign SHA256SUMS and SHA512SUMS in DIR with minisign and with
# the committed OpenPGP signing subkey.
#
# The published tag is re-verified against the ceremony anchor, and every
# unsigned check (file set, tagged public files and notes, source archive,
# exact manifests) passes before the first signature. Signatures are written
# to a private copy and moved into DIR only after all four exist and the whole
# set verifies, so an interrupted signer leaves DIR unsigned and a retry can
# run. Inputs: RECORDPRIMS_MINISIGN_KEY (minisign secret key file, outside the
# repository), RECORDPRIMS_PGP_KEY_ID (the committed signing subkey followed
# by !), RECORDPRIMS_GPG_HOMEDIR.
set -euo pipefail
# shellcheck source=scripts/release-assets.sh
source "$(dirname "$0")/release-assets.sh"
dir="${1:-}"
[[ $# -eq 1 ]] || assets_die 'usage: release-sign.sh DIR'
release_anchor
release_dir "$dir"
key="${RECORDPRIMS_MINISIGN_KEY:-}"
[[ "$key" == /* && -f "$key" && ! -L "$key" ]] || assets_die 'RECORDPRIMS_MINISIGN_KEY must be an absolute regular file'
case "$(cd "$(dirname "$key")" && pwd -P)/" in "$assets_root/"*) assets_die 'the minisign key must be outside the repository' ;; esac
home="${RECORDPRIMS_GPG_HOMEDIR:-}"
[[ "$home" == /* && -d "$home" ]] || assets_die 'RECORDPRIMS_GPG_HOMEDIR must be an absolute directory'
RECORDPRIMS_EXPECTED_TAG_OBJECT="$anchor_object" RECORDPRIMS_EXPECTED_COMMIT="$anchor_commit" \
	"$assets_root/scripts/release-verify-published-tag.sh" >/dev/null
release_anchor_local
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
release_blob config/release/signing-subkey.txt "$scratch/subkey"
[[ "${RECORDPRIMS_PGP_KEY_ID:-}" == "$(tr -d '\r\n' <"$scratch/subkey")!" ]] ||
	assets_die 'RECORDPRIMS_PGP_KEY_ID must be the committed signing subkey followed by !'
# Refuses any existing signature, a stale manifest or a changed file.
"$assets_root/scripts/release-verify-assets.sh" --unsigned "$dir" >/dev/null

# The private copy sits beside DIR, on the same filesystem, so each final move
# is a rename.
stage="$(mktemp -d "$(dirname "$dir")/.release-sign.XXXXXX")"
trap 'rm -rf "$scratch" "$stage"' EXIT
cp -p "$dir"/* "$stage/"
for sums in SHA256SUMS SHA512SUMS; do
	minisign -S -s "$key" -m "$stage/$sums" -x "$stage/$sums.minisig" -t "recordprims $anchor_tag $sums"
	gpg --homedir "$home" --batch --yes --local-user "$RECORDPRIMS_PGP_KEY_ID" --armor --detach-sign \
		--output "$stage/$sums.asc" "$stage/$sums"
done
"$assets_root/scripts/release-verify-assets.sh" "$stage" >/dev/null
for sums in SHA256SUMS SHA512SUMS; do
	for ext in minisig asc; do
		mv "$stage/$sums.$ext" "$dir/$sums.$ext"
	done
done
"$assets_root/scripts/release-verify-assets.sh" "$dir"
