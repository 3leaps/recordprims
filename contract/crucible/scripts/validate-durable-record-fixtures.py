#!/usr/bin/env python3
"""Fixture-integrity and wire-domain checker for contract: durable-record/v0.

Standard-library only, plus subprocess invocation of goneat (the JSON Schema
validator named by the contract, PDR-0006 Rule 2b) for batched schema checks.
Reuses scripts/rfc8785-canonicalize.py for revision digests without modifying
its shared I-JSON numeric behavior. This is conformance-gate material for the
contract's fixtures, not a consumer library or a store implementation.

Usage:
  validate-durable-record-fixtures.py pair <baseline.json> <reject.json>
  validate-durable-record-fixtures.py record <record.json>
  validate-durable-record-fixtures.py journal <journal.ndjson>
  validate-durable-record-fixtures.py entry <journal-entry.json>
  validate-durable-record-fixtures.py profile <stratum-profile.json>
  validate-durable-record-fixtures.py receipt <receipt-or-reconciliation.json> <journal.ndjson>
  validate-durable-record-fixtures.py v0-suite <durable-record-v0-dir>
  validate-durable-record-fixtures.py self-test
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import math
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any

_here = Path(__file__).resolve().parent
_root = _here.parent
_schemas_root = _root / "schemas/durable-record"

_spec = importlib.util.spec_from_file_location(
    "rfc8785", _here / "rfc8785-canonicalize.py"
)
_jcs = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(_jcs)
_tspec = importlib.util.spec_from_file_location("rfc3339", _here / "rfc3339-instant.py")
_rfc3339 = importlib.util.module_from_spec(_tspec)
assert _tspec.loader is not None
_spec_loader = _tspec.loader
_spec_loader.exec_module(_rfc3339)

MAX_SAFE_INTEGER = 9007199254740991


class Reject(Exception):
    def __init__(self, reason: str, detail: str) -> None:
        super().__init__(f"{reason}: {detail}")
        self.reason = reason


# --- Stage 1 & 2: strict raw JSON ingestion + protocol numeric_domain -------


class _RawNum:
    __slots__ = ("token",)

    def __init__(self, token: str) -> None:
        self.token = token


_JSON_NUM_RE = re.compile(r"^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$")


def _reject_constant(token: str) -> Any:
    raise ValueError(f"non-JSON numeric constant {token!r}")


def _check_strings_well_formed(node: Any, where: str) -> None:
    if isinstance(node, str):
        for ch in node:
            if 0xD800 <= ord(ch) <= 0xDFFF:
                raise Reject("ingest", f"{where}: lone Unicode surrogate")
    elif isinstance(node, list):
        for i, item in enumerate(node):
            _check_strings_well_formed(item, f"{where}[{i}]")
    elif isinstance(node, dict):
        for k, v in node.items():
            _check_strings_well_formed(k, where)
            _check_strings_well_formed(v, f"{where}/{k}")


def parse_raw_json(raw: str, where: str) -> Any:
    """Stage 1 (ingest): strict RFC 8259 syntax + unique keys + raw numeric tokens."""
    if not isinstance(raw, str):
        raise Reject("ingest", f"{where}: wire input must be UTF-8 text")
    try:
        ast_node = json.loads(
            raw,
            object_pairs_hook=_jcs._unique_object,
            parse_int=_RawNum,
            parse_float=_RawNum,
            parse_constant=_reject_constant,
        )
    except ValueError as e:
        raise Reject("ingest", f"{where}: {e}") from e
    _check_strings_well_formed(ast_node, where)
    return ast_node


def parse_safe_int_token(token: str, where: str) -> int:
    """Stage 2 (numeric_domain): exact integrality & safe magnitude without huge expansion."""
    m = _JSON_NUM_RE.fullmatch(token)
    if m is None:
        raise Reject("ingest", f"{where}: invalid JSON numeric token {token!r}")
    sign, int_part, frac_part, exp_part = m.groups()
    frac_part = frac_part or ""
    digits = (int_part + frac_part).lstrip("0")
    if not digits:
        return 0
    trimmed = digits.rstrip("0")
    trailing_zeros = len(digits) - len(trimmed)
    base_scale = trailing_zeros - len(frac_part)
    # Exact value is (-1 if sign == "-" else 1) * int(trimmed) * 10**(base_scale + exp_val)
    # with `trimmed` free of leading/trailing zeros. Integrality requires
    # effective_scale = base_scale + exp_val >= 0, and magnitude <= MAX_SAFE_INTEGER (< 10**16)
    # requires len(trimmed) + effective_scale <= 16, bounding |exp_val| <= abs(base_scale) + 16.
    if exp_part is not None:
        exp_neg = exp_part.startswith("-")
        exp_digits = exp_part.lstrip("+-").lstrip("0") or "0"
        exp_bound = abs(base_scale) + 16
        if len(exp_digits) > len(str(exp_bound)):
            raise Reject(
                "numeric_domain",
                f"{where}: exponent outside safe integer domain ({token!r})",
            )
        exp_abs = int(exp_digits)
        if exp_abs > exp_bound:
            raise Reject(
                "numeric_domain",
                f"{where}: exponent outside safe integer domain ({token!r})",
            )
        exp_val = -exp_abs if exp_neg else exp_abs
    else:
        exp_val = 0
    effective_scale = base_scale + exp_val
    if effective_scale < 0:
        raise Reject(
            "numeric_domain",
            f"{where}: non-integral numeric token {token!r}",
        )
    if len(trimmed) > 16 or len(trimmed) + effective_scale > 16:
        raise Reject(
            "numeric_domain",
            f"{where}: {token!r} exceeds safe integer ceiling {MAX_SAFE_INTEGER}",
        )
    magnitude = int(trimmed + ("0" * effective_scale))
    if magnitude > MAX_SAFE_INTEGER:
        raise Reject(
            "numeric_domain",
            f"{where}: {token!r} exceeds safe integer ceiling {MAX_SAFE_INTEGER}",
        )
    return -magnitude if sign == "-" else magnitude


def _materialize_opaque_num(token: str, where: str) -> int | float:
    """Materialize a non-protocol JSON number (e.g. inside record.payload) under I-JSON."""
    m = _JSON_NUM_RE.fullmatch(token)
    if m is None:
        raise Reject("ingest", f"{where}: invalid JSON numeric token {token!r}")
    sign, int_part, frac_part, exp_part = m.groups()
    is_float_spelling = frac_part is not None or exp_part is not None
    frac_str = frac_part or ""
    digits = (int_part + frac_str).lstrip("0")
    if not digits:
        if is_float_spelling:
            return -0.0 if sign == "-" else 0.0
        return 0
    if not is_float_spelling:
        if len(int_part) > 315:
            raise Reject("ingest", f"{where}: integer overflows IEEE 754 binary64")
        val = int(token)
        try:
            _jcs._require_ijson_number(val)
        except ValueError as e:
            raise Reject("ingest", f"{where}: {e}") from e
        return val
    # Guard huge positive/negative exponents so float conversion stays bounded and finite.
    if exp_part is not None:
        exp_neg = exp_part.startswith("-")
        exp_digits = exp_part.lstrip("+-").lstrip("0") or "0"
        limit = len(token) + 400
        if len(exp_digits) > len(str(limit)) or int(exp_digits) > limit:
            if exp_neg:
                return -0.0 if sign == "-" else 0.0
            raise Reject("ingest", f"{where}: non-finite IEEE 754 number {token!r}")
    fval = float(token)
    if not math.isfinite(fval):
        raise Reject("ingest", f"{where}: non-finite IEEE 754 number {token!r}")
    return fval


# Declarative protocol-integer schema path map across durable-record v0 and v1.
# Notice record.payload is "opaque": arbitrary payload numbers are never restricted
# to integers or to MAX_SAFE_INTEGER.
PROTOCOL_NUMERIC_SCHEMAS: dict[str, dict[str, Any]] = {
    "pin": {"revision": "int"},
    "revision_pin": {"revision": "int"},
    "body_ref": {"length": "int"},
    "lifecycle_body": {"length": "int"},
    "link": {"revision": "int"},
    "source_cut": {"cut_seq": "int"},
    "source_pin": {"pin": "pin"},
    "record": {
        "v": "int",
        "revision": "int",
        "supersedes": "revision_pin",
        "body": "body_ref",
        "links": ("list", "link"),
        "payload": "opaque",
    },
    "protection_request": {
        "v": "int",
        "target": "pin",
        "body": "lifecycle_body",
        "restores": "int",
    },
    "journal_entry": {
        "v": "int",
        "seq": "int",
        "group": "int",
        "group_size": "int",
        "revision": "int",
        "destination_cut_seq": "int",
        "incarnation": "int",
        "record": "record",
        "presented": "revision_pin",
        "current": "revision_pin",
        "request": "protection_request",
        "continues": "source_cut",
        "source": "source_cut",
        "imported_from": "source_pin",
        "body": "lifecycle_body",
    },
    "protection_receipt": {
        "v": "int",
        "seq": "int",
    },
    "write_receipt": {
        "v": "int",
        "seq": "int",
        "pin": "pin",
        "current": "pin",
        "receipt": "write_receipt",
    },
    "export_request": {
        "v": "int",
        "protection_cut": "int",
        "current_cut": "int",
        "selection": ("list", "pin"),
        "omitted": ("list", "pin"),
    },
    "export_result": {
        "protection_cut": "int",
        "selection": ("list", "pin"),
        "omitted": ("list", "pin"),
    },
    "body_state_request": {
        "body": "lifecycle_body",
    },
    "profile_runtime": {
        "clock_skew_bound_ms": "int",
        "idempotency_window_ms": "int",
    },
    "stratum_profile": {
        "runtime": "profile_runtime",
    },
}


def _preflight_node(node: Any, kind: Any, where: str) -> Any:
    if kind == "int":
        if isinstance(node, _RawNum):
            return parse_safe_int_token(node.token, where)
        return _preflight_node(node, "opaque", where)
    if kind == "opaque":
        if isinstance(node, _RawNum):
            return _materialize_opaque_num(node.token, where)
        if isinstance(node, list):
            return [
                _preflight_node(x, "opaque", f"{where}[{i}]")
                for i, x in enumerate(node)
            ]
        if isinstance(node, dict):
            return {
                k: _preflight_node(v, "opaque", f"{where}/{k}") for k, v in node.items()
            }
        return node
    if isinstance(kind, tuple) and kind[0] == "list":
        elem_kind = kind[1]
        if isinstance(node, list):
            return [
                _preflight_node(x, elem_kind, f"{where}[{i}]")
                for i, x in enumerate(node)
            ]
        return _preflight_node(node, "opaque", where)
    spec = PROTOCOL_NUMERIC_SCHEMAS[kind]
    if isinstance(node, dict):
        return {
            k: _preflight_node(v, spec.get(k, "opaque"), f"{where}/{k}")
            for k, v in node.items()
        }
    return _preflight_node(node, "opaque", where)


def parse_wire(raw: str, kind: str, where: str = "<wire>") -> Any:
    """Run Stage 1 (ingest) + Stage 2 (numeric_domain) on raw UTF-8 JSON text."""
    ast_node = parse_raw_json(raw, where)
    return _preflight_node(ast_node, kind, where)


def load_wire(path: str | Path, kind: str) -> Any:
    p = Path(path)
    try:
        raw = p.read_text(encoding="utf-8")
    except UnicodeDecodeError as e:
        raise Reject("ingest", f"{p}: invalid UTF-8: {e}") from e
    return parse_wire(raw, kind, str(p))


def load_wire_lines(path: str | Path, kind: str = "journal_entry") -> list[Any]:
    p = Path(path)
    try:
        text = p.read_text(encoding="utf-8")
    except UnicodeDecodeError as e:
        raise Reject("ingest", f"{p}: invalid UTF-8: {e}") from e
    out = []
    for i, ln in enumerate(text.splitlines(), start=1):
        if not ln.strip():
            continue
        out.append(parse_wire(ln, kind, f"{p}:{i}"))
    return out


def check_typed_domain(node: Any, kind: Any, where: str) -> None:
    """Enforce exact safe-integer bounds on already-parsed typed protocol objects."""
    if kind == "int":
        if type(node) is int:
            if abs(node) > MAX_SAFE_INTEGER:
                raise Reject(
                    "numeric_domain",
                    f"{where}: {node} exceeds safe integer ceiling {MAX_SAFE_INTEGER}",
                )
        elif type(node) is float:
            if (
                not math.isfinite(node)
                or node != int(node)
                or abs(int(node)) > MAX_SAFE_INTEGER
            ):
                raise Reject(
                    "numeric_domain",
                    f"{where}: {node!r} outside safe integer domain",
                )
        return
    if kind == "opaque":
        return
    if isinstance(kind, tuple) and kind[0] == "list":
        elem_kind = kind[1]
        if isinstance(node, list):
            for i, item in enumerate(node):
                check_typed_domain(item, elem_kind, f"{where}[{i}]")
        return
    spec = PROTOCOL_NUMERIC_SCHEMAS[kind]
    if isinstance(node, dict):
        for k, v in node.items():
            sub_kind = spec.get(k, "opaque")
            if sub_kind != "opaque":
                check_typed_domain(v, sub_kind, f"{where}/{k}")


def load(path: str) -> Any:
    """General strict JSON loader for test-case/pair containers (Stage 1 only)."""
    try:
        raw = Path(path).read_text(encoding="utf-8")
        doc = json.loads(
            raw,
            object_pairs_hook=_jcs._unique_object,
            parse_constant=_reject_constant,
        )
    except (UnicodeDecodeError, ValueError) as e:
        raise Reject("ingest", f"{path}: {e}") from e
    _check_strings_well_formed(doc, str(path))
    return doc


def load_lines(path: str) -> list[Any]:
    return load_wire_lines(path, "journal_entry")


# --- Stage 3: batched goneat validate suite with strict verdict separation --


def _normalize_schema_rel(schema: str | Path, default_ver: str = "v1") -> str:
    if isinstance(schema, Path):
        resolved = schema.resolve()
        try:
            rel = resolved.relative_to(_schemas_root.resolve())
            return rel.as_posix()
        except ValueError:
            raise RuntimeError(f"schema outside durable-record root: {schema}")
    s = str(schema).replace("\\", "/")
    if s.startswith("schemas/durable-record/"):
        s = s[len("schemas/durable-record/") :]
    if "/" not in s:
        if not s.endswith(".schema.json"):
            s = f"{s}.schema.json"
        s = f"{default_ver}/{s}"
    elif not s.endswith(".schema.json"):
        s = f"{s}.schema.json"
    return s


def _extract_json_report(stdout: str, stderr: str) -> dict[str, Any]:
    decoder = json.JSONDecoder()
    for stream in (stdout, stderr):
        if not stream:
            continue
        for m in re.finditer(r"(?:^|\n)\{", stream):
            start = m.end() - 1
            try:
                obj, _ = decoder.raw_decode(stream[start:])
            except json.JSONDecodeError:
                continue
            if isinstance(obj, dict) and ("files" in obj or "valid" in obj):
                return obj
    raise RuntimeError(
        f"goneat produced no structured JSON report (stdout={stdout[:200]!r}, stderr={stderr[:300]!r})"
    )


def shape_suite(
    items: list[tuple[str, str | Path, str]],
    *,
    default_ver: str = "v1",
    goneat_bin: str = "goneat",
    schemas_dir: Path | None = None,
    timeout_sec: float = 120.0,
) -> dict[str, bool]:
    """Batch-validate (item_id, schema, raw_json_text) via `goneat validate suite`.

    Returns `{item_id: bool}` where:
      - `True`: parsed and schema-valid (`valid: true`, no errors)
      - `False`: parsed and schema-invalid with structured schema `errors` diagnostics
    Raises `RuntimeError` on malformed JSON, float64 parse/range errors, missing/extra/
    duplicate/unmapped/skipped files, wrong schema source/id, process crash, or timeout.
    """
    if not items:
        return {}
    src_schemas = (schemas_dir or _schemas_root).resolve()
    seen_ids: set[str] = set()
    for item_id, _, _ in items:
        if item_id in seen_ids:
            raise RuntimeError(f"duplicate suite item id: {item_id}")
        seen_ids.add(item_id)

    with tempfile.TemporaryDirectory(prefix="durable-record-suite-") as tmp_str:
        tmp = Path(tmp_str)
        # Empty .git directory anchors goneat's repo_root to `tmp` so the relative
        # `--manifest schema-mappings.yaml` path resolves deterministically on all OSes.
        (tmp / ".git").mkdir()
        (tmp / "cases").mkdir()
        (tmp / "schemas").mkdir()

        schema_keys: dict[str, str] = {}
        expected_by_relpath: dict[str, tuple[str, str]] = {}

        for idx, (item_id, schema_ref, raw_json) in enumerate(items):
            schema_rel = _normalize_schema_rel(schema_ref, default_ver=default_ver)
            src_schema_path = src_schemas / schema_rel
            if not src_schema_path.is_file():
                raise RuntimeError(f"missing schema file: {src_schema_path}")
            if schema_rel not in schema_keys:
                key = schema_rel.replace("/", "__").removesuffix(".schema.json")
                schema_keys[schema_rel] = key
                dst_schema_path = tmp / "schemas" / schema_rel
                dst_schema_path.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(src_schema_path, dst_schema_path)
                (tmp / "cases" / key).mkdir(parents=True, exist_ok=True)
            key = schema_keys[schema_rel]
            case_rel = f"cases/{key}/{idx:05d}.json"
            (tmp / case_rel).write_text(raw_json, encoding="utf-8")
            expected_by_relpath[case_rel] = (item_id, f"schemas/{schema_rel}")

        manifest_lines = ['version: "1.0.0"', "mappings:"]
        for schema_rel, key in sorted(schema_keys.items()):
            manifest_lines.extend(
                [
                    f'  - pattern: "cases/{key}/*.json"',
                    f'    schema_id: "schemas/{schema_rel}"',
                    "    source: local",
                ]
            )
        (tmp / "schema-mappings.yaml").write_text(
            "\n".join(manifest_lines) + "\n", encoding="utf-8"
        )

        try:
            proc = subprocess.run(
                [
                    goneat_bin,
                    "validate",
                    "suite",
                    "--data",
                    "cases",
                    "--manifest",
                    "schema-mappings.yaml",
                    "--no-ignore",
                    "--fail-on-unmapped",
                    "--strict",
                    "--format",
                    "json",
                    "--log-level",
                    "error",
                ],
                cwd=tmp,
                capture_output=True,
                text=True,
                encoding="utf-8",
                errors="replace",
                timeout=timeout_sec,
            )
        except (OSError, subprocess.SubprocessError) as e:
            raise RuntimeError(f"goneat validate suite execution failed: {e}") from e

        if proc.returncode not in (0, 1):
            raise RuntimeError(
                f"goneat validate suite exited {proc.returncode}: {proc.stderr[:400]}"
            )

        report = _extract_json_report(proc.stdout, proc.stderr)
        return verify_suite_report(report, expected_by_relpath, len(items))


def verify_suite_report(
    report: dict[str, Any],
    expected_by_relpath: dict[str, tuple[str, str]],
    expected_count: int,
) -> dict[str, bool]:
    summary = report.get("summary")
    files = report.get("files")
    if not isinstance(summary, dict) or not isinstance(files, list):
        raise RuntimeError(f"malformed goneat suite report: {report!r}")
    if summary.get("skipped", 0) != 0 or summary.get("unmapped", 0) != 0:
        raise RuntimeError(f"suite had skipped or unmapped files: {summary!r}")
    if (
        summary.get("total") != expected_count
        or summary.get("validated") != expected_count
    ):
        raise RuntimeError(
            f"suite count mismatch: expected {expected_count}, summary={summary!r}"
        )
    if len(files) != expected_count:
        raise RuntimeError(
            f"suite file list length mismatch: expected {expected_count}, got {len(files)}"
        )

    verdicts: dict[str, bool] = {}
    seen_paths: set[str] = set()
    for entry in files:
        if not isinstance(entry, dict) or not isinstance(entry.get("path"), str):
            raise RuntimeError(f"malformed suite file entry: {entry!r}")
        rel_path = entry["path"].replace("\\", "/")
        if rel_path in seen_paths:
            raise RuntimeError(f"duplicate file path in suite report: {rel_path}")
        if rel_path not in expected_by_relpath:
            raise RuntimeError(f"unexpected file path in suite report: {rel_path}")
        seen_paths.add(rel_path)

        item_id, want_schema_id = expected_by_relpath[rel_path]
        schema_info = entry.get("schema")
        if not isinstance(schema_info, dict):
            raise RuntimeError(f"missing schema metadata for {rel_path}: {entry!r}")
        got_schema_id = str(schema_info.get("id", "")).replace("\\", "/")
        if schema_info.get("source") != "local" or got_schema_id != want_schema_id:
            raise RuntimeError(
                f"schema mapping mismatch for {rel_path}: got {schema_info!r}, expected local {want_schema_id!r}"
            )
        if entry.get("status") not in ("pass", "fail", "expected_fail"):
            raise RuntimeError(
                f"unexpected status for {rel_path}: {entry.get('status')!r}"
            )
        if "error" in entry and entry["error"]:
            raise RuntimeError(
                f"unable to validate {item_id} ({rel_path}): {entry['error']}"
            )
        valid = entry.get("valid")
        if not isinstance(valid, bool):
            raise RuntimeError(f"missing boolean verdict for {rel_path}: {entry!r}")
        if valid:
            if entry.get("errors"):
                raise RuntimeError(
                    f"inconsistent valid=true with errors for {rel_path}: {entry!r}"
                )
            verdicts[item_id] = True
        else:
            diagnostics = entry.get("errors")
            if not isinstance(diagnostics, list) or len(diagnostics) == 0:
                raise RuntimeError(
                    f"invalid verdict missing schema diagnostics for {item_id} ({rel_path}): {entry!r}"
                )
            verdicts[item_id] = False

    if seen_paths != set(expected_by_relpath):
        missing = sorted(set(expected_by_relpath) - seen_paths)
        raise RuntimeError(f"missing files from suite report: {missing}")

    return verdicts


# --- timestamps & JCS digest ------------------------------------------------


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


# --- record, entry & journal -----------------------------------------------


def check_record(rec, where: str) -> None:
    check_typed_domain(rec, "record", where)
    sup = rec.get("supersedes")
    if sup is not None and sup["revision"] != rec["revision"] - 1:
        raise Reject("supersedes_revision", where)


def check_entry(e, where: str) -> None:
    check_typed_domain(e, "journal_entry", where)
    if isinstance(e.get("group"), int) and isinstance(e.get("group_size"), int):
        if e["group"] + e["group_size"] - 1 > MAX_SAFE_INTEGER:
            raise Reject(
                "numeric_domain",
                f"{where}: atomic group closing seq exceeds {MAX_SAFE_INTEGER}",
            )
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
        check_typed_domain(e, "journal_entry", where)
        if isinstance(e.get("group"), int) and isinstance(e.get("group_size"), int):
            if e["group"] + e["group_size"] - 1 > MAX_SAFE_INTEGER:
                raise Reject(
                    "numeric_domain",
                    f"{where}: atomic group closing seq exceeds {MAX_SAFE_INTEGER}",
                )
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
    check_typed_domain(doc, "write_receipt", where)
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
    check_typed_domain(p, "stratum_profile", where)
    extra = set(p["runtime"]["enabled_capabilities"]) - set(
        p["build"]["compiled_capabilities"]
    )
    if extra:
        raise Reject("capability_not_compiled", f"{where}: {sorted(extra)}")
    if p["runtime"]["stratum"] not in p["build"]["compiled_strata"]:
        raise Reject("stratum_not_compiled", where)


# --- v0 batched schema suite -----------------------------------------------


def _schema_for_v0_fixture(name: str) -> str:
    if "-record-" in name:
        return "v0/record.schema.json"
    if "-journal-" in name:
        return "v0/journal-entry.schema.json"
    if "-profile-" in name:
        return "v0/stratum-profile.schema.json"
    if "-receipt-" in name or "-reconciliation-" in name:
        return "v0/write-receipt.schema.json"
    raise RuntimeError(f"unmapped v0 schema fixture name: {name}")


def run_v0_suite(base_dir: str) -> None:
    """Batch-validate all durable-record/v0 schema fixtures via goneat validate suite."""
    base = Path(base_dir)
    items: list[tuple[str, str, str]] = []
    expected: dict[str, bool] = {}

    def add(item_id: str, schema: str, raw: str, want: bool) -> None:
        items.append((item_id, schema, raw))
        expected[item_id] = want

    for p in sorted((base / "examples").glob("record.*.example.json")):
        add(
            f"ex:{p.name}", "v0/record.schema.json", p.read_text(encoding="utf-8"), True
        )
    for p in sorted((base / "examples").glob("journal-entry.*.example.json")):
        add(
            f"ex:{p.name}",
            "v0/journal-entry.schema.json",
            p.read_text(encoding="utf-8"),
            True,
        )
    for p in sorted((base / "examples").glob("write-receipt*.example.json")) + sorted(
        (base / "examples").glob("reconciliation*.example.json")
    ):
        add(
            f"ex:{p.name}",
            "v0/write-receipt.schema.json",
            p.read_text(encoding="utf-8"),
            True,
        )
    prof_path = base / "examples/stratum-profile.example.json"
    prof_raw = prof_path.read_text(encoding="utf-8")
    add(
        "ex:stratum-profile.example.json",
        "v0/stratum-profile.schema.json",
        prof_raw,
        True,
    )

    lines = [
        ln
        for ln in (base / "examples/journal.example.ndjson")
        .read_text(encoding="utf-8")
        .splitlines()
        if ln.strip()
    ]
    if not lines:
        raise Reject("empty", "journal.example.ndjson")
    for idx, ln in enumerate(lines, start=1):
        add(f"ndjson:{idx}", "v0/journal-entry.schema.json", ln + "\n", True)
    canceled_lines = [
        ln
        for ln in (base / "rejects/semantic/baseline-journal-canceled-exponents.ndjson")
        .read_text(encoding="utf-8")
        .splitlines()
        if ln.strip()
    ]
    if not canceled_lines:
        raise Reject("empty", "baseline-journal-canceled-exponents.ndjson")
    for idx, ln in enumerate(canceled_lines, start=1):
        add(f"ndjson_cancel:{idx}", "v0/journal-entry.schema.json", ln + "\n", True)

    # Benchmark-only vs non-benchmark idempotency_window_ms: none.
    prof_doc = json.loads(prof_raw)
    w1 = json.loads(prof_raw)
    w1["conformance"] = "benchmark_only"
    w1["runtime"]["idempotency_window_ms"] = "none"
    add(
        "window:benchmark_none",
        "v0/stratum-profile.schema.json",
        json.dumps(w1) + "\n",
        True,
    )
    w2 = json.loads(prof_raw)
    assert prof_doc["conformance"] != "benchmark_only"
    w2["runtime"]["idempotency_window_ms"] = "none"
    add(
        "window:non_benchmark_none",
        "v0/stratum-profile.schema.json",
        json.dumps(w2) + "\n",
        False,
    )

    baselines = sorted((base / "rejects/schema").glob("baseline-*.json"))
    rejects = sorted((base / "rejects/schema").glob("reject-*.json"))
    if not baselines or not rejects:
        raise RuntimeError("missing v0 schema baseline/reject fixtures")
    for p in baselines:
        add(
            f"base:{p.name}",
            _schema_for_v0_fixture(p.name),
            p.read_text(encoding="utf-8"),
            True,
        )
    for p in rejects:
        twin = p.parent / f"baseline-{p.name.removeprefix('reject-')}"
        if not twin.is_file():
            raise Reject("pair", f"no baseline twin for {p}")
        check_pair(load(str(twin)), load(str(p)), str(p))
        add(
            f"rej:{p.name}",
            _schema_for_v0_fixture(p.name),
            p.read_text(encoding="utf-8"),
            False,
        )

    for p in sorted((base / "rejects/receipt").glob("reject-*.json")):
        add(
            f"rcpt_rej:{p.name}",
            "v0/write-receipt.schema.json",
            p.read_text(encoding="utf-8"),
            True,
        )

    # Positive raw-wire integral spellings, safe-max boundaries, and arbitrary payload freedom.
    for tok_label, raw_len in [
        ("int_12", "12"),
        ("float_12_0", "12.0"),
        ("exp_1_2e1", "1.2e1"),
        ("zero_0_0", "0.0"),
        ("neg_zero", "-0.0"),
        ("safe_max_minus_1", "9007199254740990"),
        ("safe_max", "9007199254740991"),
        ("cancel_12e_21", "12000000000000000000000e-21"),
        ("cancel_1e_100", "1" + ("0" * 100) + "e-100"),
        ("cancel_safe_max_e_21", "9007199254740991000000000000000000000e-21"),
        ("cancel_frac_safe_max_e36", "0.000000000000000000009007199254740991e36"),
    ]:
        raw_rec = (
            '{"v":0,"stream":"pipeline-stage-results.example","record_id":"r1","revision":1,'
            '"payload_contract":"contract: example/v0",'
            f'"body":{{"digest":"sha256:2702cb31cfab1a5742cbb6aff3ef2050b60152d2c6377830fcc589990b9ec2f5","length":{raw_len},"media_type":"text/plain"}},'
            '"provenance":{"actor_kind":"process","actor":"stage-runner"},'
            '"classification":{"retention":"short","sensitivity":"0-public"}}\n'
        )
        check_record(
            parse_wire(raw_rec, "record", f"v0-raw-{tok_label}"), f"v0-raw-{tok_label}"
        )
        add(f"raw_len:{tok_label}", "v0/record.schema.json", raw_rec, True)

    raw_payload_rec = (
        '{"v":0,"stream":"pipeline-stage-results.example","record_id":"r-payload","revision":1.0,'
        '"payload_contract":"contract: example/v0",'
        '"payload":{"length":12.5,"seq":9007199254740992},'
        '"provenance":{"actor_kind":"process","actor":"stage-runner"},'
        '"classification":{"retention":"short","sensitivity":"0-public"}}\n'
    )
    check_record(
        parse_wire(raw_payload_rec, "record", "v0-raw-payload"), "v0-raw-payload"
    )
    add("raw_payload:free", "v0/record.schema.json", raw_payload_rec, True)

    verdicts = shape_suite(items, default_ver="v0")
    for item_id, want in expected.items():
        got = verdicts[item_id]
        if got != want:
            raise SystemExit(
                f"[!!] v0 schema verdict mismatch on {item_id}: got {got}, expected {want}"
            )


# --- self-test -------------------------------------------------------------


def self_test() -> None:
    """The checker's own controls: pair, coverage, numeric domain, and suite error guards."""
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

    # Bounded raw-token numeric_domain & ingest preflight controls.
    for tok, want in [
        ("0", 0),
        ("-0", 0),
        ("0.0", 0),
        ("-0.0", 0),
        ("0e99999999", 0),
        ("12", 12),
        ("12.0", 12),
        ("1.2e1", 12),
        ("1.200e+1", 12),
        ("-1", -1),
        ("-9007199254740991", -MAX_SAFE_INTEGER),
        ("9007199254740990", MAX_SAFE_INTEGER - 1),
        ("9007199254740991", MAX_SAFE_INTEGER),
        ("12000000000000000000000e-21", 12),
        ("1" + ("0" * 100) + "e-100", 1),
        ("9007199254740991000000000000000000000e-21", MAX_SAFE_INTEGER),
        ("0.000000000000000000009007199254740991e36", MAX_SAFE_INTEGER),
        ("-9007199254740991000000000000000000000e-21", -MAX_SAFE_INTEGER),
    ]:
        got = parse_safe_int_token(tok, "self-test")
        assert type(got) is int and got == want, (tok, got, want)

    for tok in [
        "0.5",
        "12.0000000000000001",
        "1.0000000000000001",
        "1200000000000000000000e-21",
        "12000000000000000000001e-21",
        "1e-400",
        "1e-99999999",
        "9007199254740992",
        "9007199254740992000000000000000000000e-21",
        "-9007199254740992",
        "9223372036854775807",
        "18446744073709551615",
        "1e400",
        "1e99999999",
        "9007199254740991.5",
    ]:
        try:
            parse_safe_int_token(tok, "self-test")
        except Reject as r:
            assert r.reason == "numeric_domain", (tok, r.reason)
        else:
            raise SystemExit(f"[!!] self-test accepted out-of-domain token {tok!r}")

    # Payload scoping: length/seq/2^53/floats inside record.payload are NOT restricted
    # by protocol numeric_domain, while protocol integer fields ARE.
    rec_with_payload = parse_wire(
        '{"v":0,"stream":"s","record_id":"r1","revision":12.0,'
        '"payload_contract":"contract: ex/v0",'
        '"payload":{"length":12.5,"seq":9007199254740992,"nested":{"revision":0.25}},'
        '"provenance":{"actor_kind":"human","actor":"op"},'
        '"classification":{"retention":"short","sensitivity":"0-public"}}',
        "record",
        "self-test-payload",
    )
    assert (
        type(rec_with_payload["revision"]) is int and rec_with_payload["revision"] == 12
    )
    assert rec_with_payload["payload"]["length"] == 12.5
    assert rec_with_payload["payload"]["seq"] == 9007199254740992
    assert digest(rec_with_payload).startswith("sha256:")

    # Non-numeric types and safe negative integers pass numeric_domain so schema/minimum handles them.
    for raw_val, expected_py in [
        ("-1", -1),
        ("true", True),
        ("null", None),
        ('"12"', "12"),
    ]:
        parsed = parse_wire(
            f'{{"v":0,"revision":{raw_val}}}', "record", "self-test-passthrough"
        )
        assert parsed["revision"] == expected_py and type(parsed["revision"]) is type(
            expected_py
        )

    # Malformed JSON, duplicate keys, NaN/Infinity, and payload binary64 overflow fail at ingest.
    for bad_raw in [
        "{",
        '{"v":0,"v":0}',
        '{"v":NaN}',
        '{"v":Infinity}',
        '{"v":0,"payload":{"x":1e400}}',
        '{"v":0,"payload":{"x":9007199254740993}}',
    ]:
        try:
            parse_wire(bad_raw, "record", "self-test-ingest")
        except Reject as r:
            assert r.reason == "ingest", (bad_raw, r.reason)
        else:
            raise SystemExit(
                f"[!!] self-test accepted malformed/non-I-JSON input {bad_raw!r}"
            )

    # Batched goneat suite negative controls: parse error, float64 range error, and missing
    # schema MUST raise RuntimeError, never return False as a negative schema verdict.
    if shutil.which("goneat"):
        verdicts = shape_suite(
            [
                (
                    "ok",
                    "v0/write-receipt.schema.json",
                    (
                        _schemas_root / "v0/examples/write-receipt.example.json"
                    ).read_text(encoding="utf-8"),
                ),
                (
                    "schema_bad",
                    "v0/write-receipt.schema.json",
                    '{"v":0,"kind":"receipt"}\n',
                ),
            ],
            default_ver="v0",
        )
        assert verdicts == {"ok": True, "schema_bad": False}, verdicts

        for bad_label, bad_item in [
            ("malformed_json", ("bad", "v0/write-receipt.schema.json", "{\n")),
            (
                "float64_overflow",
                ("huge", "v0/write-receipt.schema.json", '{"seq":1e400}\n'),
            ),
            ("missing_schema", ("noschema", "v0/does-not-exist.schema.json", "{}\n")),
        ]:
            try:
                shape_suite([bad_item], default_ver="v0")
            except RuntimeError:
                pass
            else:
                raise SystemExit(
                    f"[!!] self-test {bad_label}: shape_suite did not raise RuntimeError"
                )

    # Synthetic suite report membership & diagnostics controls (catches unmapped, skipped,
    # missing, duplicate, fallback-to-builtin schema, error-field, or diagnostic-free rejects).
    expected_map = {"cases/k/00000.json": ("id0", "schemas/v0/record.schema.json")}
    good_report = {
        "summary": {"total": 1, "validated": 1, "skipped": 0, "unmapped": 0},
        "files": [
            {
                "path": "cases/k/00000.json",
                "schema": {"id": "schemas/v0/record.schema.json", "source": "local"},
                "status": "expected_fail",
                "valid": False,
                "errors": [
                    {"path": "revision", "message": "Must be <= 9007199254740991"}
                ],
            }
        ],
    }
    assert verify_suite_report(good_report, expected_map, 1) == {"id0": False}
    for bad_report_label, bad_report in [
        (
            "unmapped",
            {
                **good_report,
                "summary": {"total": 1, "validated": 0, "skipped": 0, "unmapped": 1},
            },
        ),
        (
            "skipped",
            {
                **good_report,
                "summary": {"total": 1, "validated": 0, "skipped": 1, "unmapped": 0},
            },
        ),
        (
            "missing_file",
            {
                "summary": {"total": 1, "validated": 1, "skipped": 0, "unmapped": 0},
                "files": [],
            },
        ),
        (
            "unexpected_path",
            {
                **good_report,
                "files": [{**good_report["files"][0], "path": "cases/k/other.json"}],
            },
        ),
        (
            "builtin_fallback",
            {
                **good_report,
                "files": [
                    {
                        **good_report["files"][0],
                        "schema": {"id": "goneat-config-v1.0.0", "source": "embedded"},
                    }
                ],
            },
        ),
        (
            "tool_error_entry",
            {
                **good_report,
                "files": [{**good_report["files"][0], "error": "failed to parse JSON"}],
            },
        ),
        (
            "missing_diagnostics",
            {
                **good_report,
                "files": [{**good_report["files"][0], "errors": []}],
            },
        ),
    ]:
        try:
            verify_suite_report(bad_report, expected_map, 1)
        except RuntimeError:
            pass
        else:
            raise SystemExit(
                f"[!!] self-test {bad_report_label}: verify_suite_report did not fail"
            )

    print("ok: checker self-test")


def main(argv: list[str]) -> None:
    cmd, args = (argv[1], argv[2:]) if len(argv) > 1 else ("", [])
    try:
        if cmd == "pair" and len(args) == 2:
            check_pair(load(args[0]), load(args[1]), args[1])
        elif cmd == "record" and len(args) == 1:
            check_record(load_wire(args[0], "record"), args[0])
        elif cmd == "journal" and len(args) == 1:
            check_journal(load_wire_lines(args[0], "journal_entry"), args[0])
        elif cmd == "entry" and len(args) == 1:
            check_entry(load_wire(args[0], "journal_entry"), args[0])
        elif cmd == "profile" and len(args) == 1:
            check_profile(load_wire(args[0], "stratum_profile"), args[0])
        elif cmd == "receipt" and len(args) == 2:
            facts = check_journal(load_wire_lines(args[1], "journal_entry"), args[1])
            check_receipt_doc(load_wire(args[0], "write_receipt"), facts, args[0])
        elif cmd == "v0-suite" and len(args) == 1:
            run_v0_suite(args[0])
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
