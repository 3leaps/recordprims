#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Maintainer-only, one time: write the public pins that are then committed.
# The operator environment feeds this first write only; every later step reads
# the committed files. Nothing that already exists is overwritten.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$root"
stop() {
	echo "STOP: $1" >&2
	exit 1
}
: "${RECORDPRIMS_GPG_HOMEDIR:?load the approved GPG home}"
: "${RECORDPRIMS_PGP_KEY_ID:?load the approved signing-subkey selector (40 hex, ending in !)}"
: "${RECORDPRIMS_MINISIGN_PUB_EXPORT:?path to the approved minisign public key for this first write}"
[[ "$RECORDPRIMS_GPG_HOMEDIR" == /* && -d "$RECORDPRIMS_GPG_HOMEDIR" ]] || stop 'approved GPG home must be an absolute directory'
[[ "$RECORDPRIMS_PGP_KEY_ID" =~ ^[0-9A-F]{40}!$ ]] || stop 'signing-subkey selector must be 40 uppercase hex ending in !'
[[ -f "$RECORDPRIMS_MINISIGN_PUB_EXPORT" && -s "$RECORDPRIMS_MINISIGN_PUB_EXPORT" && ! -L "$RECORDPRIMS_MINISIGN_PUB_EXPORT" ]] ||
	stop 'minisign public export must be a nonempty regular file, not a symlink'
python3 - "$RECORDPRIMS_GPG_HOMEDIR" "$HOME/.gnupg" "$root" <<'PY'
from pathlib import Path
import sys
home, default, repo = (Path(p).resolve() for p in sys.argv[1:4])
if home == default:
    raise SystemExit('STOP: the default GPG home is not approved for this export')
if home == repo or repo in home.parents:
    raise SystemExit('STOP: the GPG home must be outside the repository')
PY
asc=docs/security/release-signing-keys.asc
pub=docs/security/recordprims-minisign.pub
subkey=config/release/signing-subkey.txt
for path in "$asc" "$pub" "$subkey"; do
	[[ ! -e "$path" && ! -L "$path" ]] || stop "$path already exists; pins are written once and reviewed"
done
[[ ! -L docs/security && ! -L config/release ]] || stop 'pin directories must not be symlinks'
# The checker is resolved, and every output is produced and fully validated in
# a scratch directory outside the repository, before anything is written here.
# shellcheck source=scripts/release-decernor.sh
source "$root/scripts/release-decernor.sh"
resolve_release_decernor || stop 'the checker is required before any pin is written'
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/docs/security" "$scratch/config/release"
gpg --homedir "$RECORDPRIMS_GPG_HOMEDIR" --batch --armor --export-options export-minimal \
	--export "$RECORDPRIMS_PGP_KEY_ID" >"$scratch/$asc" || stop 'OpenPGP public export failed'
cat "$RECORDPRIMS_MINISIGN_PUB_EXPORT" >"$scratch/$pub"
printf '%s\n' "${RECORDPRIMS_PGP_KEY_ID%!}" >"$scratch/$subkey"
"$root/scripts/release-validate-pin.sh" --staged "$scratch" || stop 'the exported pins did not validate; nothing was written'
mkdir -p docs/security config/release
for path in "$asc" "$pub" "$subkey"; do
	(set -C && cat "$scratch/$path" >"$path") || stop "$path appeared during export"
done
echo "[ok] wrote $asc, $pub and $subkey; review and commit them, then run make release-insert-anchors"
