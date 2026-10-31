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
        self.bodies = {}

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
            legacy.check_journal(self.journal + [e], "v1")
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
        # phase is published, started or removed.
        key = (e["body"]["digest"], e["body"]["length"])
        latest, phase = self.bodies.get(key, (0, None))
        n, typ = e["incarnation"], e["type"]
        if typ == "body_published":
            # A later publication closes an open removal as abandoned.
            require(n > latest, "body_lifecycle")
            self.bodies[key] = (n, "published")
        elif typ == "body_removal_started":
            require(n == latest and phase == "published", "body_lifecycle")
            self.bodies[key] = (n, "started")
        else:
            require(n == latest and phase == "started", "body_lifecycle")
            self.bodies[key] = (n, "removed")

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
        legacy.check_journal(self.journal, "v1")
        require(self.removal_stream == self.stream, "body_removal_stream")
        body = step["body"]
        _, phase = self.bodies.get((body["digest"], body["length"]), (0, None))
        present = step["present"]
        state = {
            None: "publishing" if present else "unpublished",
            "published": "available" if present else "integrity_loss",
            "started": "removal_pending",
            "removed": "publishing" if present else "removed",
        }[phase]
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
        legacy.check_journal(self.journal, "v1")
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
        legacy.check_journal(self.journal, "v1")
        require(
            self.instant(at) >= self.instant(self.journal[-1]["at"]), "journal_time"
        )
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
            if received.get("kind") == "receipt"
            else "protection_receipt",
            "receipt",
        )
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

    def reconcile(self, key, at, attempted_at):
        legacy.check_journal(self.journal, "v1")
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
        legacy.check_journal(self.journal, "v1")

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


def chain(case):
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
            return copy.deepcopy(first_journal), UNOBSERVED
        source, above = upstream if upstream else (None, None)
        state = observed(
            journal, case["window_ms"], source, above, raw_entries=raw_entries
        )
        continuation = state is not None and state.continues is not None
        upstream = (copy.deepcopy(journal), state if continuation else None)
    return upstream


def _parse_raw_receipt_step(raw_text, where):
    ast_node = legacy.parse_raw_json(raw_text, where)
    kind = (
        "write_receipt"
        if isinstance(ast_node, dict) and ast_node.get("kind") == "receipt"
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
    if "raw_journal" in out:
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
        s.removal_stream = case["profile"]["runtime"].get("body_removal_stream")
    if "source_journal" in case or "raw_source_journal" in case:
        s.source, s.upstream = chain(case)
    legacy.check_journal(case["journal"], "v1")
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
        elif "source_entry" in step:
            # The source journal advances outside this stream.
            case = copy.deepcopy(case)
            case["source_journal"].append(copy.deepcopy(step["source_entry"]))
            s.source, s.upstream = chain(case)
            results.append({"outcome": "journaled"})
        elif "raw_source_entry" in step:
            case = copy.deepcopy(case)
            case["raw_source_journal"].append(step["raw_source_entry"])
            s.source, s.upstream = chain(case)
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
    legacy.check_journal(s.journal, "v1")
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
    # (and the instant definition it references). Nothing else may differ.
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
    if instant_def.get("pattern") != "^" + CANONICAL_INSTANT.pattern + "$":
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
                if node.func.id == "refused"
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
                        and child.value.func.id == "refused"
                        and child.value.args[0].value == value
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
        assert "refusal" not in outcome, ("mutation still refused", witness, outcome)
        if alternative:
            field, expected, index = alternative
            named = copy.deepcopy(case["expected"])
            if field == "reason":
                named["results"][index] = {"outcome": "refused", "reason": expected}
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
                "allowed",
                "body_state",
            }
            assert all(r.get("outcome") in accepted for r in outcome["results"]), (
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

    if "raw_journal" in c:
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
                if parsed_rcpt.get("kind") == "receipt"
                else "protection-receipt"
            )
            add_key(schema, step["raw_receipt"])
        elif "receipt" in step:
            schema = (
                "write-receipt"
                if step["receipt"].get("kind") == "receipt"
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

    for result in c["expected"].get("results", []):
        if result.get("kind") in ("reconciliation", "receipt"):
            add_key("write-receipt", json.dumps(result))
            continue
        if result["outcome"] in ("journaled", "receipt_verified", "body_state"):
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

    items = [
        (f"k{idx}", schema_rel, payload)
        for idx, (schema_rel, payload) in enumerate(unique_keys)
    ]
    SUITE_CALLS += 1
    verdicts = legacy.shape_suite(items, default_ver="v1")
    for idx, key in enumerate(unique_keys):
        SHAPE_CACHE[key] = verdicts[f"k{idx}"]


def suite_error_controls():
    """Verify batched goneat suite rejects parse/range/schema errors as RuntimeError."""
    for label, bad_item in (
        ("malformed_json", ("bad", "v1/record.schema.json", "{\n")),
        (
            "float64_overflow",
            ("huge", "v1/record.schema.json", '{"v":1,"revision":1e400}\n'),
        ),
        ("missing_schema", ("missing", "v1/nonexistent.schema.json", "{}\n")),
    ):
        try:
            legacy.shape_suite([bad_item], default_ver="v1")
        except RuntimeError:
            pass
        else:
            raise AssertionError(f"suite_error_controls did not fail on {label}")


def check_case_stage3_shapes(c, path):
    """Run Stage 3 (shape) on destination items in order, stopping if Stage 1+2 refuses."""
    invalid = c.get("invalid_journal_entries", [])
    j_len = len(c["raw_journal"]) if "raw_journal" in c else len(c["journal"])
    assert len(invalid) == len(set(invalid)) and all(0 <= i < j_len for i in invalid), (
        path
    )
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

    def abort_stage12(err):
        assert err.reason in ("ingest", "numeric_domain"), (path, err)
        assert c["expected"] == {"refusal": err.reason}, (path, err, c["expected"])

    if "raw_profile" in c:
        try:
            legacy.parse_wire(c["raw_profile"], "stratum_profile", "raw_profile")
        except legacy.Reject as err:
            abort_stage12(err)
            return
        assert shape_raw(c["raw_profile"], "stratum-profile"), path
    elif "profile" in c:
        assert shape(c["profile"], "stratum-profile"), path

    if "raw_journal" in c:
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
                if parsed_rcpt.get("kind") == "receipt"
                else "protection-receipt"
            )
            assert shape_raw(step["raw_receipt"], schema) == (
                step_index not in invalid_receipts
            ), path
        elif "receipt" in step:
            schema = (
                "write-receipt"
                if step["receipt"].get("kind") == "receipt"
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
            if result["outcome"] in ("journaled", "receipt_verified", "body_state"):
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
    assert SUITE_CALLS == 1, f"expected 1 batched suite call, got {SUITE_CALLS}"
    print(
        f"Durable-record/v1 logical controls PASS: {len(cases)} vectors; {len(rejects)} schema rejects; {count} mutation witnesses; {SUITE_CALLS} batched suite run. No store qualification."
    )


if __name__ == "__main__":
    main()
