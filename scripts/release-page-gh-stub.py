#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Offline stand-in for the GitHub CLI in the release-page tests.

Answers only the calls the release-page scripts make, from a store directory
(GH_STUB_STORE): workflow runs and their artifacts, and releases with their
assets. Publishing marks a release immutable unless GH_STUB_MUTABLE is set.
Any other call fails loudly, so a script cannot reach an unexpected endpoint.
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


def releases():
    out = []
    for meta in sorted((store / "releases").glob("*/meta.json")):
        rel = json.loads(meta.read_text())
        rel["assets"] = [{"name": p.name, "size": p.stat().st_size} for p in sorted((meta.parent / "assets").iterdir())]
        out.append(rel)
    return out


def save(rel):
    d = store / "releases" / str(rel["id"])
    (d / "assets").mkdir(parents=True, exist_ok=True)
    meta = {k: v for k, v in rel.items() if k != "assets"}
    (d / "meta.json").write_text(json.dumps(meta))


def opt(name):
    return args[args.index(name) + 1] if name in args else None


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
    for f in files:
        shutil.copy(f, store / "releases" / str(rid) / "assets" / pathlib.Path(f).name)
elif args[:2] == ["release", "download"]:
    tag, dest = args[2], pathlib.Path(opt("--dir"))
    rel = [r for r in releases() if r["tag_name"] == tag]
    if len(rel) != 1:
        fail("release not found")
    dest.mkdir(parents=True, exist_ok=True)
    for a in (store / "releases" / str(rel[0]["id"]) / "assets").iterdir():
        shutil.copy(a, dest / a.name)
elif args and args[0] == "api":
    path = next(a for a in args[1:] if a.startswith("repos/"))
    rest = path.split("/", 3)[3]
    if rest.startswith("releases?"):
        print(json.dumps(releases()))
    elif rest.startswith("releases/tags/"):
        tag = rest.split("/", 2)[2]
        rel = [r for r in releases() if r["tag_name"] == tag and not r["draft"]]
        if not rel:
            fail("HTTP 404", 1)
        print(json.dumps(rel[0]))
    elif rest.startswith("releases/"):
        rid = rest.split("/")[1]
        rel = [r for r in releases() if str(r["id"]) == rid]
        if not rel:
            fail("HTTP 404", 1)
        rel = rel[0]
        if "PATCH" in args:
            rel["draft"] = False
            rel["immutable"] = not os.environ.get("GH_STUB_MUTABLE")
            save(rel)
        print(json.dumps(rel))
    else:
        fail(f"unexpected api path {path}")
else:
    fail(f"unexpected call: {' '.join(args)}")
