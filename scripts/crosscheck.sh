#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Cross-language differential and bidirectional interoperability check between
# the pure-Go implementation and the Rust twin (`crates/recordprims`).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

PYTHON_BIN="${PYTHON:-python3}"

echo "Building Go and Rust differential drivers..."
cargo build --quiet --locked --example recordprims-driver

"$PYTHON_BIN" -I -B - <<'PY'
import json
import os
import subprocess
import sys
import tempfile

root = os.getcwd()
vectors_path = os.path.join(root, "conformance", "boundary", "store-body-vectors.json")
if not os.path.isfile(vectors_path):
    raise SystemExit(f"crosscheck: missing {vectors_path}")

exe_suffix = ".exe" if os.name == "nt" else ""
rust_driver = os.path.join(root, "target", "debug", "examples", f"recordprims-driver{exe_suffix}")
if not os.path.isfile(rust_driver):
    raise SystemExit(f"crosscheck: missing {rust_driver}")

def run_driver(driver_bin, scenarios):
    payload = "\n".join(json.dumps(s, separators=(",", ":")) for s in scenarios) + "\n"
    proc = subprocess.run(
        [driver_bin],
        input=payload.encode("utf-8"),
        capture_output=True,
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(f"crosscheck: driver {driver_bin} exited with {proc.returncode}")
    lines = [
        line.strip()
        for line in proc.stdout.decode("utf-8").splitlines()
        if line.strip()
    ]
    if len(lines) != len(scenarios):
        raise SystemExit(
            f"crosscheck: driver {driver_bin} returned {len(lines)} lines for {len(scenarios)} scenarios"
        )
    return lines

with tempfile.TemporaryDirectory(prefix="recordprims-crosscheck-") as work_dir:
    go_driver = os.path.join(work_dir, f"go-driver{exe_suffix}")
    env = os.environ.copy()
    env["CGO_ENABLED"] = "0"
    build_proc = subprocess.run(
        ["go", "build", "-o", go_driver, "./internal/difftool"],
        cwd=root,
        env=env,
        capture_output=True,
    )
    if build_proc.returncode != 0:
        sys.stderr.write(build_proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(f"crosscheck: go build failed with {build_proc.returncode}")

    # 1. Boundary & error-mapping vectors
    with open(vectors_path, "r", encoding="utf-8") as f:
        boundary_cases = json.load(f)

    if len(boundary_cases) < 15:
        raise SystemExit(f"crosscheck: expected >= 15 boundary scenarios, got {len(boundary_cases)}")

    go_boundary = run_driver(go_driver, boundary_cases)
    rs_boundary = run_driver(rust_driver, boundary_cases)

    for g_line, r_line in zip(go_boundary, rs_boundary):
        if g_line != r_line:
            raise SystemExit(
                f"crosscheck: Go and Rust boundary vector outputs diverged:\n"
                f"  Go:   {g_line}\n"
                f"  Rust: {r_line}"
            )

    seen_errors = set()
    seen_outcomes = set()
    for line in go_boundary:
        row = json.loads(line)
        for r in row["results"]:
            if not r["ok"]:
                seen_errors.add(r.get("error_code", ""))
            elif "outcome" in r:
                seen_outcomes.add(r["outcome"])

    required_errors = {
        "store_invalid",
        "schema",
        "journal_invalid",
        "closed",
        "clock_regression",
        "locked",
    }
    required_outcomes = {
        "absent",
        "pending",
        "superseded",
        "removed",
        "body_conflict",
        "missing",
        "protection_unknown",
        "still_referenced",
    }
    missing_err = required_errors - seen_errors
    missing_out = required_outcomes - seen_outcomes
    if missing_err:
        raise SystemExit(f"crosscheck: boundary suite did not exercise errors: {sorted(missing_err)}")
    if missing_out:
        raise SystemExit(f"crosscheck: boundary suite did not exercise outcomes: {sorted(missing_out)}")

    by_id = {json.loads(line)["id"]: json.loads(line)["results"] for line in go_boundary}
    if by_id["remove-body-still-referenced-valid-untombstoned"][6].get("outcome") != "still_referenced":
        raise SystemExit("crosscheck: expected still_referenced on valid untombstoned")
    if by_id["remove-body-protection-unknown-bad-untombstoned"][6].get("outcome") != "protection_unknown":
        raise SystemExit("crosscheck: expected protection_unknown on bad untombstoned")
    if by_id["remove-body-removed-with-reviewed-continuation"][8].get("outcome") != "removed":
        raise SystemExit("crosscheck: expected removed on reviewed continuation")
    for idx in (5, 7, 9):
        if by_id["remove-body-protection-unknown-malformed-lease-incarnation"][idx].get("outcome") != "protection_unknown":
            raise SystemExit(f"crosscheck: expected protection_unknown on malformed lease step {idx}")
    if by_id["driver-cleanup-io-detail"][0].get("detail") != "recordprims: io_error{op=unlink, class=permission}":
        raise SystemExit("crosscheck: expected cleanup detail on driver-cleanup-io-detail")

    # 2. Byte-for-byte on-disk layout differential + 3. Bidirectional cross-open
    go_dir = os.path.join(work_dir, "store-go")
    rs_dir = os.path.join(work_dir, "store-rust")
    os.mkdir(go_dir, 0o700)
    os.mkdir(rs_dir, 0o700)
    os.chmod(go_dir, 0o700)
    os.chmod(rs_dir, 0o700)

    def make_diff_scenario(store_dir):
        return {
            "id": "disk-layout-differential",
            "store_dir": store_dir,
            "steps": [
                {"op": "init", "at": "2026-10-25T12:00:00.000000Z"},
                {"op": "open", "at": "2026-10-25T12:00:01.000000Z", "pending_body_ttl_ms": 60000},
                {"op": "put_body", "data_utf8": "alpha-body\n", "media_type": "text/plain"},
                {"op": "set_clock", "at": "2026-10-25T12:00:02.000000Z"},
                {"op": "put_body", "data_utf8": "beta-body\n", "media_type": "application/octet-stream"},
                {"op": "set_clock", "at": "2026-10-25T12:02:00.000000Z"},
                {
                    "op": "remove_body",
                    "digest": "sha256:6b2e50e4d6ee6bf6b3cd7a74d8e778fa548ef9988996ef1639bc1f285aa48225",
                    "length": 11,
                    "complete_incarnation": 1,
                    "reason": "operator",
                    "provenance": {
                        "actor_kind": "agent",
                        "actor": "cleaner",
                        "supervisor": "ops-lead",
                        "model": "test-model",
                        "tool": "crosscheck",
                    },
                    "authorization": {
                        "ref": "auth-001",
                        "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
                    },
                },
                {"op": "set_clock", "at": "2026-10-25T12:03:00.000000Z"},
                {"op": "put_body", "data_utf8": "alpha-body\n", "media_type": "text/plain"},
                {"op": "close"},
            ],
        }

    go_diff = run_driver(go_driver, [make_diff_scenario(go_dir)])
    rs_diff = run_driver(rust_driver, [make_diff_scenario(rs_dir)])
    if go_diff != rs_diff:
        raise SystemExit(
            f"crosscheck: Go and Rust differential script outputs diverged:\n"
            f"  Go:   {go_diff[0]}\n"
            f"  Rust: {rs_diff[0]}"
        )

    def read_bytes(base, rel):
        with open(os.path.join(base, *rel.split("/")), "rb") as f:
            return f.read()

    if read_bytes(go_dir, "profile.json") != read_bytes(rs_dir, "profile.json"):
        raise SystemExit("crosscheck: profile.json differs between Go and Rust")

    go_store = json.loads(read_bytes(go_dir, "STORE").decode("utf-8"))
    rs_store = json.loads(read_bytes(rs_dir, "STORE").decode("utf-8"))
    for k in ("format", "key_generation", "created_at", "profile_digest", "idempotency_window_ms"):
        if go_store.get(k) != rs_store.get(k):
            raise SystemExit(
                f"crosscheck: STORE[{k!r}] differs: Go={go_store.get(k)!r} Rust={rs_store.get(k)!r}"
            )

    def collect_tree(base, sub):
        tree_root = os.path.join(base, *sub.split("/"))
        out = {}
        for dirpath, _, filenames in os.walk(tree_root):
            for fn in sorted(filenames):
                full = os.path.join(dirpath, fn)
                rel = os.path.relpath(full, base).replace("\\", "/")
                with open(full, "rb") as f:
                    out[rel] = f.read()
        return out

    for sub in ("streams", "bodies/sha256", "bodies/incarnation"):
        g_map = collect_tree(go_dir, sub)
        r_map = collect_tree(rs_dir, sub)
        if set(g_map.keys()) != set(r_map.keys()):
            raise SystemExit(
                f"crosscheck: file set under {sub} differs: Go={sorted(g_map)} Rust={sorted(r_map)}"
            )
        for rel in sorted(g_map):
            if g_map[rel] != r_map[rel]:
                raise SystemExit(
                    f"crosscheck: {rel} bytes differ between Go and Rust:\n"
                    f"  Go:   {g_map[rel]!r}\n"
                    f"  Rust: {r_map[rel]!r}"
                )

    def make_cross_mutate_scenario(store_dir):
        return {
            "id": "cross-open-mutate",
            "store_dir": store_dir,
            "steps": [
                {"op": "open", "at": "2026-10-25T12:10:00.000000Z", "pending_body_ttl_ms": 1000},
                {"op": "put_body", "data_utf8": "cross-language-body\n", "media_type": "text/plain"},
                {
                    "op": "remove_body",
                    "digest": "sha256:6b2e50e4d6ee6bf6b3cd7a74d8e778fa548ef9988996ef1639bc1f285aa48225",
                    "length": 11,
                    "complete_incarnation": 2,
                    "reason": "retention",
                    "provenance": {"actor_kind": "process", "actor": "cross-gc"},
                },
                {"op": "close"},
            ],
        }

    def make_cross_verify_scenario(store_dir):
        return {
            "id": "cross-reopen-verify",
            "store_dir": store_dir,
            "steps": [
                {"op": "open", "at": "2026-10-25T12:20:00.000000Z", "pending_body_ttl_ms": 1000},
                {
                    "op": "remove_body",
                    "digest": "sha256:6b2e50e4d6ee6bf6b3cd7a74d8e778fa548ef9988996ef1639bc1f285aa48225",
                    "length": 11,
                    "reason": "retention",
                    "provenance": {"actor_kind": "process", "actor": "cross-gc"},
                },
                {"op": "close"},
            ],
        }

    # Go store -> mutated by Rust -> verified by Go
    cross_go_by_rust = run_driver(rust_driver, [make_cross_mutate_scenario(go_dir)])
    cross_go_reopen = run_driver(go_driver, [make_cross_verify_scenario(go_dir)])

    # Rust store -> mutated by Go -> verified by Rust
    cross_rust_by_go = run_driver(go_driver, [make_cross_mutate_scenario(rs_dir)])
    cross_rust_reopen = run_driver(rust_driver, [make_cross_verify_scenario(rs_dir)])

    if cross_go_by_rust != cross_rust_by_go:
        raise SystemExit("crosscheck: cross-mutation outputs diverged")
    if cross_go_reopen != cross_rust_reopen:
        raise SystemExit("crosscheck: cross-reopen outputs diverged")

    for sub in ("streams", "bodies/sha256", "bodies/incarnation"):
        g_map = collect_tree(go_dir, sub)
        r_map = collect_tree(rs_dir, sub)
        if g_map != r_map:
            raise SystemExit(f"crosscheck: post-cross-mutation {sub} trees diverged")


    # 4. Streams: the same op script on fresh stores gives identical outputs,
    # identical journals and heads/records indexes, and keys indexes that
    # differ only in the per-store keyed MAC. Each side then reads the other's
    # streams without rewriting any index.
    import hashlib

    def jcs(o):
        return json.dumps(o, sort_keys=True, separators=(",", ":"), ensure_ascii=False)

    def digest(o):
        return "sha256:" + hashlib.sha256(jcs(o).encode()).hexdigest()

    BODY_DIG = "sha256:492989c80e0dc0a4fbd5bb5823d942793b1d74527b60fdd0458d8d12526dff1c"
    PROV = {"actor_kind": "process", "actor": "crosscheck"}
    CLS = {"retention": "standard", "sensitivity": "0-public"}

    def rec(stream, rid, rev, payload=None, sup=None, body=False):
        r = {"v": 1, "stream": stream, "record_id": rid, "revision": rev,
             "payload_contract": "contract: example/v0", "provenance": PROV, "classification": CLS}
        if payload is not None:
            r["payload"] = payload
        if sup is not None:
            r["supersedes"] = {"revision": rev - 1, "digest": sup}
        if body:
            r["body"] = {"digest": BODY_DIG, "length": 11, "media_type": "text/plain"}
        return r

    def stream_steps():
        S, Q = "obs.cpu", "obs.single"
        a1 = rec(S, "cpu-0", 1, {"load": 3})
        a2 = rec(S, "cpu-0", 2, {"load": 5}, sup=digest(a1))
        bad = rec(S, "cpu-0", 3, {"load": 9}, sup="sha256:" + "0" * 64)
        b1 = rec(S, "cpu-1", 1, None, body=True)
        q1 = rec(Q, "only", 1, {"x": 1})
        q2 = rec(Q, "only", 2, {"x": 2}, sup=digest(q1))
        nohead = rec(S, "ghost", 2, {"load": 0}, sup="sha256:" + "1" * 64)
        t = lambda m, s: {"op": "set_clock", "at": f"2026-10-25T12:{m:02d}:{s:02d}.000000Z"}
        return [
            {"op": "open_stream", "stream": S, "stream_mode": "revisioned"},
            {"op": "open_stream", "stream": Q, "stream_mode": "single_revision"},
            {"op": "open_stream", "stream": S, "stream_mode": "single_revision"},
            {"op": "open_stream", "stream": "bodies", "stream_mode": "revisioned"},
            t(5, 0), {"op": "append", "stream": S, "record": a1, "key": "k-a1"},
            t(5, 1), {"op": "append", "stream": S, "record": a1, "key": "k-a1"},
            t(5, 2), {"op": "append", "stream": S, "record": a2, "key": "k-a1"},
            t(5, 3), {"op": "append", "stream": S, "record": a2, "key": "k-a2"},
            t(5, 4), {"op": "append", "stream": S, "record": bad, "key": "k-bad"},
            t(5, 5), {"op": "append", "stream": S, "record": nohead, "key": "k-ghost"},
            t(5, 6), {"op": "append", "stream": S, "record": b1, "key": "k-b1"},
            t(5, 7), {"op": "append", "stream": Q, "record": q1, "key": "k-q1"},
            t(5, 8), {"op": "append", "stream": Q, "record": q2, "key": "k-q2"},
            {"op": "head", "stream": S, "record_id": "cpu-0"},
            {"op": "head", "stream": S, "record_id": "nobody"},
            {"op": "revisions", "stream": S, "record_id": "cpu-0"},
            {"op": "revisions", "stream": S, "record_id": "cpu-1"},
            t(5, 9), {"op": "reconcile", "stream": S, "key": "k-a2", "attempted_at": "2026-10-25T12:05:03.000000Z"},
            {"op": "reconcile", "stream": S, "key": "k-never", "attempted_at": "2026-10-25T12:05:03.000000Z"},
            {"op": "release", "stream": S, "purpose": "serve", "closure": "full_content"},
            {"op": "release", "stream": S, "purpose": "export", "closure": "metadata_only"},
            {"op": "release", "stream": Q, "purpose": "reuse", "closure": "full_content"},
            t(4, 0), {"op": "append", "stream": S, "record": rec(S, "late", 1, {}), "key": "k-late"},
            {"op": "head", "stream": "never.opened", "record_id": "x"},
        ]

    def scenario(store_dir=None):
        sc = {"id": "stream-differential", "steps": [
            {"op": "init", "at": "2026-10-25T12:00:00.000000Z"},
            {"op": "open", "at": "2026-10-25T12:00:01.000000Z", "pending_body_ttl_ms": 60000},
            {"op": "put_body", "data_utf8": "alpha-body\n", "media_type": "text/plain"},
        ] + stream_steps() + [{"op": "close"}]}
        if store_dir:
            sc["store_dir"] = store_dir
        return sc


    go_sdir = os.path.join(work_dir, "streams-go")
    rs_sdir = os.path.join(work_dir, "streams-rust")
    for d in (go_sdir, rs_sdir):
        os.mkdir(d, 0o700)
        os.chmod(d, 0o700)
    go_stream = run_driver(go_driver, [scenario(go_sdir)])
    rs_stream = run_driver(rust_driver, [scenario(rs_sdir)])
    if go_stream != rs_stream:
        for g_r, r_r in zip(json.loads(go_stream[0])["results"], json.loads(rs_stream[0])["results"]):
            if g_r != r_r:
                raise SystemExit(f"crosscheck: stream step diverged:\n  Go:   {g_r}\n  Rust: {r_r}")
        raise SystemExit("crosscheck: stream scenario outputs diverged")
    stream_rows = json.loads(go_stream[0])["results"]
    seen_stream = {r["data"].get("outcome") for r in stream_rows if r.get("op") == "append" and r.get("ok")}
    for want in ("appended", "revised", "idempotency_conflict", "revision_conflict", "no_head", "mode_violation"):
        if want not in seen_stream:
            raise SystemExit(f"crosscheck: stream scenario did not exercise append outcome {want!r}")
    if not any(f["kind"] == "body" for r in stream_rows if r.get("op") == "release" for f in r["data"]["frames"]):
        raise SystemExit("crosscheck: stream scenario released no body frame")

    # keys.idx carries a MAC under each store's own diagnostic key, so its
    # key ids and the content digest over them differ between two stores.
    # The cross-open below proves each side computes the other's MACs: a
    # reader that disagreed would rewrite the writer's keys.idx.
    def mask_keys(raw):
        doc = json.loads(raw)
        doc["content_digest"] = "masked"
        for e in doc["entries"]:
            e["key_id"] = "masked"
        return doc

    g_map = collect_tree(go_sdir, "streams")
    r_map = collect_tree(rs_sdir, "streams")
    if set(g_map) != set(r_map):
        raise SystemExit(f"crosscheck: stream file sets differ: Go={sorted(g_map)} Rust={sorted(r_map)}")
    if not any(rel.endswith("/keys.idx") for rel in g_map):
        raise SystemExit("crosscheck: stream scenario wrote no indexes")
    for rel in sorted(g_map):
        if rel.endswith("/keys.idx"):
            same = mask_keys(g_map[rel]) == mask_keys(r_map[rel])
        else:
            same = g_map[rel] == r_map[rel]
        if not same:
            raise SystemExit(
                f"crosscheck: {rel} differs between Go and Rust:\n  Go:   {g_map[rel]!r}\n  Rust: {r_map[rel]!r}"
            )

    def read_steps(store_dir):
        steps = [{"op": "open", "at": "2026-10-25T12:30:00.000000Z"}]
        for sid, mode in (("obs.cpu", "revisioned"), ("obs.single", "single_revision")):
            steps.append({"op": "open_stream", "stream": sid, "stream_mode": mode})
        steps += [
            {"op": "head", "stream": "obs.cpu", "record_id": "cpu-0"},
            {"op": "revisions", "stream": "obs.cpu", "record_id": "cpu-0"},
            {"op": "release", "stream": "obs.cpu", "purpose": "serve", "closure": "full_content"},
            {"op": "release", "stream": "obs.single", "purpose": "export", "closure": "metadata_only"},
            {"op": "close"},
        ]
        return {"id": "stream-cross-read", "store_dir": store_dir, "steps": steps}

    for writer_dir, reader, label in ((go_sdir, rust_driver, "Rust reading Go"), (rs_sdir, go_driver, "Go reading Rust")):
        before = collect_tree(writer_dir, "streams")
        own = go_driver if reader == rust_driver else rust_driver
        want = run_driver(own, [read_steps(writer_dir)])
        got = run_driver(reader, [read_steps(writer_dir)])
        if want != got:
            raise SystemExit(f"crosscheck: {label} streams diverged:\n  writer: {want[0]}\n  reader: {got[0]}")
        if collect_tree(writer_dir, "streams") != before:
            raise SystemExit(f"crosscheck: {label} rewrote stream files on a read")

print(
    f"[ok] crosscheck ({len(boundary_cases)} boundary scenarios, "
    "byte-identical store/body/stream layout, Go<->Rust cross-open)"
)
PY
