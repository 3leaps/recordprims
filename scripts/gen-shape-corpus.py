#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Regenerate conformance/shape-verdicts.json.gz: shape-stage documents with
the verdict of the contract's reference shape validator (goneat, through the
vendored scripts/validate-durable-record-fixtures.py shape_suite).

Run on a contract pin bump only, from the repository root, with goneat on PATH:
  python3 -I -B scripts/gen-shape-corpus.py
CI never runs goneat; it replays the committed file (make conformance).

Documents: the v0/v1 schema reject pairs, baselines and examples, every journal
entry and profile in the v1 vectors, structural mutations of each, and probes
of format date-time edge cases. Only documents that pass the reference's
stages 1 and 2 are kept. The output is a deterministic stratified sample:
every valid case, every date probe, and invalid cases up to 8000 in total.
"""
import copy, glob, gzip, hashlib, importlib.util, itertools, json, os, random, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
os.chdir(os.path.join(ROOT, "contract", "crucible"))
spec = importlib.util.spec_from_file_location("ref", "scripts/validate-durable-record-fixtures.py")
ref = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ref)
OUT = os.path.join(ROOT, "conformance", "shape-verdicts.json.gz")
random.seed(20261106)
SCHEMA_OF = {  # schema stem -> wire kind for parse_wire
    "journal-entry": "journal_entry", "record": "record", "write-receipt": "write_receipt",
    "stratum-profile": "stratum_profile", "protection-request": "protection_request",
    "protection-receipt": "protection_receipt", "export-request": "export_request",
    "export-result": "export_result", "body-state-request": "body_state_request",
}
V0_PREFIX = {"journal": "journal-entry", "record": "record", "receipt": "write-receipt", "profile": "stratum-profile"}
D = "schemas/durable-record"
bases = []  # (fam, stem, doc)


def add(fam, stem, doc):
    if stem in SCHEMA_OF and isinstance(doc, dict):
        bases.append((fam, stem, doc))


for f in sorted(glob.glob(D + "/v1/rejects/schema/*.json")):
    d = json.load(open(f))
    add("v1", d["schema"], d["baseline"])
    add("v1", d["schema"], d["reject"])
for f in sorted(glob.glob(D + "/v0/rejects/schema/*.json")):
    name = os.path.basename(f).split("-", 1)[1]
    stem = V0_PREFIX.get(name.split("-")[0])
    if stem:
        add("v0", stem, json.load(open(f)))
for fam in ("v0", "v1"):
    for f in sorted(glob.glob(D + "/%s/examples/*.json" % fam)):
        stem = os.path.basename(f).split(".")[0]
        add(fam, stem, json.load(open(f)))
for f in sorted(glob.glob(D + "/v1/vectors/*.json")):
    v = json.load(open(f))
    for e in v.get("journal", []):
        add("v1" if e.get("v") == 1 else "v0", "journal-entry", e)
    for e in v.get("source_journal", []):
        add("v1" if e.get("v") == 1 else "v0", "journal-entry", e)
    if isinstance(v.get("profile"), dict):
        add("v1", "stratum-profile", v["profile"])

ALT = ["x", "", 0, 1, -1, 1.5, True, False, None, [], {}, "\U0001F600", "a" * 300]


def paths(o, p=()):
    yield p, o
    if isinstance(o, dict):
        for k, v in o.items():
            yield from paths(v, p + (k,))
    elif isinstance(o, list):
        for i, v in enumerate(o):
            yield from paths(v, p + (i,))


def setp(o, p, val):
    o = copy.deepcopy(o)
    cur = o
    for k in p[:-1]:
        cur = cur[k]
    cur[p[-1]] = val
    return o


def delp(o, p):
    o = copy.deepcopy(o)
    cur = o
    for k in p[:-1]:
        cur = cur[k]
    del cur[p[-1]]
    return o


def mutations(doc):
    yield doc
    for p, val in list(paths(doc)):
        if not p:
            continue
        if isinstance(p[-1], str):
            yield delp(doc, p)
        if isinstance(val, dict):
            yield setp(doc, p, dict(val, zz_extra=1))
        if isinstance(val, list):
            yield setp(doc, p, val + val[:1])  # duplicate an item (uniqueItems)
            yield setp(doc, p, [])
        if isinstance(val, bool):
            yield setp(doc, p, not val)
        elif isinstance(val, int):
            for d in (-1, 1):
                yield setp(doc, p, val + d)
            yield setp(doc, p, float(val))  # 12 -> 12.0 spelling
        elif isinstance(val, str):
            yield setp(doc, p, val + "\U0001F600")
            yield setp(doc, p, val[:-1] if val else "x")
            yield setp(doc, p, val + "\n")
            yield setp(doc, p, val.upper())
        for alt in random.sample(ALT, 3):
            yield setp(doc, p, alt)
    if isinstance(doc, dict):
        yield dict(doc, zz_extra=1)


items, seen = [], set()
for fam, stem, doc in bases:
    for m in mutations(doc):
        raw = json.dumps(m, ensure_ascii=False, separators=(",", ":"))
        key = fam + "/" + stem + "\0" + raw
        if key in seen:
            continue
        seen.add(key)
        try:
            ref.parse_wire(raw, SCHEMA_OF[stem], "gen")
        except ref.Reject:
            continue
        items.append((fam, stem, raw))
random.shuffle(items)
items = items[:12000]


def suite(batch_items):
    verdicts = {}
    for i in range(0, len(batch_items), 1500):
        chunk = batch_items[i:i + 1500]
        res = ref.shape_suite(chunk, default_ver="v1", timeout_sec=600.0)
        for (cid, schema, raw) in chunk:
            verdicts[cid] = (schema, raw, res[cid])
    return verdicts


main_items = [("c%d" % i, "%s/%s.schema.json" % (fam, stem), raw) for i, (fam, stem, raw) in enumerate(items)]
cases = [{"schema": s, "raw": r, "valid": v} for (s, r, v) in suite(main_items).values()]

# format date-time probes, in a v0 position (format alone) and a v1 position (format + pattern).
v0base = json.load(open(sorted(glob.glob(D + "/v0/rejects/schema/baseline-journal-*.json"))[0]))
v1base = next(e for f in sorted(glob.glob(D + "/v1/vectors/*.json")) for e in json.load(open(f)).get("journal", [])
              if e.get("v") == 1 and e.get("type") == "stream_opened")
dates_ = ["2026-10-20", "2024-02-29", "2026-02-29", "1900-02-29", "2000-02-29", "2026-02-30", "2026-04-31",
          "2026-00-10", "2026-13-01", "2026-10-00", "2026-10-32", "0000-01-01", "9999-12-31", "2026-1-01"]
times_ = ["00:00:00", "23:59:59", "24:00:00", "23:60:00", "23:59:60", "1:00:00", "00:00"]
fracs_ = [".000000", "", ".1", ".123456789", ",5"]
zones_ = ["Z", "z", "+00:00", "+24:00", "-12:60", "+0100", ""]
probes = {d + "T" + t + fr + z for d, t, fr, z in itertools.product(dates_, times_, fracs_, zones_)}
probes |= {"2026-10-20", "2026-02-30", "00:00:00", "00:00:00Z", "2026-10", "20261020", "2026-10-20T00:00:00ZZ", ""}
probe_items = []
for i, s in enumerate(sorted(probes)):
    for fam, base in (("v0", v0base), ("v1", v1base)):
        doc = dict(base)
        doc["at"] = s
        raw = json.dumps(doc, ensure_ascii=False)
        try:
            ref.parse_wire(raw, "journal_entry", "g")
        except ref.Reject:
            continue
        probe_items.append(("p%d%s" % (i, fam), fam + "/journal-entry.schema.json", raw))
probes_out = [{"schema": s, "raw": r, "valid": v} for (s, r, v) in suite(probe_items).values()]

valid = [c for c in cases if c["valid"]]
invalid = [c for c in cases if not c["valid"]]
pick = valid + probes_out
pick += random.sample(invalid, max(0, 8000 - len(pick)))
pick.sort(key=lambda c: (c["schema"], c["raw"]))
data = json.dumps(pick, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode()
open(OUT, "wb").write(gzip.compress(data, 9, mtime=0))
print("%d cases (%d valid), sha256 %s" % (len(pick), sum(c["valid"] for c in pick), hashlib.sha256(data).hexdigest()))
