#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Controls for license-check: the committed files pass, and each way the
# license files have drifted before is refused.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
py="${PYTHON:-python3}"
check() { "$py" -I -B "$root/scripts/license-check.py" "$1" >/dev/null 2>&1; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fresh() { rm -rf "$tmp/r"; mkdir "$tmp/r"; cp "$root"/LICENSE-APACHE "$root"/LICENSE-MIT "$root"/NOTICE "$tmp/r/"; }
refuse() { if check "$tmp/r"; then echo "license-check control failed: $1 passed" >&2; exit 1; fi; }

fresh; check "$tmp/r" || { echo 'license-check control failed: the committed files are refused' >&2; exit 1; }
fresh; "$py" -I -c 'import sys;p=sys.argv[1];t=open(p).read();assert "submitted to Licensor" in t;open(p,"w").write(t.replace("submitted to Licensor","submitted to the Licensor"))' "$tmp/r/LICENSE-APACHE"; refuse 'the "the Licensor" wording'
fresh; printf '\n---\n\n## Project Name Notice\n' >>"$tmp/r/LICENSE-MIT"; refuse 'a notice appended to LICENSE-MIT'
fresh; printf '\nSubject to 3 Leaps OSS policies\n' >>"$tmp/r/LICENSE-APACHE"; refuse 'a notice appended to LICENSE-APACHE'
fresh; rm "$tmp/r/NOTICE"; refuse 'a missing NOTICE'
fresh; "$py" -I -c 'import sys;p=sys.argv[1];t=open(p).read();open(p,"w").write(t.replace("a North Carolina LLC","a Florida LLC with offices in South Carolina"))' "$tmp/r/NOTICE"; refuse 'the old domicile in NOTICE'
# Windows runners without symlink privilege make ln -s a copy; the control
# needs a real symlink, so it runs only where one was created.
fresh; rm "$tmp/r/LICENSE-MIT"; ln -s "$root/LICENSE-MIT" "$tmp/r/LICENSE-MIT" 2>/dev/null || true
if [[ -L "$tmp/r/LICENSE-MIT" ]]; then refuse 'a symlinked LICENSE-MIT'; link=', symlink'; else link=' (symlink control skipped: no symlinks here)'; fi
echo "[ok] license-check controls: the Licensor typo, appended notices, missing NOTICE, old domicile$link"
