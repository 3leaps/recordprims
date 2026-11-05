#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Keep the license files verbatim and the notices in NOTICE.

pkg.go.dev and go-licenses classify each LICENSE* file by text coverage; an
edited license text (an added word, an appended notice) drops below their
threshold and the module is shown as unlicensed. The license files must
therefore stay byte-for-byte the standard texts, and every project-specific
notice belongs in NOTICE, which those tools never classify.

Usage: license-check.py [root]   (default: the repository root)
Standard library only.
"""
from __future__ import annotations

import hashlib
import sys
from pathlib import Path

# SHA-256 of the verbatim texts (Apache-2.0 with the short copyright
# boilerplate; MIT). A copyright-year change updates these pins.
PINNED = {
    "LICENSE-APACHE": "f4a1dc0015bb78511c552663111804773aefa63f175c84faa3e1d4d67c3110fb",
    "LICENSE-MIT": "dba32836ab63ce4d995b152efb1c2b0fa418835da6f34c58ac6ff9e2c6353cf1",
}
NOTICE_REQUIRED = (
    "recordprims",
    "LICENSE-MIT",
    "LICENSE-APACHE",
    "3 Leaps, LLC, a North Carolina LLC.",
)


def main(argv: list[str]) -> int:
    root = Path(argv[1]) if len(argv) > 1 else Path(__file__).resolve().parent.parent
    errors = []
    for name, want in PINNED.items():
        path = root / name
        if path.is_symlink() or not path.is_file():
            errors.append(f"{name}: missing or not a regular file")
            continue
        got = hashlib.sha256(path.read_bytes()).hexdigest()
        if got != want:
            errors.append(f"{name}: not the verbatim license text (sha256 {got}); notices go in NOTICE")
    notice = root / "NOTICE"
    if notice.is_symlink() or not notice.is_file():
        errors.append("NOTICE: missing or not a regular file")
    else:
        text = notice.read_text(encoding="utf-8")
        errors += [f"NOTICE: missing {s!r}" for s in NOTICE_REQUIRED if s not in text]
    for e in errors:
        print(f"license-check: {e}", file=sys.stderr)
    if errors:
        return 1
    print("[ok] license-check: verbatim LICENSE-APACHE and LICENSE-MIT, NOTICE present")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
