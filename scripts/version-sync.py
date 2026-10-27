#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Keep every version site in step with VERSION, the single source of truth.

Usage:
  version-sync.py          write VERSION into Cargo.toml and Cargo.lock
  version-sync.py check    fail if any site differs from VERSION

The sites are the [workspace.package] version in Cargo.toml and the workspace
crates' entries in Cargo.lock. The Go module takes its version from the tag,
so it has no version site. Nothing outside these files is touched.
"""

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SEMVER = re.compile(r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)")
# Workspace members whose Cargo.lock entries carry the workspace version.
CRATES = ("recordprims",)
CARGO_TOML = re.compile(r'(?ms)^(\[workspace\.package\]\n(?:(?!^\[).)*?^version = )"([^"]*)"')


def lock_pattern(crate):
    return re.compile(r'(?m)^(\[\[package\]\]\nname = "' + re.escape(crate) + r'"\nversion = )"([^"]*)"')


def read_version():
    version = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    if not SEMVER.fullmatch(version):
        raise ValueError("VERSION must hold one stable semantic version")
    return version


def sites(text_toml, text_lock):
    """Return {site: version} for every version site."""
    found = {}
    match = CARGO_TOML.search(text_toml)
    if not match:
        raise ValueError("Cargo.toml has no [workspace.package] version")
    found["Cargo.toml [workspace.package]"] = match.group(2)
    for crate in CRATES:
        matches = lock_pattern(crate).findall(text_lock)
        if len(matches) != 1:
            raise ValueError(f"Cargo.lock must hold exactly one {crate} entry (found {len(matches)})")
        found[f"Cargo.lock {crate}"] = matches[0][1]
    return found


def main():
    args = sys.argv[1:]
    if args not in ([], ["check"]):
        raise ValueError("usage: version-sync.py [check]")
    version = read_version()
    toml_path, lock_path = ROOT / "Cargo.toml", ROOT / "Cargo.lock"
    text_toml = toml_path.read_text(encoding="utf-8")
    text_lock = lock_path.read_text(encoding="utf-8")
    if args == ["check"]:
        wrong = {site: v for site, v in sites(text_toml, text_lock).items() if v != version}
        for site, v in wrong.items():
            print(f"error: {site} is {v}; VERSION is {version}", file=sys.stderr)
        if wrong:
            sys.exit("version-check: run make version-sync")
        print(f"[ok] version-check {version}")
        return
    sites(text_toml, text_lock)  # validate the shape before writing anything
    text_toml = CARGO_TOML.sub(lambda m: f'{m.group(1)}"{version}"', text_toml, count=1)
    for crate in CRATES:
        text_lock = lock_pattern(crate).sub(lambda m: f'{m.group(1)}"{version}"', text_lock, count=1)
    toml_path.write_text(text_toml, encoding="utf-8")
    lock_path.write_text(text_lock, encoding="utf-8")
    print(f"[ok] synced Cargo.toml and Cargo.lock to {version}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        sys.exit(f"error: {error}")
