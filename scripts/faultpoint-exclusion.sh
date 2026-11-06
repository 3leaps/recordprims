#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Release builds must not compile the failure-injection seam: without the
# recordprims_faultpoints tag, internal/jfile builds seam_release.go and
# never seam_faultpoints.go.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
files="$(CGO_ENABLED=0 go list -f '{{join .GoFiles " "}}' ./internal/jfile)"
case " $files " in
  *' seam_faultpoints.go '*) echo 'faultpoint-exclusion: seam compiled into a release build' >&2; exit 1 ;;
esac
case " $files " in
  *' seam_release.go '*) ;;
  *) echo 'faultpoint-exclusion: release seam file missing' >&2; exit 1 ;;
esac
tagged="$(CGO_ENABLED=0 go list -tags recordprims_faultpoints -f '{{join .GoFiles " "}}' ./internal/jfile)"
case " $tagged " in
  *' seam_faultpoints.go '*) ;;
  *) echo 'faultpoint-exclusion: tagged build lacks the seam (check is vacuous)' >&2; exit 1 ;;
esac
echo "[ok] faultpoint-exclusion"
