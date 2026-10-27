# SPDX-License-Identifier: MIT OR Apache-2.0
"""Read every committed blob in a tree through git, never the working tree.

Shared by content-check.py and surface-scan.sh. Unlike `git archive`, this
does not honour export-ignore or any other attribute: what is committed is
what gets checked. A symlink is a blob whose content is its target path.
"""

import subprocess


def committed(root, ref):
    """Yield (path, mode, bytes) for every blob in the tree at REF."""
    listing = subprocess.run(
        ["git", "-C", str(root), "ls-tree", "-r", "-z", "--full-tree", ref],
        capture_output=True, check=True,
    ).stdout
    entries = []
    for record in listing.split(b"\0"):
        if not record:
            continue
        meta, _, name = record.partition(b"\t")
        mode, kind, oid = meta.split(b" ")
        if kind == b"blob":
            entries.append((name.decode("utf-8", "surrogateescape"), mode.decode(), oid.decode()))
    if not entries:
        return
    batch = subprocess.run(
        ["git", "-C", str(root), "cat-file", "--batch"],
        input=b"".join(oid.encode() + b"\n" for _, _, oid in entries),
        capture_output=True, check=True,
    ).stdout
    offset = 0
    for name, mode, oid in entries:
        header_end = batch.index(b"\n", offset)
        header = batch[offset:header_end].split(b" ")
        if len(header) != 3 or header[0].decode() != oid or header[1] != b"blob":
            raise ValueError(f"unexpected git cat-file output for {name}")
        size = int(header[2])
        start = header_end + 1
        yield name, mode, batch[start:start + size]
        offset = start + size + 1
