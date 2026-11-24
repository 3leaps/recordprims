#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Offline stand-in for the GitHub CLI in the release-page tests.

Answers only the calls the release-page scripts make, from a store directory
(GH_STUB_STORE): workflow runs and their artifacts, and releases with their
assets. An asset is `uploaded`, or `starter` (zero bytes, left by an upload
that failed). Publishing marks a release immutable unless GH_STUB_MUTABLE is
set. Any other call fails loudly, so a script cannot reach an unexpected
endpoint.
"""

import json
import os
import pathlib
import shutil
import sys

store = pathlib.Path(os.environ["GH_STUB_STORE"])
args = sys.argv[1:]
(store / "calls.log").open("a").write(" ".join(args) + "\n")


def fail(msg, code=1):
    sys.stderr.write(f"gh stub: {msg}\n")
    sys.exit(code)


def starters(rdir):
    path = rdir / "starter.json"
    return set(json.loads(path.read_text())) if path.exists() else set()


def set_starters(rdir, names):
    (rdir / "starter.json").write_text(json.dumps(sorted(names)))


def releases():
    out = []
    for meta in sorted((store / "releases").glob("*/meta.json")):
        rel = json.loads(meta.read_text())
        rdir = meta.parent
        marked = starters(rdir)
        rel["assets"] = [
            {"id": rel["id"] * 100 + i, "name": p.name, "size": p.stat().st_size,
             "state": "starter" if p.name in marked else "uploaded"}
            for i, p in enumerate(sorted((rdir / "assets").iterdir()))
        ]
        out.append(rel)
    return out


def save(rel):
    d = store / "releases" / str(rel["id"])
    (d / "assets").mkdir(parents=True, exist_ok=True)
    meta = {k: v for k, v in rel.items() if k != "assets"}
    (d / "meta.json").write_text(json.dumps(meta))


def opt(name):
    return args[args.index(name) + 1] if name in args else None


def opts(name):
    return [args[i + 1] for i, a in enumerate(args[:-1]) if a == name]


if args[:2] == ["run", "list"]:
    print((store / "runs.json").read_text())
elif args[:2] == ["run", "view"]:
    for run in json.loads((store / "runs.json").read_text()):
        if str(run["databaseId"]) == args[2]:
            print(json.dumps({**run, "workflowName": run.get("workflowName", "Release")}))
            break
    else:
        fail("run not found")
elif args[:2] == ["run", "download"]:
    src = store / "artifacts" / args[2]
    if not src.is_dir():
        fail("no artifacts")
    shutil.copytree(src, opt("-D"))
elif args[:2] == ["release", "create"]:
    tag = args[2]
    if any(r["tag_name"] == tag for r in releases()):
        fail("release exists")
    rid = 1000 + len(releases())
    files = [a for a in args[3:] if a.startswith("/") and a != opt("--notes-file")]
    rel = {"id": rid, "tag_name": tag, "draft": "--draft" in args, "immutable": False,
           "target_commitish": opt("--target"), "body": pathlib.Path(opt("--notes-file")).read_text().rstrip("\n")}
    save(rel)
    rdir = store / "releases" / str(rid)
    # GH_STUB_FAIL_AFTER=N: the draft exists, then the upload stops after N
    # assets; with GH_STUB_STARTER set, the next asset is left as a zero-byte
    # starter asset.
    limit = int(os.environ.get("GH_STUB_FAIL_AFTER", len(files)))
    for f in files[:limit]:
        shutil.copy(f, rdir / "assets" / pathlib.Path(f).name)
    if limit < len(files):
        if os.environ.get("GH_STUB_STARTER"):
            name = pathlib.Path(files[limit]).name
            (rdir / "assets" / name).write_bytes(b"")
            set_starters(rdir, {name})
        fail("upload interrupted (HTTP 502)")
elif args[:2] == ["release", "upload"]:
    tag = args[2]
    rel = [r for r in releases() if r["tag_name"] == tag]
    if len(rel) != 1:
        fail("release not found")
    dest = store / "releases" / str(rel[0]["id"]) / "assets"
    for f in [a for a in args[3:] if a.startswith("/")]:
        if (dest / pathlib.Path(f).name).exists():
            fail("asset exists")
        shutil.copy(f, dest / pathlib.Path(f).name)
elif args[:2] == ["release", "download"]:
    tag, dest = args[2], pathlib.Path(opt("--dir"))
    rel = [r for r in releases() if r["tag_name"] == tag]
    if len(rel) != 1:
        fail("release not found")
    patterns = opts("-p")
    chosen = [a for a in rel[0]["assets"] if not patterns or a["name"] in patterns]
    if any(a["state"] != "uploaded" for a in chosen):
        fail("asset not downloadable (starter)")
    dest.mkdir(parents=True, exist_ok=True)
    for a in chosen:
        shutil.copy(store / "releases" / str(rel[0]["id"]) / "assets" / a["name"], dest / a["name"])
elif args and args[0] == "api":
    path = next(a for a in args[1:] if a.startswith("repos/"))
    rest = path.split("/", 3)[3]
    if rest == "immutable-releases":
        print(json.dumps({"enabled": not os.environ.get("GH_STUB_IMMUTABLE_OFF")}))
    elif rest.startswith("releases?"):
        print(json.dumps(releases()))
    elif rest.startswith("releases/tags/"):
        tag = rest.split("/", 2)[2]
        rel = [r for r in releases() if r["tag_name"] == tag and not r["draft"]]
        if not rel:
            fail("HTTP 404", 1)
        print(json.dumps(rel[0]))
    elif rest.startswith("releases/assets/") and "DELETE" in args:
        aid = int(rest.split("/")[2])
        for rel in releases():
            for a in rel["assets"]:
                if a["id"] == aid:
                    rdir = store / "releases" / str(rel["id"])
                    (rdir / "assets" / a["name"]).unlink()
                    set_starters(rdir, starters(rdir) - {a["name"]})
                    sys.exit(0)
        fail("HTTP 404", 1)
    elif rest.startswith("releases/"):
        rid = rest.split("/")[1]
        rel = [r for r in releases() if str(r["id"]) == rid]
        if not rel:
            fail("HTTP 404", 1)
        rel = rel[0]
        if "DELETE" in args:
            shutil.rmtree(store / "releases" / rid)
            sys.exit(0)
        if "PATCH" in args:
            rel["draft"] = False
            rel["immutable"] = not os.environ.get("GH_STUB_MUTABLE")
            save(rel)
        print(json.dumps(rel))
    else:
        fail(f"unexpected api path {path}")
else:
    fail(f"unexpected call: {' '.join(args)}")
