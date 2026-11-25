#!/usr/bin/env python3
"""Controls for contract: record-collection/v0.

Declared invocation set: python3 (stdlib only), goneat (JSON Schema validation).
Canonical digests reuse scripts/rfc8785-canonicalize.py.

Asserts:
  - every example validates against the schema its filename prefix names
  - record-collection.schema.json is mirrored exactly inside collection-document.schema.json
  - shared $defs (digest, pin) match schemas/durable-record/v1/record.schema.json verbatim
  - baseline-* schema reject fixtures pass, reject-* fail, each pair differs in
    exactly one leaf, and rejects/schema/index.json agrees with the files
  - every behavioral vector in vectors/ matches the reference verifier and
    vectors/INVENTORY.json matches the vector directory exactly
  - every semantic reject in rejects/semantic/ is caught by the reference verifier
  - fixtures carry no machine-local paths or credential-shaped strings
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib
import re
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASE = ROOT / "schemas/record-collection/v0"
DR_RECORD_SCHEMA = ROOT / "schemas/durable-record/v1/record.schema.json"

_spec = importlib.util.spec_from_file_location(
    "jcs", ROOT / "scripts/rfc8785-canonicalize.py"
)
jcs = importlib.util.module_from_spec(_spec)
sys.modules["jcs"] = jcs
_spec.loader.exec_module(jcs)

failures: list[str] = []


def fail(msg: str) -> None:
    failures.append(msg)
    print(f"[!!] {msg}", file=sys.stderr)


def jcs_digest(value) -> str:
    return "sha256:" + hashlib.sha256(jcs.jcs_dumps(value).encode("utf-8")).hexdigest()


def bytes_digest(raw: bytes) -> str:
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def valid_with_schema_path(schema_path: pathlib.Path, data: pathlib.Path) -> bool:
    r = subprocess.run(
        [
            "goneat",
            "validate",
            "data",
            "--schema-file",
            str(schema_path),
            "--data",
            str(data),
        ],
        capture_output=True,
    )
    return r.returncode == 0


def valid(schema: str, data: pathlib.Path) -> bool:
    return valid_with_schema_path(BASE / schema, data)


def valid_obj(schema_path: pathlib.Path, obj, tmp_dir: pathlib.Path, name: str) -> bool:
    p = tmp_dir / name
    p.write_text(json.dumps(obj, indent=2) + "\n", encoding="utf-8")
    return valid_with_schema_path(schema_path, p)


def load(p: pathlib.Path):
    return json.loads(p.read_text(encoding="utf-8"))


PREFIX_SCHEMA = {
    "collection-document": "collection-document.schema.json",
    "record-collection": "record-collection.schema.json",
}


def diff_count(a, b) -> int:
    if isinstance(a, dict) and isinstance(b, dict):
        n = 0
        for k in set(a) | set(b):
            if k not in a or k not in b:
                n += 1
            else:
                n += diff_count(a[k], b[k])
        return n
    if isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            return 1
        return sum(diff_count(x, y) for x, y in zip(a, b))
    return 0 if a == b and type(a) is type(b) else 1


def pin_key(pin: dict) -> tuple[str, str, str, int]:
    return (pin["digest"], pin["stream"], pin["record_id"], pin["revision"])


def pin_ref(pin: dict) -> tuple[str, str, int]:
    return (pin["stream"], pin["record_id"], pin["revision"])


def canonical_set_payload(members: list[dict], **extra) -> dict:
    ordered = sorted(members, key=lambda m: pin_key(m["pin"]))
    out = {
        "kind": "set",
        "members": ordered,
        "member_count": len(ordered),
    }
    out.update(extra)
    return out


def verify_collection_payload(
    collection: dict, predecessor: dict | None = None
) -> tuple[str, str | None]:
    """Validate structural/semantic invariants of a collection payload."""
    kind = collection["kind"]
    members = collection["members"]
    member_count = collection["member_count"]
    allow_repeats = collection.get("allow_repeats", False)
    append_only = collection.get("append_only", False)

    if member_count != len(members):
        return ("refuse", "member_count_mismatch")

    keys = [pin_key(m["pin"]) for m in members]
    if kind == "set":
        if allow_repeats:
            return ("refuse", "set_allow_repeats_forbidden")
        if len(set(keys)) != len(keys):
            return ("refuse", "duplicate_member")
        if keys != sorted(keys):
            return ("refuse", "set_not_canonical")
    else:
        if not allow_repeats and len(set(keys)) != len(keys):
            return ("refuse", "duplicate_member")

    if predecessor is not None:
        if predecessor.get("append_only", False):
            if not append_only:
                return ("refuse", "append_only_removed")
            if predecessor["kind"] != kind:
                return ("refuse", "append_only_kind_changed")
            prev_keys = [pin_key(m["pin"]) for m in predecessor["members"]]
            if kind == "set":
                prev_set = set(prev_keys)
                curr_set = set(keys)
                if not (prev_set < curr_set):
                    return ("refuse", "append_only_not_extending")
            else:
                if len(keys) <= len(prev_keys):
                    return ("refuse", "append_only_not_extending")
                if keys[: len(prev_keys)] != prev_keys:
                    return ("refuse", "append_only_prefix_mismatch")

    return ("ok", None)


def evaluate_vector(vec: dict) -> dict:
    """Reference verifier for record-collection/v0 behavioral vectors."""
    mode = vec["mode"]

    if mode == "canonical_set_equivalence":
        left = canonical_set_payload(
            vec["input_a_members"], **vec.get("extra_fields", {})
        )
        right = canonical_set_payload(
            vec["input_b_members"], **vec.get("extra_fields", {})
        )
        d_left = jcs_digest(left)
        d_right = jcs_digest(right)
        same = (left == right) and (d_left == d_right)
        return {
            "outcome": "ok" if same else "refuse",
            "reason": None if same else "canonical_digest_mismatch",
            "canonical_digest": d_left,
        }

    collection = vec["collection"]
    predecessor = vec.get("predecessor")
    outcome, reason = verify_collection_payload(collection, predecessor)
    if outcome != "ok":
        return {
            "outcome": "refuse",
            "reason": reason,
            "member_outcomes": [],
            "collection_digest": jcs_digest(collection),
        }

    store = vec.get("store")
    closure_claim = collection.get("closure_claim", "none")
    if store is None:
        if closure_claim == "complete_at_write" and len(collection["members"]) > 0:
            return {
                "outcome": "refuse",
                "reason": "closure_evidence_absent",
                "closure_outcome": "closure_evidence_absent",
                "member_outcomes": [],
                "collection_digest": jcs_digest(collection),
            }
        result = {
            "outcome": "ok",
            "reason": None,
            "member_outcomes": [],
            "collection_digest": jcs_digest(collection),
        }
        if closure_claim == "complete_at_write":
            result["closure_outcome"] = "closure_verified_at_write"
        if collection["kind"] == "list":
            result["ordered_record_ids"] = [
                m["pin"]["record_id"] for m in collection["members"]
            ]
        return result

    records_by_ref: dict[tuple[str, str, int], dict] = {}
    for entry in store.get("records", []):
        rec = entry["record"]
        ref = (rec["stream"], rec["record_id"], rec["revision"])
        records_by_ref[ref] = entry

    bodies_by_digest: dict[str, dict] = {}
    for b in store.get("bodies", []):
        bodies_by_digest[b["declared_digest"]] = b

    member_outcomes: list[dict] = []
    first_integrity_failure: str | None = None
    first_availability_failure: str | None = None

    for m in collection["members"]:
        pin = m["pin"]
        ref = pin_ref(pin)
        entry = records_by_ref.get(ref)
        if entry is None:
            member_outcomes.append({"pin": pin, "status": "member_unresolved"})
            if first_availability_failure is None:
                first_availability_failure = "member_unresolved"
            continue

        if entry.get("status") == "held":
            member_outcomes.append({"pin": pin, "status": "member_held"})
            if first_availability_failure is None:
                first_availability_failure = "member_held"
            continue

        rec = entry["record"]
        actual_rec_digest = jcs_digest(rec)
        if actual_rec_digest != pin["digest"]:
            member_outcomes.append({"pin": pin, "status": "member_digest_mismatch"})
            if first_integrity_failure is None:
                first_integrity_failure = "member_digest_mismatch"
            continue

        body = rec.get("body")
        if body is not None:
            b_entry = bodies_by_digest.get(body["digest"])
            if b_entry is None or b_entry.get("status") == "missing":
                member_outcomes.append({"pin": pin, "status": "body_missing"})
                if first_availability_failure is None:
                    first_availability_failure = "body_missing"
                continue
            if b_entry.get("status") == "held":
                member_outcomes.append({"pin": pin, "status": "body_held"})
                if first_availability_failure is None:
                    first_availability_failure = "body_held"
                continue
            raw_bytes = b_entry["content_utf8"].encode("utf-8")
            actual_body_digest = bytes_digest(raw_bytes)
            if actual_body_digest != body["digest"] or len(raw_bytes) != body["length"]:
                member_outcomes.append({"pin": pin, "status": "body_digest_mismatch"})
                if first_integrity_failure is None:
                    first_integrity_failure = "body_digest_mismatch"
                continue
            if actual_body_digest == actual_rec_digest:
                member_outcomes.append(
                    {"pin": pin, "status": "record_body_digest_collision"}
                )
                if first_integrity_failure is None:
                    first_integrity_failure = "record_body_digest_collision"
                continue

        member_outcomes.append({"pin": pin, "status": "verified"})

    # A collection establishes selected membership only, never complete stream coverage.
    coverage_check = vec.get("coverage_check")
    coverage_outcome: str | None = None
    if coverage_check is not None:
        journal_refs = {
            (ev["stream"], ev["record_id"], ev["revision"])
            for ev in coverage_check["journal_events"]
        }
        pinned_refs = {pin_ref(m["pin"]) for m in collection["members"]}
        if journal_refs <= pinned_refs:
            coverage_outcome = "coverage_unproven"
        else:
            coverage_outcome = "coverage_incomplete"

    if first_integrity_failure is not None:
        final_outcome = "refuse"
        final_reason = first_integrity_failure
    elif first_availability_failure is not None:
        if closure_claim == "complete_at_write" or vec.get(
            "require_full_resolution", True
        ):
            final_outcome = "refuse"
            final_reason = first_availability_failure
        else:
            final_outcome = "partial"
            final_reason = first_availability_failure
    else:
        final_outcome = "ok"
        final_reason = None

    # Verify earlier revision resolvability when testing append-correction history
    earlier_resolvable: bool | None = None
    if predecessor is not None and vec.get("verify_predecessor_resolvable"):
        prev_ok = True
        for m in predecessor["members"]:
            pin = m["pin"]
            entry = records_by_ref.get(pin_ref(pin))
            if (
                entry is None
                or entry.get("status") == "held"
                or jcs_digest(entry["record"]) != pin["digest"]
            ):
                prev_ok = False
                break
        earlier_resolvable = prev_ok

    closure_outcome: str | None = None
    if closure_claim == "complete_at_write":
        snapshot_scope = store.get("snapshot_scope", "current_read")
        if snapshot_scope == "at_write":
            if first_integrity_failure is None and first_availability_failure is None:
                closure_outcome = "closure_verified_at_write"
            else:
                closure_outcome = "closure_refuted_at_write"
        else:
            closure_outcome = "closure_unproven"

    out = {
        "outcome": final_outcome,
        "reason": final_reason,
        "member_outcomes": [mo["status"] for mo in member_outcomes],
        "collection_digest": jcs_digest(collection),
    }
    if closure_outcome is not None:
        out["closure_outcome"] = closure_outcome
    if collection["kind"] == "list":
        out["ordered_record_ids"] = [
            m["pin"]["record_id"] for m in collection["members"]
        ]
    if coverage_outcome is not None:
        out["coverage_outcome"] = coverage_outcome
    if earlier_resolvable is not None:
        out["predecessor_resolvable"] = earlier_resolvable
    return out


def check_vector_against_expected(vec: dict) -> list[str]:
    errs: list[str] = []
    actual = evaluate_vector(vec)
    exp = vec.get("expected")
    if not isinstance(exp, dict) or not exp:
        return ["expected: must be a non-empty object"]

    mode = vec.get("mode")
    required_keys = ["outcome", "reason"]
    if mode == "canonical_set_equivalence":
        required_keys.append("canonical_digest")
    else:
        col = vec.get("collection", {})
        if vec.get("store") is not None:
            required_keys.append("member_outcomes")
        if vec.get("coverage_check") is not None:
            required_keys.append("coverage_outcome")
        if vec.get("verify_predecessor_resolvable"):
            required_keys.append("predecessor_resolvable")
        if (
            col.get("closure_claim") == "complete_at_write"
            and actual.get("closure_outcome") is not None
        ):
            required_keys.append("closure_outcome")
        if col.get("kind") == "list" and exp.get("outcome") == "ok":
            required_keys.append("ordered_record_ids")

    for req_key in required_keys:
        if req_key not in exp:
            errs.append(f"expected missing required key: {req_key}")

    for key in (
        "outcome",
        "reason",
        "closure_outcome",
        "coverage_outcome",
        "predecessor_resolvable",
        "ordered_record_ids",
    ):
        if key in exp and actual.get(key) != exp[key]:
            errs.append(f"{key}: expected {exp[key]!r}, got {actual.get(key)!r}")

    if (
        "member_outcomes" in exp
        and actual.get("member_outcomes") != exp["member_outcomes"]
    ):
        errs.append(
            f"member_outcomes: expected {exp['member_outcomes']!r}, got {actual.get('member_outcomes')!r}"
        )

    if (
        "canonical_digest" in exp
        and actual.get("canonical_digest") != exp["canonical_digest"]
    ):
        errs.append(
            f"canonical_digest: expected {exp['canonical_digest']!r}, got {actual.get('canonical_digest')!r}"
        )

    if (
        "collection_digest" in exp
        and actual.get("collection_digest") != exp["collection_digest"]
    ):
        errs.append(
            f"collection_digest: expected {exp['collection_digest']!r}, got {actual.get('collection_digest')!r}"
        )

    return errs


def main() -> int:
    # 1. Validate positive examples
    print("    record-collection: examples...")
    examples = sorted((BASE / "examples").glob("*.json"))
    if not examples:
        fail("no example files found")
    for ex in examples:
        matched = None
        for prefix, schema in PREFIX_SCHEMA.items():
            if ex.name.startswith(prefix + "."):
                matched = schema
                break
        if matched is None:
            fail(f"example has unknown schema prefix: {ex.name}")
            continue
        if not valid(matched, ex):
            fail(f"example failed schema {matched}: {ex.name}")
            continue
        ex_obj = load(ex)
        col_obj = (
            ex_obj["collection"]
            if matched == "collection-document.schema.json"
            else ex_obj
        )
        ex_outcome, ex_reason = verify_collection_payload(col_obj)
        if ex_outcome != "ok":
            fail(f"example failed semantic verification ({ex_reason}): {ex.name}")

    # 2. Mirror check: record-collection.schema.json == collection-document.schema.json#/$defs/recordCollection
    print("    record-collection: schema mirror and durable-record/v1 pin parity...")
    rc_schema = load(BASE / "record-collection.schema.json")
    doc_schema = load(BASE / "collection-document.schema.json")
    dr_schema = load(DR_RECORD_SCHEMA)

    rc_body = {
        k: rc_schema[k]
        for k in ("type", "additionalProperties", "required", "properties", "allOf")
    }
    if doc_schema["$defs"]["recordCollection"] != rc_body:
        fail(
            "collection-document.schema.json $defs.recordCollection drifted from record-collection.schema.json"
        )

    for def_name in ("digest", "pin"):
        if rc_schema["$defs"][def_name] != dr_schema["$defs"][def_name]:
            fail(
                f"record-collection.schema.json $defs.{def_name} drifted from durable-record/v1"
            )
        if doc_schema["$defs"][def_name] != dr_schema["$defs"][def_name]:
            fail(
                f"collection-document.schema.json $defs.{def_name} drifted from durable-record/v1"
            )

    if rc_schema["$defs"]["member"] != doc_schema["$defs"]["member"]:
        fail(
            "$defs.member drifted between record-collection.schema.json and collection-document.schema.json"
        )

    # 3. Schema reject pairs + index.json
    print("    record-collection: schema reject pairs...")
    rej_dir = BASE / "rejects/schema"
    idx = load(rej_dir / "index.json")
    cases = idx.get("cases", [])
    indexed_files: set[str] = {"index.json"}
    for case in cases:
        schema = case["schema"]
        base_f = rej_dir / case["baseline"]
        rej_f = rej_dir / case["reject"]
        indexed_files.add(case["baseline"])
        indexed_files.add(case["reject"])
        if not base_f.exists() or not rej_f.exists():
            fail(f"missing schema reject pair files for {case['id']}")
            continue
        if not valid(schema, base_f):
            fail(
                f"schema reject baseline did not validate: {case['id']} ({case['baseline']})"
            )
        if valid(schema, rej_f):
            fail(
                f"schema reject unexpectedly validated: {case['id']} ({case['reject']})"
            )
        dc = diff_count(load(base_f), load(rej_f))
        if dc != 1:
            fail(f"schema reject pair {case['id']} has {dc} diffs (expected 1)")

    actual_rej_files = {p.name for p in rej_dir.glob("*.json")}
    if actual_rej_files != indexed_files:
        fail(
            f"rejects/schema files mismatch index.json: extra={actual_rej_files - indexed_files}, missing={indexed_files - actual_rej_files}"
        )

    # 4. Behavioral vectors + INVENTORY.json
    print("    record-collection: behavioral vectors...")
    vec_dir = BASE / "vectors"
    inventory = load(vec_dir / "INVENTORY.json")
    inv_entries = inventory.get("vectors", [])
    inv_ids = [entry["id"] for entry in inv_entries]
    inv_by_id = {entry["id"]: entry for entry in inv_entries}
    if inventory.get("count") != len(inv_entries):
        fail(
            f"vectors/INVENTORY.json count {inventory.get('count')!r} != {len(inv_entries)}"
        )
    inv_files = {f"{vid}.json" for vid in inv_ids} | {"INVENTORY.json"}
    actual_vec_files = {p.name for p in vec_dir.glob("*.json")}
    if actual_vec_files != inv_files:
        fail(
            f"vectors/ files mismatch INVENTORY.json: extra={actual_vec_files - inv_files}, missing={inv_files - actual_vec_files}"
        )

    with tempfile.TemporaryDirectory() as tmp_name:
        tmp_dir = pathlib.Path(tmp_name)
        rc_schema_path = BASE / "record-collection.schema.json"
        for vid in inv_ids:
            vp = vec_dir / f"{vid}.json"
            if not vp.exists():
                continue
            vec = load(vp)
            if vec.get("id") != vid:
                fail(f"vector {vp.name} id field {vec.get('id')!r} != {vid!r}")
            inv_row = inv_by_id[vid]
            if inv_row.get("mode") != vec.get("mode"):
                fail(
                    f"INVENTORY.json mode for {vid}: {inv_row.get('mode')!r} != {vec.get('mode')!r}"
                )
            if inv_row.get("expected_outcome") != vec.get("expected", {}).get(
                "outcome"
            ):
                fail(
                    f"INVENTORY.json expected_outcome for {vid}: {inv_row.get('expected_outcome')!r} != {vec.get('expected', {}).get('outcome')!r}"
                )
            if "collection" in vec:
                if not valid_obj(
                    rc_schema_path, vec["collection"], tmp_dir, f"{vid}.collection.json"
                ):
                    fail(
                        f"vector {vid}: collection failed record-collection.schema.json"
                    )
            if "predecessor" in vec:
                if not valid_obj(
                    rc_schema_path,
                    vec["predecessor"],
                    tmp_dir,
                    f"{vid}.predecessor.json",
                ):
                    fail(
                        f"vector {vid}: predecessor failed record-collection.schema.json"
                    )
            for idx_rec, entry in enumerate(vec.get("store", {}).get("records", [])):
                if not valid_obj(
                    DR_RECORD_SCHEMA,
                    entry["record"],
                    tmp_dir,
                    f"{vid}.rec.{idx_rec}.json",
                ):
                    fail(
                        f"vector {vid}: store.records[{idx_rec}].record failed durable-record/v1/record.schema.json"
                    )
            errs = check_vector_against_expected(vec)
            for e in errs:
                fail(f"vector {vid}: {e}")

        # 5. Semantic rejects (must be caught by check_vector_against_expected)
        print("    record-collection: semantic rejects...")
        sem_dir = BASE / "rejects/semantic"
        sem_files = sorted(sem_dir.glob("reject-*.json"))
        if not sem_files:
            fail("no semantic reject fixtures found")
        for sf in sem_files:
            vec = load(sf)
            for idx_rec, entry in enumerate(vec.get("store", {}).get("records", [])):
                if not valid_obj(
                    DR_RECORD_SCHEMA,
                    entry["record"],
                    tmp_dir,
                    f"{sf.stem}.rec.{idx_rec}.json",
                ):
                    fail(
                        f"semantic reject {sf.name}: store.records[{idx_rec}].record failed durable-record/v1/record.schema.json"
                    )
            errs = check_vector_against_expected(vec)
            if not errs:
                fail(
                    f"semantic reject {sf.name} unexpectedly passed vector verification"
                )

    # 6. Hygiene scan (no local paths or credential-shaped strings)
    print("    record-collection: fixture hygiene...")
    bad_patterns = [
        re.compile(r"/Users/|/home/|C:\\{1,2}Users\\{1,2}"),
        re.compile(
            r"ghp_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|-----BEGIN .*PRIVATE KEY-----"
        ),
    ]
    for p in sorted(BASE.rglob("*")):
        if not p.is_file():
            continue
        text = p.read_text(encoding="utf-8")
        for pat in bad_patterns:
            if pat.search(text):
                fail(f"hygiene violation ({pat.pattern}) in {p.relative_to(ROOT)}")

    if failures:
        print(
            f"[!!] record-collection controls FAILED ({len(failures)} failure(s))",
            file=sys.stderr,
        )
        return 1

    print(
        f"[ok] record-collection controls passed: "
        f"{len(examples)} examples, {len(cases)} schema reject pairs, "
        f"{len(inv_ids)} behavioral vectors, {len(sem_files)} semantic rejects"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
