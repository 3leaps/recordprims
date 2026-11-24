#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer only: sign SHA256SUMS and SHA512SUMS in DIR with minisign and with
# the committed OpenPGP signing subkey.
#
# The published tag is re-verified against the ceremony anchor. DIR is copied
# to a sibling, DIR.signing.XXXXXX; every unsigned check (file set, tagged
# public files and notes, source archive, exact manifests) runs on that copy,
# the bytes the signers read, and the copy is signed and fully verified. It
# then replaces DIR as a set: DIR is renamed to DIR.unsigned, the signed copy
# to DIR, and DIR.unsigned is removed. DIR is therefore either the unsigned set
# or the complete signed set. An interruption between the two renames leaves
# DIR.unsigned; the next run detects it and names the recovery.
# Inputs: RECORDPRIMS_MINISIGN_KEY (minisign secret key file, outside the
# repository), RECORDPRIMS_PGP_KEY_ID (the committed signing subkey followed
# by !), RECORDPRIMS_GPG_HOMEDIR.
set -euo pipefail
# shellcheck source=scripts/release-assets.sh
source "$(dirname "$0")/release-assets.sh"
dir="${1:-}"
[[ $# -eq 1 ]] || assets_die 'usage: release-sign.sh DIR'
[[ "$dir" == /* ]] || assets_die 'DIR must be an absolute path'
dir="${dir%/}"
backup="$dir.unsigned"
if [[ -e "$backup" || -L "$backup" ]]; then
	if [[ -e "$dir" ]]; then
		assets_die "an interrupted signing left $backup beside $dir; run make release-verify on $dir, and if it passes remove $backup (otherwise remove $dir, rename $backup to $dir and sign again)"
	fi
	assets_die "an interrupted signing left $dir missing and the unsigned set in $backup; rename $backup to $dir and run make release-sign again"
fi
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
stage="$(mktemp -d "$dir.signing.XXXXXX")"
trap 'rm -rf "$scratch" "$stage"' EXIT
release_blob config/release/signing-subkey.txt "$scratch/subkey"
[[ "${RECORDPRIMS_PGP_KEY_ID:-}" == "$(tr -d '\r\n' <"$scratch/subkey")!" ]] ||
	assets_die 'RECORDPRIMS_PGP_KEY_ID must be the committed signing subkey followed by !'

cp -pR "$dir/." "$stage/"
# The copy is what the signers read: it must pass every unsigned check (no
# signature yet, no stale manifest, no changed file).
"$assets_root/scripts/release-verify-assets.sh" --unsigned "$stage" >/dev/null
for sums in SHA256SUMS SHA512SUMS; do
	minisign -S -s "$key" -m "$stage/$sums" -x "$stage/$sums.minisig" -t "recordprims $anchor_tag $sums"
	gpg --homedir "$home" --batch --yes --local-user "$RECORDPRIMS_PGP_KEY_ID" --armor --detach-sign \
		--output "$stage/$sums.asc" "$stage/$sums"
done
"$assets_root/scripts/release-verify-assets.sh" "$stage" >/dev/null

# Promote the signed set: two renames on one filesystem, then drop the old set.
mv "$dir" "$backup"
mv "$stage" "$dir"
rm -rf "$backup"
"$assets_root/scripts/release-verify-assets.sh" "$dir"
