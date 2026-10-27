#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Report each host tool's version against the recommended minimum declared in
# .goneat/tools.yaml, and fail when one is missing, unreadable or older.
#
# Usage: scripts/doctor.sh [release|dev]   (default: release)
#
# The comparison is scripts/doctor.py, which fails closed: a version it cannot
# read is a failure, never a skip. (goneat doctor tools skips versions with
# two parts, such as jq-1.6 and minisign 0.12, so it is not used to decide.)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
scope="${1:-release}"
case "$scope" in release | dev) ;; *)
	echo "error: unknown scope $scope (release or dev)" >&2
	exit 2
	;;
esac
exec python3 -I -B "$root/scripts/doctor.py" "$root/.goneat/tools.yaml" "$root/config/doctor-versions.txt" "$scope"
