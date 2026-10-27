#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Source this file, then call resolve_release_decernor. Every caller derives or
# verifies a fingerprint anchor, so the checker comes only from the absolute
# path in RECORDPRIMS_DECERNOR_BIN: no unprefixed variable, no working-tree build,
# no PATH lookup.
resolve_release_decernor() {
	local candidate short extended version
	[[ -n "${RECORDPRIMS_DECERNOR_BIN:-}" ]] || {
		echo 'error: RECORDPRIMS_DECERNOR_BIN is required (absolute path to decernor >= 0.1.8)' >&2
		return 1
	}
	candidate="$RECORDPRIMS_DECERNOR_BIN"
	[[ "$candidate" == /* && -f "$candidate" && ! -L "$candidate" && -x "$candidate" ]] || {
		echo 'error: RECORDPRIMS_DECERNOR_BIN must be an absolute executable regular file, not a symlink' >&2
		return 1
	}
	short="$("$candidate" version)" || return 1
	extended="$("$candidate" version -e)" || return 1
	[[ "$short" =~ ^decernor\ ([0-9]+\.[0-9]+\.[0-9]+)$ ]] || {
		echo 'error: RECORDPRIMS_DECERNOR_BIN does not identify as decernor' >&2
		return 1
	}
	version="${BASH_REMATCH[1]}"
	python3 - "$version" "$extended" <<'PY' || return 1
import re
import sys
version, extended = sys.argv[1:]
lines = extended.splitlines()
if len(lines) != 6 or not re.fullmatch(r'Version:\s+' + re.escape(version), lines[0]) or any(
    not re.fullmatch(label + r':\s+\S.*', line)
    for label, line in zip(('Commit', 'Build Date', 'Go Version', 'Gofulmen', 'Crucible'), lines[1:])
):
    raise SystemExit('error: invalid decernor extended identity')
if tuple(map(int, version.split('.'))) < (0, 1, 8):
    raise SystemExit('error: decernor >= 0.1.8 required')
PY
	# shellcheck disable=SC2034 # Consumed by scripts sourcing this helper.
	RELEASE_DECERNOR_BIN="$candidate"
}
