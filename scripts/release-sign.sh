#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer only: sign SHA256SUMS and SHA512SUMS in DIR with minisign and with
# the committed OpenPGP signing subkey, then verify the whole staged set.
# Inputs: RECORDPRIMS_MINISIGN_KEY (minisign secret key file, outside the
# repository), RECORDPRIMS_PGP_KEY_ID (the committed signing subkey followed
# by !), RECORDPRIMS_GPG_HOMEDIR. Existing signatures are never overwritten.
set -euo pipefail
# shellcheck source=scripts/release-assets.sh
source "$(dirname "$0")/release-assets.sh"
dir="${1:-}"
[[ $# -eq 1 ]] || assets_die 'usage: release-sign.sh DIR'
release_anchor
release_anchor_local
release_dir "$dir"
key="${RECORDPRIMS_MINISIGN_KEY:-}"
[[ "$key" == /* && -f "$key" && ! -L "$key" ]] || assets_die 'RECORDPRIMS_MINISIGN_KEY must be an absolute regular file'
case "$(cd "$(dirname "$key")" && pwd -P)/" in "$assets_root/"*) assets_die 'the minisign key must be outside the repository' ;; esac
home="${RECORDPRIMS_GPG_HOMEDIR:-}"
[[ "$home" == /* && -d "$home" ]] || assets_die 'RECORDPRIMS_GPG_HOMEDIR must be an absolute directory'
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
release_blob config/release/signing-subkey.txt "$scratch/subkey"
[[ "${RECORDPRIMS_PGP_KEY_ID:-}" == "$(tr -d '\r\n' <"$scratch/subkey")!" ]] ||
	assets_die 'RECORDPRIMS_PGP_KEY_ID must be the committed signing subkey followed by !'
for sums in SHA256SUMS SHA512SUMS; do
	[[ -f "$dir/$sums" && ! -L "$dir/$sums" ]] || assets_die "$sums is not staged"
	[[ ! -e "$dir/$sums.minisig" && ! -e "$dir/$sums.asc" ]] || assets_die "$sums is already signed"
done
for sums in SHA256SUMS SHA512SUMS; do
	minisign -S -s "$key" -m "$dir/$sums" -x "$dir/$sums.minisig" -t "recordprims $anchor_tag $sums"
	gpg --homedir "$home" --batch --yes --local-user "$RECORDPRIMS_PGP_KEY_ID" --armor --detach-sign \
		--output "$dir/$sums.asc" "$dir/$sums"
done
"$assets_root/scripts/release-verify-assets.sh" "$dir"
