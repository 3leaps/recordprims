#!/usr/bin/env python3
"""Fixture-integrity checker for contract: durable-record/v0.

Standard-library only. Reuses scripts/rfc8785-canonicalize.py for revision
digests. This is conformance-gate material for the contract's fixtures, not a
consumer library or a store implementation.

Usage:
  validate-durable-record-fixtures.py pair <baseline.json> <reject.json>
  validate-durable-record-fixtures.py journal <journal.ndjson>
  validate-durable-record-fixtures.py entry <journal-entry.json>
  validate-durable-record-fixtures.py profile <stratum-profile.json>
  validate-durable-record-fixtures.py receipt <receipt-or-reconciliation.json> <journal.ndjson>
  validate-durable-record-fixtures.py self-test

journal: seq gap-free from 1; stream_opened first and only first; one stream
per file; atomic groups tracked across rows (a group starts at its own seq,
every member carries the same group and group_size, no group is abandoned or
resized before it closes, and the final group is complete); every digest equals
SHA-256 over RFC 8785 bytes of its record; record.stream equals the line stream;
revision chain linear (appended at 1, revised at head + 1 with supersedes equal
to the current head); single_revision streams contain no revised entries;
conflicted.current equals the head at that point.

receipt: an accepted receipt (appended or revised) must match the journal entry
at its seq in stream, idempotency key, outcome, pin, body status and the
stream's failure model. A found reconciliation must embed such a receipt with
the same stream and key as the lookup. A rejection receipt must name the
journal's stream, and for revision_conflict its current pin must be a head that
the journal accepted.
"""

from __future__ import annotations

import hashlib
import importlib.util
import re
import sys
from pathlib import Path

_here = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location(
    "rfc8785", _here / "rfc8785-canonicalize.py"
)
_jcs = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(_jcs)
_tspec = importlib.util.spec_from_file_location("rfc3339", _here / "rfc3339-instant.py")
_rfc3339 = importlib.util.module_from_spec(_tspec)
assert _tspec.loader is not None
_tspec.loader.exec_module(_rfc3339)


class Reject(Exception):
    def __init__(self, reason: str, detail: str) -> None:
        super().__init__(f"{reason}: {detail}")
        self.reason = reason


def load(path: str):
    try:
        return _jcs.jcs_loads(Path(path).read_text(encoding="utf-8"))
    except ValueError as e:
        raise Reject("ingest", f"{path}: {e}") from e


def load_lines(path: str) -> list:
    out = []
    for i, ln in enumerate(
        Path(path).read_text(encoding="utf-8").splitlines(), start=1
    ):
        if not ln.strip():
            continue
        try:
            out.append(_jcs.jcs_loads(ln))
        except ValueError as e:
            raise Reject("ingest", f"{path}:{i}: {e}") from e
    return out


_FRACTION = re.compile(r"[Tt]\d{2}:\d{2}:\d{2}\.(\d+)")


def instant(text: str):
    # The shared helper resolves to microseconds. This checker fails closed on
    # any fraction it cannot represent exactly (non-zero digits beyond six)
    # rather than truncating, so a sub-microsecond ordering cannot pass.
    m = _FRACTION.search(text)
    if m and len(m.group(1)) > 6 and m.group(1)[6:].strip("0"):
        raise Reject(
            "timestamp", f"{text!r}: precision beyond microseconds is not representable"
        )
    try:
        return _rfc3339.parse_rfc3339(text)
    except ValueError as e:
        raise Reject("timestamp", f"{text!r}: {e}") from e


def digest(record) -> str:
    return (
        "sha256:" + hashlib.sha256(_jcs.jcs_dumps(record).encode("utf-8")).hexdigest()
    )


# --- one-field pairs -------------------------------------------------------


_MISSING = object()


def _get(doc, path: str):
    cur = doc
    for part in path.strip("/").split("/"):
        if isinstance(cur, dict):
            if part not in cur:
                return _MISSING
            cur = cur[part]
        elif isinstance(cur, list):
            cur = cur[int(part)]
        else:
            return _MISSING
    return cur


def leaf_diff(a, b, path: str = "") -> list[str]:
    if isinstance(a, dict) and isinstance(b, dict):
        out: list[str] = []
        for k in sorted(set(a) | set(b)):
            if k not in a or k not in b:
                out.append(f"{path}/{k}")
            else:
                out.extend(leaf_diff(a[k], b[k], f"{path}/{k}"))
        return out
    if isinstance(a, list) and isinstance(b, list) and len(a) == len(b):
        out = []
        for i, (x, y) in enumerate(zip(a, b)):
            out.extend(leaf_diff(x, y, f"{path}/{i}"))
        return out
    return [] if a == b and type(a) is type(b) else [path or "/"]


def check_pair(a, b, label: str) -> None:
    d = leaf_diff(a, b)
    if len(d) == 1:
        return
    # A rename is exactly one member present only in the baseline and one
    # present only in the reject, under the same parent object.
    if len(d) == 2:
        only_a = [x for x in d if _get(b, x) is _MISSING and _get(a, x) is not _MISSING]
        only_b = [x for x in d if _get(a, x) is _MISSING and _get(b, x) is not _MISSING]
        if (
            len(only_a) == 1
            and len(only_b) == 1
            and only_a[0].rsplit("/", 1)[0] == only_b[0].rsplit("/", 1)[0]
            and _get(a, only_a[0]) == _get(b, only_b[0])
            and type(_get(a, only_a[0])) is type(_get(b, only_b[0]))
        ):
            return
    raise Reject("pair", f"{label} differs from its baseline at {d}")


# --- journal ---------------------------------------------------------------


def check_entry(e, where: str) -> None:
    if e.get("type") in ("appended", "revised"):
        rec = e["record"]
        if rec.get("stream") != e.get("stream"):
            raise Reject("stream_mismatch", where)
        if digest(rec) != e.get("digest"):
            raise Reject(
                "digest", f"{where}: recorded {e.get('digest')} computed {digest(rec)}"
            )
        sup = rec.get("supersedes")
        if sup is not None and sup["revision"] != rec["revision"] - 1:
            raise Reject("supersedes_revision", where)


def check_journal(lines: list, label: str) -> dict:
    """Validate a journal; return facts used by receipt checks."""
    if not lines:
        raise Reject("empty", label)
    heads: dict[str, dict] = {}
    accepted: dict[int, dict] = {}
    accepted_heads: set[tuple] = set()
    mode = failure_model = stream = None
    open_group = None  # (group, group_size) of the most recent row
    for i, e in enumerate(lines, start=1):
        where = f"{label}:{i}"
        if e["seq"] != i:
            raise Reject("seq_gap", f"{where}: expected seq {i}, found {e['seq']}")
        if stream is None:
            stream = e["stream"]
        elif e["stream"] != stream:
            raise Reject("stream_mismatch", where)
        if (e["type"] == "stream_opened") != (i == 1):
            raise Reject("stream_opened_position", where)
        if e["type"] == "stream_opened":
            mode, failure_model = e["mode"], e["failure_model"]
        g = (e["group"], e["group_size"])
        if open_group is not None and i < open_group[0] + open_group[1]:
            if g != open_group:
                raise Reject(
                    "group",
                    f"{where}: group {open_group} abandoned or changed before it closed",
                )
        elif e["group"] != i:
            raise Reject("group", f"{where}: a new group must start at its own seq")
        open_group = g
        check_entry(e, where)
        if e["type"] in ("appended", "revised"):
            rec = e["record"]
            rid = rec["record_id"]
            head = heads.get(rid)
            if e["type"] == "appended":
                if head is not None or rec["revision"] != 1:
                    raise Reject(
                        "revision_chain", f"{where}: appended over an existing head"
                    )
            else:
                if mode == "single_revision":
                    raise Reject("mode_violation", where)
                if (
                    head is None
                    or rec["revision"] != head["revision"] + 1
                    or rec["supersedes"] != head
                ):
                    raise Reject(
                        "revision_chain", f"{where}: supersedes is not the current head"
                    )
            heads[rid] = {"revision": rec["revision"], "digest": e["digest"]}
            accepted[i] = e
            accepted_heads.add((rid, rec["revision"], e["digest"]))
        elif e["type"] == "conflicted":
            cur = e.get("current")
            if cur is not None and cur != heads.get(e["record_id"]):
                raise Reject(
                    "conflict_current",
                    f"{where}: current is not the head at this point",
                )
            if cur is None and e["record_id"] in heads:
                raise Reject(
                    "conflict_current", f"{where}: current omitted while a head exists"
                )
    if open_group is not None and len(lines) < open_group[0] + open_group[1] - 1:
        raise Reject("inconsistent_cut", f"{label}: final group truncated")
    return {
        "stream": stream,
        "failure_model": failure_model,
        "accepted": accepted,
        "accepted_heads": accepted_heads,
    }


# --- receipts --------------------------------------------------------------


def check_accepted_receipt(r, facts: dict, where: str) -> None:
    if r["outcome"] not in ("appended", "revised"):
        raise Reject(
            "receipt_not_accepted",
            f"{where}: outcome {r['outcome']} is not an accepted write",
        )
    e = facts["accepted"].get(r["seq"])
    if e is None:
        raise Reject("receipt_seq", f"{where}: seq {r['seq']} is not an accepted entry")
    rec = e["record"]
    expected = {
        "stream": e["stream"],
        "idempotency_key": e["idempotency_key"],
        "outcome": e["type"],
        "pin": {
            "stream": e["stream"],
            "record_id": rec["record_id"],
            "revision": rec["revision"],
            "digest": e["digest"],
        },
        "failure_model": facts["failure_model"],
        "body_status": e.get("body_status"),
    }
    for k, v in expected.items():
        if r.get(k) != v:
            raise Reject(
                "receipt_mismatch", f"{where}: {k} is {r.get(k)!r}, journal says {v!r}"
            )


def check_receipt_doc(doc, facts: dict, where: str) -> None:
    if doc["kind"] == "reconciliation":
        if doc["stream"] != facts["stream"]:
            raise Reject(
                "receipt_mismatch", f"{where}: lookup stream is not the journal stream"
            )
        if doc["result"] == "found":
            inner = doc["receipt"]
            if (
                inner["stream"] != doc["stream"]
                or inner["idempotency_key"] != doc["idempotency_key"]
            ):
                raise Reject(
                    "reconciliation_mismatch",
                    f"{where}: embedded receipt is for another stream or key",
                )
            check_accepted_receipt(inner, facts, where)
        elif doc["result"] == "not_found":
            keys = {e["idempotency_key"] for e in facts["accepted"].values()}
            if doc["idempotency_key"] in keys:
                raise Reject(
                    "reconciliation_mismatch",
                    f"{where}: not_found for a key the journal accepted",
                )
            # Zero-skew interval arithmetic on parsed instants. With a profile
            # bound B, [attempted_at - B, attempted_at + B] must lie within the
            # coverage interval; this fixture check has no profile, so B = 0.
            att = instant(doc["attempted_at"])
            if not (
                instant(doc["window_covers_from"])
                <= att
                <= instant(doc["window_covers_until"])
            ):
                raise Reject(
                    "coverage",
                    f"{where}: coverage interval does not contain attempted_at",
                )
        return
    if doc["stream"] != facts["stream"]:
        raise Reject(
            "receipt_mismatch", f"{where}: receipt stream is not the journal stream"
        )
    if doc["outcome"] in ("appended", "revised"):
        check_accepted_receipt(doc, facts, where)
    elif doc["outcome"] == "revision_conflict":
        c = doc["current"]
        if (
            c["stream"] != facts["stream"]
            or (c["record_id"], c["revision"], c["digest"])
            not in facts["accepted_heads"]
        ):
            raise Reject(
                "receipt_mismatch",
                f"{where}: current is not a head the journal accepted",
            )


# --- profile ---------------------------------------------------------------


def check_profile(p, where: str) -> None:
    extra = set(p["runtime"]["enabled_capabilities"]) - set(
        p["build"]["compiled_capabilities"]
    )
    if extra:
        raise Reject("capability_not_compiled", f"{where}: {sorted(extra)}")
    if p["runtime"]["stratum"] not in p["build"]["compiled_strata"]:
        raise Reject("stratum_not_compiled", where)


# --- self-test -------------------------------------------------------------


def self_test() -> None:
    """The checker's own controls: each mutation must be rejected for its reason."""
    base = {"a": 1, "b": {"c": 2, "d": 3}}
    check_pair(base, {"a": 9, "b": {"c": 2, "d": 3}}, "one-value")
    check_pair(base, {"a": 1, "b": {"c": 2, "e": 3}}, "sibling-rename")
    check_pair(base, {"a": 1, "b": {"c": 2}}, "one-removed")
    for label, mutated in [
        ("rename-with-value-change", {"a": 1, "b": {"c": 2, "e": 9}}),
        ("rename-with-type-change", {"a": 1, "b": {"c": 2, "e": "3"}}),
        ("two-values", {"a": 9, "b": {"c": 8, "d": 3}}),
        ("two-sibling-values", {"a": 1, "b": {"c": 8, "d": 9}}),
        ("two-removed", {"a": 1, "b": {}}),
        ("cross-parent-rename", {"z": 1, "b": {"c": 2, "d": 3, "a": 1}}),
    ]:
        try:
            check_pair(base, mutated, label)
        except Reject as r:
            if r.reason != "pair":
                raise SystemExit(
                    f"[!!] self-test {label}: rejected as {r.reason}"
                ) from None
            continue
        raise SystemExit(f"[!!] self-test {label}: accepted a multi-field pair")

    # Coverage compares parsed instants, not strings.
    facts = {
        "stream": "s",
        "failure_model": "none",
        "accepted": {},
        "accepted_heads": set(),
    }

    def nf(frm: str, att: str, until: str) -> dict:
        return {
            "kind": "reconciliation",
            "stream": "s",
            "idempotency_key": "k",
            "result": "not_found",
            "attempted_at": att,
            "window_covers_from": frm,
            "window_covers_until": until,
        }

    for label, doc in [
        (
            "submicrosecond-from",
            nf(
                "2026-10-12T12:00:00.0000001Z",
                "2026-10-12T12:00:00Z",
                "2026-10-12T13:00:00Z",
            ),
        ),
        (
            "submicrosecond-attempt",
            nf(
                "2026-10-12T12:00:00Z",
                "2026-10-12T12:00:00.0000001Z",
                "2026-10-12T13:00:00Z",
            ),
        ),
    ]:
        try:
            check_receipt_doc(doc, facts, label)
        except Reject as r:
            if r.reason != "timestamp":
                raise SystemExit(
                    f"[!!] self-test {label}: rejected as {r.reason}"
                ) from None
            continue
        raise SystemExit(f"[!!] self-test {label}: unrepresentable precision accepted")
    check_receipt_doc(
        nf(
            "2026-10-12T12:00:00.000000000Z",
            "2026-10-12T12:00:00Z",
            "2026-10-12T13:00:00Z",
        ),
        facts,
        "trailing-zero-nanoseconds",
    )
    for label, doc, ok in [
        (
            "fractional-from-after-attempt",
            nf(
                "2026-10-12T12:00:00.500Z",
                "2026-10-12T12:00:00Z",
                "2026-10-12T13:00:00Z",
            ),
            False,
        ),
        (
            "fractional-attempt-inside",
            nf(
                "2026-10-12T12:00:00Z",
                "2026-10-12T12:00:00.500Z",
                "2026-10-12T13:00:00Z",
            ),
            True,
        ),
        (
            "equivalent-offset-inside",
            nf(
                "2026-10-12T08:00:00-04:00",
                "2026-10-12T12:00:00Z",
                "2026-10-12T12:00:00.001Z",
            ),
            True,
        ),
        (
            "equivalent-offset-outside",
            nf(
                "2026-10-12T08:00:00.001-04:00",
                "2026-10-12T12:00:00Z",
                "2026-10-12T13:00:00Z",
            ),
            False,
        ),
    ]:
        try:
            check_receipt_doc(doc, facts, label)
            accepted = True
        except Reject as r:
            if r.reason != "coverage":
                raise SystemExit(
                    f"[!!] self-test {label}: rejected as {r.reason}"
                ) from None
            accepted = False
        if accepted != ok:
            raise SystemExit(
                f"[!!] self-test {label}: coverage wrongly {'accepted' if accepted else 'rejected'}"
            )
    print("ok: checker self-test")


def main(argv: list[str]) -> None:
    cmd, args = (argv[1], argv[2:]) if len(argv) > 1 else ("", [])
    try:
        if cmd == "pair" and len(args) == 2:
            check_pair(load(args[0]), load(args[1]), args[1])
        elif cmd == "journal" and len(args) == 1:
            check_journal(load_lines(args[0]), args[0])
        elif cmd == "entry" and len(args) == 1:
            check_entry(load(args[0]), args[0])
        elif cmd == "profile" and len(args) == 1:
            check_profile(load(args[0]), args[0])
        elif cmd == "receipt" and len(args) == 2:
            facts = check_journal(load_lines(args[1]), args[1])
            check_receipt_doc(load(args[0]), facts, args[0])
        elif cmd == "self-test" and not args:
            self_test()
        else:
            print(__doc__, file=sys.stderr)
            sys.exit(2)
    except Reject as r:
        print(f"[!!] {r}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main(sys.argv)
