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
import re
from pathlib import Path
import subprocess
import tempfile

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


def pin_key(p):
    return p["stream"], p["record_id"], p["revision"], p["digest"]


def require(ok, reason):
    if not ok:
        raise legacy.Reject(reason, "durable-record/v1")


class State:
    def __init__(self, window):
        require(type(window) is int and window > 0, "idempotency_window")
        self.window = window
        self.journal = []
        self.records = {}
        self.holds = {}
        self.active = set()
        self.keys = {}
        self.retired = set()
        self.stream = None
        self.failure_model = None

    def remember(self, key, identity, at, receipt):
        self.keys[key] = (identity, legacy.instant(at), copy.deepcopy(receipt))

    def live(self, key, at):
        prior = self.keys.get(key)
        if prior and timedelta(0) <= legacy.instant(at) - prior[1] < timedelta(
            milliseconds=self.window
        ):
            return prior
        return None

    def ingest(self, e):
        # The v0 journal invariants also apply to the expanded v1 vocabulary.
        require(e["v"] == 1, "journal_version")
        # Validate complete groups; partial groups cannot be observed by requests.
        if e["seq"] == e["group"] + e["group_size"] - 1:
            legacy.check_journal(self.journal + [e], "v1")
        if self.journal:
            require(
                legacy.instant(e["at"]) >= legacy.instant(self.journal[-1]["at"]),
                "journal_time",
            )
        typ = e["type"]
        if typ == "stream_opened":
            self.stream, self.failure_model = e["stream"], e["failure_model"]
        elif typ in ("appended", "revised"):
            require(e["record"]["record_id"] not in self.retired, "retired_record")
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
            self.retired.add(e["record_id"])
        self.journal.append(copy.deepcopy(e))

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
        if legacy.instant(r["observed_at"]) > legacy.instant(at):
            return "observation_order"
        if r["action"] == "restore":
            if r["restores"] not in self.active:
                return "hold_not_active"
            hold = self.holds[r["restores"]]
            if any(r.get(k) != hold.get(k) for k in ("target", "scope", "body")):
                return "hold_mismatch"
            if legacy.instant(r["observed_at"]) < legacy.instant(hold["observed_at"]):
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

    def apply(self, r, seq):
        if r["action"] == "tombstone":
            self.holds[seq] = copy.deepcopy(r)
            self.active.add(seq)
        else:
            self.active.remove(r["restores"])

    def request(self, r, at):
        legacy.check_journal(self.journal, "v1")
        require(
            legacy.instant(at) >= legacy.instant(self.journal[-1]["at"]), "journal_time"
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
        require(
            received["outcome"] in ("tombstoned", "untombstoned"), "receipt_mismatch"
        )
        require(received["v"] == 1, "receipt_mismatch")
        seq = received["seq"]
        require(1 <= seq <= len(self.journal), "receipt_mismatch")
        entry = self.journal[seq - 1]
        require(entry["type"] in ("tombstoned", "untombstoned"), "receipt_mismatch")
        require(received == self.receipt(entry["request"], seq), "receipt_mismatch")
        return {"outcome": "receipt_verified"}

    def reconcile(self, key, at, attempted_at):
        legacy.check_journal(self.journal, "v1")
        require(
            legacy.instant(at) >= legacy.instant(self.journal[-1]["at"]), "journal_time"
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
        ):
            return refused("protection_unavailable")
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


def evaluate(case):
    s = State(case["window_ms"])
    legacy.check_journal(case["journal"], "v1")
    for e in case["journal"]:
        s.ingest(e)
    results = []
    for step in case["steps"]:
        if "request" in step:
            results.append(s.request(step["request"], step["at"]))
        elif "receipt" in step:
            results.append(s.verify_receipt(step["receipt"]))
        elif "reconcile" in step:
            results.append(s.reconcile(**step["reconcile"]))
        elif "entry" in step:
            s.ingest(step["entry"])
            results.append({"outcome": "journaled"})
        else:
            results.append(s.use(step["use"]))
    legacy.check_journal(s.journal, "v1")
    return {
        "results": results,
        "active_holds": sorted(s.active),
        "journal_size": len(s.journal),
    }


def shape(doc, name):
    with tempfile.TemporaryDirectory(prefix="record-v1-shape-") as tmp:
        p = Path(tmp) / "input.json"
        p.write_text(json.dumps(doc), encoding="utf-8")
        result = subprocess.run(
            [
                "goneat",
                "validate",
                "data",
                "--schema-file",
                str(BASE / (name + ".schema.json")),
                "--data",
                str(p),
            ],
            capture_output=True,
            text=True,
        )
        if result.returncode not in (0, 1):
            raise RuntimeError(result.stderr)
        return result.returncode == 0


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


def envelope_matches(original, copied):
    def structural(document):
        return {
            key: value
            for key, value in document.items()
            if key not in ("$id", "description")
        }

    return structural(original) == structural(copied)


def reason_controls(cases):
    source = Path(__file__).read_text().split("def reason_controls(", 1)[0]
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
    standard = (ROOT / "docs/standards/durable-record-v1-contract.md").read_text()
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
    source = Path(__file__).read_text().split("def reason_controls(", 1)[0]
    specs = [
        ("ingest", "slide_dedup", "", 0, "dedup-non-sliding", None),
        ("use", "if_reason", "invalid_purpose", 0, "unknown-purpose", None),
        ("ingest", "require", "retired_record", 0, "write-retired-record", None),
        ("ingest", "require", "accepted_key_reuse", 0, "content-live-key-reuse", None),
        ("ingest", "require", "accepted_key_reuse", 1, "journal-live-key-reuse", None),
        ("ingest", "require", "journal_time", 0, "journal-backwards-time", None),
        ("ingest", "require", "journal_version", 0, "v0-journal", None),
        ("ingest", "require", "operation_type", 0, "journal-operation-type", None),
        ("ingest", "require", "request_digest", 0, "journal-digest-mismatch", None),
        ("ingest", "require", "invalid_deduplication", 0, "dedup-protection", None),
        ("ingest", "require", "invalid_deduplication", 0, "dedup-absent", None),
        ("State", "require", "idempotency_window", 0, "zero-window", None),
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
            4,
            "receipt-wrong-digest",
            None,
        ),
    ]
    for method, kind, value, occurrence, witness, alternative in specs:
        tree = ast.parse(source)
        state = next(
            n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == "State"
        )
        scope = (
            state
            if method == "State"
            else next(
                n
                for n in state.body
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
                "journaled",
                "receipt_verified",
                "allowed",
            }
            assert all(r.get("outcome") in accepted for r in outcome["results"]), (
                "mutation did not accept",
                witness,
                outcome,
            )
        print("[ok] mutation:", witness, kind, value)
    return len(specs)


def main():
    # The content envelope is reused exactly, not a new digest-bearing version.
    original = load(BASE.parent / "v0/record.schema.json")
    copied = load(BASE / "record.schema.json")
    assert envelope_matches(original, copied), "record_envelope_drift"
    for field in ("$schema", "title", "$ref"):
        changed = copy.deepcopy(copied)
        changed[field] = "changed"
        assert not envelope_matches(original, changed), ("drift control", field)
    definitions = {}
    for path in sorted(BASE.glob("*.schema.json")):
        for name, definition in load(path).get("$defs", {}).items():
            if name in definitions:
                assert definition == definitions[name], (path, name)
            definitions[name] = definition
    cases = sorted((BASE / "vectors").glob("*.json"))
    assert cases, "missing conformance corpus"
    rejects = sorted((BASE / "rejects/schema").glob("reject-*.json"))
    inventory = (BASE / "rejects/README.md").read_text()
    listed = re.findall(r"\]\((?:schema/|\.\./vectors/)([^)]+)\)", inventory)
    assert sorted(listed) == sorted(p.name for p in cases + rejects), (
        "fixture inventory drift"
    )
    assert rejects, "missing schema rejects"
    for path in cases:
        c = load(path)
        invalid = c.get("invalid_journal_entries", [])
        assert len(invalid) == len(set(invalid)) and all(
            0 <= i < len(c["journal"]) for i in invalid
        )
        for i, e in enumerate(c["journal"]):
            assert shape(e, "journal-entry") == (i not in invalid), path
        invalid_uses = c.get("invalid_use_steps", [])
        assert len(invalid_uses) == len(set(invalid_uses)) and all(
            0 <= i < len(c["steps"]) and "use" in c["steps"][i] for i in invalid_uses
        )
        invalid_receipts = c.get("invalid_receipt_steps", [])
        assert len(invalid_receipts) == len(set(invalid_receipts)) and all(
            0 <= i < len(c["steps"]) and "receipt" in c["steps"][i]
            for i in invalid_receipts
        )
        for step_index, step in enumerate(c["steps"]):
            if "request" in step:
                assert shape(step["request"], "protection-request"), path
            elif "use" in step:
                assert shape(step["use"], "export-request") == (
                    step_index not in invalid_uses
                ), path
            elif "receipt" in step:
                assert shape(step["receipt"], "protection-receipt") == (
                    step_index not in invalid_receipts
                ), path
            elif "entry" in step:
                assert shape(step["entry"], "journal-entry"), path
        got = check_case(c)
        assert got == c["expected"], (path.name, got, c["expected"])
        for result in got.get("results", []):
            if result.get("kind") == "reconciliation":
                assert shape(result, "write-receipt"), (path.name, result)
                continue
            if result["outcome"] in ("journaled", "receipt_verified"):
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
    for path in sorted((BASE / "examples").glob("*.json")):
        assert shape(load(path), path.name.split(".")[0]), path
        if path.name.startswith("stratum-profile."):
            legacy.check_profile(load(path), path.name)
    reason_controls(cases)
    count = mutation_controls()
    print(
        f"Durable-record/v1 logical controls PASS: {len(cases)} vectors; {len(rejects)} schema rejects; {count} mutation witnesses. No store qualification."
    )


if __name__ == "__main__":
    main()
