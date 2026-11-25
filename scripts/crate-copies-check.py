#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Verify the files copied into crates/recordprims so the packaged crate is
self-contained.

The crate embeds the shape schemas and ships the license texts and notice, and a crate
package can only carry files under its own directory. Each copy must match
its source byte for byte, and the copied schema set must equal the source
set: a missing, extra or changed file fails. Standard library only.
"""
from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CRATE = ROOT / "crates" / "recordprims"

# (source directory or file, copy) pairs.
COPIES = [
    (ROOT / "internal" / "shape" / "schemas", CRATE / "schemas"),
    (ROOT / "LICENSE-MIT", CRATE / "LICENSE-MIT"),
    (ROOT / "LICENSE-APACHE", CRATE / "LICENSE-APACHE"),
    (ROOT / "NOTICE", CRATE / "NOTICE"),
]


def files(base: Path) -> dict[str, Path]:
    return {p.relative_to(base).as_posix(): p for p in sorted(base.rglob("*")) if not p.is_dir()}


def main() -> int:
    problems: list[str] = []
    for src, dst in COPIES:
        name = dst.relative_to(ROOT).as_posix()
        if src.is_dir():
            if not dst.is_dir():
                problems.append(f"missing copy: {name}/")
                continue
            want, have = files(src), files(dst)
            for rel in sorted(want.keys() - have.keys()):
                problems.append(f"missing copy: {name}/{rel}")
            for rel in sorted(have.keys() - want.keys()):
                problems.append(f"extra file in copy: {name}/{rel}")
            for rel in sorted(want.keys() & have.keys()):
                if want[rel].read_bytes() != have[rel].read_bytes():
                    problems.append(f"copy differs from source: {name}/{rel}")
        elif not dst.is_file():
            problems.append(f"missing copy: {name}")
        elif src.read_bytes() != dst.read_bytes():
            problems.append(f"copy differs from source: {name}")
    for p in problems:
        print(f"error: {p}", file=sys.stderr)
    if problems:
        return 1
    print("[ok] crate copies match their sources")
    return 0


if __name__ == "__main__":
    sys.exit(main())
