#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Prepare the release tag message outside the repository, never clobbering.

The message is generated, not written by hand: exactly one line,
"recordprims X.Y.Z" (the repository name and the version), followed by a
newline. `--validate-message FILE` (used by the tag scripts) refuses any other
bytes, so a tag carries exactly the message prepared for it and nothing needs
editing at tag time.

One file per release tag in RECORDPRIMS_TAG_MESSAGE_DIR, named after the tag
with "/" replaced by "__" and a ".txt" suffix.
"""

import os
import re
import sys
from pathlib import Path

REPOSITORY = "recordprims"
SEMVER = re.compile(r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)")


def fail(reason: str) -> None:
    raise ValueError(reason)


def message_for(version: str) -> bytes:
    if not SEMVER.fullmatch(version):
        fail("the version must be a stable semantic version")
    return f"{REPOSITORY} {version}\n".encode()


def validate_message(data: bytes, version: str) -> None:
    expected = message_for(version)
    if data != expected:
        fail(f"tag message must be exactly {expected.decode().strip()!r} and a newline")


def tag_set(root: Path, version: str) -> list[str]:
    """The fixed release tag set: the one tag the crate and the root Go module share."""
    if not (root / "go.mod").is_file():
        fail("go.mod is required at the repository root; the release tag also releases the Go module")
    return [f"v{version}"]


def message_dir(root: Path, tag: str) -> Path:
    raw = os.environ.get("RECORDPRIMS_TAG_MESSAGE_DIR", "")
    directory = Path(raw)
    if (
        not raw
        or not directory.is_absolute()
        or ".." in directory.parts
        or directory.name != tag
    ):
        fail(
            "RECORDPRIMS_TAG_MESSAGE_DIR must be an absolute directory ending in the release tag"
        )
    # The directory itself must not be a symlink. Ancestors may be (macOS /tmp
    # and /var are), so the repository check uses the fully resolved path.
    if directory.is_symlink():
        fail("RECORDPRIMS_TAG_MESSAGE_DIR may not be a symlink")
    resolved = directory.resolve(strict=False)
    if resolved == root or root in resolved.parents:
        fail("RECORDPRIMS_TAG_MESSAGE_DIR must be outside the repository")
    if resolved.name != tag:
        fail("RECORDPRIMS_TAG_MESSAGE_DIR must resolve to a directory named for the release tag")
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    if directory.is_symlink() or not directory.is_dir():
        fail("RECORDPRIMS_TAG_MESSAGE_DIR must be a real directory")
    return directory


def prepare() -> None:
    root = Path(__file__).resolve().parent.parent
    version = (root / "VERSION").read_text().strip()
    release_tag = os.environ.get("RECORDPRIMS_RELEASE_TAG", "")
    if release_tag != f"v{version}":
        fail("RECORDPRIMS_RELEASE_TAG must match VERSION")
    directory = message_dir(root, release_tag)
    for tag in tag_set(root, version):
        file = directory / f"{tag.replace('/', '__')}.txt"
        try:
            descriptor = os.open(
                file, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
            )
        except FileExistsError:
            state = "preserved"
        else:
            with os.fdopen(descriptor, "wb") as output:
                output.write(message_for(version))
            state = "created"
        if file.is_symlink() or not file.is_file():
            fail(f"{file.name} must be a regular file")
        # An existing file is kept, never rewritten, but it must already be the
        # generated message.
        validate_message(file.read_bytes(), version)
        print(f"[ok] {state} {file.name}: {message_for(version).decode().strip()}")


def main() -> None:
    args = sys.argv[1:]
    if len(args) == 3 and args[0] == "--validate-message":
        validate_message(Path(args[1]).read_bytes(), args[2])
    elif not args:
        prepare()
    else:
        fail("usage: release-prepare-tag-message.py [--validate-message FILE VERSION]")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        detail = (
            str(error)
            if isinstance(error, ValueError)
            else "unable to prepare the tag message"
        )
        print(f"error: {detail}", file=sys.stderr)
        sys.exit(1)
