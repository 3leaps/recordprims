#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Verify the vendored crucible contracts against contract/PIN.md.

Recomputes the Git object id of every pinned file (blob) and directory (tree)
from the bytes on disk, without calling git, and checks contract/INVENTORY.sha256.
Any missing, extra or changed file fails. Standard library only.
"""
from __future__ import annotations

import hashlib
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
VENDOR = ROOT / "contract" / "crucible"
PIN = ROOT / "contract" / "PIN.md"
INVENTORY = ROOT / "contract" / "INVENTORY.sha256"

# Git file modes. Vendored files are never executable or symlinks; a mode
# change would alter the tree id, so the check treats any other mode as drift.
BLOB_MODE = b"100644"
TREE_MODE = b"40000"


def blob_id(data: bytes) -> str:
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


def tree_id(path: Path) -> str:
    entries = []
    for child in path.iterdir():
        if child.name == "__pycache__":
            continue
        if child.is_symlink():
            raise SystemExit(f"contract-check: symlink in vendored contract: {child.relative_to(ROOT)}")
        if child.is_dir():
            mode, oid = TREE_MODE, tree_id(child)
        else:
            mode, oid = BLOB_MODE, blob_id(child.read_bytes())
        entries.append((child.name, mode, oid))
    # Git sorts tree entries by name, treating directories as if suffixed with "/".
    entries.sort(key=lambda e: e[0] + ("/" if e[1] == TREE_MODE else ""))
    body = b"".join(m + b" " + n.encode() + b"\0" + bytes.fromhex(o) for n, m, o in entries)
    return hashlib.sha1(b"tree %d\0" % len(body) + body).hexdigest()


HEADING = re.compile(r"^(#{1,6})\s")
ROW = re.compile(r"^\|\s*`([^`]+)`\s*\|\s*(tree\s+)?`([0-9a-f]{40})`\s*\|$")
# The header and separator rows of a vendored-set table.
TABLE_FRAME = re.compile(r"^\|\s*(Path\s*\|\s*Git blob or tree|-+\s*\|\s*-+)\s*\|$")


def pinned_rows() -> list[tuple[str, str, bool]]:
    """(path, object id, is_tree) rows from every 'Vendored set' table.

    A heading starts a section only at column 0; an indented line that reads
    as a heading is refused rather than guessed at. In a vendored-set
    section every table line must be the header, the separator or a pinned
    row, so a malformed row fails instead of being skipped.
    """
    rows = []
    sections = 0
    in_set = False
    found = 0
    for n, line in enumerate(PIN.read_text(encoding="utf-8").splitlines(), 1):
        if line != line.lstrip() and HEADING.match(line.lstrip()):
            raise SystemExit(f"contract-check: contract/PIN.md:{n}: indented heading")
        if HEADING.match(line):
            if in_set and not found:
                raise SystemExit("contract-check: a vendored-set section in contract/PIN.md has no pinned rows")
            in_set = line.startswith("## Vendored set")
            sections += in_set
            found = 0
            continue
        if not in_set or not line.lstrip().startswith("|"):
            continue
        if TABLE_FRAME.match(line.strip()):
            continue
        m = ROW.match(line.strip())
        if not m:
            raise SystemExit(f"contract-check: contract/PIN.md:{n}: malformed vendored-set row")
        rows.append((m.group(1).rstrip("/"), m.group(3), bool(m.group(2))))
        found += 1
    if in_set and not found:
        raise SystemExit("contract-check: a vendored-set section in contract/PIN.md has no pinned rows")
    if not sections:
        raise SystemExit("contract-check: no vendored-set section in contract/PIN.md")
    paths = [r[0] for r in rows]
    if len(paths) != len(set(paths)):
        raise SystemExit("contract-check: a path is pinned twice in contract/PIN.md")
    return rows


def covered(rel: str, rows: list[tuple[str, str, bool]]) -> bool:
    """Whether a vendored file is a pinned blob or lies under a pinned tree."""
    return any(rel == p if not t else rel.startswith(p + "/") for p, _, t in rows)


def main() -> int:
    failures = []
    rows = pinned_rows()
    for rel, want, is_tree in rows:
        target = VENDOR / rel
        if not target.exists():
            failures.append(f"missing: {rel}")
            continue
        got = tree_id(target) if is_tree else blob_id(target.read_bytes())
        if got != want:
            failures.append(f"object id mismatch: {rel} (pinned {want[:12]}, on disk {got[:12]})")

    expected = {}
    for line in INVENTORY.read_text(encoding="utf-8").splitlines():
        digest, rel = line.split("  ", 1)
        expected[rel] = digest
    actual = {}
    for path in sorted(VENDOR.rglob("*")):
        if path.is_file() and "__pycache__" not in path.parts:
            actual[path.relative_to(VENDOR).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    for rel in sorted(expected.keys() - actual.keys()):
        failures.append(f"inventory lists a missing file: {rel}")
    for rel in sorted(actual.keys() - expected.keys()):
        failures.append(f"file not in inventory: {rel}")
    for rel in sorted(expected.keys() & actual.keys()):
        if expected[rel] != actual[rel]:
            failures.append(f"sha256 mismatch: {rel}")
    for rel in sorted(actual):
        if not covered(rel, rows):
            failures.append(f"vendored file not under a pinned path: {rel}")

    for f in failures:
        print(f"contract-check: {f}", file=sys.stderr)
    if failures:
        return 1
    print(f"[ok] contract-check: {len(rows)} pinned objects, {len(actual)} files")
    return 0


if __name__ == "__main__":
    sys.exit(main())
