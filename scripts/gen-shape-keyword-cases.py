#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Regenerate internal/shape/testdata/keyword-verdicts.json: synthetic schemas
that each exercise one keyword or composition in isolation, with the verdict of
the contract's reference shape validator (goneat, through the vendored
shape_suite) for each instance. The Go unit tests replay it against the same
compiled schemas, so every admitted keyword is checked against the reference
directly, not only through the pinned schemas' compositions.

Run with goneat on PATH, from the repository root:
  python3 -I -B scripts/gen-shape-keyword-cases.py
"""
import importlib.util, json, os, sys, tempfile
from pathlib import Path

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
spec = importlib.util.spec_from_file_location(
    "ref", os.path.join(ROOT, "contract", "crucible", "scripts", "validate-durable-record-fixtures.py"))
ref = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ref)
OUT = os.path.join(ROOT, "internal", "shape", "testdata", "keyword-verdicts.json")
DRAFT = "https://json-schema.org/draft/2020-12/schema"

# (name, schema body without $schema, instances)
CASES = [
    ("type-integer", {"type": "integer"}, ["1", "-1", "12.0", "1.2e1", "0", "-0.0", "1.5", "\"1\"", "true", "null", "1e-5"]),
    ("type-string", {"type": "string"}, ["\"x\"", "1", "null"]),
    ("type-object", {"type": "object"}, ["{}", "[]", "1"]),
    ("type-array", {"type": "array"}, ["[]", "{}"]),
    ("type-boolean", {"type": "boolean"}, ["true", "0"]),
    ("type-null", {"type": "null"}, ["null", "0", "false"]),
    ("const-1", {"const": 1}, ["1", "1.0", "1e0", "10e-1", "true", "\"1\"", "2"]),
    ("const-true", {"const": True}, ["true", "1", "false"]),
    ("const-object", {"const": {"a": 1, "b": [1, "x"]}}, ["{\"b\":[1.0,\"x\"],\"a\":1}", "{\"a\":1}", "{\"a\":1,\"b\":[\"x\",1]}", "{\"a\":1,\"b\":[1,\"x\"],\"c\":0}"]),
    ("enum", {"enum": ["a", 2, None]}, ["\"a\"", "2.0", "null", "\"b\"", "3", "false"]),
    ("uniqueItems", {"uniqueItems": True}, ["[1,\"1\",true]", "[{\"a\":1},{\"a\":2}]", "[]", "[1,1.0]", "[{\"a\":1,\"b\":2},{\"b\":2,\"a\":1}]", "[null,null]", "[[1],[1]]"]),
    ("unique-key-prefixes", {"uniqueItems": True},
     ["[[\"a,sb\",\"c\"],[\"a\",\"b,sc\"]]", "[{\"a=sb\":\"c\"},{\"a\":\"b=sc\"}]",
      "[{\"1:a\":1},{\"1\":{\"a\":1}}]", "[[\"ab\"],[\"a\",\"b\"]]", "[{\"a=s6:x\":\"c\"},{\"a\":\"x=s1:c\"}]", "[[\"x\",\"x\"],[\"x\",\"x\"]]"]),
    ("length", {"minLength": 2, "maxLength": 3}, ["\"ab\"", "\"abc\"", "\"\U0001F600\U0001F600\"", "\"a\U0001F600b\"", "\"a\"", "\"abcd\"", "\"\U0001F600\"", "\"\U0001F600\U0001F600\U0001F600\U0001F600\"", "\"\u00e9\u00e9\""]),
    ("bounds", {"minimum": 1, "maximum": 9007199254740991}, ["1", "1.0", "9007199254740991", "\"x\"", "0", "9007199254740992", "-1"]),
    ("minimum-only", {"minimum": 0}, ["-1", "-2", "0", "-0.0", "1", "-1e-99", "\"x\""]),
    ("maximum-only", {"maximum": 0}, ["1", "2", "-1", "0", "1e-99", "-1e99"]),
    ("minimum-negative", {"minimum": -5}, ["-6", "-5", "-4.5", "0"]),
    ("minimum-positive", {"minimum": 2}, ["-1", "-3", "-1.5", "2", "3"]),
    ("maximum-negative", {"maximum": -2}, ["1", "3", "1.5", "-2", "-3"]),
    ("object", {"type": "object", "required": ["a"], "properties": {"a": {"type": "string"}, "b": {"type": "integer"}}, "additionalProperties": False},
     ["{\"a\":\"x\"}", "{\"a\":\"x\",\"b\":2}", "{}", "{\"a\":1}", "{\"a\":\"x\",\"b\":\"y\"}", "{\"a\":\"x\",\"c\":1}"]),
    ("additional-own-properties-only", {"type": "object", "properties": {"a": {}}, "additionalProperties": False, "allOf": [{"properties": {"b": {}}}]},
     ["{\"a\":1}", "{\"b\":1}"]),
    ("array", {"type": "array", "items": {"type": "integer"}, "minItems": 1, "maxItems": 2, "contains": {"const": 1}},
     ["[1]", "[2,1]", "[]", "[2]", "[1,1,1]", "[1,\"x\"]"]),
    ("oneOf", {"oneOf": [{"type": "integer"}, {"minimum": 5}]}, ["1", "5.5", "\"x\"", "7"]),
    ("anyOf", {"anyOf": [{"type": "integer"}, {"type": "string"}]}, ["1", "\"x\"", "true"]),
    ("allOf", {"allOf": [{"type": "integer"}, {"minimum": 2}]}, ["2", "1", "\"x\""]),
    ("not", {"not": {"type": "string"}}, ["1", "\"x\""]),
    ("if-then-else", {"if": {"properties": {"k": {"const": "a"}}}, "then": {"required": ["x"]}, "else": {"required": ["y"]}},
     ["{\"k\":\"a\",\"x\":1}", "{\"k\":\"b\",\"y\":1}", "{\"x\":1}", "{\"k\":\"a\",\"y\":1}", "{\"k\":\"b\",\"x\":1}", "{\"y\":1}"]),
    ("if-alone", {"if": {"type": "string"}}, ["1", "\"x\""]),
    ("ref-root", {"$ref": "#/$defs/r", "$defs": {"r": {"type": "object", "required": ["a"]}}}, ["{\"a\":1}", "{}", "1"]),
    ("date-time", {"type": "string", "format": "date-time"},
     ["\"2026-10-20T00:00:01.000000Z\"", "\"2026-10-20\"", "\"00:00:00\"", "\"2026-02-30T00:00:00Z\"", "\"2026-10-20T24:00:00Z\"", "\"x\"", "1"]),
]

with tempfile.TemporaryDirectory(prefix="shape-keyword-") as tmp:
    items, meta = [], {}
    for name, body, instances in CASES:
        schema = dict({"$schema": DRAFT}, **body)
        d = Path(tmp) / "kw"
        d.mkdir(exist_ok=True)
        (d / (name + ".schema.json")).write_text(json.dumps(schema), encoding="utf-8")
        for i, inst in enumerate(instances):
            iid = "%s#%d" % (name, i)
            items.append((iid, "kw/%s.schema.json" % name, inst))
            meta[iid] = (name, body, inst)
    res = ref.shape_suite(items, default_ver="kw", schemas_dir=Path(tmp))
out = [{"name": meta[k][0], "schema": meta[k][1], "instance": meta[k][2], "valid": res[k]} for k, _, _ in items]
os.makedirs(os.path.dirname(OUT), exist_ok=True)
with open(OUT, "w", encoding="utf-8") as f:
    json.dump(out, f, ensure_ascii=True, indent=1, sort_keys=True)
    f.write("\n")
print(len(out), "cases,", sum(o["valid"] for o in out), "valid")
