#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Every `make <target>` that RELEASE_CHECKLIST.md names must be a target in
# the Makefile, so the checklist cannot drift from the commands it describes.
# Needs no keys or tools beyond git and grep, so it runs in make check.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
missing=0
while IFS= read -r target; do
	grep -Eq "^${target}:" "$root/Makefile" || {
		echo "error: RELEASE_CHECKLIST.md names missing make target $target" >&2
		missing=1
	}
done < <(grep -oE 'make [a-z][a-z0-9-]+' "$root/RELEASE_CHECKLIST.md" | awk '{print $2}' | sort -u)
[[ "$missing" -eq 0 ]] || exit 1
echo '[ok] every make target named in RELEASE_CHECKLIST.md exists'
