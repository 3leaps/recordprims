#!/usr/bin/env python3
"""Logical fixture controls for durable-record/v1, not a storage implementation.

Validation interpreter: Python >= 3.9. Standard library only. External invocation:
goneat, the JSON Schema validator named by the contract (PDR-0006 Rule 2b).
Reuses v0 fixture/canonicalization helpers; makes no independent-reader claim.
"""

from __future__ import annotations

import ast
import copy
import hashlib
from datetime import timedelta
import importlib.util
import json
import math
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BASE = ROOT / "schemas/durable-record/v1"
_spec = importlib.util.spec_from_file_location(
    "record_v0", ROOT / "scripts/validate-durable-record-fixtures.py"
)
legacy = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(legacy)


def load(path):
    return legacy.load(str(path))


def digest(value):
    return legacy.digest(value)


def pin(entry):
    r = entry["record"]
    return dict(
        stream=r["stream"],
        record_id=r["record_id"],
        revision=r["revision"],
        digest=entry["digest"],
    )


def retirement_request(e):
    # The keyed retirement digest binds exactly these fields; journal-assigned
    # seq, at, group and type are excluded.
    return {
        k: e[k] for k in ("stream", "record_id", "reason", "idempotency_key") if k in e
    }


def pin_key(p):
    return p["stream"], p["record_id"], p["revision"], p["digest"]


CAPABILITY = {
    0: "contract: durable-record/v0",
    1: "contract: durable-record/v1",
}


def complete_cut(journal, cut_seq):
    # A cut ends a complete group: the entry at cut_seq closes its group.
    if not 1 <= cut_seq <= len(journal):
        return False
    e = journal[cut_seq - 1]
    return e["seq"] == e["group"] + e["group_size"] - 1


def manifest_digest(holds, retired, inherited=None):
    def ordered(members):
        return sorted(members, key=lambda m: legacy._jcs.jcs_dumps(m))

    body = dict(holds=ordered(holds), retired=ordered(retired))
    if inherited is not None:
        body["inherited"] = ordered(inherited)
    return digest(body)


def restriction_state(journal):
    """Active holds, retired ids and latest review of a journal prefix."""
    holds, retired, review = {}, set(), None
    for e in journal:
        if e["type"] == "tombstoned":
            holds[e["seq"]] = e["request_digest"]
        elif e["type"] == "untombstoned":
            holds.pop(e["request"]["restores"], None)
        elif e["type"] == "retired":
            retired.add(e["record_id"])
        elif e["type"] == "restrictions_reviewed":
            review = e
    return holds, retired, review


def source_manifest(journal):
    # Effective source restriction manifest, qualified by capability and
    # stream, including what the source itself inherited.
    capability, stream = CAPABILITY[journal[0]["v"]], journal[0]["stream"]
    holds, retired, review = restriction_state(journal)
    inherited = []
    if review is not None:
        inherited.append(
            dict(
                capability=review["source"]["capability"],
                stream=review["source"]["stream"],
                source_state_digest=review["source_state_digest"],
            )
        )
    return manifest_digest(
        [
            dict(capability=capability, stream=stream, seq=seq, request_digest=d)
            for seq, d in holds.items()
        ],
        [dict(capability=capability, stream=stream, record_id=r) for r in retired],
        inherited,
    )


def applied_manifest(journal):
    # Destination applied manifest: the holds and retirements the
    # destination itself has established.
    capability, stream = CAPABILITY[1], journal[0]["stream"]
    holds, retired, _ = restriction_state(journal)
    return manifest_digest(
        [
            dict(capability=capability, stream=stream, seq=seq, request_digest=d)
            for seq, d in holds.items()
        ],
        [dict(capability=capability, stream=stream, record_id=r) for r in retired],
    )


def source_pins(journal):
    return {
        (e["stream"], e["record"]["record_id"], e["record"]["revision"], e["digest"])
        for e in journal
        if e["type"] in ("appended", "revised")
    }


LIFECYCLE = ("body_published", "body_removal_started", "body_removed")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
MAX_SAFE_INTEGER = legacy.MAX_SAFE_INTEGER
EMPTY_MANIFEST_DIGEST = (
    "sha256:5cb72fd9337154074dc353b97f04c8dc84bba51bf1aa1f73db49a14dd890b93c"
)


def compute_recovery_row(prior_phase, prior_bound, floor, obs, damaged, referenced):
    if prior_phase is None:
        if not damaged:
            if obs == "absent":
                return "unpublished"
            return "publishing"
        if floor == 0 and obs == "absent":
            return "unpublished"
        if floor > 0 and obs == "absent":
            return "integrity_loss"
        return "publishing"
    if prior_phase == "published":
        if obs == "present_valid":
            if not damaged or floor == prior_bound:
                return "available"
            return "publishing"
        return "integrity_loss"
    if prior_phase == "started":
        if not damaged:
            return "removal_pending"
        if obs == "present_valid":
            if floor == prior_bound:
                return "available"
            return "publishing"
        if floor == prior_bound:
            return "removal_pending"
        return "integrity_loss"
    if prior_phase == "removed":
        if obs == "absent":
            if not damaged or floor == prior_bound:
                return "removed"
            return "integrity_loss"
        return "publishing"
    if prior_phase == "recovery_integrity_loss":
        return "integrity_loss"
    if obs == "present_valid":
        return "publishing"
    return "integrity_loss"


def _apply_recovery_transition(
    prior_phase, prior_bound, open_set, floor, obs, post_state
):
    new_open = set(open_set)
    if prior_phase == "started":
        if floor == prior_bound and obs == "present_valid":
            return (floor, "published"), set()
        if floor == prior_bound and obs == "absent":
            return (floor, "started"), new_open
        return (
            (floor, "recovery_publishing")
            if obs == "present_valid"
            else (floor, "recovery_integrity_loss")
        ), set()
    if post_state == "unpublished":
        return (0, None), set()
    if post_state == "available":
        return (floor, "published"), set()
    if post_state == "removed":
        return (floor, "removed"), set()
    if post_state == "publishing":
        return (floor, "recovery_publishing"), set()
    return (floor, "recovery_integrity_loss"), set()


def integral(value):
    # The JSON Schema integer grammar bounded by durable-record's exact I-JSON
    # safe integer domain: a finite, nonnegative integer <= 2^53 - 1, however
    # it is spelled (12, 12.0, 1.2e1). A Boolean is not a number here, though
    # Python treats it as one.
    if type(value) is int:
        return 0 <= value <= MAX_SAFE_INTEGER
    return (
        type(value) is float
        and math.isfinite(value)
        and value == int(value)
        and 0 <= int(value) <= MAX_SAFE_INTEGER
    )


def check_integral():
    for value in (
        0,
        12,
        MAX_SAFE_INTEGER - 1,
        MAX_SAFE_INTEGER,
        0.0,
        -0.0,
        12.0,
        1.2e1,
    ):
        assert integral(value), ("integer length rejected", value)
    for value in (
        -1,
        -(10**400),
        MAX_SAFE_INTEGER + 1,
        2**63 - 1,
        2**64 - 1,
        10**400,
        -1.0,
        0.5,
        float(MAX_SAFE_INTEGER + 1),
        float("inf"),
        float("-inf"),
        float("nan"),
        True,
        False,
        None,
        "12",
    ):
        assert not integral(value), ("invalid length accepted", value)


def require(ok, reason):
    if not ok:
        raise legacy.Reject(reason, "durable-record/v1")


CANONICAL_INSTANT = re.compile(
    r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z"
)


def canonical(text):
    return isinstance(text, str) and CANONICAL_INSTANT.fullmatch(text) is not None


class State:
    def __init__(self, window):
        require(type(window) is int and window > 0, "idempotency_window")
        require(window <= MAX_SAFE_INTEGER, "numeric_domain")
        self.window = window
        self.journal = []
        self.records = {}
        self.holds = {}
        self.active = set()
        self.keys = {}
        self.retired = set()
        self.stream = None
        self.failure_model = None
        self.source = None
        self.continues = None
        self.imports = []
        self.review = None
        self.restricted_at = []
        self.upstream = None
        self.removal_stream = None
        self.incarnation_allocation = None
        self.quarantined_designated_streams = []
        self.expected_quarantined_chain = []
        self.referenced_bodies = set()
        self.source_seal_final = False
        self.sealed_ancestor_hold = False
        self.bodies = {}
        self.open_starts = {}

    def instant(self, text):
        # Every v1 wire instant uses one spelling. A reader refuses any other
        # spelling rather than normalizing it, since digests cover the bytes.
        # The canonical pattern narrows RFC 3339; it does not replace calendar
        # validity, so an impossible date or time is refused the same way.
        require(canonical(text), "instant_spelling")
        try:
            return legacy.instant(text)
        except legacy.Reject:
            require(False, "instant_spelling")

    def remember(self, key, identity, at, receipt):
        self.keys[key] = (identity, self.instant(at), copy.deepcopy(receipt))

    def live(self, key, at):
        prior = self.keys.get(key)
        if prior and timedelta(0) <= self.instant(at) - prior[1] < timedelta(
            milliseconds=self.window
        ):
            return prior
        return None

    def ingest(self, e):
        # The v0 journal invariants and safe-integer wire bounds also apply to
        # the expanded v1 vocabulary. Group closing-seq overflow is checked both
        # here and in legacy.check_journal, so window-above-safe-max serves as
        # the single-guard numeric_domain mutation witness.
        legacy.check_typed_domain(e, "journal_entry", "journal")
        require(
            e["group"] + e["group_size"] - 1 <= MAX_SAFE_INTEGER,
            "numeric_domain",
        )
        require(e["v"] == 1, "journal_version")
        self.instant(e["at"])
        if "record" in e:
            require(e["record"]["v"] == 1, "record_version")
            if "produced_at" in e["record"]:
                self.instant(e["record"]["produced_at"]["instant"])
        if "request" in e:
            self.instant(e["request"]["observed_at"])
        # Validate complete groups; partial groups cannot be observed by requests.
        if e["seq"] == e["group"] + e["group_size"] - 1:
            legacy.check_journal(self.journal + [e], "v1", check_entry_at=False)
        if self.journal:
            require(
                self.instant(e["at"]) >= self.instant(self.journal[-1]["at"]),
                "journal_time",
            )
        typ = e["type"]
        if self.removal_stream is not None or typ in LIFECYCLE:
            # The designated stream carries only stream_opened and lifecycle
            # entries, and lifecycle entries appear nowhere else. A continuation
            # designation is also refused by the continuation checks, so its
            # vector has no single-guard mutation witness.
            designated = self.removal_stream == e["stream"]
            if designated:
                require(
                    (typ in LIFECYCLE or typ == "stream_opened")
                    and "continues" not in e,
                    "body_removal_stream",
                )
            else:
                require(typ not in LIFECYCLE, "body_removal_stream")
        if typ in LIFECYCLE:
            self.track_body(e)
        if typ == "stream_opened":
            self.stream, self.failure_model = e["stream"], e["failure_model"]
            if "continues" in e:
                self.open_continuation(e["continues"])
        elif typ in ("appended", "revised"):
            require(e["record"]["record_id"] not in self.retired, "retired_record")
            if "imported_from" in e:
                self.import_record(e)
            require(
                self.live(e["idempotency_key"], e["at"]) is None, "accepted_key_reuse"
            )
            self.records[pin_key(pin(e))] = e["record"]
            receipt = dict(
                v=1,
                kind="receipt",
                stream=self.stream,
                idempotency_key=e["idempotency_key"],
                outcome=typ,
                pin=pin(e),
                seq=e["seq"],
                failure_model=self.failure_model,
            )
            if "body_status" in e:
                receipt["body_status"] = e["body_status"]
            self.remember(
                e["idempotency_key"], ("content", e["digest"]), e["at"], receipt
            )
        elif typ in ("tombstoned", "untombstoned"):
            r = e["request"]
            require(e["request_digest"] == digest(r), "request_digest")
            require(
                typ == ("tombstoned" if r["action"] == "tombstone" else "untombstoned"),
                "operation_type",
            )
            # An accepted journal line cannot be a replay of an already-live key.
            require(
                self.live(r["idempotency_key"], e["at"]) is None, "accepted_key_reuse"
            )
            reason = self.refusal(r, e["at"])
            if reason is not None:
                raise legacy.Reject(reason, "protection journal operation")
            receipt = self.receipt(r, e["seq"])
            self.apply(r, e["seq"])
            self.remember(
                r["idempotency_key"], ("protection", digest(r)), e["at"], receipt
            )
        elif typ == "deduplicated":
            prior = self.live(e["idempotency_key"], e["at"])
            require(
                prior is not None
                and prior[0][0] == "content"
                and prior[2]["pin"]["record_id"] == e["record_id"],
                "invalid_deduplication",
            )
        elif typ == "retired":
            require(
                any(k[1] == e["record_id"] for k in self.records), "target_not_found"
            )
            if "idempotency_key" in e:
                # A keyed retirement shares the stream's key namespace and is
                # acknowledged by its own accepted receipt.
                r = retirement_request(e)
                require(e["request_digest"] == digest(r), "request_digest")
                require(
                    self.live(e["idempotency_key"], e["at"]) is None,
                    "accepted_key_reuse",
                )
                self.remember(
                    e["idempotency_key"],
                    ("retirement", digest(r)),
                    e["at"],
                    self.retirement_receipt(r, e["seq"]),
                )
            self.retired.add(e["record_id"])
        elif typ == "restrictions_reviewed":
            self.check_review(e)
        if typ in ("tombstoned", "untombstoned", "retired"):
            self.restricted_at.append(e["seq"])
        self.journal.append(copy.deepcopy(e))

    def source_cut(self, cut):
        # The source journal the store observes; the cut must be a complete
        # group whose prefix digest matches, under the declared capability.
        source = self.source
        require(source is not None, "continuation")
        require(
            cut["capability"] == CAPABILITY.get(source[0]["v"])
            and all(e["v"] == source[0]["v"] for e in source),
            "continuation",
        )
        require(cut["stream"] == source[0]["stream"], "continuation")
        require(complete_cut(source, cut["cut_seq"]), "continuation")
        prefix = source[: cut["cut_seq"]]
        require(digest(prefix) == cut["cut_digest"], "continuation")
        return prefix

    def open_continuation(self, cut):
        self.source_cut(cut)
        self.continues = copy.deepcopy(cut)

    def import_record(self, e):
        imported = e["imported_from"]
        require(self.continues is not None, "continuation")
        require(
            imported["capability"] == self.continues["capability"]
            and imported["pin"]["stream"] == self.continues["stream"],
            "continuation",
        )
        self.imports.append((e["seq"], pin_key(imported["pin"])))

    def track_body(self, e):
        # One lifecycle per {digest, length}: (latest incarnation, phase), where
        # phase is published, started, removed, recovery_publishing or
        # recovery_integrity_loss.
        key = (e["body"]["digest"], e["body"]["length"])
        latest, phase = self.bodies.get(key, (0, None))
        n, typ = e["incarnation"], e["type"]
        if typ == "body_published":
            # A later publication closes an open removal as abandoned and
            # clears carried recovery publishing/integrity-loss state.
            require(n > latest, "body_lifecycle")
            self.bodies[key] = (n, "published")
        elif typ == "body_removal_started":
            require(n == latest and phase == "published", "body_lifecycle")
            self.bodies[key] = (n, "started")
            self.open_starts.setdefault(key, set()).add(n)
        else:
            require(n == latest and phase == "started", "body_lifecycle")
            self.bodies[key] = (n, "removed")
            self.open_starts.get(key, set()).discard(n)

    def body_state(self, step):
        # The observation is closed: an exact body and a real boolean. Anything
        # else, including an unknown or failed observation, is refused before
        # any state is derived, never read as present or absent.
        require(
            isinstance(step, dict)
            and set(step) == {"body", "present"}
            and type(step["present"]) is bool
            and isinstance(step["body"], dict)
            and set(step["body"]) == {"digest", "length"}
            and isinstance(step["body"]["digest"], str)
            and DIGEST.fullmatch(step["body"]["digest"]) is not None
            and integral(step["body"]["length"]),
            "body_observation",
        )
        legacy.check_journal(self.journal, "v1", check_entry_at=False)
        require(self.removal_stream == self.stream, "body_removal_stream")
        body = step["body"]
        bound, phase = self.bodies.get((body["digest"], int(body["length"])), (0, None))
        obs = "present_valid" if step["present"] else "absent"
        is_ref = (body["digest"], int(body["length"])) in self.referenced_bodies
        state = compute_recovery_row(phase, bound, bound, obs, False, is_ref)
        return {"outcome": "body_state", "state": state}

    def check_review(self, e):
        require(self.continues is not None, "continuation")
        src = e["source"]
        require(
            src["capability"] == self.continues["capability"]
            and src["stream"] == self.continues["stream"],
            "continuation",
        )
        # A review is its own atomic group and covers the complete cut
        # immediately before it.
        require(
            e["group"] == e["seq"]
            and e["group_size"] == 1
            and e["destination_cut_seq"] == e["seq"] - 1,
            "continuation",
        )
        prefix = self.source_cut(src)
        # A review may advance past the opening cut, never before it, and the
        # opening prefix must still hash to the declared continues digest.
        # Each of these two guards backs up the other, and the opening prefix
        # is also checked when the stream opens, so neither has a single-guard
        # mutation witness; vectors cover both.
        require(src["cut_seq"] >= self.continues["cut_seq"], "continuation")
        require(
            digest(prefix[: self.continues["cut_seq"]]) == self.continues["cut_digest"],
            "continuation",
        )
        require(source_manifest(prefix) == e["source_state_digest"], "continuation")
        known = source_pins(prefix)
        require(all(p in known for _, p in self.imports), "continuation")
        require(
            applied_manifest(self.journal) == e["destination_applied_state_digest"],
            "continuation",
        )
        self.review = copy.deepcopy(e)

    def review_pending(self, r):
        # The store supplies the source's current state digest as a trusted
        # input; it must equal the observed source, then match the review.
        if self.continues is None:
            return "source_state_digest" in r
        if self.upstream is UNOBSERVED:
            return True
        if r.get("source_state_digest") != source_manifest(self.source):
            return True
        return None

    def review_current(self):
        # A review covers imports and restriction state through its own cut.
        # A later import or destination restriction change reopens it; so does
        # a change in the source's effective state, or in any ancestor's.
        if self.continues is None:
            return True
        if self.review is None:
            return False
        after = self.review["seq"]
        if any(seq > after for seq, _ in self.imports):
            return False
        if any(seq > after for seq in self.restricted_at):
            return False
        if self.source_seal_final:
            return True
        if source_manifest(self.source) != self.review["source_state_digest"]:
            return False
        return self.upstream is None or self.upstream.review_current()

    def refusal(self, r, at):
        if r["stream"] != self.stream or r["target"]["stream"] != self.stream:
            return "target_not_found"
        record = self.records.get(pin_key(r["target"]))
        if record is None:
            return "target_not_found"
        if r["scope"] == "body" and (
            "body" not in record
            or any(record["body"][k] != r["body"][k] for k in ("digest", "length"))
        ):
            return "body_mismatch"
        if self.instant(r["observed_at"]) > self.instant(at):
            return "observation_order"
        if r["action"] == "restore":
            if r["restores"] not in self.active:
                return "hold_not_active"
            hold = self.holds[r["restores"]]
            if any(r.get(k) != hold.get(k) for k in ("target", "scope", "body")):
                return "hold_mismatch"
            if self.instant(r["observed_at"]) < self.instant(hold["observed_at"]):
                return "observation_order"
        return None

    def receipt(self, r, seq):
        return dict(
            v=1,
            stream=r["stream"],
            idempotency_key=r["idempotency_key"],
            outcome="tombstoned" if r["action"] == "tombstone" else "untombstoned",
            seq=seq,
            request_digest=digest(r),
            failure_model=self.failure_model,
        )

    def retirement_receipt(self, r, seq):
        return dict(
            v=1,
            kind="receipt",
            stream=r["stream"],
            idempotency_key=r.get("idempotency_key"),
            outcome="retired",
            seq=seq,
            record_id=r["record_id"],
            request_digest=digest(r),
            failure_model=self.failure_model,
        )

    def retire(self, r, at):
        # Keyed retirement request: replay, conflict and acceptance mirror the
        # protection-receipt rules. Key lookup precedes current-state checks.
        legacy.check_journal(self.journal, "v1", check_entry_at=False)
        require(
            self.instant(at) >= self.instant(self.journal[-1]["at"]), "journal_time"
        )
        prior = (
            self.live(r["idempotency_key"], at) if r["stream"] == self.stream else None
        )
        if prior:
            if prior[0] == ("retirement", digest(r)):
                return copy.deepcopy(prior[2])
            reason = "idempotency_conflict"
        elif r["stream"] != self.stream or not any(
            k[1] == r["record_id"] for k in self.records
        ):
            reason = "target_not_found"
        else:
            reason = None
        if reason:
            return dict(
                v=1,
                kind="receipt",
                stream=r["stream"],
                idempotency_key=r["idempotency_key"],
                outcome=reason,
            )
        seq = len(self.journal) + 1
        require(seq <= MAX_SAFE_INTEGER, "numeric_domain")
        self.ingest(
            dict(
                v=1,
                seq=seq,
                group=seq,
                group_size=1,
                stream=self.stream,
                type="retired",
                at=at,
                record_id=r["record_id"],
                reason=r["reason"],
                idempotency_key=r["idempotency_key"],
                request_digest=digest(r),
            )
        )
        return self.retirement_receipt(r, seq)

    def apply(self, r, seq):
        if r["action"] == "tombstone":
            self.holds[seq] = copy.deepcopy(r)
            self.active.add(seq)
        else:
            self.active.remove(r["restores"])

    def request(self, r, at):
        legacy.check_typed_domain(r, "protection_request", "request")
        legacy.check_journal(self.journal, "v1", check_entry_at=False)
        require(
            self.instant(at) >= self.instant(self.journal[-1]["at"]), "journal_time"
        )
        self.instant(r["observed_at"])
        # Stream scope and live-key replay precede target/current-state checks.
        prior = (
            self.live(r["idempotency_key"], at) if r["stream"] == self.stream else None
        )
        if prior:
            if prior[0] == ("protection", digest(r)):
                return copy.deepcopy(prior[2])
            reason = "idempotency_conflict"
        else:
            reason = self.refusal(r, at)
        if reason:
            return dict(
                v=1,
                stream=r["stream"],
                idempotency_key=r["idempotency_key"],
                outcome=reason,
            )
        seq = len(self.journal) + 1
        require(seq <= MAX_SAFE_INTEGER, "numeric_domain")
        receipt = self.receipt(r, seq)
        self.ingest(
            dict(
                v=1,
                seq=seq,
                group=seq,
                group_size=1,
                stream=self.stream,
                type=receipt["outcome"],
                at=at,
                request=copy.deepcopy(r),
                request_digest=receipt["request_digest"],
            )
        )
        return receipt

    def blocked(self, p):
        record = self.records[pin_key(p)]
        for seq in self.active:
            h = self.holds[seq]
            if h["scope"] == "record" and p["record_id"] == h["target"]["record_id"]:
                return True
            if h["scope"] == "revision" and p == h["target"]:
                return True
            if (
                h["scope"] == "body"
                and "body" in record
                and all(record["body"][k] == h["body"][k] for k in ("digest", "length"))
            ):
                return True
        return False

    def verify_receipt(self, received):
        legacy.check_typed_domain(
            received,
            "write_receipt"
            if received.get("kind") in ("receipt", "reconciliation")
            else "protection_receipt",
            "receipt",
        )
        if received.get("kind") == "reconciliation":
            return self.verify_reconciliation(received)
        require(
            received["outcome"] in ("tombstoned", "untombstoned", "retired"),
            "receipt_mismatch",
        )
        require(received["v"] == 1, "receipt_mismatch")
        seq = received["seq"]
        require(1 <= seq <= len(self.journal), "receipt_mismatch")
        entry = self.journal[seq - 1]
        if received["outcome"] == "retired":
            # Only a keyed retirement is acknowledged; an unkeyed entry has no
            # receipt. The exact-receipt comparison below also refuses any
            # schema-valid receipt for an unkeyed entry, so this guard states
            # the rule rather than being its only enforcement.
            require(
                entry["type"] == "retired" and "idempotency_key" in entry,
                "receipt_mismatch",
            )
            expected = self.retirement_receipt(retirement_request(entry), seq)
        else:
            require(entry["type"] in ("tombstoned", "untombstoned"), "receipt_mismatch")
            expected = self.receipt(entry["request"], seq)
        require(received == expected, "receipt_mismatch")
        return {"outcome": "receipt_verified"}

    def verify_reconciliation(self, received):
        self.instant(received["attempted_at"])
        if "window_covers_from" in received:
            self.instant(received["window_covers_from"])
        if "window_covers_until" in received:
            self.instant(received["window_covers_until"])
        return {"outcome": "receipt_verified"}

    def reconcile(self, key, at, attempted_at):
        legacy.check_journal(self.journal, "v1", check_entry_at=False)
        self.instant(attempted_at)
        require(
            self.instant(at) >= self.instant(self.journal[-1]["at"]), "journal_time"
        )
        result = dict(
            v=1,
            kind="reconciliation",
            stream=self.stream,
            idempotency_key=key,
            attempted_at=attempted_at,
            result="unknown",
            reason="coverage_unprovable",
        )
        prior = self.live(key, at)
        if prior:
            result.pop("reason")
            result.update(result="found", receipt=copy.deepcopy(prior[2]))
        # This logical interpreter has no authoritative coverage provider.
        # It never manufactures not_found from an absent cache entry.
        return result

    def use(self, r):
        legacy.check_typed_domain(r, "export_request", "use")
        legacy.check_journal(self.journal, "v1", check_entry_at=False)

        def refused(reason):
            return dict(outcome="refused", reason=reason)

        if r["purpose"] not in ("serve", "export", "reuse"):
            return refused("invalid_purpose")
        if (
            r["stream"] != self.stream
            or r["protection_state"] != "verified"
            or r["protection_cut"] != len(self.journal)
            or r["current_cut"] != len(self.journal)
            or self.review_pending(r)
        ):
            return refused("protection_unavailable")
        if not self.review_current():
            return refused("restrictions_pending")
        selected = {pin_key(p) for p in r["selection"]}
        omitted = {pin_key(p) for p in r["omitted"]}
        if not (selected | omitted) <= self.records.keys():
            return refused("target_not_found")
        if selected & omitted or selected | omitted != self.records.keys():
            return refused("export_selection")
        if r["inherited_block"] or any(self.blocked(p) for p in r["selection"]):
            return refused("protected_use")
        if r["purpose"] == "serve" and any(
            p["record_id"] in self.retired for p in r["selection"]
        ):
            return refused("protected_use")
        supplied = {}
        for b in r["bodies"]:
            raw = bytes.fromhex(b["hex"])
            if (
                "sha256:" + hashlib.sha256(raw).hexdigest() != b["digest"]
                or b["digest"] in supplied
            ):
                return refused("body_closure")
            supplied[b["digest"]] = len(raw)
        needed = {}
        for key in selected:
            body = self.records[key].get("body")
            if body is not None:
                if (
                    body["digest"] in needed
                    and needed[body["digest"]] != body["length"]
                ):
                    return refused("body_closure")
                needed[body["digest"]] = body["length"]
        if any(k not in needed or needed[k] != v for k, v in supplied.items()):
            return refused("body_closure")
        if r["closure"] == "full_content" and supplied != needed:
            return refused("body_closure")
        return dict(
            outcome="allowed",
            closure=r["closure"],
            selection=r["selection"],
            omitted=r["omitted"],
            protection_cut=r["protection_cut"],
        )

    def retention_revisions(self):
        return list(self.records.items())

    def removable_incarnation_ok(self, body, incarnation, mode):
        key = (body["digest"], int(body["length"]))
        latest_n, phase = self.bodies.get(key, (0, None))
        if mode == "start":
            return phase == "published" and incarnation == latest_n
        if mode == "retry_unlink":
            return (
                incarnation in self.open_starts.get(key, set())
                and phase == "started"
                and incarnation == latest_n
            )
        return False

    def valid_removal_authorization(
        self, auth, step, decision_ts, not_before_ts, expires_at_ts
    ):
        if not isinstance(auth, dict):
            return False
        if set(auth) != REQUIRED_AUTH_KEYS:
            return False
        if not (
            isinstance(auth["ref"], str)
            and auth["ref"] != ""
            and isinstance(auth["digest"], str)
            and DIGEST.fullmatch(auth["digest"]) is not None
        ):
            return False
        auth_policy = auth["policy"]
        if not (
            isinstance(auth_policy, dict)
            and set(auth_policy) == {"id", "version"}
            and isinstance(auth_policy["id"], str)
            and auth_policy["id"] != ""
            and isinstance(auth_policy["version"], str)
            and auth_policy["version"] != ""
        ):
            return False
        if auth_policy["id"] != step["expected_policy"]["id"]:
            return False
        if auth_policy["version"] != step["expected_policy"]["version"]:
            return False
        if auth["store"] != step["store"]:
            return False
        auth_body = auth["body"]
        if not (
            isinstance(auth_body, dict)
            and set(auth_body) == {"digest", "length"}
            and isinstance(auth_body["digest"], str)
            and DIGEST.fullmatch(auth_body["digest"]) is not None
            and integral(auth_body["length"])
        ):
            return False
        if auth_body["digest"] != step["body"]["digest"]:
            return False
        if int(auth_body["length"]) != int(step["body"]["length"]):
            return False
        if not integral(auth["incarnation"]) or int(auth["incarnation"]) < 1:
            return False
        if int(auth["incarnation"]) != int(step["incarnation"]):
            return False
        if auth["action"] != "remove_body":
            return False
        if auth["scope_verified"] is not True:
            return False
        if not_before_ts is None or expires_at_ts is None:
            return False
        if decision_ts < not_before_ts:
            return False
        if decision_ts > expires_at_ts:
            return False
        return True

    def verify_seal_step(self, step_ctx):
        require(
            isinstance(step_ctx, dict)
            and {"seal", "expected_store", "accepted_trust_profile"}
            <= set(step_ctx)
            <= ALLOWED_SEAL_CTX_KEYS,
            "continuation",
        )
        seal = step_ctx["seal"]
        legacy.check_typed_domain(seal, "stream_seal", "verify_seal")
        self.instant(seal.get("sealed_at") if isinstance(seal, dict) else None)
        ok_shape = (
            shape_raw(step_ctx["raw_seal"], "stream-seal")
            if "raw_seal" in step_ctx
            else shape(seal, "stream-seal")
        )
        require(ok_shape, "continuation")
        journal, raw_entries = _parse_source_spec(
            step_ctx["raw_journal"]
            if "raw_journal" in step_ctx
            else step_ctx.get("journal")
        )
        require(
            isinstance(journal, list)
            and journal
            and all(isinstance(e, dict) for e in journal)
            and "v" in journal[0],
            "continuation",
        )
        is_final, _anc_hold, _prefix = _evaluate_seal_context(
            self,
            step_ctx,
            journal,
            self.window,
            raw_entries=raw_entries,
            observed_upstream=UNOBSERVED
            if journal[0].get("continues") is not None
            else None,
            check_top_seal=False,
        )
        if "expected_manifest" in step_ctx:
            exp_m = step_ctx["expected_manifest"]
            if "expected_manifest_jcs" in step_ctx:
                require(
                    legacy._jcs.jcs_dumps(exp_m) == step_ctx["expected_manifest_jcs"],
                    "continuation",
                )
            require(
                digest(exp_m) == seal["effective_restriction_manifest_digest"],
                "continuation",
            )
        return {
            "outcome": "seal_verified",
            "final": is_final,
            "effective_restriction_manifest_digest": seal[
                "effective_restriction_manifest_digest"
            ],
        }

    def removal_eligibility(self, step):
        legacy.check_typed_domain(step, "removal_eligibility", "removal_eligibility")
        legacy.check_journal(self.journal, "v1", check_entry_at=False)
        decision_ts = self.instant(
            step.get("decision_time") if isinstance(step, dict) else None
        )
        auth = step.get("authorization") if isinstance(step, dict) else None
        auth_not_before_ts = None
        auth_expires_at_ts = None
        if isinstance(auth, dict):
            if "not_before" in auth:
                auth_not_before_ts = self.instant(auth["not_before"])
            if "expires_at" in auth:
                auth_expires_at_ts = self.instant(auth["expires_at"])

        def removal_refused(reason):
            return dict(outcome="removal_eligibility", verdict="refused", reason=reason)

        target_body = step.get("body") if isinstance(step, dict) else None
        exp_pol = step.get("expected_policy") if isinstance(step, dict) else None
        store_streams = step.get("store_streams") if isinstance(step, dict) else None
        observed_list = step.get("observed_streams") if isinstance(step, dict) else None
        step_shape_ok = (
            isinstance(step, dict)
            and REQUIRED_REMOVAL_STEP_KEYS <= set(step) <= ALLOWED_REMOVAL_STEP_KEYS
            and isinstance(step["store"], str)
            and step["store"] != ""
            and isinstance(store_streams, list)
            and len(store_streams) >= 1
            and all(isinstance(s, str) and s != "" for s in store_streams)
            and isinstance(observed_list, list)
            and isinstance(target_body, dict)
            and set(target_body) == {"digest", "length"}
            and isinstance(target_body["digest"], str)
            and DIGEST.fullmatch(target_body["digest"]) is not None
            and integral(target_body["length"])
            and isinstance(exp_pol, dict)
            and set(exp_pol) == {"id", "version"}
            and isinstance(exp_pol["id"], str)
            and exp_pol["id"] != ""
            and isinstance(exp_pol["version"], str)
            and exp_pol["version"] != ""
        )

        duplicate_store_streams = False
        duplicate_observed_streams = False
        coverage_missing = False
        stream_damaged = False
        unobservable_source = False
        same_store_source_mismatch = False
        quarantined_in_store_streams = False
        quarantined_chain_mismatch = False
        local_hold = False
        inherited_hold = False
        source_chain_hold = False
        pending_review_hold = False
        sealed_ancestor_hold = False
        all_refs = []
        active_refs = []
        replayed_by_name = {}

        effective_quarantined = (
            step.get("quarantined_designated_streams")
            if isinstance(step, dict) and "quarantined_designated_streams" in step
            else self.quarantined_designated_streams
        )
        if not isinstance(effective_quarantined, list) or (
            effective_quarantined != self.expected_quarantined_chain
        ):
            quarantined_chain_mismatch = True

        if isinstance(store_streams, list) and isinstance(observed_list, list):
            if isinstance(effective_quarantined, list) and any(
                s in effective_quarantined for s in store_streams
            ):
                quarantined_in_store_streams = True
            expected_sharing = [s for s in store_streams if s != self.removal_stream]
            obs_names = [
                obs.get("stream")
                for obs in observed_list
                if isinstance(obs, dict) and isinstance(obs.get("stream"), str)
            ]
            observed_by_name = {
                obs["stream"]: obs
                for obs in observed_list
                if isinstance(obs, dict) and isinstance(obs.get("stream"), str)
            }
            duplicate_store_streams = len(store_streams) != len(set(store_streams))
            duplicate_observed_streams = len(obs_names) != len(set(obs_names))
            coverage_missing = (
                (
                    self.removal_stream is not None
                    and self.removal_stream not in store_streams
                )
                or len(obs_names) != len(observed_list)
                or any(
                    name not in store_streams or name == self.removal_stream
                    for name in obs_names
                )
            )
            for s_name in expected_sharing:
                if s_name not in observed_by_name:
                    coverage_missing = True
                    continue
                obs = observed_by_name[s_name]
                st, damaged, unobs, chain_journals = _replay_observed_sharing_stream(
                    obs, self.window
                )
                if damaged:
                    stream_damaged = True
                    continue
                if unobs:
                    unobservable_source = True
                    continue
                replayed_by_name[s_name] = (obs, st, chain_journals)
                if obs.get("inherited_block") is True:
                    inherited_hold = True
                if st.sealed_ancestor_hold:
                    sealed_ancestor_hold = True
                for anc_j in chain_journals:
                    anc_holds, _, _ = restriction_state(anc_j)
                    if anc_holds:
                        source_chain_hold = True
                if not st.review_current():
                    pending_review_hold = True
                if (
                    isinstance(target_body, dict)
                    and set(target_body)
                    == {
                        "digest",
                        "length",
                    }
                    and integral(target_body["length"])
                ):
                    for pk, rec in st.retention_revisions():
                        if body_matches_target(rec.get("body"), target_body):
                            all_refs.append((s_name, pk))
                            p_dict = dict(
                                stream=pk[0],
                                record_id=pk[1],
                                revision=pk[2],
                                digest=pk[3],
                            )
                            if st.blocked(p_dict):
                                local_hold = True
                            if rec["record_id"] not in st.retired:
                                active_refs.append((s_name, pk))

            for _s_name, (_obs, st, _cj) in replayed_by_name.items():
                if st.continues is not None and st.continues["stream"] in store_streams:
                    src_name = st.continues["stream"]
                    if src_name not in replayed_by_name:
                        unobservable_source = True
                    elif st.source != replayed_by_name[src_name][1].journal:
                        same_store_source_mismatch = True

        def check_p1():
            if self.removal_stream is None or self.stream != self.removal_stream:
                return removal_refused("body_removal_stream")
            if not step_shape_ok:
                return removal_refused("body_lifecycle")
            if step.get("mode") not in ("start", "retry_unlink"):
                return removal_refused("body_lifecycle")
            if step.get("reason") not in (
                "retention",
                "operator",
                "rights_restricted",
            ):
                return removal_refused("body_lifecycle")
            if not integral(step["incarnation"]) or int(step["incarnation"]) < 1:
                return removal_refused("body_lifecycle")
            if not self.removable_incarnation_ok(
                target_body, int(step["incarnation"]), step["mode"]
            ):
                return removal_refused("body_lifecycle")
            return None

        def check_p2():
            if duplicate_store_streams:
                return removal_refused("protection_unavailable")
            if duplicate_observed_streams:
                return removal_refused("protection_unavailable")
            if coverage_missing:
                return removal_refused("protection_unavailable")
            if stream_damaged:
                return removal_refused("protection_unavailable")
            if unobservable_source:
                return removal_refused("protection_unavailable")
            if same_store_source_mismatch:
                return removal_refused("protection_unavailable")
            if quarantined_in_store_streams:
                return removal_refused("protection_unavailable")
            if quarantined_chain_mismatch:
                return removal_refused("protection_unavailable")
            return None

        def check_p2b():
            if decision_ts < self.instant(self.journal[-1]["at"]):
                return removal_refused("journal_time")
            for _s_name, (_obs, st, _cj) in replayed_by_name.items():
                if decision_ts < st.instant(st.journal[-1]["at"]):
                    return removal_refused("journal_time")
            return None

        def check_p3():
            if local_hold:
                return removal_refused("protected_use")
            if inherited_hold:
                return removal_refused("protected_use")
            if source_chain_hold:
                return removal_refused("protected_use")
            if pending_review_hold:
                return removal_refused("protected_use")
            if sealed_ancestor_hold:
                return removal_refused("protected_use")
            return None

        def check_p4():
            if active_refs:
                return removal_refused("still_referenced")
            return None

        def check_p5():
            if all_refs and not self.valid_removal_authorization(
                auth, step, decision_ts, auth_not_before_ts, auth_expires_at_ts
            ):
                return removal_refused("removal_authorization")
            return None

        precedence_order = [
            check_p1,
            check_p2,
            check_p2b,
            check_p3,
            check_p4,
            check_p5,
        ]
        for check_fn in precedence_order:
            verdict = check_fn()
            if verdict is not None:
                return verdict
        return {"outcome": "removal_eligibility", "verdict": "eligible"}


REQUIRED_REMOVAL_STEP_KEYS = {
    "store",
    "store_streams",
    "observed_streams",
    "body",
    "incarnation",
    "mode",
    "reason",
    "decision_time",
    "expected_policy",
}
ALLOWED_REMOVAL_STEP_KEYS = REQUIRED_REMOVAL_STEP_KEYS | {
    "authorization",
    "quarantined_designated_streams",
}
REQUIRED_AUTH_KEYS = {
    "ref",
    "digest",
    "store",
    "body",
    "incarnation",
    "policy",
    "action",
    "scope_verified",
    "not_before",
    "expires_at",
}
ALLOWED_OBS_KEYS = {
    "stream",
    "damaged",
    "journal",
    "raw_journal",
    "source_journal",
    "raw_source_journal",
    "ancestors",
    "raw_ancestors",
    "inherited_block",
    "source_seal",
}
ALLOWED_SEAL_CTX_KEYS = {
    "seal",
    "raw_seal",
    "expected_store",
    "accepted_trust_profile",
    "closer_authenticated",
    "source_write_fenced",
    "trailing_bytes",
    "live_source_unobservable",
    "journal",
    "raw_journal",
    "expected_manifest",
    "expected_manifest_jcs",
    "manifest",
    "ancestor_seal_contexts",
}


def _norm_body_field(b, k):
    return int(b[k]) if k == "length" else b[k]


def body_matches_target(rec_body, target_body):
    return (
        rec_body is not None
        and integral(rec_body["length"])
        and integral(target_body["length"])
        and all(
            _norm_body_field(rec_body, k) == _norm_body_field(target_body, k)
            for k in ("digest", "length")
        )
    )


def _replay_observed_sharing_stream(obs, window_ms):
    if (
        not isinstance(obs, dict)
        or not ({"stream"} <= set(obs) <= ALLOWED_OBS_KEYS)
        or not isinstance(obs.get("stream"), str)
    ):
        return None, True, False, []
    if "damaged" in obs and type(obs["damaged"]) is not bool:
        return None, True, False, []
    if obs.get("damaged", False) is True:
        return None, True, False, []
    if (
        ("journal" in obs and "raw_journal" in obs)
        or ("source_journal" in obs and "raw_source_journal" in obs)
        or ("ancestors" in obs and "raw_ancestors" in obs)
    ):
        return None, True, False, []
    if ("ancestors" in obs or "raw_ancestors" in obs) and not (
        "source_journal" in obs or "raw_source_journal" in obs
    ):
        return None, False, True, []
    raw_entries = None
    if "raw_journal" in obs:
        try:
            journal = [
                legacy.parse_wire(raw, "journal_entry", "observed_raw_journal")
                for raw in obs["raw_journal"]
            ]
            raw_entries = obs["raw_journal"]
        except legacy.Reject:
            return None, True, False, []
    elif "journal" in obs and isinstance(obs["journal"], list) and obs["journal"]:
        journal = obs["journal"]
    else:
        return None, True, False, []

    for idx, entry in enumerate(journal):
        ok_shape = (
            shape_raw(raw_entries[idx], "journal-entry")
            if raw_entries is not None
            else shape(entry, "journal-entry")
        )
        if not ok_shape:
            return None, True, False, []

    st = State(window_ms)
    chain_journals = []
    if "source_journal" in obs or "raw_source_journal" in obs:
        try:
            obs_case = {**obs, "window_ms": window_ms}
            (st.source, st.upstream), chain_journals = _chain_with_journals(obs_case)
            if "source_seal" in obs:
                if not chain_journals and st.source is not None:
                    chain_journals = [copy.deepcopy(st.source)]
                _apply_case_source_seal(obs_case, st)
        except legacy.Reject:
            return None, False, True, []
        if st.upstream is UNOBSERVED:
            return None, False, True, []
    elif "source_seal" in obs:
        return None, False, True, []

    try:
        legacy.check_journal(journal, "v1", check_entry_at=False)
        for entry in journal:
            st.ingest(entry)
    except legacy.Reject:
        if (
            journal
            and isinstance(journal[0], dict)
            and "continues" in journal[0]
            and st.source is None
        ):
            return None, False, True, []
        return None, True, False, []

    if st.stream != obs.get("stream"):
        return None, True, False, []
    if st.continues is not None:
        if "inherited_block" not in obs:
            return None, False, True, []
        if type(obs.get("inherited_block", False)) is not bool:
            return None, False, True, []
    else:
        if (
            "inherited_block" in obs
            or "source_journal" in obs
            or "raw_source_journal" in obs
            or "ancestors" in obs
            or "raw_ancestors" in obs
            or "source_seal" in obs
        ):
            return None, False, True, []
    return st, False, False, chain_journals


class Unobserved:
    def review_current(self):
        return False


UNOBSERVED = Unobserved()


def observed(journal, window, source, upstream, raw_entries=None):
    """Replay one observed source journal under its declared family.

    An invalid source cannot supply restriction state: wire ingestion, numeric
    domain, shape, journal and family semantics are all checked before its
    manifest is derived.
    """
    family = journal[0]["v"]
    require(family in CAPABILITY, "continuation")
    schema = BASE.parent / f"v{family}" / "journal-entry.schema.json"
    for idx, e in enumerate(journal):
        ok_shape = (
            shape_raw(raw_entries[idx], schema)
            if raw_entries is not None
            else shape(e, schema)
        )
        require(e.get("v") == family and ok_shape, "continuation")
    state = None
    try:
        legacy.check_journal(journal, "source")
        if family == 0:
            return None
        state = State(window)
        state.source, state.upstream = source, upstream
        for e in journal:
            state.ingest(e)
    except legacy.Reject:
        require(False, "continuation")
    return state


def _parse_source_spec(spec):
    if isinstance(spec, list) and spec and all(isinstance(x, str) for x in spec):
        try:
            parsed = [
                legacy.parse_wire(x, "journal_entry", f"source[{i}]")
                for i, x in enumerate(spec)
            ]
        except legacy.Reject:
            require(False, "continuation")
        return parsed, spec
    return spec, None


def _chain_with_journals(case):
    # The observed source chain: the immediate source, then each ancestor the
    # store observes. Each is replayed under its own family; a source that is
    # itself a continuation is replayed over its own source, so its review
    # currency can be checked.
    first_spec = (
        case["raw_source_journal"]
        if "raw_source_journal" in case
        else case["source_journal"]
    )
    anc_specs = (
        case["raw_ancestors"] if "raw_ancestors" in case else case.get("ancestors", [])
    )
    specs = [first_spec] + list(anc_specs)
    upstream = None
    validated_rev = []
    for spec in reversed(specs):
        journal, raw_entries = _parse_source_spec(spec)
        # A malformed container is refused by name, never by an exception.
        # Disabling this guard crashes rather than accepts, so its vectors have
        # no mutation witness.
        require(
            isinstance(journal, list)
            and journal
            and all(isinstance(e, dict) for e in journal)
            and "v" in journal[0],
            "continuation",
        )
        if journal[0].get("continues") is not None and upstream is None:
            # A continuation source whose own source is not observed cannot
            # show that the chain is current.
            first_journal, _ = _parse_source_spec(specs[0])
            return (copy.deepcopy(first_journal), UNOBSERVED), []
        if journal[0].get("continues") is None and upstream is not None:
            require(False, "continuation")
        source, above = upstream if upstream else (None, None)
        state = observed(
            journal, case["window_ms"], source, above, raw_entries=raw_entries
        )
        continuation = state is not None and state.continues is not None
        upstream = (copy.deepcopy(journal), state if continuation else None)
        validated_rev.append(copy.deepcopy(journal))
    return upstream, list(reversed(validated_rev))


def chain(case):
    upstream, _ = _chain_with_journals(case)
    return upstream


class UncurrentSealed:
    def review_current(self):
        return False


UNCURRENT_SEALED = UncurrentSealed()


def _scan_quarantined_designated_image(
    raw_img, expected_stream, init_bodies, init_open_starts, window_ms
):
    require(isinstance(raw_img, str), "recovery_incomplete")
    img_bytes = raw_img.encode("utf-8")
    img_digest = "sha256:" + hashlib.sha256(img_bytes).hexdigest()
    hop_st = State(window_ms)
    hop_st.removal_stream = expected_stream
    hop_st.incarnation_allocation = "prewrite_durable_ledger"
    hop_st.bodies = copy.deepcopy(init_bodies)
    hop_st.open_starts = copy.deepcopy(init_open_starts)
    offset = 0
    longest_prefix = []
    longest_offset = 0
    longest_bodies = copy.deepcopy(init_bodies)
    longest_open = copy.deepcopy(init_open_starts)
    for ln in raw_img.splitlines(keepends=True):
        if not ln.endswith("\n") or "\r" in ln:
            break
        try:
            e = legacy.parse_wire(ln, "journal_entry", "quarantined")
        except legacy.Reject:
            break
        if not shape_raw(ln, "journal-entry"):
            break
        try:
            hop_st.ingest(e)
        except legacy.Reject:
            break
        offset += len(ln.encode("utf-8"))
        if e["seq"] == e["group"] + e["group_size"] - 1:
            longest_prefix = copy.deepcopy(hop_st.journal)
            longest_offset = offset
            longest_bodies = copy.deepcopy(hop_st.bodies)
            longest_open = copy.deepcopy(hop_st.open_starts)
    is_damaged = longest_offset < len(img_bytes)
    return longest_prefix, is_damaged, img_digest, longest_bodies, longest_open


def _validate_and_replay_recovery_chain(case, s):
    quarantined_list = list(s.quarantined_designated_streams)
    quarantined_map = case.get("quarantined_streams", {})
    candidates = case.get("candidate_streams", [])
    first_entry = case["journal"][0] if case.get("journal") else {}
    has_recovery_header = isinstance(first_entry, dict) and "recovery" in first_entry
    if has_recovery_header:
        require(
            s.removal_stream is not None
            and first_entry.get("stream") == s.removal_stream,
            "body_removal_stream",
        )
    if has_recovery_header or len(quarantined_list) > 0:
        require(
            s.incarnation_allocation == "prewrite_durable_ledger",
            "body_removal_stream",
        )

    seen_prev: dict[str, set[str]] = {}
    open_entries = []
    if has_recovery_header:
        open_entries.append(first_entry)
    for cand in candidates:
        if isinstance(cand, dict) and "recovery" in cand:
            open_entries.append(cand)
    if isinstance(quarantined_map, dict):
        for raw_q in quarantined_map.values():
            if isinstance(raw_q, str):
                first_ln = raw_q.splitlines(keepends=True)[:1]
                if first_ln and first_ln[0].endswith("\n") and "\r" not in first_ln[0]:
                    try:
                        qe = legacy.parse_wire(first_ln[0], "journal_entry", "q_open")
                        if isinstance(qe, dict) and "recovery" in qe:
                            open_entries.append(qe)
                    except legacy.Reject:
                        pass
    for oe in open_entries:
        prev_s = oe["recovery"]["previous_stream"]
        seen_prev.setdefault(prev_s, set()).add(oe["stream"])

    fork_detected = any(len(claimants) > 1 for claimants in seen_prev.values())
    require(not fork_detected, "body_removal_stream")
    stale_profile_detected = (
        s.removal_stream is not None and s.removal_stream in seen_prev
    )
    require(not stale_profile_detected, "body_removal_stream")

    expected_chain = []
    hops = []
    if s.removal_stream is not None and first_entry.get("stream") == s.removal_stream:
        visited = {first_entry["stream"]}
        cur_open = first_entry
        while isinstance(cur_open, dict) and "recovery" in cur_open:
            rec = cur_open["recovery"]
            prev_name = rec["previous_stream"]
            require(prev_name not in visited, "body_removal_stream")
            visited.add(prev_name)
            expected_chain.append(prev_name)
            if (
                not isinstance(quarantined_map, dict)
                or prev_name not in quarantined_map
            ):
                require(quarantined_list == expected_chain, "body_removal_stream")
                require(False, "body_removal_stream")
            raw_img = quarantined_map[prev_name]
            hops.append((cur_open, prev_name, raw_img))
            next_open = None
            if isinstance(raw_img, str):
                first_ln = raw_img.splitlines(keepends=True)[:1]
                if first_ln and first_ln[0].endswith("\n") and "\r" not in first_ln[0]:
                    try:
                        parsed0 = legacy.parse_wire(
                            first_ln[0], "journal_entry", "q_hop"
                        )
                        if (
                            isinstance(parsed0, dict)
                            and parsed0.get("type") == "stream_opened"
                            and parsed0.get("stream") == prev_name
                        ):
                            next_open = parsed0
                    except legacy.Reject:
                        next_open = None
            cur_open = next_open
        require(quarantined_list == expected_chain, "body_removal_stream")
    elif len(quarantined_list) > 0 and "quarantined_streams" in case:
        # Non-designated stream evaluated with an explicit store quarantined map:
        pass
    s.expected_quarantined_chain = list(expected_chain)

    if not hops:
        return

    bodies: dict[tuple[str, int], tuple[int, str | None]] = {}
    open_starts: dict[tuple[str, int], set[int]] = {}
    expected_store = case.get("expected_store")
    hop_contexts = case.get("recovery_contexts", {})

    for succ_open, prev_name, raw_img in reversed(hops):
        rec = succ_open["recovery"]
        if expected_store is not None:
            require(rec["store"] == expected_store, "body_removal_stream")
        (
            longest_prefix,
            is_damaged,
            img_digest,
            longest_bodies,
            longest_open,
        ) = _scan_quarantined_designated_image(
            raw_img, prev_name, bodies, open_starts, s.window
        )
        hctx = hop_contexts.get(succ_open["stream"], {})
        ledger_map = {
            (b["body"]["digest"], int(b["body"]["length"])): int(b["floor"])
            for b in hctx.get("ledger_floors", [])
        }
        live_obs_map = {
            (b["body"]["digest"], int(b["body"]["length"])): b["observation"]
            for b in hctx.get("live_activation_observations", [])
        }
        items = rec["reconciled_bodies"]
        jcs_bodies = [legacy._jcs.jcs_dumps(it["body"]) for it in items]
        item_keys = [(it["body"]["digest"], int(it["body"]["length"])) for it in items]
        required_keys = set(longest_bodies.keys()) | set(ledger_map.keys())

        def rec_r1():
            require(len(longest_prefix) >= 1, "recovery_incomplete")
            require(is_damaged, "recovery_incomplete")
            require(
                rec["quarantined_image_digest"] == img_digest, "recovery_incomplete"
            )
            require(rec["cut_seq"] == len(longest_prefix), "recovery_incomplete")
            require(
                rec["cut_digest"] == digest(longest_prefix[: int(rec["cut_seq"])]),
                "recovery_incomplete",
            )
            require(
                jcs_bodies == sorted(jcs_bodies)
                and len(item_keys) == len(set(item_keys)),
                "recovery_incomplete",
            )
            require(required_keys - set(item_keys) == set(), "recovery_incomplete")
            for it in items:
                k = (it["body"]["digest"], int(it["body"]["length"]))
                prior_bound, prior_phase = longest_bodies.get(k, (0, None))
                fl = int(it["floor"])
                require(fl >= prior_bound, "recovery_incomplete")
                if k in ledger_map:
                    require(fl == ledger_map[k], "recovery_incomplete")
                is_ref = k in s.referenced_bodies
                expected_state = compute_recovery_row(
                    prior_phase,
                    prior_bound,
                    fl,
                    it["activation_observation"],
                    True,
                    is_ref,
                )
                require(
                    it["post_recovery_state"] == expected_state, "recovery_incomplete"
                )

        def rec_r2():
            for it in items:
                require(int(it["floor"]) < MAX_SAFE_INTEGER, "numeric_domain")

        def rec_r3():
            for it in items:
                k = (it["body"]["digest"], int(it["body"]["length"]))
                live_obs = live_obs_map.get(k, it["activation_observation"])
                require(live_obs != "present_corrupt", "body_observation")

        def rec_r4_r19():
            for it in items:
                k = (it["body"]["digest"], int(it["body"]["length"]))
                live_obs = live_obs_map.get(k, it["activation_observation"])
                require(live_obs == it["activation_observation"], "recovery_incomplete")

        rec_precedence = [rec_r1, rec_r2, rec_r3, rec_r4_r19]
        for r_fn in rec_precedence:
            r_fn()

        next_bodies = dict(longest_bodies)
        next_open = copy.deepcopy(longest_open)
        for it in items:
            k = (it["body"]["digest"], int(it["body"]["length"]))
            prior_bound, prior_phase = longest_bodies.get(k, (0, None))
            fl = int(it["floor"])
            new_bp, new_os = _apply_recovery_transition(
                prior_phase,
                prior_bound,
                longest_open.get(k, set()),
                fl,
                it["activation_observation"],
                it["post_recovery_state"],
            )
            next_bodies[k] = new_bp
            next_open[k] = new_os
        bodies = next_bodies
        open_starts = next_open

    s.bodies = bodies
    s.open_starts = open_starts


def _check_sealed_intermediate_journal(journal, anc_seal, window_ms, raw_entries=None):
    family = journal[0]["v"]
    require(family in CAPABILITY, "continuation")
    schema = BASE.parent / f"v{family}" / "journal-entry.schema.json"
    for idx, e in enumerate(journal):
        ok_shape = (
            shape_raw(raw_entries[idx], schema)
            if raw_entries is not None
            else shape(e, schema)
        )
        require(e.get("v") == family and ok_shape, "continuation")
    try:
        legacy.check_journal(journal, "source")
    except legacy.Reject:
        require(False, "continuation")
    continues = journal[0].get("continues")
    require(continues is not None, "continuation")
    if anc_seal is not None:
        require(
            continues["capability"] == anc_seal["capability"]
            and continues["stream"] == anc_seal["stream"]
            and int(continues["cut_seq"]) <= int(anc_seal["cut_seq"]),
            "continuation",
        )
        if int(continues["cut_seq"]) == int(anc_seal["cut_seq"]):
            require(continues["cut_digest"] == anc_seal["cut_digest"], "continuation")
    if family == 0:
        return True
    st = State(window_ms)
    latest_rev = None
    imports_seq = []
    restricted_seq = []
    for idx, e in enumerate(journal):
        try:
            st.instant(e["at"])
            if idx > 0:
                require(
                    st.instant(e["at"]) >= st.instant(journal[idx - 1]["at"]),
                    "continuation",
                )
            if "record" in e:
                require(e["record"]["v"] == 1, "continuation")
                if "produced_at" in e["record"]:
                    st.instant(e["record"]["produced_at"]["instant"])
            if "request" in e:
                st.instant(e["request"]["observed_at"])
        except legacy.Reject:
            require(False, "continuation")
        typ = e["type"]
        if typ == "stream_opened":
            st.stream, st.failure_model = e["stream"], e["failure_model"]
            st.continues = copy.deepcopy(continues)
        elif typ in ("appended", "revised"):
            require(e["record"]["record_id"] not in st.retired, "continuation")
            if "imported_from" in e:
                imp = e["imported_from"]
                require(
                    imp["capability"] == continues["capability"]
                    and imp["pin"]["stream"] == continues["stream"],
                    "continuation",
                )
                imports_seq.append(e["seq"])
            st.records[pin_key(pin(e))] = e["record"]
        elif typ in ("tombstoned", "untombstoned"):
            r = e["request"]
            require(e["request_digest"] == digest(r), "continuation")
            require(
                typ == ("tombstoned" if r["action"] == "tombstone" else "untombstoned"),
                "continuation",
            )
            if st.refusal(r, e["at"]) is not None:
                require(False, "continuation")
            st.apply(r, e["seq"])
            restricted_seq.append(e["seq"])
        elif typ == "retired":
            require(any(k[1] == e["record_id"] for k in st.records), "continuation")
            if "idempotency_key" in e:
                r = retirement_request(e)
                require(e["request_digest"] == digest(r), "continuation")
            st.retired.add(e["record_id"])
            restricted_seq.append(e["seq"])
        elif typ == "restrictions_reviewed":
            src = e["source"]
            require(
                src["capability"] == continues["capability"]
                and src["stream"] == continues["stream"]
                and e["group"] == e["seq"]
                and e["group_size"] == 1
                and e["destination_cut_seq"] == e["seq"] - 1
                and int(src["cut_seq"]) >= int(continues["cut_seq"])
                and applied_manifest(journal[:idx])
                == e["destination_applied_state_digest"],
                "continuation",
            )
            if anc_seal is not None:
                require(
                    int(src["cut_seq"]) == int(anc_seal["cut_seq"])
                    and src["cut_digest"] == anc_seal["cut_digest"]
                    and e["source_state_digest"]
                    == anc_seal["effective_restriction_manifest_digest"],
                    "continuation",
                )
            latest_rev = e
    if latest_rev is None:
        return False
    after = latest_rev["seq"]
    if any(sq > after for sq in imports_seq) or any(
        sq > after for sq in restricted_seq
    ):
        return False
    return True


def _seal_establishes_finality(seal_ctx, ancestors_final):
    if seal_ctx.get("closer_authenticated") is not True:
        return False
    if seal_ctx.get("source_write_fenced") is not True:
        return False
    if not ancestors_final:
        return False
    return True


def _verify_seal_against_journal(inst_checker, seal_ctx, source_journal):
    seal = seal_ctx["seal"]
    require(seal["v"] == 1, "continuation")
    require(seal["store"] == seal_ctx["expected_store"], "continuation")
    require(seal["trust_profile"] == seal_ctx["accepted_trust_profile"], "continuation")
    require(
        seal["capability"] == CAPABILITY.get(source_journal[0]["v"])
        and seal["stream"] == source_journal[0]["stream"],
        "continuation",
    )
    cut_seq = int(seal["cut_seq"])
    require(complete_cut(source_journal, cut_seq), "continuation")
    prefix = source_journal[:cut_seq]
    require(digest(prefix) == seal["cut_digest"], "continuation")
    trailing_bytes = seal_ctx.get("trailing_bytes", "")
    require(isinstance(trailing_bytes, str) and trailing_bytes == "", "continuation")
    if seal_ctx.get("source_write_fenced") is True:
        require(len(source_journal) == cut_seq, "continuation")
    require(
        source_manifest(prefix) == seal["effective_restriction_manifest_digest"],
        "continuation",
    )
    jcs_ancestors = [legacy._jcs.jcs_dumps(m) for m in seal["ancestor_seals"]]
    require(jcs_ancestors == sorted(jcs_ancestors), "continuation")
    anc_stream_keys = [(m["capability"], m["stream"]) for m in seal["ancestor_seals"]]
    require(len(anc_stream_keys) == len(set(anc_stream_keys)), "continuation")
    return prefix


def _verify_manifest_preimage(m_doc, expected_digest):
    require(
        isinstance(m_doc, dict)
        and set(m_doc) == {"holds", "retired", "inherited"}
        and all(isinstance(m_doc[k], list) for k in ("holds", "retired", "inherited")),
        "continuation",
    )
    require(digest(m_doc) == expected_digest, "continuation")


def _evaluate_seal_context(
    inst_checker,
    seal_ctx,
    source_journal,
    window_ms,
    *,
    raw_entries=None,
    observed_upstream=None,
    check_top_seal=True,
):
    if check_top_seal:
        require(
            isinstance(seal_ctx, dict)
            and {"seal", "expected_store", "accepted_trust_profile"}
            <= set(seal_ctx)
            <= ALLOWED_SEAL_CTX_KEYS,
            "continuation",
        )
        seal = seal_ctx["seal"]
        legacy.check_typed_domain(seal, "stream_seal", "source_seal")
        inst_checker.instant(seal.get("sealed_at") if isinstance(seal, dict) else None)
        ok_shape = (
            shape_raw(seal_ctx["raw_seal"], "stream-seal")
            if "raw_seal" in seal_ctx
            else shape(seal, "stream-seal")
        )
        require(ok_shape, "continuation")
    else:
        seal = seal_ctx["seal"]

    if "manifest" in seal_ctx:
        _verify_manifest_preimage(
            seal_ctx["manifest"], seal["effective_restriction_manifest_digest"]
        )

    is_cont = source_journal[0].get("continues") is not None
    seal_key = (seal["capability"], seal["stream"])
    anc_ctxs = seal_ctx.get("ancestor_seal_contexts", [])
    ctx_by_key = {}
    anc_journals_by_key = {}
    for ac in anc_ctxs:
        require(
            isinstance(ac, dict)
            and {"seal", "expected_store", "accepted_trust_profile"}
            <= set(ac)
            <= ALLOWED_SEAL_CTX_KEYS,
            "continuation",
        )
        ac_seal = ac["seal"]
        legacy.check_typed_domain(ac_seal, "stream_seal", "ancestor_seal")
        inst_checker.instant(
            ac_seal.get("sealed_at") if isinstance(ac_seal, dict) else None
        )
        ac_shape = (
            shape_raw(ac["raw_seal"], "stream-seal")
            if "raw_seal" in ac
            else shape(ac_seal, "stream-seal")
        )
        require(ac_shape, "continuation")
        require(ac_seal["v"] == 1, "continuation")
        require(ac_seal["store"] == ac["expected_store"], "continuation")
        require(
            ac_seal["trust_profile"] == ac["accepted_trust_profile"], "continuation"
        )
        ac_trailing = ac.get("trailing_bytes", "")
        require(isinstance(ac_trailing, str) and ac_trailing == "", "continuation")
        ac_jcs_anc = [legacy._jcs.jcs_dumps(m) for m in ac_seal["ancestor_seals"]]
        require(
            ac_jcs_anc == sorted(ac_jcs_anc)
            and len(ac_jcs_anc) == len(set(ac_jcs_anc)),
            "continuation",
        )
        ac_key = (ac_seal["capability"], ac_seal["stream"])
        require(ac_key != seal_key and ac_key not in ctx_by_key, "continuation")
        ac_sub_keys = [
            (m["capability"], m["stream"]) for m in ac_seal["ancestor_seals"]
        ]
        require(
            ac_key not in ac_sub_keys and len(ac_sub_keys) == len(set(ac_sub_keys)),
            "continuation",
        )
        if "manifest" in ac:
            _verify_manifest_preimage(
                ac["manifest"], ac_seal["effective_restriction_manifest_digest"]
            )
        if "journal" in ac or "raw_journal" in ac:
            aj, araw = _parse_source_spec(
                ac["raw_journal"] if "raw_journal" in ac else ac["journal"]
            )
            require(
                isinstance(aj, list)
                and aj
                and all(isinstance(e, dict) for e in aj)
                and "v" in aj[0],
                "continuation",
            )
            anc_journals_by_key[ac_key] = (ac, aj, araw)
        ctx_by_key[ac_key] = ac

    anc_journal_uncurrent = set()
    anc_journal_upstreams = {}
    for ac_key, (ac, aj, araw) in anc_journals_by_key.items():
        ac_seal = ac["seal"]
        if aj[0].get("continues") is None:
            observed(aj, window_ms, None, None, raw_entries=araw)
            _verify_seal_against_journal(inst_checker, ac, aj)
            require(ac_seal["ancestor_seals"] == [], "continuation")
        else:
            aj_prefix = _verify_seal_against_journal(inst_checker, ac, aj)
            _ah, _ar, aj_rev = restriction_state(aj_prefix)
            aj_up_key = (
                (aj_rev["source"]["capability"], aj_rev["source"]["stream"])
                if aj_rev is not None
                else (
                    aj_prefix[0]["continues"]["capability"],
                    aj_prefix[0]["continues"]["stream"],
                )
            )
            anc_journal_upstreams[ac_key] = aj_up_key
            up_ctx = ctx_by_key.get(aj_up_key)
            aj_cur = _check_sealed_intermediate_journal(
                aj,
                up_ctx["seal"] if up_ctx is not None else None,
                window_ms,
                raw_entries=araw,
            )
            if not aj_cur or all(
                (m["capability"], m["stream"]) != aj_up_key
                for m in ac_seal["ancestor_seals"]
            ):
                anc_journal_uncurrent.add(ac_key)

    if not is_cont:
        if observed_upstream is None:
            observed(source_journal, window_ms, None, None, raw_entries=raw_entries)
        prefix = _verify_seal_against_journal(inst_checker, seal_ctx, source_journal)
        require(seal["ancestor_seals"] == [] and not anc_ctxs, "continuation")
        is_final = _seal_establishes_finality(seal_ctx, True)
        return is_final, False, prefix

    # Sealed intermediate continuation source.
    prefix = _verify_seal_against_journal(inst_checker, seal_ctx, source_journal)
    top_anc_by_key = {}
    for entry in seal["ancestor_seals"]:
        key = (entry["capability"], entry["stream"])
        require(key != seal_key, "continuation")
        top_anc_by_key[key] = entry
        if key in ctx_by_key:
            ac = ctx_by_key[key]
            ac_seal = ac["seal"]
            require(
                digest(ac_seal) == entry["seal_digest"]
                and ac_seal["store"] == entry["store"]
                and int(ac_seal["cut_seq"]) == int(entry["cut_seq"]),
                "continuation",
            )

    _h, _r, mid_rev = restriction_state(prefix)
    needed_key = (
        (mid_rev["source"]["capability"], mid_rev["source"]["stream"])
        if mid_rev is not None
        else (
            prefix[0]["continues"]["capability"],
            prefix[0]["continues"]["stream"],
        )
    )
    matching_ctx = ctx_by_key.get(needed_key)
    mid_current = _check_sealed_intermediate_journal(
        source_journal,
        matching_ctx["seal"] if matching_ctx is not None else None,
        window_ms,
        raw_entries=raw_entries,
    )

    ancestors_final = True
    unobserved_anc_hold = False
    reachable_keys = set()
    canonical_edge_by_key = {}

    def _bind_edge(key, edge):
        if key in canonical_edge_by_key:
            require(canonical_edge_by_key[key] == edge, "continuation")
        else:
            canonical_edge_by_key[key] = edge

    def walk_ancestor(key, expected_entry, visiting):
        nonlocal ancestors_final, unobserved_anc_hold
        require(key not in visiting, "continuation")
        if expected_entry is not None:
            _bind_edge(
                key,
                (
                    expected_entry["store"],
                    expected_entry["capability"],
                    expected_entry["stream"],
                    int(expected_entry["cut_seq"]),
                    expected_entry["seal_digest"],
                ),
            )
        reachable_keys.add(key)
        if key in top_anc_by_key:
            top_e = top_anc_by_key[key]
            _bind_edge(
                key,
                (
                    top_e["store"],
                    top_e["capability"],
                    top_e["stream"],
                    int(top_e["cut_seq"]),
                    top_e["seal_digest"],
                ),
            )
        if key not in top_anc_by_key:
            ancestors_final = False
            unobserved_anc_hold = True
        ac = ctx_by_key.get(key)
        if ac is None:
            ancestors_final = False
            unobserved_anc_hold = True
            return
        ac_seal = ac["seal"]
        _bind_edge(
            key,
            (
                ac_seal["store"],
                ac_seal["capability"],
                ac_seal["stream"],
                int(ac_seal["cut_seq"]),
                digest(ac_seal),
            ),
        )
        if (
            ac.get("closer_authenticated") is not True
            or ac.get("source_write_fenced") is not True
            or key in anc_journal_uncurrent
        ):
            ancestors_final = False
        m_digest = ac_seal["effective_restriction_manifest_digest"]
        if "manifest" in ac:
            if len(ac["manifest"].get("holds", [])) > 0:
                unobserved_anc_hold = True
        elif m_digest != EMPTY_MANIFEST_DIGEST:
            unobserved_anc_hold = True
        next_visiting = visiting | {key}
        sub_keys_present = set()
        for sub_e in ac_seal["ancestor_seals"]:
            sub_k = (sub_e["capability"], sub_e["stream"])
            sub_keys_present.add(sub_k)
            walk_ancestor(sub_k, sub_e, next_visiting)
        if (
            key in anc_journal_upstreams
            and anc_journal_upstreams[key] not in sub_keys_present
        ):
            walk_ancestor(anc_journal_upstreams[key], None, next_visiting)

    walk_ancestor(needed_key, None, {seal_key})
    require(set(top_anc_by_key.keys()) <= reachable_keys, "continuation")

    is_final = _seal_establishes_finality(seal_ctx, ancestors_final)
    if not mid_current and is_final:
        # Sealed cut is authentic/final, but intermediate review itself was not current.
        return "uncurrent_final", unobserved_anc_hold, prefix
    return is_final, unobserved_anc_hold, prefix


def _apply_case_source_seal(case, s):
    seal_ctx = case["source_seal"]
    raw_entries = case.get("raw_source_journal")
    is_final, anc_hold, prefix = _evaluate_seal_context(
        s,
        seal_ctx,
        s.source,
        case["window_ms"],
        raw_entries=raw_entries,
        observed_upstream=s.upstream,
    )
    if is_final == "uncurrent_final":
        s.source = copy.deepcopy(prefix)
        s.upstream = UNCURRENT_SEALED
        s.source_seal_final = False
        s.sealed_ancestor_hold = anc_hold
    elif is_final is True:
        s.upstream = None
        s.source_seal_final = True
        s.sealed_ancestor_hold = anc_hold
    else:
        s.source_seal_final = False
        if seal_ctx.get("live_source_unobservable") is True:
            s.upstream = UNOBSERVED


def _parse_raw_receipt_step(raw_text, where):
    ast_node = legacy.parse_raw_json(raw_text, where)
    kind = (
        "write_receipt"
        if isinstance(ast_node, dict)
        and ast_node.get("kind") in ("receipt", "reconciliation")
        else "protection_receipt"
    )
    return legacy._preflight_node(ast_node, kind, where)


def materialize_destination_wire(case):
    """Run Stage 1 (ingest) + Stage 2 (numeric_domain) on destination raw_* fields."""
    out = copy.deepcopy(case)
    if "raw_profile" in out:
        out["profile"] = legacy.parse_wire(
            out["raw_profile"], "stratum_profile", "raw_profile"
        )
    if "raw_journal_ndjson" in out:
        out["journal"] = legacy.parse_wire_lines(
            out["raw_journal_ndjson"], "raw_journal_ndjson", "journal_entry"
        )
    elif "raw_journal" in out:
        out["journal"] = [
            legacy.parse_wire(raw, "journal_entry", f"raw_journal[{i}]")
            for i, raw in enumerate(out["raw_journal"])
        ]
    steps = []
    for idx, step in enumerate(out["steps"]):
        s = copy.deepcopy(step)
        if "raw_request" in s:
            s["request"] = legacy.parse_wire(
                s["raw_request"], "protection_request", f"steps[{idx}].raw_request"
            )
        elif "raw_receipt" in s:
            s["receipt"] = _parse_raw_receipt_step(
                s["raw_receipt"], f"steps[{idx}].raw_receipt"
            )
        elif "raw_entry" in s:
            s["entry"] = legacy.parse_wire(
                s["raw_entry"], "journal_entry", f"steps[{idx}].raw_entry"
            )
        elif "raw_body_state" in s:
            s["body_state"] = legacy.parse_wire(
                s["raw_body_state"],
                "body_state_request",
                f"steps[{idx}].raw_body_state",
            )
        elif "raw_removal_eligibility" in s:
            s["removal_eligibility"] = legacy.parse_wire(
                s["raw_removal_eligibility"],
                "removal_eligibility",
                f"steps[{idx}].raw_removal_eligibility",
            )
        elif "raw_verify_seal" in s:
            s["verify_seal"] = legacy.parse_wire(
                s["raw_verify_seal"],
                "seal_context",
                f"steps[{idx}].raw_verify_seal",
            )
        elif "raw_use" in s:
            s["use"] = legacy.parse_wire(
                s["raw_use"], "export_request", f"steps[{idx}].raw_use"
            )
        steps.append(s)
    out["steps"] = steps
    return out


def evaluate(case):
    case = materialize_destination_wire(case)
    s = State(case["window_ms"])
    if "profile" in case:
        legacy.check_profile(case["profile"], "profile")
        rt = case["profile"]["runtime"]
        s.removal_stream = rt.get("body_removal_stream")
        s.incarnation_allocation = rt.get("incarnation_allocation")
        s.quarantined_designated_streams = list(
            rt.get("quarantined_designated_streams", [])
        )
    if "sharing_streams" in case:
        for sj in case["sharing_streams"]:
            legacy.check_journal(sj, "v1", check_entry_at=False)
            sh_st = State(case["window_ms"])
            for se in sj:
                sh_st.ingest(se)
            for _pk, rec in sh_st.retention_revisions():
                if "body" in rec:
                    s.referenced_bodies.add(
                        (rec["body"]["digest"], int(rec["body"]["length"]))
                    )
    _validate_and_replay_recovery_chain(case, s)
    if "source_journal" in case or "raw_source_journal" in case:
        s.source, s.upstream = chain(case)
        if "source_seal" in case:
            _apply_case_source_seal(case, s)
    legacy.check_journal(case["journal"], "v1", check_entry_at=False)
    for e in case["journal"]:
        s.ingest(e)
    results = []
    for step in case["steps"]:
        if "request" in step:
            results.append(s.request(step["request"], step["at"]))
        elif "receipt" in step:
            results.append(s.verify_receipt(step["receipt"]))
        elif "retire" in step:
            results.append(s.retire(step["retire"], step["at"]))
        elif "reconcile" in step:
            results.append(s.reconcile(**step["reconcile"]))
        elif "entry" in step:
            s.ingest(step["entry"])
            results.append({"outcome": "journaled"})
        elif "body_state" in step:
            results.append(s.body_state(step["body_state"]))
        elif "removal_eligibility" in step:
            results.append(s.removal_eligibility(step["removal_eligibility"]))
        elif "verify_seal" in step:
            results.append(s.verify_seal_step(step["verify_seal"]))
        elif "source_entry" in step:
            # The source journal advances outside this stream.
            case = copy.deepcopy(case)
            case["source_journal"].append(copy.deepcopy(step["source_entry"]))
            s.source, s.upstream = chain(case)
            if "source_seal" in case:
                _apply_case_source_seal(case, s)
            results.append({"outcome": "journaled"})
        elif "raw_source_entry" in step:
            case = copy.deepcopy(case)
            case["raw_source_journal"].append(step["raw_source_entry"])
            s.source, s.upstream = chain(case)
            if "source_seal" in case:
                _apply_case_source_seal(case, s)
            results.append({"outcome": "journaled"})
        elif "ancestor_entry" in step:
            # An ancestor journal advances outside this stream and its source.
            case = copy.deepcopy(case)
            case["ancestors"][step["ancestor"]].append(
                copy.deepcopy(step["ancestor_entry"])
            )
            s.source, s.upstream = chain(case)
            results.append({"outcome": "journaled"})
        elif "raw_ancestor_entry" in step:
            case = copy.deepcopy(case)
            case["raw_ancestors"][step["ancestor"]].append(step["raw_ancestor_entry"])
            s.source, s.upstream = chain(case)
            results.append({"outcome": "journaled"})
        else:
            results.append(s.use(step["use"]))
    legacy.check_journal(s.journal, "v1", check_entry_at=False)
    return {
        "results": results,
        "active_holds": sorted(s.active),
        "journal_size": len(s.journal),
    }


SHAPE_CACHE: dict[tuple[str, str], bool] = {}
SUITE_CALLS = 0


def _norm_raw(raw_json: str) -> str:
    return raw_json if raw_json.endswith("\n") else raw_json + "\n"


def shape_raw(raw_json, name):
    global SUITE_CALLS
    schema_rel = legacy._normalize_schema_rel(name, default_ver="v1")
    payload = _norm_raw(raw_json)
    key = (schema_rel, payload)
    if key not in SHAPE_CACHE:
        SUITE_CALLS += 1
        verdicts = legacy.shape_suite(
            [("single", schema_rel, payload)], default_ver="v1"
        )
        SHAPE_CACHE[key] = verdicts["single"]
    return SHAPE_CACHE[key]


def shape(doc, name):
    return shape_raw(json.dumps(doc), name)


def check_case(case):
    try:
        return evaluate(case)
    except legacy.Reject as e:
        return {"refusal": e.reason}


def check_reason_table(rows, emitted, covered):
    assert len(rows) == len(set(rows)), "reason_table_drift: duplicate row"
    assert set(rows) == emitted == covered & emitted, (
        "reason_table_drift",
        set(rows) - emitted,
        emitted - set(rows),
        emitted - covered,
    )


def envelope_matches(original, amended):
    # A0: the v1 record schema equals the v0 record schema after changing v
    # from const 0 to const 1 and adding only the optional produced_at field
    # (and the instant definition it references). The latter owns the explicit
    # seconds constraint that rejects leap seconds at Stage 3. Nothing else may
    # differ.
    def structural(document):
        return {
            key: value
            for key, value in document.items()
            if key not in ("$id", "description")
        }

    expected = structural(copy.deepcopy(original))
    candidate = structural(copy.deepcopy(amended))
    try:
        record = candidate["$defs"]["record"]
        produced = record["properties"].pop("produced_at")
        instant_def = candidate["$defs"].pop("instant")
    except KeyError:
        return False
    if "produced_at" in record.get("required", []):
        return False
    if record["properties"].get("v") != {"const": 1}:
        return False
    record["properties"]["v"] = {"const": 0}
    if produced.get("properties", {}).get("instant") != {"$ref": "#/$defs/instant"}:
        return False
    expected_instant_pattern = (
        "^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-5][0-9]\\.[0-9]{6}Z$"
    )
    if instant_def.get("pattern") != expected_instant_pattern:
        return False
    return expected == candidate


def reason_controls(cases):
    source = (
        Path(__file__).read_text(encoding="utf-8").split("def reason_controls(", 1)[0]
    )
    tree = ast.parse(source)
    emitted = set()
    for node in ast.walk(tree):
        if (
            isinstance(node, ast.Assign)
            and any(isinstance(t, ast.Name) and t.id == "reason" for t in node.targets)
            and isinstance(node.value, ast.Constant)
            and isinstance(node.value.value, str)
        ):
            emitted.add(node.value.value)
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
            index = (
                1
                if node.func.id == "require"
                else 0
                if node.func.id in ("refused", "removal_refused")
                else None
            )
            if index is not None and len(node.args) > index:
                arg = node.args[index]
                if isinstance(arg, ast.Constant) and isinstance(arg.value, str):
                    emitted.add(arg.value)
        if isinstance(node, ast.FunctionDef) and node.name == "refusal":
            emitted.update(
                n.value.value
                for n in ast.walk(node)
                if isinstance(n, ast.Return)
                and isinstance(n.value, ast.Constant)
                and isinstance(n.value.value, str)
            )
    standard = (ROOT / "docs/standards/durable-record-v1-contract.md").read_text(
        encoding="utf-8"
    )
    section = standard.split("## Refusal vocabulary", 1)[1].split("## Conformance", 1)[
        0
    ]
    rows = re.findall(r"^\| `([^`]+)`\s+\|", section, re.M)
    covered = set()
    for path in cases:
        expected = load(path)["expected"]
        if "refusal" in expected:
            covered.add(expected["refusal"])
        for result in expected.get("results", []):
            covered.add(result.get("reason", result.get("outcome")))
    check_reason_table(rows, emitted, covered)
    for changed_rows, changed_emitted, changed_covered in (
        (rows[1:], emitted, covered),
        (rows, emitted | {"undocumented"}, covered),
        (rows, emitted, covered - {"receipt_mismatch"}),
        (rows + rows[:1], emitted, covered),
    ):
        try:
            check_reason_table(changed_rows, changed_emitted, changed_covered)
        except AssertionError as error:
            assert "reason_table_drift" in str(error)
        else:
            raise AssertionError("reason_table_drift control did not fire")


def mutation_controls():
    # Fixed AST edits. No fixture-supplied code; a mutation must match exactly
    # one selected guard and yield acceptance or a precisely named alternative.
    source = (
        Path(__file__).read_text(encoding="utf-8").split("def reason_controls(", 1)[0]
    )
    specs = [
        ("ingest", "slide_dedup", "", 0, "dedup-non-sliding", None),
        ("use", "if_reason", "invalid_purpose", 0, "unknown-purpose", None),
        ("ingest", "require", "retired_record", 0, "write-retired-record", None),
        ("ingest", "require", "accepted_key_reuse", 0, "content-live-key-reuse", None),
        ("ingest", "require", "accepted_key_reuse", 1, "journal-live-key-reuse", None),
        ("ingest", "require", "journal_time", 0, "journal-backwards-time", None),
        ("ingest", "require", "journal_version", 0, "v0-journal", None),
        ("ingest", "require", "record_version", 0, "record-envelope-v0", None),
        ("instant", "require", "instant_spelling", 0, "instant-journal-at", None),
        ("instant", "require", "instant_spelling", 1, "instant-year-zero", None),
        (
            "instant",
            "require",
            "instant_spelling",
            1,
            "produced-at-impossible-date",
            None,
        ),
        (
            "ingest",
            "require",
            "request_digest",
            1,
            "retire-journal-digest-mismatch",
            None,
        ),
        ("ingest", "require", "operation_type", 0, "journal-operation-type", None),
        ("ingest", "require", "request_digest", 0, "journal-digest-mismatch", None),
        ("ingest", "require", "invalid_deduplication", 0, "dedup-protection", None),
        ("ingest", "require", "invalid_deduplication", 0, "dedup-absent", None),
        ("State", "require", "idempotency_window", 0, "zero-window", None),
        ("State", "require", "numeric_domain", 0, "window-above-safe-max", None),
        (
            "refusal",
            "compare",
            "r['stream'] != self.stream",
            0,
            "foreign-request",
            None,
        ),
        ("use", "compare", "r['stream'] != self.stream", 0, "foreign-export", None),
        (
            "request",
            "compare_true",
            "r['stream'] == self.stream",
            0,
            "foreign-replay-key",
            ("outcome", "idempotency_conflict", 1),
        ),
        (
            "blocked",
            "body_length",
            "",
            0,
            "body-length-isolation",
            ("reason", "protected_use", 1),
        ),
        ("refusal", "if_reason", "body_mismatch", 0, "wrong-body", None),
        ("refusal", "if_reason", "observation_order", 0, "future-observation", None),
        (
            "refusal",
            "if_reason",
            "observation_order",
            1,
            "restore-old-observation",
            None,
        ),
        ("refusal", "if_reason", "hold_mismatch", 0, "restore-wrong-scope", None),
        (
            "use",
            "compare",
            "r['protection_cut'] != len(self.journal)",
            0,
            "stale-cut",
            None,
        ),
        (
            "use",
            "compare",
            "r['current_cut'] != len(self.journal)",
            0,
            "wrong-current-cut",
            None,
        ),
        (
            "use",
            "compare",
            "r['protection_state'] != 'verified'",
            0,
            "unavailable",
            None,
        ),
        ("use", "if_reason", "export_selection", 0, "incomplete-selection", None),
        ("use", "if_reason", "protected_use", 0, "inherited-protection", None),
        ("use", "if_reason", "protected_use", 1, "restore-does-not-unretire", None),
        ("use", "if_reason", "body_closure", 3, "full-content-missing", None),
        ("request", "replay", "", 0, "replay-after-restore", ("seq", 5, 2)),
        ("apply", "restore_all", "", 0, "independent-holds", ("outcome", "allowed", 3)),
        ("blocked", "body_match", "", 0, "shared-body", None),
        (
            "verify_receipt",
            "require",
            "receipt_mismatch",
            5,
            "receipt-wrong-digest",
            None,
        ),
        (
            "source_cut",
            "require",
            "continuation",
            1,
            "continuation-wrong-capability",
            None,
        ),
        (
            "source_cut",
            "require",
            "continuation",
            3,
            "continuation-incomplete-cut",
            None,
        ),
        ("source_cut", "require", "continuation", 4, "review-wrong-cut-digest", None),
        ("import_record", "require", "continuation", 1, "import-other-source", None),
        ("observed", "require", "continuation", 1, "source-record-extra-field", None),
        ("observed", "require", "continuation", 2, "source-invalid-restore", None),
        (
            "observed",
            "require",
            "continuation",
            2,
            "source-v0-invalid-timestamp-beyond-cut",
            None,
        ),
        (
            "observed",
            "require",
            "continuation",
            2,
            "ancestor-v0-invalid-timestamp",
            None,
        ),
        ("check_review", "require", "continuation", 2, "review-not-singleton", None),
        (
            "check_review",
            "require",
            "continuation",
            2,
            "review-wrong-destination-cut",
            None,
        ),
        (
            "check_review",
            "require",
            "continuation",
            5,
            "review-wrong-source-state",
            None,
        ),
        (
            "check_review",
            "require",
            "continuation",
            6,
            "review-import-beyond-cut",
            None,
        ),
        (
            "check_review",
            "require",
            "continuation",
            7,
            "review-wrong-applied-state",
            None,
        ),
        (
            "review_pending",
            "compare",
            "'source_state_digest' in r",
            0,
            "source-state-on-plain-stream",
            None,
        ),
        (
            "review_pending",
            "compare",
            "r.get('source_state_digest') != source_manifest(self.source)",
            0,
            "continuation-source-state-mismatch",
            None,
        ),
        (
            "review_pending",
            "compare",
            "self.upstream is UNOBSERVED",
            0,
            "continuation-ancestor-unobserved",
            ("reason", "restrictions_pending", 0),
        ),
        (
            "use",
            "if_reason",
            "restrictions_pending",
            0,
            "continuation-unreviewed",
            None,
        ),
        (
            "review_current",
            "compare",
            "seq > after",
            0,
            "continuation-import-after-review",
            None,
        ),
        (
            "review_current",
            "compare",
            "seq > after",
            1,
            "continuation-destination-restriction-after-review",
            None,
        ),
        (
            "review_current",
            "compare",
            "source_manifest(self.source) != self.review['source_state_digest']",
            0,
            "continuation-source-changed",
            None,
        ),
        (
            "review_current",
            "compare_true",
            "self.upstream is None",
            0,
            "continuation-ancestor-changed",
            None,
        ),
        (
            "ingest",
            "require",
            "body_removal_stream",
            1,
            "body-entry-in-content-stream",
            None,
        ),
        (
            "ingest",
            "require",
            "body_removal_stream",
            0,
            "body-content-in-removal-stream",
            None,
        ),
        (
            "ingest",
            "require",
            "body_removal_stream",
            1,
            "body-entry-without-profile",
            None,
        ),
        (
            "track_body",
            "require",
            "body_lifecycle",
            0,
            "body-publish-not-increasing",
            None,
        ),
        ("track_body", "require", "body_lifecycle", 0, "body-publish-decreasing", None),
        ("track_body", "require", "body_lifecycle", 1, "body-start-unpublished", None),
        (
            "track_body",
            "require",
            "body_lifecycle",
            1,
            "body-start-wrong-incarnation",
            None,
        ),
        ("track_body", "require", "body_lifecycle", 1, "body-start-twice", None),
        (
            "track_body",
            "require",
            "body_lifecycle",
            1,
            "body-start-after-removed",
            None,
        ),
        (
            "track_body",
            "require",
            "body_lifecycle",
            2,
            "body-removed-without-start",
            None,
        ),
        ("track_body", "require", "body_lifecycle", 2, "body-removed-abandoned", None),
        ("track_body", "require", "body_lifecycle", 2, "body-removed-twice", None),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-null-present",
            None,
        ),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-string-present",
            None,
        ),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-boolean-length",
            None,
        ),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-fractional-length",
            None,
        ),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-extra-field",
            None,
        ),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-bad-digest",
            None,
        ),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-removal-started-null",
            None,
        ),
        (
            "body_state",
            "require",
            "body_observation",
            0,
            "body-observation-none-zero",
            None,
        ),
        (
            "body_state",
            "require",
            "body_removal_stream",
            0,
            "body-state-without-profile",
            None,
        ),
        (
            "body_state",
            "require",
            "body_removal_stream",
            0,
            "body-state-other-stream",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "body_removal_stream",
            0,
            "removal-wrong-designated-stream",
            ("reason", "body_lifecycle", 0),
        ),
        (
            "removal_eligibility",
            "if_reason",
            "still_referenced",
            0,
            "removal-active-reference",
            None,
        ),
        (
            "removal_eligibility",
            "invert_not_in",
            "rec['record_id'] not in st.retired",
            0,
            "removal-all-retired-single-stream",
            ("reason", "still_referenced", 0),
        ),
        (
            "retention_revisions",
            "latest_revision_only",
            "",
            0,
            "removal-superseded-active-revision",
            None,
        ),
        (
            "removal_eligibility",
            "key_by_first_pin",
            "",
            0,
            "removal-shared-retired-and-active",
            None,
        ),
        (
            "body_matches_target",
            "body_length",
            "",
            0,
            "removal-body-length-isolation",
            ("reason", "still_referenced", 0),
        ),
        (
            "removal_eligibility",
            "first_stream_only",
            "",
            0,
            "removal-multi-stream-second-active",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "body_lifecycle",
            0,
            "removal-step-extra-field",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            0,
            "removal-duplicate-store-stream",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            1,
            "removal-duplicate-observed-stream",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            2,
            "removal-missing-store-stream",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            3,
            "removal-damaged-sharing-stream",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            4,
            "removal-unobservable-continuation-source",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            5,
            "removal-same-store-source-mismatch",
            None,
        ),
        (
            "_replay_observed_sharing_stream",
            "compare",
            "'inherited_block' not in obs",
            0,
            "removal-missing-inherited-block",
            None,
        ),
        (
            "_replay_observed_sharing_stream",
            "compare",
            "type(obs.get('inherited_block', False)) is not bool",
            0,
            "removal-nonbool-inherited-block",
            None,
        ),
        (
            "_replay_observed_sharing_stream",
            "compare",
            "'inherited_block' in obs",
            0,
            "removal-plain-stream-with-inherited-block",
            None,
        ),
        (
            "removal_eligibility",
            "prefix_removal_stream",
            "",
            0,
            "removal-non-designated-bodies-stream",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "journal_time",
            0,
            "removal-decision-time-before-designated-cut",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "journal_time",
            1,
            "removal-decision-time-before-sharing-cut",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protected_use",
            0,
            "removal-retired-with-body-hold",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protected_use",
            1,
            "removal-inherited-block",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protected_use",
            2,
            "removal-source-active-record-hold-reviewed",
            None,
        ),
        (
            "removal_eligibility",
            "immediate_source_only",
            "",
            0,
            "removal-ancestor-active-hold-reviewed",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protected_use",
            3,
            "removal-continuation-review-pending",
            None,
        ),
        (
            "removal_eligibility",
            "reason_bypass",
            "",
            0,
            "removal-rights-restricted-still-referenced",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "removal_authorization",
            0,
            "removal-auth-missing",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "set(auth) != REQUIRED_AUTH_KEYS",
            0,
            "removal-auth-extra-field",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "auth_policy['id'] != step['expected_policy']['id']",
            0,
            "removal-auth-wrong-policy-id",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "auth_policy['version'] != step['expected_policy']['version']",
            0,
            "removal-auth-wrong-policy-version",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "auth['store'] != step['store']",
            0,
            "removal-auth-wrong-store",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "auth_body['digest'] != step['body']['digest']",
            0,
            "removal-auth-wrong-body-digest",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "int(auth_body['length']) != int(step['body']['length'])",
            0,
            "removal-auth-wrong-body-length",
            None,
        ),
        (
            "integral",
            "allow_bool_int",
            "",
            0,
            "removal-auth-bool-incarnation",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "int(auth['incarnation']) != int(step['incarnation'])",
            0,
            "removal-auth-wrong-incarnation",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "auth['action'] != 'remove_body'",
            0,
            "removal-auth-wrong-action",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "auth['scope_verified'] is not True",
            0,
            "removal-auth-unverified-scope",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "decision_ts < not_before_ts",
            0,
            "removal-auth-before-not-before",
            None,
        ),
        (
            "valid_removal_authorization",
            "compare",
            "decision_ts > expires_at_ts",
            0,
            "removal-auth-expired",
            None,
        ),
        (
            "removal_eligibility",
            "skip_retry_reverify",
            "",
            0,
            "removal-retry-new-active-ref",
            None,
        ),
        (
            "removable_incarnation_ok",
            "abandoned_incarnation_ok",
            "",
            0,
            "removal-retry-abandoned-by-republish",
            None,
        ),
        (
            "removal_eligibility",
            "swap_precedence",
            "0",
            0,
            "removal-precedence-p1-over-p2",
            ("reason", "protection_unavailable", 0),
        ),
        (
            "removal_eligibility",
            "swap_precedence",
            "1",
            0,
            "removal-precedence-p2-over-p2b",
            ("reason", "journal_time", 0),
        ),
        (
            "removal_eligibility",
            "swap_precedence",
            "2",
            0,
            "removal-precedence-p2b-over-p3",
            ("reason", "protected_use", 0),
        ),
        (
            "removal_eligibility",
            "swap_precedence",
            "3",
            0,
            "removal-precedence-p3-over-p4",
            ("reason", "still_referenced", 0),
        ),
        (
            "removal_eligibility",
            "swap_precedence",
            "4",
            0,
            "removal-precedence-p4-over-p5",
            ("reason", "removal_authorization", 0),
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "body_removal_stream",
            0,
            "recovery-header-on-content-stream",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "body_removal_stream",
            1,
            "recovery-missing-incarnation-allocation",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "body_removal_stream",
            1,
            "recovery-effect-dependent-allocation",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "body_removal_stream",
            2,
            "recovery-fork-predecessor",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "body_removal_stream",
            3,
            "recovery-stale-profile",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "body_removal_stream",
            7,
            "recovery-extra-quarantined-stream",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            6,
            "removal-quarantined-in-store-streams",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protection_unavailable",
            7,
            "removal-quarantined-extra-stream",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "recovery_incomplete",
            1,
            "recovery-undamaged-predecessor",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "recovery_incomplete",
            3,
            "recovery-shorter-than-longest-prefix",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "recovery_incomplete",
            6,
            "recovery-r1-missing-body-floor",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "recovery_incomplete",
            7,
            "recovery-r1-floor-below-prior-bound",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "ignore_hop_floor",
            "",
            0,
            "recovery-successor-publish-at-or-below-floor",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "ignore_hop_floor",
            "",
            0,
            "recovery-two-hop-s2-floor-below-s1",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "numeric_domain",
            0,
            "recovery-r2-floor-at-ceiling",
            None,
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "body_observation",
            0,
            "recovery-r3-present-corrupt",
            ("top_refusal", "recovery_incomplete"),
        ),
        (
            "_apply_recovery_transition",
            "raw_concat_recovery",
            "",
            0,
            "recovery-r12-then-byte-loss",
            ("state", "removal_pending", 0),
        ),
        (
            "_apply_recovery_transition",
            "raw_concat_recovery",
            "",
            0,
            "recovery-r12-complete-refused",
            None,
        ),
        (
            "_apply_recovery_transition",
            "leave_r13_open",
            "",
            0,
            "recovery-r13-complete-refused",
            None,
        ),
        (
            "compute_recovery_row",
            "referenced_unpublished_loss",
            "",
            0,
            "recovery-r4-ref-true",
            ("state", "integrity_loss", 0),
        ),
        (
            "compute_recovery_row",
            "row_return",
            "integrity_loss->unpublished",
            0,
            "recovery-r6-wrong-state-unpublished",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "publishing->available",
            1,
            "recovery-r7-wrong-state-available",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "publishing->available",
            2,
            "recovery-r9-wrong-state-available",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "integrity_loss->available",
            1,
            "recovery-r10-wrong-state-available",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "removal_pending->integrity_loss",
            1,
            "recovery-r15-wrong-state-integrity-loss",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "integrity_loss->removal_pending",
            2,
            "recovery-r16-wrong-state-removal-pending",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "removed->integrity_loss",
            0,
            "recovery-r17-wrong-state-integrity-loss",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "integrity_loss->removed",
            3,
            "recovery-r18-wrong-state-removed",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "publishing->available",
            4,
            "recovery-r19-wrong-state-available",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "integrity_loss->publishing",
            4,
            "recovery-r10-two-hop-wrong-publishing",
            None,
        ),
        (
            "compute_recovery_row",
            "row_return",
            "integrity_loss->removed",
            5,
            "recovery-r19-two-hop-wrong-removed",
            None,
        ),
        (
            "_apply_recovery_transition",
            "collapse_recovery_integrity_loss",
            "",
            0,
            "recovery-r10-sticky-reappear",
            ("state", "publishing", 0),
        ),
        (
            "_apply_recovery_transition",
            "collapse_r19_publishing_to_removed",
            "",
            0,
            "recovery-r19-subsequent-byte-loss",
            ("state", "removed", 0),
        ),
        (
            "_validate_and_replay_recovery_chain",
            "swap_rec_precedence",
            "0",
            0,
            "recovery-precedence-r1-over-r2",
            ("top_refusal", "numeric_domain", 0),
        ),
        (
            "_validate_and_replay_recovery_chain",
            "swap_rec_precedence",
            "0",
            0,
            "recovery-precedence-r1-state-over-r2",
            ("top_refusal", "numeric_domain", 0),
        ),
        (
            "_validate_and_replay_recovery_chain",
            "swap_rec_precedence",
            "1",
            0,
            "recovery-precedence-r2-over-r3",
            ("top_refusal", "body_observation", 0),
        ),
        (
            "_validate_and_replay_recovery_chain",
            "swap_rec_precedence",
            "2",
            0,
            "recovery-precedence-r3-over-table",
            ("top_refusal", "recovery_incomplete", 0),
        ),
        (
            "_validate_and_replay_recovery_chain",
            "require",
            "recovery_incomplete",
            10,
            "recovery-live-obs-mismatch",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            1,
            "seal-cross-store-replay",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            2,
            "seal-trust-profile-mismatch",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            4,
            "seal-cut-mid-group",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            4,
            "seal-cut-beyond-end",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            5,
            "seal-cut-digest-mismatch",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            6,
            "seal-trailing-damaged-bytes",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            7,
            "seal-trailing-entry-after-cut",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            8,
            "seal-manifest-digest-mismatch",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            9,
            "seal-ancestor-seals-unsorted",
            None,
        ),
        (
            "_verify_seal_against_journal",
            "require",
            "continuation",
            10,
            "seal-ancestor-seals-duplicate",
            None,
        ),
        (
            "_seal_establishes_finality",
            "if_false_return",
            "",
            0,
            "seal-closer-unauthenticated",
            None,
        ),
        (
            "_seal_establishes_finality",
            "if_false_return",
            "",
            1,
            "seal-consumer-pin-unfenced-unobservable",
            None,
        ),
        (
            "_seal_establishes_finality",
            "if_false_return",
            "",
            1,
            "seal-consumer-pin-unfenced-later-hold",
            None,
        ),
        (
            "_seal_establishes_finality",
            "if_false_return",
            "",
            2,
            "seal-ancestor-missing",
            None,
        ),
        (
            "_seal_establishes_finality",
            "if_false_return",
            "",
            2,
            "seal-ancestor-unfenced",
            None,
        ),
        (
            "instant",
            "require",
            "instant_spelling",
            1,
            "seal-instant-year-zero",
            None,
        ),
        (
            "removal_eligibility",
            "if_reason",
            "protected_use",
            4,
            "removal-sealed-ancestor-active-hold",
            None,
        ),
        (
            "_verify_manifest_preimage",
            "require",
            "continuation",
            0,
            "seal-ancestor-manifest-missing-holds-key",
            None,
        ),
        (
            "_verify_manifest_preimage",
            "require",
            "continuation",
            1,
            "seal-ancestor-manifest-digest-mismatch",
            None,
        ),
        (
            "_evaluate_seal_context",
            "skip_transitive_ancestor_seals",
            "",
            0,
            "seal-ancestor-transitive-omitted",
            None,
        ),
        (
            "_evaluate_seal_context",
            "round1_len2_ancestor_shortcut",
            "",
            0,
            "seal-ancestor-four-level-omitted-root",
            None,
        ),
        (
            "_evaluate_seal_context",
            "compare",
            "ac.get('closer_authenticated') is not True",
            0,
            "seal-ancestor-unauthenticated-closer",
            None,
        ),
        (
            "_evaluate_seal_context",
            "compare",
            "ac.get('source_write_fenced') is not True",
            0,
            "seal-ancestor-four-level-unfenced-root",
            None,
        ),
        (
            "_evaluate_seal_context",
            "compare",
            "key in anc_journal_uncurrent",
            0,
            "seal-ancestor-live-journal-uncurrent",
            None,
        ),
        (
            "_evaluate_seal_context",
            "allow_unrelated_ancestor_padding",
            "",
            0,
            "seal-ancestor-unrelated-padding",
            None,
        ),
    ]
    for method, kind, value, occurrence, witness, alternative in specs:
        tree = ast.parse(source)
        state = next(
            n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == "State"
        )
        # State methods first, then module-level functions.
        scope = (
            state
            if method == "State"
            else next(
                n
                for n in state.body + tree.body
                if isinstance(n, ast.FunctionDef) and n.name == method
            )
        )
        matches = []
        for n in ast.walk(scope):
            if (
                kind == "require"
                and isinstance(n, ast.Call)
                and isinstance(n.func, ast.Name)
                and n.func.id == "require"
                and isinstance(n.args[1], ast.Constant)
                and n.args[1].value == value
            ):
                matches.append(n)
            elif (
                kind in ("compare", "compare_true")
                and isinstance(n, ast.Compare)
                and ast.unparse(n) == value
            ):
                matches.append(n)
            elif kind == "if_reason" and isinstance(n, ast.If):
                # Only direct returns; do not accidentally select an outer branch.
                for child in n.body:
                    if isinstance(child, ast.Return) and (
                        isinstance(child.value, ast.Constant)
                        and child.value.value == value
                        or isinstance(child.value, ast.Call)
                        and isinstance(child.value.func, ast.Name)
                        and child.value.func.id in ("refused", "removal_refused")
                        and child.value.args[0].value == value
                    ):
                        matches.append(n)
            elif (
                kind == "invert_not_in"
                and isinstance(n, ast.Compare)
                and ast.unparse(n) == value
            ):
                matches.append(n)
            elif kind == "latest_revision_only" and n is scope:
                matches.append(n)
            elif (
                kind == "key_by_first_pin"
                and isinstance(n, ast.If)
                and ast.unparse(n.test)
                == "body_matches_target(rec.get('body'), target_body)"
            ):
                matches.append(n)
            elif (
                kind == "first_stream_only"
                and isinstance(n, ast.For)
                and ast.unparse(n.iter) == "expected_sharing"
            ):
                matches.append(n)
            elif (
                kind == "immediate_source_only"
                and isinstance(n, ast.For)
                and ast.unparse(n.iter) == "chain_journals"
            ):
                matches.append(n)
            elif (
                kind == "allow_bool_int"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "type(value) is int"
            ):
                matches.append(n)
            elif (
                kind == "prefix_removal_stream"
                and isinstance(n, ast.Compare)
                and ast.unparse(n) == "s != self.removal_stream"
            ):
                matches.append(n)
            elif (
                kind in ("reason_bypass", "skip_retry_reverify", "swap_precedence")
                and isinstance(n, ast.Assign)
                and len(n.targets) == 1
                and isinstance(n.targets[0], ast.Name)
                and n.targets[0].id == "precedence_order"
            ):
                matches.append(n)
            elif (
                kind == "abandoned_incarnation_ok"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "mode == 'retry_unlink'"
            ):
                matches.append(n)
            elif (
                kind == "slide_dedup"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "typ == 'deduplicated'"
            ):
                matches.append(n)
            elif (
                kind == "body_length"
                and isinstance(n, ast.Tuple)
                and ast.literal_eval(n) == ("digest", "length")
            ):
                matches.append(n)
            elif (
                kind == "body_match"
                and isinstance(n, ast.If)
                and "h['scope'] == 'body'" in ast.unparse(n.test)
            ):
                matches.append(n)
            elif (
                kind == "replay"
                and isinstance(n, ast.Return)
                and ast.unparse(n.value) == "copy.deepcopy(prior[2])"
            ):
                matches.append(n)
            elif (
                kind == "restore_all"
                and isinstance(n, ast.Call)
                and ast.unparse(n.func) == "self.active.remove"
            ):
                matches.append(n)
            elif (
                kind == "if_false_return"
                and isinstance(n, ast.If)
                and len(n.body) == 1
                and isinstance(n.body[0], ast.Return)
                and isinstance(n.body[0].value, ast.Constant)
                and n.body[0].value.value is False
            ):
                matches.append(n)
            elif (
                kind == "swap_rec_precedence"
                and isinstance(n, ast.Assign)
                and len(n.targets) == 1
                and isinstance(n.targets[0], ast.Name)
                and n.targets[0].id == "rec_precedence"
            ):
                matches.append(n)
            elif (
                kind == "raw_concat_recovery"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "prior_phase == 'started'"
            ):
                matches.append(n)
            elif (
                kind == "leave_r13_open"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "prior_phase == 'started'"
            ):
                matches.append(n)
            elif (
                kind == "ignore_hop_floor"
                and isinstance(n, ast.Assign)
                and ast.unparse(n) == "next_bodies[k] = new_bp"
            ):
                matches.append(n)
            elif (
                kind == "referenced_unpublished_loss"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "prior_phase is None"
            ):
                matches.append(n)
            elif (
                kind == "row_return"
                and isinstance(n, ast.Return)
                and isinstance(n.value, ast.Constant)
                and n.value.value == value.split("->", 1)[0]
            ):
                matches.append(n)
            elif (
                kind == "collapse_recovery_integrity_loss"
                and isinstance(n, ast.Return)
                and "recovery_integrity_loss" in ast.unparse(n)
                and "if " not in ast.unparse(n)
            ):
                matches.append(n)
            elif (
                kind == "collapse_r19_publishing_to_removed"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "post_state == 'publishing'"
            ):
                matches.append(n)
            elif (
                kind == "skip_transitive_ancestor_seals"
                and isinstance(n, ast.For)
                and ast.unparse(n.iter) == "ac_seal['ancestor_seals']"
            ):
                matches.append(n)
            elif (
                kind == "round1_len2_ancestor_shortcut"
                and isinstance(n, ast.If)
                and ast.unparse(n.test) == "key not in top_anc_by_key"
            ):
                matches.append(n)
            elif (
                kind == "allow_unrelated_ancestor_padding"
                and isinstance(n, ast.Call)
                and isinstance(n.func, ast.Name)
                and n.func.id == "require"
                and "reachable_keys" in ast.unparse(n.args[0])
            ):
                matches.append(n)
        matches.sort(key=lambda n: (n.lineno, n.col_offset))
        assert len(matches) > occurrence, (
            "no-op mutation",
            method,
            kind,
            value,
            len(matches),
        )
        target = matches[occurrence]
        if kind == "slide_dedup":
            target.body.append(
                ast.parse(
                    "self.remember(e['idempotency_key'], prior[0], e['at'], prior[2])"
                ).body[0]
            )
        elif kind == "require":
            target.args[0] = ast.Constant(True)
        elif kind in ("if_reason", "body_match"):
            target.test = ast.Constant(False)
        elif kind == "body_length":
            target.elts = [ast.Constant("digest")]
        elif kind == "replay":
            target.value = ast.parse(
                "self.receipt(r, len(self.journal))", mode="eval"
            ).body
        elif kind == "restore_all":
            target.func.attr = "clear"
            target.args = []
        elif kind == "invert_not_in":
            target.ops = [ast.In()]
        elif kind == "latest_revision_only":
            target.body = ast.parse(
                "latest = {}\n"
                "for k, r in sorted(self.records.items(), key=lambda x: x[0][2]):\n"
                "    latest[r['record_id']] = (k, r)\n"
                "return list(latest.values())\n"
            ).body
        elif kind == "key_by_first_pin":
            target.body.append(ast.Break())
        elif kind == "first_stream_only":
            target.iter = ast.parse("expected_sharing[:1]", mode="eval").body
        elif kind == "immediate_source_only":
            target.iter = ast.parse("chain_journals[:1]", mode="eval").body
        elif kind == "allow_bool_int":
            target.test = ast.parse("isinstance(value, int)", mode="eval").body
        elif kind == "prefix_removal_stream":
            replacement = ast.parse(
                "not s.startswith(self.removal_stream or '')", mode="eval"
            ).body
            for parent in ast.walk(tree):
                for field, old in ast.iter_fields(parent):
                    if old is target:
                        setattr(parent, field, replacement)
                    elif isinstance(old, list):
                        for i, member in enumerate(old):
                            if member is target:
                                old[i] = replacement
        elif kind == "reason_bypass":
            target.value = ast.parse(
                "([check_p1, check_p2, check_p2b] if step['reason'] in ('operator', 'rights_restricted') else [check_p1, check_p2, check_p2b, check_p3, check_p4, check_p5])",
                mode="eval",
            ).body
        elif kind == "skip_retry_reverify":
            target.value = ast.parse(
                "([check_p1] if step['mode'] == 'retry_unlink' else [check_p1, check_p2, check_p2b, check_p3, check_p4, check_p5])",
                mode="eval",
            ).body
        elif kind == "abandoned_incarnation_ok":
            target.body = ast.parse(
                "return incarnation in self.open_starts.get(key, set())\n"
            ).body
        elif kind == "swap_precedence":
            swap_i = int(value)
            elts = target.value.elts
            elts[swap_i], elts[swap_i + 1] = elts[swap_i + 1], elts[swap_i]
        elif kind == "if_false_return":
            target.test = ast.Constant(False)
        elif kind == "swap_rec_precedence":
            swap_i = int(value)
            elts = target.value.elts
            elts[swap_i], elts[swap_i + 1] = elts[swap_i + 1], elts[swap_i]
        elif kind == "raw_concat_recovery":
            target.body = ast.parse("return (floor, 'started'), new_open\n").body
        elif kind == "leave_r13_open":
            target.body[-1] = ast.parse(
                "return (prior_bound, 'started'), new_open\n"
            ).body[0]
        elif kind == "ignore_hop_floor":
            target.value = ast.parse("(prior_bound, new_bp[1])", mode="eval").body
        elif kind == "referenced_unpublished_loss":
            target.body.insert(
                0,
                ast.parse(
                    "if referenced and floor == 0 and obs == 'absent':\n"
                    "    return 'integrity_loss'\n"
                ).body[0],
            )
        elif kind == "row_return":
            target.value = ast.Constant(value.split("->", 1)[1])
        elif kind == "collapse_recovery_integrity_loss":
            target.value = ast.parse(
                "((floor, 'recovery_publishing'), set())", mode="eval"
            ).body
        elif kind == "collapse_r19_publishing_to_removed":
            target.body = ast.parse("return (floor, 'removed'), set()\n").body
        elif kind == "skip_transitive_ancestor_seals":
            target.body = [ast.Pass()]
        elif kind == "round1_len2_ancestor_shortcut":
            target.test = ast.parse(
                "key not in top_anc_by_key and len(top_anc_by_key) < 2", mode="eval"
            ).body
        elif kind == "allow_unrelated_ancestor_padding":
            target.args[0] = ast.Constant(True)
        else:
            replacement = ast.Constant(kind == "compare_true")
            for parent in ast.walk(tree):
                for field, old in ast.iter_fields(parent):
                    if old is target:
                        setattr(parent, field, replacement)
                    elif isinstance(old, list):
                        for i, member in enumerate(old):
                            if member is target:
                                old[i] = replacement
        ns = {"__file__": __file__, "__name__": "record_mutation"}
        exec(compile(ast.fix_missing_locations(tree), __file__, "exec"), ns)
        ns["SHAPE_CACHE"].update(SHAPE_CACHE)
        case = load(BASE / "vectors" / (witness + ".json"))
        outcome = ns["check_case"](case)
        assert outcome != case["expected"], (
            "mutation ineffective",
            witness,
            kind,
            value,
        )
        if alternative and alternative[0] == "top_refusal":
            assert outcome == {"refusal": alternative[1]}, (
                "wrong top-level refusal alternative",
                witness,
                outcome,
                alternative,
            )
            print("[ok] mutation:", witness, kind, value)
            continue
        assert "refusal" not in outcome, ("mutation still refused", witness, outcome)
        if alternative:
            field, expected, index = alternative
            named = copy.deepcopy(case["expected"])
            if field == "reason":
                if named["results"][index].get("outcome") == "removal_eligibility":
                    named["results"][index] = {
                        "outcome": "removal_eligibility",
                        "verdict": "refused",
                        "reason": expected,
                    }
                else:
                    named["results"][index] = {
                        "outcome": "refused",
                        "reason": expected,
                    }
            elif expected == "allowed":
                use = case["steps"][index]["use"]
                named["results"][index] = dict(
                    outcome="allowed",
                    **{
                        key: use[key]
                        for key in ("closure", "selection", "omitted", "protection_cut")
                    },
                )
                named["active_holds"] = []
            else:
                named["results"][index][field] = expected
            assert outcome == named, (
                "wrong named alternative",
                witness,
                outcome,
                named,
            )
        else:
            accepted = {
                "tombstoned",
                "untombstoned",
                "retired",
                "journaled",
                "receipt_verified",
                "seal_verified",
                "allowed",
                "body_state",
            }
            assert all(
                r.get("outcome") in accepted
                or (
                    r.get("outcome") == "removal_eligibility"
                    and r.get("verdict") == "eligible"
                )
                for r in outcome["results"]
            ), (
                "mutation did not accept",
                witness,
                outcome,
            )
        print("[ok] mutation:", witness, kind, value)
    return len(specs)


def _collect_case_shape_items(c, add_key):
    """Collect every Stage-3 schema check reachable after Stage 1+2 wire preflight."""
    if "raw_profile" in c:
        try:
            legacy.parse_wire(c["raw_profile"], "stratum_profile", "raw_profile")
        except legacy.Reject:
            return
        add_key("stratum-profile", c["raw_profile"])
    elif "profile" in c:
        add_key("stratum-profile", json.dumps(c["profile"]))

    if "raw_journal_ndjson" in c:
        try:
            for raw in legacy.parse_wire_lines(
                c["raw_journal_ndjson"], "raw_journal_ndjson", "journal_entry"
            ):
                add_key("journal-entry", json.dumps(raw))
        except legacy.Reject:
            return
    elif "raw_journal" in c:
        for i, raw in enumerate(c["raw_journal"]):
            try:
                legacy.parse_wire(raw, "journal_entry", f"raw_journal[{i}]")
            except legacy.Reject:
                return
            add_key("journal-entry", raw)
    elif "journal" in c:
        for e in c["journal"]:
            add_key("journal-entry", json.dumps(e))

    for idx, step in enumerate(c["steps"]):
        if "raw_request" in step:
            try:
                legacy.parse_wire(
                    step["raw_request"],
                    "protection_request",
                    f"steps[{idx}].raw_request",
                )
            except legacy.Reject:
                return
            add_key("protection-request", step["raw_request"])
        elif "request" in step:
            add_key("protection-request", json.dumps(step["request"]))
        elif "raw_use" in step:
            try:
                legacy.parse_wire(
                    step["raw_use"], "export_request", f"steps[{idx}].raw_use"
                )
            except legacy.Reject:
                return
            add_key("export-request", step["raw_use"])
        elif "use" in step:
            add_key("export-request", json.dumps(step["use"]))
        elif "raw_receipt" in step:
            try:
                parsed_rcpt = _parse_raw_receipt_step(
                    step["raw_receipt"], f"steps[{idx}].raw_receipt"
                )
            except legacy.Reject:
                return
            schema = (
                "write-receipt"
                if parsed_rcpt.get("kind") in ("receipt", "reconciliation")
                else "protection-receipt"
            )
            add_key(schema, step["raw_receipt"])
        elif "receipt" in step:
            schema = (
                "write-receipt"
                if step["receipt"].get("kind") in ("receipt", "reconciliation")
                else "protection-receipt"
            )
            add_key(schema, json.dumps(step["receipt"]))
        elif "raw_entry" in step:
            try:
                legacy.parse_wire(
                    step["raw_entry"], "journal_entry", f"steps[{idx}].raw_entry"
                )
            except legacy.Reject:
                return
            add_key("journal-entry", step["raw_entry"])
        elif "entry" in step:
            add_key("journal-entry", json.dumps(step["entry"]))
        elif "raw_body_state" in step:
            try:
                legacy.parse_wire(
                    step["raw_body_state"],
                    "body_state_request",
                    f"steps[{idx}].raw_body_state",
                )
            except legacy.Reject:
                return
            add_key("body-state-request", step["raw_body_state"])
        elif "body_state" in step:
            add_key("body-state-request", json.dumps(step["body_state"]))
        elif "raw_removal_eligibility" in step:
            try:
                legacy.parse_wire(
                    step["raw_removal_eligibility"],
                    "removal_eligibility",
                    f"steps[{idx}].raw_removal_eligibility",
                )
            except legacy.Reject:
                return
            add_key("removal-eligibility-request", step["raw_removal_eligibility"])
        elif "removal_eligibility" in step:
            add_key(
                "removal-eligibility-request", json.dumps(step["removal_eligibility"])
            )

    def collect_source_list(spec):
        if isinstance(spec, list) and spec and all(isinstance(x, str) for x in spec):
            try:
                parsed = [
                    legacy.parse_wire(x, "journal_entry", f"source[{i}]")
                    for i, x in enumerate(spec)
                ]
            except legacy.Reject:
                return
            if isinstance(parsed[0], dict) and parsed[0].get("v") in CAPABILITY:
                fam = parsed[0]["v"]
                sch = BASE.parent / f"v{fam}/journal-entry.schema.json"
                for e, raw in zip(parsed, spec):
                    if isinstance(e, dict) and e.get("v") == fam:
                        add_key(sch, raw)
        elif isinstance(spec, list) and spec and isinstance(spec[0], dict):
            fam = spec[0].get("v")
            if fam in CAPABILITY:
                sch = BASE.parent / f"v{fam}/journal-entry.schema.json"
                for e in spec:
                    if isinstance(e, dict) and e.get("v") == fam:
                        add_key(sch, json.dumps(e))

    def collect_seal_ctx(sctx):
        if not isinstance(sctx, dict):
            return
        if "raw_seal" in sctx and isinstance(sctx["raw_seal"], str):
            try:
                legacy.parse_wire(sctx["raw_seal"], "stream_seal", "raw_seal")
                add_key("stream-seal", sctx["raw_seal"])
            except legacy.Reject:
                pass
        elif "seal" in sctx and isinstance(sctx["seal"], dict):
            add_key("stream-seal", json.dumps(sctx["seal"]))
        if "raw_journal" in sctx:
            collect_source_list(list(sctx["raw_journal"]))
        elif "journal" in sctx:
            collect_source_list(sctx["journal"])
        for ac in sctx.get("ancestor_seal_contexts", []):
            collect_seal_ctx(ac)

    for idx, step in enumerate(c["steps"]):
        if "raw_verify_seal" in step:
            try:
                vctx = legacy.parse_wire(
                    step["raw_verify_seal"],
                    "seal_context",
                    f"steps[{idx}].raw_verify_seal",
                )
            except legacy.Reject:
                continue
            collect_seal_ctx(vctx)
        elif "verify_seal" in step:
            collect_seal_ctx(step["verify_seal"])

    if "source_seal" in c:
        collect_seal_ctx(c["source_seal"])

    if "quarantined_streams" in c and isinstance(c["quarantined_streams"], dict):
        for raw_img in c["quarantined_streams"].values():
            if isinstance(raw_img, str):
                for ln in raw_img.splitlines(keepends=True):
                    if not ln.endswith("\n") or "\r" in ln:
                        break
                    try:
                        legacy.parse_wire(ln, "journal_entry", "quarantined")
                    except legacy.Reject:
                        break
                    add_key("journal-entry", ln)

    if "candidate_streams" in c and isinstance(c["candidate_streams"], list):
        for ce in c["candidate_streams"]:
            if isinstance(ce, dict):
                add_key("journal-entry", json.dumps(ce))

    if "sharing_streams" in c and isinstance(c["sharing_streams"], list):
        for sj in c["sharing_streams"]:
            if isinstance(sj, list):
                for se in sj:
                    if isinstance(se, dict):
                        add_key("journal-entry", json.dumps(se))

    if "raw_source_journal" in c:
        src_copy = list(c["raw_source_journal"])
        collect_source_list(src_copy)
        for step in c["steps"]:
            if "raw_source_entry" in step:
                src_copy.append(step["raw_source_entry"])
                collect_source_list(src_copy)
    elif "source_journal" in c:
        src_copy = copy.deepcopy(c["source_journal"])
        collect_source_list(src_copy)
        for step in c["steps"]:
            if "source_entry" in step and isinstance(src_copy, list):
                src_copy.append(copy.deepcopy(step["source_entry"]))
                collect_source_list(src_copy)

    if "raw_ancestors" in c:
        anc_copy = [list(a) for a in c["raw_ancestors"]]
        for a in anc_copy:
            collect_source_list(a)
        for step in c["steps"]:
            if "raw_ancestor_entry" in step:
                anc_copy[step["ancestor"]].append(step["raw_ancestor_entry"])
                collect_source_list(anc_copy[step["ancestor"]])
    elif "ancestors" in c:
        anc_copy = copy.deepcopy(c["ancestors"])
        for a in anc_copy:
            collect_source_list(a)
        for step in c["steps"]:
            if "ancestor_entry" in step:
                anc_copy[step["ancestor"]].append(copy.deepcopy(step["ancestor_entry"]))
                collect_source_list(anc_copy[step["ancestor"]])

    for idx, step in enumerate(c["steps"]):
        rem = None
        if "raw_removal_eligibility" in step:
            try:
                rem = legacy.parse_wire(
                    step["raw_removal_eligibility"],
                    "removal_eligibility",
                    f"steps[{idx}].raw_removal_eligibility",
                )
            except legacy.Reject:
                continue
        elif "removal_eligibility" in step:
            rem = step["removal_eligibility"]
        if isinstance(rem, dict):
            for obs in rem.get("observed_streams", []):
                if not isinstance(obs, dict):
                    continue
                if "raw_journal" in obs and isinstance(obs["raw_journal"], list):
                    try:
                        for j_i, raw in enumerate(obs["raw_journal"]):
                            legacy.parse_wire(
                                raw, "journal_entry", f"observed_raw[{j_i}]"
                            )
                            add_key("journal-entry", raw)
                    except legacy.Reject:
                        pass
                elif "journal" in obs and isinstance(obs["journal"], list):
                    for e in obs["journal"]:
                        if isinstance(e, dict):
                            add_key("journal-entry", json.dumps(e))
                if "raw_source_journal" in obs:
                    collect_source_list(list(obs["raw_source_journal"]))
                elif "source_journal" in obs:
                    collect_source_list(obs["source_journal"])
                if "raw_ancestors" in obs:
                    for a in obs["raw_ancestors"]:
                        collect_source_list(list(a))
                elif "ancestors" in obs:
                    for a in obs["ancestors"]:
                        collect_source_list(a)
                if "source_seal" in obs:
                    collect_seal_ctx(obs["source_seal"])

    for result in c["expected"].get("results", []):
        if result.get("kind") in ("reconciliation", "receipt"):
            add_key("write-receipt", json.dumps(result))
            continue
        if result["outcome"] in (
            "journaled",
            "receipt_verified",
            "body_state",
            "removal_eligibility",
            "seal_verified",
        ):
            continue
        name = (
            "export-result"
            if result["outcome"] in ("allowed", "refused")
            else "protection-receipt"
        )
        add_key(name, json.dumps(result))


def prewarm_shape_cache(cases, rejects, examples):
    global SUITE_CALLS
    unique_keys: list[tuple[str, str]] = []
    seen: set[tuple[str, str]] = set()

    def add_key(schema_ref, raw_json):
        schema_rel = legacy._normalize_schema_rel(schema_ref, default_ver="v1")
        payload = _norm_raw(raw_json)
        key = (schema_rel, payload)
        if key not in seen:
            seen.add(key)
            unique_keys.append(key)

    for path in cases:
        _collect_case_shape_items(load(path), add_key)
    for path in rejects:
        rc = load(path)
        add_key(rc["schema"], json.dumps(rc["baseline"]))
        add_key(rc["schema"], json.dumps(rc["reject"]))
    for path in examples:
        add_key(path.name.split(".")[0], json.dumps(load(path)))
    v0_src_entry = load(BASE / "vectors/continuation-v0-source.json")["source_journal"][
        0
    ]
    add_key(
        BASE.parent / "v0/journal-entry.schema.json",
        json.dumps({**v0_src_entry, "at": "２０２６-11-05T12:34:56Z"}),
    )
    # Prewarm compact NDJSON lines scanned during AbortWriter.activate_recovery faultpoints.
    for aw_entry in (
        {
            "v": 1,
            "seq": 1,
            "group": 1,
            "group_size": 1,
            "stream": "store.bodies",
            "type": "stream_opened",
            "at": "2026-10-20T00:00:01.000000Z",
            "mode": "single_revision",
            "failure_model": "survives_power_loss",
        },
        {
            "v": 1,
            "seq": 2,
            "group": 2,
            "group_size": 1,
            "stream": "store.bodies",
            "type": "body_published",
            "at": "2026-10-20T00:00:02.000000Z",
            "body": {
                "digest": "sha256:13550350a8681c84c861aac2e5b440161c2b33a3e4f302ac680ca5b686de48de",
                "length": 8,
            },
            "incarnation": 1,
        },
    ):
        add_key("journal-entry", json.dumps(aw_entry, separators=(",", ":")) + "\n")

    items = [
        (f"k{idx}", schema_rel, payload)
        for idx, (schema_rel, payload) in enumerate(unique_keys)
    ]
    SUITE_CALLS += 1
    verdicts = legacy.shape_suite(items, default_ver="v1")
    for idx, key in enumerate(unique_keys):
        SHAPE_CACHE[key] = verdicts[f"k{idx}"]


def suite_error_controls():
    """Separate genuine suite failures from the validator's numeric-decoder profile."""
    for label, bad_item in (
        ("malformed_json", ("bad", "v1/record.schema.json", "{\n")),
        ("missing_schema", ("missing", "v1/nonexistent.schema.json", "{}\n")),
    ):
        try:
            legacy.shape_suite([bad_item], default_ver="v1")
        except RuntimeError:
            pass
        else:
            raise AssertionError(f"suite_error_controls did not fail on {label}")

    # Legacy float64 decoding fails before schema evaluation. Exact JSON decoding
    # reaches the record's bounded revision check and returns an ordinary invalid
    # verdict. Detect the profile from this behavior, never a version label.
    try:
        verdicts = legacy.shape_suite(
            [("huge", "v1/record.schema.json", '{"v":1,"revision":1e400}\n')],
            default_ver="v1",
        )
    except RuntimeError as exc:
        message = str(exc)
        assert "1e400" in message and "float64" in message, message
    else:
        assert verdicts == {"huge": False}, verdicts


class AbortWriter:
    """Logical model of the single-writer incomplete-append abort boundary and incarnation allocation."""

    def __init__(self, window_ms=60000, profile=None):
        self.window_ms = window_ms
        self.profile = copy.deepcopy(profile) if profile is not None else None
        runtime = (self.profile or {}).get("runtime", {})
        self.incarnation_allocation = runtime.get(
            "incarnation_allocation", "effect_dependent"
        )
        self.disk_bytes = ""
        self.committed_cut_offset = 0
        self.committed_cut_seq = 0
        self.state = State(window_ms)
        if self.profile is not None:
            legacy.check_profile(self.profile, "profile")
            self.state.removal_stream = runtime.get("body_removal_stream")
            self.state.incarnation_allocation = runtime.get("incarnation_allocation")
            self.state.quarantined_designated_streams = list(
                runtime.get("quarantined_designated_streams", [])
            )
        self.poisoned = False
        self.active_generation = 0
        self.activation_in_progress = False
        self.quarantined_images: dict[str, str] = {}
        self.candidate_streams: list[dict] = []
        self.durable_effect_relied_on: dict[tuple[str, int], int] = {}
        self.durable_burned_ledger: dict[tuple[str, int], int] = {}

    def open_handle(self):
        require(
            not self.poisoned and not self.activation_in_progress, "inconsistent_cut"
        )
        return (self.state.removal_stream, self.active_generation)

    def append_with_handle(self, handle, entries, **kwargs):
        require(
            handle == (self.state.removal_stream, self.active_generation),
            "body_removal_stream",
        )
        return self.append_group(entries, **kwargs)

    def unlink_with_handle(self, handle, body, incarnation):
        require(
            not self.poisoned and not self.activation_in_progress, "inconsistent_cut"
        )
        require(
            handle == (self.state.removal_stream, self.active_generation),
            "body_removal_stream",
        )
        key = (body["digest"], int(body["length"]))
        require(incarnation in self.state.open_starts.get(key, set()), "body_lifecycle")
        return "unlinked"

    def allocate_incarnation(self, body, requested_n=None):
        require(
            not self.poisoned and not self.activation_in_progress, "inconsistent_cut"
        )
        key = (body["digest"], body["length"])
        committed_max = self.state.bodies.get(key, (0, None))[0]
        if self.incarnation_allocation == "prewrite_durable_ledger":
            burned_max = self.durable_burned_ledger.get(key, 0)
            floor = max(committed_max, burned_max)
            n = (floor + 1) if requested_n is None else requested_n
            require(n > floor, "body_lifecycle")
            require(n <= MAX_SAFE_INTEGER, "numeric_domain")
            self.durable_burned_ledger[key] = max(burned_max, n)
            return n
        effect_max = self.durable_effect_relied_on.get(key, 0)
        floor = max(committed_max, effect_max)
        n = (floor + 1) if requested_n is None else requested_n
        require(n > floor, "body_lifecycle")
        require(n <= MAX_SAFE_INTEGER, "numeric_domain")
        return n

    def record_body_effect(self, body, n):
        key = (body["digest"], body["length"])
        self.durable_effect_relied_on[key] = max(
            self.durable_effect_relied_on.get(key, 0), n
        )

    def append_group(
        self,
        entries,
        *,
        k_written=None,
        errno=None,
        fsync_ok=True,
        lost_exclusion=False,
        gate=None,
    ):
        require(
            not self.poisoned and not self.activation_in_progress, "inconsistent_cut"
        )
        payload = "".join(json.dumps(e, separators=(",", ":")) + "\n" for e in entries)
        total_len = len(payload.encode("utf-8"))
        if k_written is None and errno is None and fsync_ok and not lost_exclusion:
            for e in entries:
                self.state.ingest(e)
            self.disk_bytes += payload
            self.committed_cut_offset = len(self.disk_bytes)
            self.committed_cut_seq = len(self.state.journal)
            return "committed"

        written = total_len if k_written is None else k_written
        if written > 0:
            self.disk_bytes += payload[:written]

        can_abort = (
            not lost_exclusion
            and k_written is not None
            and 0 <= k_written < total_len
            and errno in ("ENOSPC", "EDQUOT")
        )
        if not can_abort:
            self.poisoned = True
            return "poisoned"

        gate_cfg = gate or {}
        same_file = gate_cfg.get("same_file", True)
        truncate_ok = gate_cfg.get("truncate_ok", True)
        abort_fsync_ok = gate_cfg.get("abort_fsync_ok", True)
        fstat_matches = gate_cfg.get("fstat_matches", True)
        prior_cut_valid = gate_cfg.get("prior_cut_valid", True)

        if truncate_ok:
            self.disk_bytes = self.disk_bytes[: self.committed_cut_offset]

        revalidated_ok = False
        if (
            prior_cut_valid
            and fstat_matches
            and len(self.disk_bytes) == self.committed_cut_offset
        ):
            try:
                parsed = (
                    legacy.parse_wire_lines(self.disk_bytes, "abort_revalidate")
                    if self.disk_bytes
                    else []
                )
                if parsed:
                    legacy.check_journal(parsed, "v1", check_entry_at=False)
                revalidated_ok = len(parsed) == self.committed_cut_seq
            except legacy.Reject:
                revalidated_ok = False

        if not (same_file and truncate_ok and abort_fsync_ok and revalidated_ok):
            self.poisoned = True
            return "poisoned"
        return "aborted"

    def body_state(self, step):
        require(
            not self.poisoned and not self.activation_in_progress, "inconsistent_cut"
        )
        return self.state.body_state(step)

    def activate_recovery(
        self,
        successor_opened,
        live_observations=None,
        *,
        crash_at=None,
        successor_fsync_ok=True,
        profile_fsync_ok=True,
    ):
        require(self.profile is not None, "body_removal_stream")
        require(
            self.incarnation_allocation == "prewrite_durable_ledger",
            "body_removal_stream",
        )
        # Step 1: Enter continuous store-wide exclusion & permanently fence old handles.
        self.activation_in_progress = True
        self.active_generation += 1
        if crash_at == "mid_activation":
            return "crashed_mid_activation"

        prev_stream = self.state.removal_stream
        quarantined_candidate = dict(self.quarantined_images)
        quarantined_candidate[prev_stream] = self.disk_bytes
        new_profile = copy.deepcopy(self.profile)
        new_rt = new_profile["runtime"]
        q_list = [prev_stream] + list(new_rt.get("quarantined_designated_streams", []))
        new_rt["quarantined_designated_streams"] = q_list
        new_rt["body_removal_stream"] = successor_opened["stream"]
        legacy.check_profile(new_profile, "profile")

        replayed = State(self.window_ms)
        replayed.removal_stream = new_rt["body_removal_stream"]
        replayed.incarnation_allocation = new_rt["incarnation_allocation"]
        replayed.quarantined_designated_streams = list(q_list)
        ledger_floors = [
            {"body": {"digest": k[0], "length": k[1]}, "floor": v}
            for k, v in sorted(self.durable_burned_ledger.items())
        ]
        obs_items = list(live_observations or [])
        sim_case = {
            "window_ms": self.window_ms,
            "profile": new_profile,
            "journal": [successor_opened],
            "quarantined_streams": quarantined_candidate,
            "recovery_contexts": {
                successor_opened["stream"]: {
                    "ledger_floors": ledger_floors,
                    "live_activation_observations": obs_items,
                }
            },
        }
        _validate_and_replay_recovery_chain(sim_case, replayed)
        legacy.check_journal([successor_opened], "v1", check_entry_at=False)
        replayed.ingest(successor_opened)

        if crash_at == "after_successor_before_profile_swap":
            self.candidate_streams.append(copy.deepcopy(successor_opened))
            return "crashed_before_profile_swap"

        if not (successor_fsync_ok and profile_fsync_ok):
            self.poisoned = True
            return "poisoned"

        succ_payload = json.dumps(successor_opened, separators=(",", ":")) + "\n"
        self.quarantined_images = quarantined_candidate
        self.profile = new_profile
        self.disk_bytes = succ_payload
        self.committed_cut_offset = len(succ_payload)
        self.committed_cut_seq = 1
        self.state = replayed
        self.poisoned = False
        self.activation_in_progress = False
        return "activated"

    def reopen_from_disk(self):
        # Fresh reopen after kill/power loss ignores in-memory state and
        # committed_cut_offset, scanning physical disk_bytes from byte 0 while
        # preserving durable effect/ledger records and quarantined images.
        reopened = AbortWriter(self.window_ms, self.profile)
        reopened.disk_bytes = self.disk_bytes
        reopened.quarantined_images = dict(self.quarantined_images)
        reopened.candidate_streams = copy.deepcopy(self.candidate_streams)
        reopened.durable_effect_relied_on = dict(self.durable_effect_relied_on)
        reopened.durable_burned_ledger = dict(self.durable_burned_ledger)
        parsed = (
            legacy.parse_wire_lines(reopened.disk_bytes, "reopen")
            if reopened.disk_bytes
            else []
        )
        if reopened.profile is not None:
            sim_case = {
                "window_ms": reopened.window_ms,
                "profile": reopened.profile,
                "journal": parsed,
                "quarantined_streams": reopened.quarantined_images,
                "candidate_streams": reopened.candidate_streams,
            }
            _validate_and_replay_recovery_chain(sim_case, reopened.state)
        legacy.check_journal(parsed, "v1", check_entry_at=False)
        for e in parsed:
            reopened.state.ingest(e)
        reopened.committed_cut_offset = len(reopened.disk_bytes)
        reopened.committed_cut_seq = len(reopened.state.journal)
        return reopened


def timestamp_and_abort_controls():
    """Verify v1 timestamp position-coverage mutations, Stage 3->4 fallback, and abort faultpoints."""
    source = (
        Path(__file__)
        .read_text(encoding="utf-8")
        .split("def timestamp_and_abort_controls(", 1)[0]
    )
    # 1. Position-deletion controls: removing self.instant(...) at each v1 timestamp position
    #    must cause its corresponding vector to stop refusing as instant_spelling.
    for pos_label, method_name, target_expr, witness in (
        (
            "record.produced_at.instant",
            "ingest",
            "self.instant(e['record']['produced_at']['instant'])",
            "produced-at-instant-spelling",
        ),
        (
            "request.observed_at",
            "request",
            "self.instant(r['observed_at'])",
            "instant-observed-at-seven-digits",
        ),
        (
            "reconcile.attempted_at",
            "reconcile",
            "self.instant(attempted_at)",
            "instant-reconcile-attempted-at",
        ),
        (
            "verify_reconciliation.attempted_at",
            "verify_reconciliation",
            "self.instant(received['attempted_at'])",
            "reconciliation-attempted-at-year-zero",
        ),
        (
            "verify_reconciliation.window_covers_from",
            "verify_reconciliation",
            "self.instant(received['window_covers_from'])",
            "reconciliation-window-covers-from-impossible-date",
        ),
        (
            "verify_reconciliation.window_covers_until",
            "verify_reconciliation",
            "self.instant(received['window_covers_until'])",
            "reconciliation-window-covers-until-year-zero",
        ),
        (
            "removal_eligibility.decision_time",
            "removal_eligibility",
            "self.instant(step.get('decision_time') if isinstance(step, dict) else None)",
            "removal-instant-decision-time-spelling",
        ),
        (
            "removal_eligibility.authorization.not_before",
            "removal_eligibility",
            "self.instant(auth['not_before'])",
            "removal-instant-auth-not-before-spelling",
        ),
        (
            "removal_eligibility.authorization.expires_at",
            "removal_eligibility",
            "self.instant(auth['expires_at'])",
            "removal-instant-auth-expires-at-spelling",
        ),
        (
            "verify_seal.sealed_at",
            "verify_seal_step",
            "self.instant(seal.get('sealed_at') if isinstance(seal, dict) else None)",
            "seal-instant-year-zero",
        ),
    ):
        tree = ast.parse(source)
        state_cls = next(
            n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == "State"
        )
        fn_node = next(
            n
            for n in state_cls.body
            if isinstance(n, ast.FunctionDef) and n.name == method_name
        )
        replaced = 0
        for parent in ast.walk(fn_node):
            for field, old in ast.iter_fields(parent):
                if isinstance(old, ast.Call) and ast.unparse(old) == target_expr:
                    setattr(
                        parent,
                        field,
                        ast.parse(
                            "self.instant('0001-01-01T00:00:02.000000Z')", mode="eval"
                        ).body,
                    )
                    replaced += 1
        assert replaced == 1, ("position-deletion target mismatch", pos_label, replaced)
        ns = {"__file__": __file__, "__name__": "pos_mutation"}
        exec(compile(ast.fix_missing_locations(tree), __file__, "exec"), ns)
        ns["SHAPE_CACHE"].update(SHAPE_CACHE)
        outcome = ns["check_case"](load(BASE / "vectors" / f"{witness}.json"))
        assert outcome.get("refusal") != "instant_spelling", (
            "position-deletion did not unmask",
            pos_label,
            outcome,
        )

    # 2. The contract-owned seconds pattern makes leap seconds fail Stage 3; the
    #    other calendar controls may be caught there by format enforcement. Force
    #    each cached negative through Stage 4 as a defense-in-depth control.
    for vec_name, schema_rel, doc_fn in (
        (
            "instant-leap-second.json",
            "v1/journal-entry.schema.json",
            lambda c: (c["journal"][0], c["journal"][0]["at"]),
        ),
        (
            "instant-impossible-date.json",
            "v1/journal-entry.schema.json",
            lambda c: (c["journal"][0], c["journal"][0]["at"]),
        ),
        (
            "produced-at-impossible-date.json",
            "v1/journal-entry.schema.json",
            lambda c: (
                c["journal"][1],
                c["journal"][1]["record"]["produced_at"]["instant"],
            ),
        ),
        (
            "reconciliation-window-covers-from-impossible-date.json",
            "v1/write-receipt.schema.json",
            lambda c: (
                c["steps"][0]["receipt"],
                c["steps"][0]["receipt"]["window_covers_from"],
            ),
        ),
        (
            "removal-instant-auth-expires-at-spelling.json",
            "v1/removal-eligibility-request.schema.json",
            lambda c: (
                c["steps"][0]["removal_eligibility"],
                c["steps"][0]["removal_eligibility"]["authorization"]["expires_at"],
            ),
        ),
        (
            "seal-instant-impossible-date.json",
            "v1/stream-seal.schema.json",
            lambda c: (
                c["steps"][0]["verify_seal"]["seal"],
                c["steps"][0]["verify_seal"]["seal"]["sealed_at"],
            ),
        ),
    ):
        vec_case = load(BASE / "vectors" / vec_name)
        bad_doc, bad_ts = doc_fn(vec_case)
        assert CANONICAL_INSTANT.fullmatch(bad_ts) is not None, (vec_name, bad_ts)
        schema_key = (schema_rel, _norm_raw(json.dumps(bad_doc)))
        assert SHAPE_CACHE.get(schema_key) is False, (vec_name, schema_key)
        try:
            SHAPE_CACHE[schema_key] = True
            fallback_case = {
                k: v for k, v in vec_case.items() if not k.startswith("invalid_")
            }
            check_case_stage3_shapes(fallback_case, vec_name)
            res = check_case(fallback_case)
            assert res == {"refusal": "instant_spelling"}, (vec_name, res)
        finally:
            SHAPE_CACHE[schema_key] = False

    # Non-ASCII digits in v0 continuation source entry: rejected at Stage 3 (shape)
    # under qualified goneat format: "date-time", and also refused at Stage 4 as
    # continuation if Stage 3 is overridden (defense-in-depth).
    v0_src_case = load(BASE / "vectors/continuation-v0-source.json")
    v0_nonascii_entry = {
        **v0_src_case["source_journal"][0],
        "at": "２０２６-11-05T12:34:56Z",
    }
    v0_schema_path = BASE.parent / "v0/journal-entry.schema.json"
    assert shape(v0_nonascii_entry, v0_schema_path) is False
    v0_key = ("v0/journal-entry.schema.json", _norm_raw(json.dumps(v0_nonascii_entry)))
    try:
        SHAPE_CACHE[v0_key] = True
        assert shape(v0_nonascii_entry, v0_schema_path) is True
        bad_src_case = copy.deepcopy(v0_src_case)
        bad_src_case["source_journal"][0] = v0_nonascii_entry
        assert check_case(bad_src_case) == {"refusal": "continuation"}
    finally:
        SHAPE_CACHE[v0_key] = False

    # 3. Incomplete-append abort & faultpoint matrix controls.
    opened = {
        "v": 1,
        "seq": 1,
        "group": 1,
        "group_size": 1,
        "stream": "records.example",
        "type": "stream_opened",
        "at": "2026-10-20T00:00:01.000000Z",
        "mode": "revisioned",
        "failure_model": "survives_power_loss",
    }
    rec1 = {
        "v": 1,
        "stream": "records.example",
        "record_id": "first",
        "revision": 1,
        "payload_contract": "contract: example/v0",
        "payload": {"note": "one"},
        "provenance": {"actor_kind": "human", "actor": "operator"},
        "classification": {"retention": "standard", "sensitivity": "0-public"},
    }
    entry2 = {
        "v": 1,
        "seq": 2,
        "group": 2,
        "group_size": 1,
        "stream": "records.example",
        "type": "appended",
        "at": "2026-10-20T00:00:02.000000Z",
        "record": rec1,
        "digest": digest(rec1),
        "idempotency_key": "write-first-1",
    }
    entry2_len = len((json.dumps(entry2, separators=(",", ":")) + "\n").encode("utf-8"))

    # 3a. pre_first_entry_bytes (k == 0): abort before any bytes after C=1; next write gets seq=2.
    w_pre = AbortWriter(60000)
    assert w_pre.append_group([opened]) == "committed"
    assert w_pre.append_group([entry2], k_written=0, errno="ENOSPC") == "aborted"
    assert not w_pre.poisoned and w_pre.committed_cut_seq == 1
    assert w_pre.append_group([entry2]) == "committed"
    assert w_pre.committed_cut_seq == 2 and w_pre.state.journal[-1]["seq"] == 2

    # A physical blank NDJSON line is invalid framing, whether a recovery scans it
    # after a crash or the abort path revalidates the retained cut.
    for blank_label, blank_line in (("empty", "\n"), ("whitespace", "   \n")):
        w_blank = AbortWriter(60000)
        assert w_blank.append_group([opened]) == "committed"
        w_blank.disk_bytes += blank_line
        try:
            w_blank.reopen_from_disk()
        except legacy.Reject as err:
            assert err.reason == "ingest", (blank_label, err.reason)
        else:
            raise AssertionError(f"{blank_label} NDJSON line was accepted on reopen")

        # Model an abort recovery whose durable cut incorrectly includes the blank
        # physical line. Revalidation must poison rather than retain it.
        w_blank.committed_cut_offset = len(w_blank.disk_bytes)
        assert (
            w_blank.append_group([entry2], k_written=0, errno="ENOSPC") == "poisoned"
        ), blank_label

    # 3b. mid_line_without_newline (0 < k < L - 1) & line_bytes_without_trailing_newline (k == L - 1):
    #     In-process abort truncates back to byte_offset(C) and allows retry with same idempotency_key
    #     at seq=2. If process dies before truncate completes, reopen_from_disk refuses as ingest.
    for k_partial in (entry2_len // 2, entry2_len - 1):
        w_abort = AbortWriter(60000)
        assert w_abort.append_group([opened]) == "committed"
        assert (
            w_abort.append_group([entry2], k_written=k_partial, errno="EDQUOT")
            == "aborted"
        )
        assert (
            not w_abort.poisoned
            and len(w_abort.disk_bytes) == w_abort.committed_cut_offset
        )
        assert w_abort.append_group([entry2]) == "committed"
        assert w_abort.committed_cut_seq == 2

        # Untruncated crash before truncate completes: reopen from physical bytes refuses as ingest.
        w_crash = AbortWriter(60000)
        assert w_crash.append_group([opened]) == "committed"
        raw_entry2 = json.dumps(entry2, separators=(",", ":")) + "\n"
        w_crash.disk_bytes += raw_entry2[:k_partial]
        try:
            w_crash.reopen_from_disk()
        except legacy.Reject as err:
            assert err.reason == "ingest", (k_partial, err.reason)
        else:
            raise AssertionError(
                f"untruncated k={k_partial} was not refused as ingest on reopen"
            )

    # 3c. partial_atomic_group_m_of_g: 1 of 2 complete NDJSON lines written before ENOSPC.
    #     In-process abort truncates to C=1; untruncated reopen refuses as inconsistent_cut.
    rec2 = {**rec1, "record_id": "second", "payload": {"note": "two"}}
    group_m1 = {**entry2, "group": 2, "group_size": 2}
    group_m2 = {
        **entry2,
        "seq": 3,
        "group": 2,
        "group_size": 2,
        "record": rec2,
        "digest": digest(rec2),
        "idempotency_key": "write-second-1",
    }
    m1_len = len((json.dumps(group_m1, separators=(",", ":")) + "\n").encode("utf-8"))
    w_grp = AbortWriter(60000)
    assert w_grp.append_group([opened]) == "committed"
    assert (
        w_grp.append_group([group_m1, group_m2], k_written=m1_len, errno="ENOSPC")
        == "aborted"
    )
    assert w_grp.committed_cut_seq == 1
    assert w_grp.append_group([entry2]) == "committed"
    assert w_grp.committed_cut_seq == 2

    w_grp_crash = AbortWriter(60000)
    assert w_grp_crash.append_group([opened]) == "committed"
    w_grp_crash.disk_bytes += json.dumps(group_m1, separators=(",", ":")) + "\n"
    try:
        w_grp_crash.reopen_from_disk()
    except legacy.Reject as err:
        assert err.reason == "inconsistent_cut", err.reason
    else:
        raise AssertionError(
            "untruncated partial atomic group was not refused as inconsistent_cut"
        )

    # 3d. Non-abortable faults (E1 / F4):
    #     - full_write_failed_fsync (k == L, fsync_ok=False): MUST poison WITHOUT truncating disk_bytes.
    #     - EIO, uncertain write count (k_written=None), lost_exclusion=True: MUST poison.
    #     - Any completion-gate failure: MUST poison.
    w_fsync_fail = AbortWriter(60000)
    assert w_fsync_fail.append_group([opened]) == "committed"
    cut1_offset = w_fsync_fail.committed_cut_offset
    assert (
        w_fsync_fail.append_group(
            [entry2], k_written=entry2_len, errno="EIO", fsync_ok=False
        )
        == "poisoned"
    )
    assert w_fsync_fail.poisoned is True
    assert len(w_fsync_fail.disk_bytes) == cut1_offset + entry2_len
    assert w_fsync_fail.committed_cut_seq == 1
    for post_poison_fn in (
        lambda: w_fsync_fail.append_group([entry2]),
        lambda: w_fsync_fail.allocate_incarnation(
            {"digest": "sha256:" + "0" * 64, "length": 1}
        ),
    ):
        try:
            post_poison_fn()
        except legacy.Reject as err:
            assert err.reason == "inconsistent_cut"
        else:
            raise AssertionError("poisoned writer did not refuse subsequent operation")

    for bad_kwargs in (
        {"k_written": 10, "errno": "EIO"},
        {"k_written": None, "errno": "ENOSPC"},
        {"k_written": 10, "errno": "ENOSPC", "lost_exclusion": True},
        {"k_written": 10, "errno": "ENOSPC", "gate": {"same_file": False}},
        {"k_written": 10, "errno": "ENOSPC", "gate": {"truncate_ok": False}},
        {"k_written": 10, "errno": "ENOSPC", "gate": {"abort_fsync_ok": False}},
        {"k_written": 10, "errno": "ENOSPC", "gate": {"fstat_matches": False}},
        {"k_written": 10, "errno": "ENOSPC", "gate": {"prior_cut_valid": False}},
    ):
        w_bad = AbortWriter(60000)
        assert w_bad.append_group([opened]) == "committed"
        assert w_bad.append_group([entry2], **bad_kwargs) == "poisoned", bad_kwargs
        assert w_bad.poisoned is True, bad_kwargs

    # 3e. Body lifecycle abort & incarnation_allocation modes (E2 / E3 / F4).
    body_ref = {
        "digest": "sha256:13550350a8681c84c861aac2e5b440161c2b33a3e4f302ac680ca5b686de48de",
        "length": 8,
    }
    body_stream_opened = {
        "v": 1,
        "seq": 1,
        "group": 1,
        "group_size": 1,
        "stream": "store.bodies",
        "type": "stream_opened",
        "at": "2026-10-20T00:00:01.000000Z",
        "mode": "single_revision",
        "failure_model": "survives_power_loss",
    }

    def make_body_profile(alloc_mode=None):
        runtime = {
            "stratum": "journal-files",
            "failure_model": "survives_power_loss",
            "history_policy": "append_only",
            "enabled_capabilities": ["body_ref", "tombstoning"],
            "idempotency_window_ms": 60000,
            "deletion": {
                "live_head": "supported",
                "retained_history": "unsupported",
                "managed_replicas": "not_applicable",
                "external_bodies": "supported",
                "decryption": "unsupported",
                "unmanaged_copies": "outside_claim",
            },
            "body_removal_stream": "store.bodies",
        }
        if alloc_mode is not None:
            runtime["incarnation_allocation"] = alloc_mode
        return {
            "capabilities": ["contract: durable-record/v1"],
            "origin": "declared",
            "conformance": "none",
            "build": {
                "name": "example-store",
                "version": "0.1.0",
                "compiled_strata": ["journal-files"],
                "compiled_capabilities": ["body_ref", "tombstoning"],
            },
            "runtime": runtime,
        }

    def make_pub(seq, n, at="2026-10-20T00:00:02.000000Z"):
        return {
            "v": 1,
            "seq": seq,
            "group": seq,
            "group_size": 1,
            "stream": "store.bodies",
            "type": "body_published",
            "at": at,
            "body": body_ref,
            "incarnation": n,
        }

    # Default omitted incarnation_allocation behaves as effect_dependent:
    # - zero-effect abort (k=0, no record_body_effect) allows reusing n=1 or choosing a gap (n=3).
    # - once record_body_effect(body_ref, 1) has occurred, n=1 is refused both in-process and
    #   after reopen_from_disk(), and retry must use n' > 1 (e.g. 2 or 4).
    for mode_arg in (None, "effect_dependent"):
        w_eff = AbortWriter(60000, make_body_profile(mode_arg))
        assert w_eff.incarnation_allocation == "effect_dependent"
        assert w_eff.append_group([body_stream_opened]) == "committed"
        n1 = w_eff.allocate_incarnation(body_ref)
        assert n1 == 1
        assert (
            w_eff.append_group([make_pub(2, n1)], k_written=0, errno="ENOSPC")
            == "aborted"
        )
        # Zero physical effect relied on n=1 -> n=1 can be reused:
        assert w_eff.allocate_incarnation(body_ref, requested_n=1) == 1
        # Now physical placement relies on n=1 before the journal append aborts:
        w_eff.record_body_effect(body_ref, 1)
        assert (
            w_eff.append_group([make_pub(2, 1)], k_written=12, errno="ENOSPC")
            == "aborted"
        )
        assert w_eff.body_state({"body": body_ref, "present": True}) == {
            "outcome": "body_state",
            "state": "publishing",
        }
        try:
            w_eff.allocate_incarnation(body_ref, requested_n=1)
        except legacy.Reject as err:
            assert err.reason == "body_lifecycle"
        else:
            raise AssertionError("effect_dependent allowed reuse of effect-relied n=1")
        # Across restart, effect_relied_on_max=1 is still enforced and gaps (e.g. n=3) are valid:
        w_eff_reopened = w_eff.reopen_from_disk()
        try:
            w_eff_reopened.allocate_incarnation(body_ref, requested_n=1)
        except legacy.Reject as err:
            assert err.reason == "body_lifecycle"
        else:
            raise AssertionError(
                "effect_dependent after restart allowed reuse of effect-relied n=1"
            )
        n_gap = w_eff_reopened.allocate_incarnation(body_ref, requested_n=3)
        assert n_gap == 3
        assert w_eff_reopened.append_group([make_pub(2, n_gap)]) == "committed"
        assert w_eff_reopened.body_state({"body": body_ref, "present": True}) == {
            "outcome": "body_state",
            "state": "available",
        }

    # prewrite_durable_ledger mode:
    # - even on k=0 with zero physical body effects, n=1 is burned in the durable ledger before
    #   the write and cannot be reused in-process or after restart.
    w_led = AbortWriter(60000, make_body_profile("prewrite_durable_ledger"))
    assert w_led.append_group([body_stream_opened]) == "committed"
    assert w_led.allocate_incarnation(body_ref) == 1
    assert (
        w_led.append_group([make_pub(2, 1)], k_written=0, errno="ENOSPC") == "aborted"
    )
    for target_w in (w_led, w_led.reopen_from_disk()):
        try:
            target_w.allocate_incarnation(body_ref, requested_n=1)
        except legacy.Reject as err:
            assert err.reason == "body_lifecycle"
        else:
            raise AssertionError("prewrite_durable_ledger allowed reuse of burned n=1")
    w_led_reopened = w_led.reopen_from_disk()
    assert w_led_reopened.allocate_incarnation(body_ref) == 2
    assert w_led_reopened.append_group([make_pub(2, 2)]) == "committed"

    # body_removal_started and body_removed abort at seq = cut_seq + 1:
    # retry uses the SAME incarnation n (does not burn n).
    removal_started = {
        "v": 1,
        "seq": 3,
        "group": 3,
        "group_size": 1,
        "stream": "store.bodies",
        "type": "body_removal_started",
        "at": "2026-10-20T00:00:03.000000Z",
        "body": body_ref,
        "incarnation": 2,
        "reason": "retention",
        "authorization": {
            "ref": "policy:retention",
            "digest": "sha256:" + "a" * 64,
        },
        "provenance": {"actor_kind": "process", "actor": "sweeper"},
    }
    removal_done = {
        "v": 1,
        "seq": 4,
        "group": 4,
        "group_size": 1,
        "stream": "store.bodies",
        "type": "body_removed",
        "at": "2026-10-20T00:00:04.000000Z",
        "body": body_ref,
        "incarnation": 2,
    }
    assert (
        w_led_reopened.append_group([removal_started], k_written=15, errno="ENOSPC")
        == "aborted"
    )
    assert w_led_reopened.body_state({"body": body_ref, "present": True}) == {
        "outcome": "body_state",
        "state": "available",
    }
    assert w_led_reopened.append_group([removal_started]) == "committed"
    assert w_led_reopened.body_state({"body": body_ref, "present": True}) == {
        "outcome": "body_state",
        "state": "removal_pending",
    }
    assert (
        w_led_reopened.append_group([removal_done], k_written=15, errno="EDQUOT")
        == "aborted"
    )
    assert w_led_reopened.body_state({"body": body_ref, "present": False}) == {
        "outcome": "body_state",
        "state": "removal_pending",
    }
    assert w_led_reopened.append_group([removal_done]) == "committed"
    assert w_led_reopened.body_state({"body": body_ref, "present": False}) == {
        "outcome": "body_state",
        "state": "removed",
    }

    # MAX_SAFE_INTEGER exhaustion in allocate_incarnation refuses as numeric_domain.
    w_max = AbortWriter(60000, make_body_profile("prewrite_durable_ledger"))
    assert w_max.append_group([body_stream_opened]) == "committed"
    assert (
        w_max.allocate_incarnation(body_ref, requested_n=MAX_SAFE_INTEGER)
        == MAX_SAFE_INTEGER
    )
    try:
        w_max.allocate_incarnation(body_ref)
    except legacy.Reject as err:
        assert err.reason == "numeric_domain"
    else:
        raise AssertionError(
            "MAX_SAFE_INTEGER exhaustion did not refuse as numeric_domain"
        )

    # 3f. Recovery activation crash boundaries & stale-handle fencing (§2.B).
    def make_recovery_opened(
        prev_bytes, prefix_entries, post_state="available", floor=1
    ):
        img_dig = "sha256:" + hashlib.sha256(prev_bytes.encode("utf-8")).hexdigest()
        return {
            "v": 1,
            "seq": 1,
            "group": 1,
            "group_size": 1,
            "stream": "store.bodies.1",
            "type": "stream_opened",
            "at": "2026-10-20T00:00:10.000000Z",
            "mode": "single_revision",
            "failure_model": "survives_power_loss",
            "recovery": {
                "store": "store.alpha",
                "previous_stream": "store.bodies",
                "cut_seq": len(prefix_entries),
                "cut_digest": digest(prefix_entries),
                "quarantined_image_digest": img_dig,
                "reconciled_bodies": [
                    {
                        "body": body_ref,
                        "floor": floor,
                        "activation_observation": "present_valid",
                        "post_recovery_state": post_state,
                    }
                ],
                "authorization": {
                    "ref": "policy:recovery",
                    "digest": "sha256:" + "b" * 64,
                },
                "provenance": {"actor_kind": "human", "actor": "operator"},
            },
        }

    def _run_stale_handle_probe(w):
        stale_h = w.open_handle()
        w.disk_bytes += '{"corrupt_tail":'
        succ_open = make_recovery_opened(
            w.disk_bytes, [body_stream_opened, make_pub(2, 1)]
        )
        assert (
            w.activate_recovery(
                succ_open, [{"body": body_ref, "observation": "present_valid"}]
            )
            == "activated"
        )
        succ_pub = {
            **make_pub(2, 2, at="2026-10-20T00:00:11.000000Z"),
            "stream": "store.bodies.1",
        }
        try:
            w.append_with_handle(stale_h, [succ_pub])
        except legacy.Reject as err:
            return err.reason == "body_removal_stream"
        return False

    def _run_mid_activation_probe(w):
        w.disk_bytes += '{"corrupt_tail":'
        succ_open = make_recovery_opened(
            w.disk_bytes, [body_stream_opened, make_pub(2, 1)]
        )
        assert (
            w.activate_recovery(
                succ_open,
                [{"body": body_ref, "observation": "present_valid"}],
                crash_at="mid_activation",
            )
            == "crashed_mid_activation"
        )
        try:
            w.body_state({"body": body_ref, "present": True})
        except legacy.Reject as err:
            return err.reason == "inconsistent_cut"
        return False

    # Verify full stale-handle fencing (both append & unlink) and fresh handle success after recovery.
    w_rec = AbortWriter(60000, make_body_profile("prewrite_durable_ledger"))
    assert w_rec.append_group([body_stream_opened]) == "committed"
    assert w_rec.allocate_incarnation(body_ref) == 1
    assert w_rec.append_group([make_pub(2, 1)]) == "committed"
    stale_handle = w_rec.open_handle()
    w_rec.disk_bytes += '{"torn":'
    succ_header = make_recovery_opened(
        w_rec.disk_bytes, [body_stream_opened, make_pub(2, 1)]
    )
    assert (
        w_rec.activate_recovery(
            succ_header, [{"body": body_ref, "observation": "present_valid"}]
        )
        == "activated"
    )
    assert w_rec.profile["runtime"]["body_removal_stream"] == "store.bodies.1"
    assert w_rec.profile["runtime"]["quarantined_designated_streams"] == [
        "store.bodies"
    ]
    for stale_op in (
        lambda: w_rec.append_with_handle(
            stale_handle,
            [
                {
                    **make_pub(2, 2, at="2026-10-20T00:00:12.000000Z"),
                    "stream": "store.bodies.1",
                }
            ],
        ),
        lambda: w_rec.unlink_with_handle(stale_handle, body_ref, 1),
    ):
        try:
            stale_op()
        except legacy.Reject as err:
            assert err.reason == "body_removal_stream"
        else:
            raise AssertionError("stale predecessor handle was not fenced")
    fresh_handle = w_rec.open_handle()
    succ_rs = {
        **removal_started,
        "seq": 2,
        "group": 2,
        "stream": "store.bodies.1",
        "at": "2026-10-20T00:00:12.000000Z",
        "incarnation": 1,
    }
    assert w_rec.append_with_handle(fresh_handle, [succ_rs]) == "committed"
    assert w_rec.unlink_with_handle(fresh_handle, body_ref, 1) == "unlinked"

    # Crash after writing successor stream_opened before atomic profile swap -> reopen refuses stale profile.
    w_swap_crash = AbortWriter(60000, make_body_profile("prewrite_durable_ledger"))
    assert w_swap_crash.append_group([body_stream_opened]) == "committed"
    assert w_swap_crash.allocate_incarnation(body_ref) == 1
    assert w_swap_crash.append_group([make_pub(2, 1)]) == "committed"
    clean_prefix_bytes = w_swap_crash.disk_bytes
    w_swap_crash.disk_bytes += '{"torn":'
    succ_hdr_swap = make_recovery_opened(
        w_swap_crash.disk_bytes, [body_stream_opened, make_pub(2, 1)]
    )
    assert (
        w_swap_crash.activate_recovery(
            succ_hdr_swap,
            [{"body": body_ref, "observation": "present_valid"}],
            crash_at="after_successor_before_profile_swap",
        )
        == "crashed_before_profile_swap"
    )
    # Even if predecessor tail were clean on disk, unswapped profile with successor present refuses as stale.
    w_swap_crash.disk_bytes = clean_prefix_bytes
    try:
        w_swap_crash.reopen_from_disk()
    except legacy.Reject as err:
        assert err.reason == "body_removal_stream"
    else:
        raise AssertionError("crash before profile swap did not refuse stale profile")

    # Failed fsync on successor or profile during activation latches poisoned fail-closed.
    for fsync_kwargs in (
        {"successor_fsync_ok": False},
        {"profile_fsync_ok": False},
    ):
        w_act_fsync = AbortWriter(60000, make_body_profile("prewrite_durable_ledger"))
        assert w_act_fsync.append_group([body_stream_opened]) == "committed"
        assert w_act_fsync.allocate_incarnation(body_ref) == 1
        assert w_act_fsync.append_group([make_pub(2, 1)]) == "committed"
        w_act_fsync.disk_bytes += '{"torn":'
        sh = make_recovery_opened(
            w_act_fsync.disk_bytes, [body_stream_opened, make_pub(2, 1)]
        )
        assert (
            w_act_fsync.activate_recovery(
                sh, [{"body": body_ref, "observation": "present_valid"}], **fsync_kwargs
            )
            == "poisoned"
        )
        assert w_act_fsync.poisoned is True

    # 4. AST mutation controls on AbortWriter (F4): prove each abort/gate/incarnation guard
    #    kills its mutant.
    for mut_label, target_old, target_new, probe_fn in (
        (
            "allow_full_write_failed_fsync_truncate",
            "0 <= k_written < total_len",
            "0 <= k_written <= total_len",
            lambda cls: (
                lambda w: (
                    (
                        w.append_group([opened]),
                        w.append_group(
                            [entry2],
                            k_written=entry2_len,
                            errno="ENOSPC",
                            fsync_ok=False,
                        ),
                    )[1]
                    == "poisoned"
                )
            )(cls(60000)),
        ),
        (
            "skip_abort_fsync_gate",
            "same_file and truncate_ok and abort_fsync_ok and revalidated_ok",
            "same_file and truncate_ok and revalidated_ok",
            lambda cls: (
                lambda w: (
                    (
                        w.append_group([opened]),
                        w.append_group(
                            [entry2],
                            k_written=10,
                            errno="ENOSPC",
                            gate={"abort_fsync_ok": False},
                        ),
                    )[1]
                    == "poisoned"
                )
            )(cls(60000)),
        ),
        (
            "reuse_effect_relied_on_incarnation",
            "floor = max(committed_max, effect_max)",
            "floor = committed_max",
            lambda cls: (
                lambda w: (
                    (
                        w.append_group([body_stream_opened]),
                        w.record_body_effect(body_ref, 1),
                        w.append_group([make_pub(2, 1)], k_written=10, errno="ENOSPC"),
                        w.allocate_incarnation(body_ref),
                    )[3]
                    == 2
                )
            )(cls(60000, make_body_profile("effect_dependent"))),
        ),
        (
            "reuse_prewrite_burned_ledger_incarnation",
            "floor = max(committed_max, burned_max)",
            "floor = committed_max",
            lambda cls: (
                lambda w: (
                    (
                        w.append_group([body_stream_opened]),
                        w.allocate_incarnation(body_ref),
                        w.append_group([make_pub(2, 1)], k_written=0, errno="ENOSPC"),
                        w.allocate_incarnation(body_ref),
                    )[3]
                    == 2
                )
            )(cls(60000, make_body_profile("prewrite_durable_ledger"))),
        ),
        (
            "skip_stale_handle_generation_fence",
            "handle == (self.state.removal_stream, self.active_generation)",
            "True",
            lambda cls: (
                lambda w: (
                    w.append_group([body_stream_opened]),
                    w.allocate_incarnation(body_ref),
                    w.append_group([make_pub(2, 1)]),
                    _run_stale_handle_probe(w),
                )[3]
            )(cls(60000, make_body_profile("prewrite_durable_ledger"))),
        ),
        (
            "skip_mid_activation_exclusion",
            "self.activation_in_progress = True",
            "self.activation_in_progress = False",
            lambda cls: (
                lambda w: (
                    w.append_group([body_stream_opened]),
                    w.allocate_incarnation(body_ref),
                    w.append_group([make_pub(2, 1)]),
                    _run_mid_activation_probe(w),
                )[3]
            )(cls(60000, make_body_profile("prewrite_durable_ledger"))),
        ),
    ):
        assert probe_fn(AbortWriter) is True, ("baseline probe failed", mut_label)
        mut_src = source.replace(target_old, target_new, 1)
        assert mut_src != source, ("abort mutation target not found", mut_label)
        ns = {"__file__": __file__, "__name__": "abort_mutation"}
        exec(compile(ast.parse(mut_src), __file__, "exec"), ns)
        ns["SHAPE_CACHE"].update(SHAPE_CACHE)
        assert probe_fn(ns["AbortWriter"]) is False, (
            "abort mutant survived",
            mut_label,
        )


def check_case_stage3_shapes(c, path):
    """Run Stage 3 (shape) on destination items in order, stopping if Stage 1+2 refuses."""
    invalid = c.get("invalid_journal_entries", [])
    invalid_uses = c.get("invalid_use_steps", [])
    assert len(invalid_uses) == len(set(invalid_uses)) and all(
        0 <= i < len(c["steps"])
        and ("use" in c["steps"][i] or "raw_use" in c["steps"][i])
        for i in invalid_uses
    ), path
    invalid_requests = c.get("invalid_request_steps", [])
    assert len(invalid_requests) == len(set(invalid_requests)) and all(
        0 <= i < len(c["steps"])
        and ("request" in c["steps"][i] or "raw_request" in c["steps"][i])
        for i in invalid_requests
    ), path
    invalid_states = c.get("invalid_body_state_steps", [])
    assert len(invalid_states) == len(set(invalid_states)) and all(
        0 <= i < len(c["steps"])
        and ("body_state" in c["steps"][i] or "raw_body_state" in c["steps"][i])
        for i in invalid_states
    ), path
    invalid_receipts = c.get("invalid_receipt_steps", [])
    assert len(invalid_receipts) == len(set(invalid_receipts)) and all(
        0 <= i < len(c["steps"])
        and ("receipt" in c["steps"][i] or "raw_receipt" in c["steps"][i])
        for i in invalid_receipts
    ), path
    invalid_removals = c.get("invalid_removal_steps", [])
    assert len(invalid_removals) == len(set(invalid_removals)) and all(
        0 <= i < len(c["steps"])
        and (
            "removal_eligibility" in c["steps"][i]
            or "raw_removal_eligibility" in c["steps"][i]
        )
        for i in invalid_removals
    ), path
    invalid_seals = c.get("invalid_seal_steps", [])
    assert len(invalid_seals) == len(set(invalid_seals)) and all(
        0 <= i < len(c["steps"])
        and ("verify_seal" in c["steps"][i] or "raw_verify_seal" in c["steps"][i])
        for i in invalid_seals
    ), path

    def abort_stage12(err):
        assert err.reason in ("ingest", "numeric_domain"), (path, err)
        assert c["expected"] == {"refusal": err.reason}, (path, err, c["expected"])

    if "raw_journal_ndjson" in c:
        try:
            journal_entries = legacy.parse_wire_lines(
                c["raw_journal_ndjson"], "raw_journal_ndjson", "journal_entry"
            )
        except legacy.Reject as err:
            abort_stage12(err)
            return
    elif "raw_journal" in c:
        journal_entries = c["raw_journal"]
    else:
        journal_entries = c["journal"]
    assert len(invalid) == len(set(invalid)) and all(
        0 <= i < len(journal_entries) for i in invalid
    ), path

    if "raw_profile" in c:
        try:
            legacy.parse_wire(c["raw_profile"], "stratum_profile", "raw_profile")
        except legacy.Reject as err:
            abort_stage12(err)
            return
        assert shape_raw(c["raw_profile"], "stratum-profile"), path
    elif "profile" in c:
        assert shape(c["profile"], "stratum-profile"), path

    if "candidate_streams" in c:
        for ce in c["candidate_streams"]:
            assert shape(ce, "journal-entry"), path
    if "sharing_streams" in c:
        for sj in c["sharing_streams"]:
            for se in sj:
                assert shape(se, "journal-entry"), path

    if "raw_journal_ndjson" in c:
        for i, entry in enumerate(journal_entries):
            assert shape(entry, "journal-entry") == (i not in invalid), path
    elif "raw_journal" in c:
        for i, raw in enumerate(c["raw_journal"]):
            try:
                legacy.parse_wire(raw, "journal_entry", f"raw_journal[{i}]")
            except legacy.Reject as err:
                abort_stage12(err)
                return
            assert shape_raw(raw, "journal-entry") == (i not in invalid), path
    else:
        for i, e in enumerate(c["journal"]):
            assert shape(e, "journal-entry") == (i not in invalid), path

    for step_index, step in enumerate(c["steps"]):
        if "raw_request" in step:
            try:
                legacy.parse_wire(
                    step["raw_request"],
                    "protection_request",
                    f"steps[{step_index}].raw_request",
                )
            except legacy.Reject as err:
                abort_stage12(err)
                return
            assert shape_raw(step["raw_request"], "protection-request") == (
                step_index not in invalid_requests
            ), path
        elif "request" in step:
            assert shape(step["request"], "protection-request") == (
                step_index not in invalid_requests
            ), path
        elif "raw_use" in step:
            try:
                legacy.parse_wire(
                    step["raw_use"], "export_request", f"steps[{step_index}].raw_use"
                )
            except legacy.Reject as err:
                abort_stage12(err)
                return
            assert shape_raw(step["raw_use"], "export-request") == (
                step_index not in invalid_uses
            ), path
        elif "use" in step:
            assert shape(step["use"], "export-request") == (
                step_index not in invalid_uses
            ), path
        elif "raw_receipt" in step:
            try:
                parsed_rcpt = _parse_raw_receipt_step(
                    step["raw_receipt"], f"steps[{step_index}].raw_receipt"
                )
            except legacy.Reject as err:
                abort_stage12(err)
                return
            schema = (
                "write-receipt"
                if parsed_rcpt.get("kind") in ("receipt", "reconciliation")
                else "protection-receipt"
            )
            assert shape_raw(step["raw_receipt"], schema) == (
                step_index not in invalid_receipts
            ), path
        elif "receipt" in step:
            schema = (
                "write-receipt"
                if step["receipt"].get("kind") in ("receipt", "reconciliation")
                else "protection-receipt"
            )
            assert shape(step["receipt"], schema) == (
                step_index not in invalid_receipts
            ), path
        elif "raw_entry" in step:
            try:
                legacy.parse_wire(
                    step["raw_entry"],
                    "journal_entry",
                    f"steps[{step_index}].raw_entry",
                )
            except legacy.Reject as err:
                abort_stage12(err)
                return
            assert shape_raw(step["raw_entry"], "journal-entry"), path
        elif "entry" in step:
            assert shape(step["entry"], "journal-entry"), path
        elif "raw_body_state" in step:
            try:
                legacy.parse_wire(
                    step["raw_body_state"],
                    "body_state_request",
                    f"steps[{step_index}].raw_body_state",
                )
            except legacy.Reject as err:
                abort_stage12(err)
                return
            assert shape_raw(step["raw_body_state"], "body-state-request") == (
                step_index not in invalid_states
            ), path
        elif "body_state" in step:
            assert shape(step["body_state"], "body-state-request") == (
                step_index not in invalid_states
            ), path
        elif "raw_removal_eligibility" in step:
            try:
                legacy.parse_wire(
                    step["raw_removal_eligibility"],
                    "removal_eligibility",
                    f"steps[{step_index}].raw_removal_eligibility",
                )
            except legacy.Reject as err:
                abort_stage12(err)
                return
            assert shape_raw(
                step["raw_removal_eligibility"], "removal-eligibility-request"
            ) == (step_index not in invalid_removals), path
        elif "removal_eligibility" in step:
            assert shape(
                step["removal_eligibility"], "removal-eligibility-request"
            ) == (step_index not in invalid_removals), path
        elif "raw_verify_seal" in step:
            try:
                vctx = legacy.parse_wire(
                    step["raw_verify_seal"],
                    "seal_context",
                    f"steps[{step_index}].raw_verify_seal",
                )
            except legacy.Reject as err:
                abort_stage12(err)
                return
            if "raw_seal" in vctx:
                assert shape_raw(vctx["raw_seal"], "stream-seal") == (
                    step_index not in invalid_seals
                ), path
            else:
                assert shape(vctx["seal"], "stream-seal") == (
                    step_index not in invalid_seals
                ), path
        elif "verify_seal" in step:
            vctx = step["verify_seal"]
            if "raw_seal" in vctx:
                assert shape_raw(vctx["raw_seal"], "stream-seal") == (
                    step_index not in invalid_seals
                ), path
            else:
                assert shape(vctx["seal"], "stream-seal") == (
                    step_index not in invalid_seals
                ), path


def main():
    check_integral()
    suite_error_controls()
    # A0: the v1 record envelope is the v0 envelope with v const 1 plus only
    # the optional produced_at field. The embedded copy in journal entries
    # must equal the standalone schema's record definition.
    original = load(BASE.parent / "v0/record.schema.json")
    amended = load(BASE / "record.schema.json")
    assert envelope_matches(original, amended), "record_envelope_drift"
    drifts = []
    for field in ("$schema", "title", "$ref"):
        changed = copy.deepcopy(amended)
        changed[field] = "changed"
        drifts.append((field, changed))
    changed = copy.deepcopy(amended)
    changed["$defs"]["record"]["properties"]["v"] = {"const": 0}
    drifts.append(("v still 0", changed))
    changed = copy.deepcopy(amended)
    changed["$defs"]["record"]["required"].append("produced_at")
    drifts.append(("produced_at required", changed))
    changed = copy.deepcopy(amended)
    changed["$defs"]["record"]["properties"]["labels"]["maxItems"] = 65
    drifts.append(("other field changed", changed))
    changed = copy.deepcopy(amended)
    changed["$defs"]["record"]["properties"]["observed"] = {"type": "string"}
    drifts.append(("extra field", changed))
    changed = copy.deepcopy(amended)
    changed["$defs"]["instant"]["pattern"] = "^.*$"
    drifts.append(("loose instant", changed))
    for label, changed in drifts:
        assert not envelope_matches(original, changed), ("drift control", label)
    definitions = {}
    for path in sorted(BASE.glob("*.schema.json")):
        for name, definition in load(path).get("$defs", {}).items():
            if name in definitions:
                assert definition == definitions[name], (path, name)
            definitions[name] = definition
    cases = sorted((BASE / "vectors").glob("*.json"))
    assert cases, "missing conformance corpus"
    rejects = sorted((BASE / "rejects/schema").glob("reject-*.json"))
    examples = sorted((BASE / "examples").glob("*.json"))
    inventory = (BASE / "rejects/README.md").read_text(encoding="utf-8")
    listed = re.findall(r"\]\((?:schema/|\.\./vectors/)([^)]+)\)", inventory)
    assert sorted(listed) == sorted(p.name for p in cases + rejects), (
        "fixture inventory drift"
    )
    assert rejects, "missing schema rejects"
    prewarm_shape_cache(cases, rejects, examples)
    for path in cases:
        c = load(path)
        check_case_stage3_shapes(c, path)
        got = check_case(c)
        assert got == c["expected"], (path.name, got, c["expected"])
        for result in got.get("results", []):
            if result.get("kind") in ("reconciliation", "receipt"):
                assert shape(result, "write-receipt"), (path.name, result)
                continue
            if result["outcome"] in (
                "journaled",
                "receipt_verified",
                "body_state",
                "removal_eligibility",
                "seal_verified",
            ):
                continue
            name = (
                "export-result"
                if result["outcome"] in ("allowed", "refused")
                else "protection-receipt"
            )
            assert shape(result, name), (path.name, result)
        print("[ok]", path.name)
    for path in rejects:
        case = load(path)
        assert shape(case["baseline"], case["schema"]), path
        assert not shape(case["reject"], case["schema"]), path
        legacy.check_pair(case["baseline"], case["reject"], path.name)
    for path in examples:
        assert shape(load(path), path.name.split(".")[0]), path
        if path.name.startswith("stratum-profile."):
            legacy.check_profile(load(path), path.name)
    reason_controls(cases)
    count = mutation_controls()
    timestamp_and_abort_controls()
    assert SUITE_CALLS == 1, f"expected 1 batched suite call, got {SUITE_CALLS}"
    print(
        f"Durable-record/v1 logical controls PASS: {len(cases)} vectors; {len(rejects)} schema rejects; {count} mutation witnesses; {SUITE_CALLS} batched suite run. No store qualification."
    )


if __name__ == "__main__":
    main()
