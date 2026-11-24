#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Verify a staged or downloaded release page in DIR against the anchored
# commit: exactly the expected files; the public files and the page body equal
# their blobs in that commit; the source archive holds exactly that commit;
# both manifests list exactly the summed files with matching digests; and each
# manifest carries a minisign signature by the committed public key and an
# OpenPGP signature by the committed signing subkey of the committed primary
# (and of RECORDPRIMS_GPG_SIGNING_FINGERPRINT, when set). Reads no keyring.
#
# With --unsigned, DIR must hold the summed files, both manifests and the page
# body and no signature yet; every check except the signatures runs. Signing
# runs this first, so it never signs a stale manifest.
set -euo pipefail
# shellcheck source=scripts/release-assets.sh
source "$(dirname "$0")/release-assets.sh"
unsigned=0
if [[ "${1:-}" == --unsigned ]]; then
	unsigned=1
	shift
fi
dir="${1:-}"
[[ $# -eq 1 ]] || assets_die 'usage: release-verify-assets.sh [--unsigned] DIR'
release_anchor
release_dir "$dir"
scratch="$(mktemp -d)"
trap 'gpgconf --homedir "$scratch/gpg" --kill all >/dev/null 2>&1 || true; rm -rf "$scratch"' EXIT

# Exactly the expected files, all regular.
{
	if [[ "$unsigned" == 1 ]]; then
		release_summed
		printf 'SHA256SUMS\nSHA512SUMS\n'
	else
		release_uploaded
	fi
	release_notes_name
} | LC_ALL=C sort >"$scratch/want"
(cd "$dir" && find . -mindepth 1 -maxdepth 1 | sed 's#^\./##' | LC_ALL=C sort) >"$scratch/have"
diff -u "$scratch/want" "$scratch/have" >&2 || assets_die "$dir does not hold exactly the release assets"
while read -r name; do
	[[ -f "$dir/$name" && ! -L "$dir/$name" ]] || assets_die "$name must be a regular file"
done <"$scratch/want"

# Public files and notes are the tagged blobs.
while read -r name path; do
	release_blob "$path" "$scratch/blob"
	cmp -s "$scratch/blob" "$dir/$name" || assets_die "$name differs from $path in the tagged commit"
done < <(release_public_files)
release_blob "docs/releases/$anchor_tag.md" "$scratch/blob"
cmp -s "$scratch/blob" "$dir/$(release_notes_name)" || assets_die 'the page body differs from the tagged release notes'

# The source archive holds exactly the anchored commit.
source_name="$(release_source_name)"
release_source_tar "$scratch/expected.tar"
gzip -dc "$dir/$source_name" >"$scratch/got.tar" || assets_die "$source_name is not a gzip archive"
cmp -s "$scratch/got.tar" "$scratch/expected.tar" || assets_die "$source_name does not hold exactly the anchored commit"

# Manifests: exactly the summed files, in order, with matching digests.
for algo in sha256 sha512; do
	sums="SHA${algo#sha}SUMS"
	: >"$scratch/manifest"
	while read -r name; do
		printf '%s  %s\n' "$(release_hash "$algo" "$dir/$name")" "$name" >>"$scratch/manifest"
	done < <(release_summed)
	cmp -s "$scratch/manifest" "$dir/$sums" || assets_die "$sums does not match the staged files"
done

if [[ "$unsigned" == 1 ]]; then
	echo "[ok] unsigned release assets in $dir verified against $anchor_tag at $anchor_commit"
	exit 0
fi

# Signatures, against keys read from the tagged commit only.
release_blob docs/security/release-signing-keys.asc "$scratch/keys.asc"
release_blob docs/security/recordprims-minisign.pub "$scratch/minisign.pub"
release_blob config/release/signing-subkey.txt "$scratch/subkey"
release_blob keys/expected-fingerprints.txt "$scratch/anchors"
subkey="$(tr -d '\r\n' <"$scratch/subkey")"
primary="$(awk '$1=="gpg" {print $2}' "$scratch/anchors")"
[[ "$subkey" =~ ^[0-9A-F]{40}$ && "$primary" =~ ^[0-9A-F]{40}$ ]] || assets_die 'committed signing subkey or primary is malformed'
if [[ -n "${RECORDPRIMS_GPG_SIGNING_FINGERPRINT:-}" ]]; then
	[[ "$primary" == "$RECORDPRIMS_GPG_SIGNING_FINGERPRINT" ]] || assets_die 'the committed primary is not the approved primary'
fi
mkdir -m 700 "$scratch/gpg"
gpg --homedir "$scratch/gpg" --batch --quiet --import "$scratch/keys.asc" 2>/dev/null || assets_die 'committed key import failed'
for sums in SHA256SUMS SHA512SUMS; do
	minisign -V -q -p "$scratch/minisign.pub" -m "$dir/$sums" -x "$dir/$sums.minisig" >/dev/null 2>&1 ||
		assets_die "$sums.minisig does not verify with the committed minisign key"
	gpg --homedir "$scratch/gpg" --batch --status-fd 1 --verify "$dir/$sums.asc" "$dir/$sums" >"$scratch/status" 2>/dev/null ||
		assets_die "$sums.asc does not verify with the committed key"
	awk -v primary="$primary" -v permitted="$subkey" '
		$1=="[GNUPG:]" && $2=="VALIDSIG" {valid++; signer=$3; signer_primary=$NF}
		$1=="[GNUPG:]" && $2 ~ /^(EXPKEYSIG|EXPSIG|REVKEYSIG|KEYREVOKED|BADSIG|ERRSIG)$/ {bad++}
		END {if (bad || valid != 1 || signer != permitted || signer_primary != primary) exit 1}
	' "$scratch/status" || assets_die "$sums.asc is not signed by the committed signing subkey"
done
echo "[ok] release assets in $dir verified against $anchor_tag at $anchor_commit"
