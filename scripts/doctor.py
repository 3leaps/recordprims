#!/usr/bin/env python3
# SPDX-License-Identifier: MIT OR Apache-2.0
"""Report each host tool's version against its declared minimum; fail closed.

Usage: doctor.py CONFIG PATTERNS SCOPE

CONFIG is .goneat/tools.yaml; PATTERNS is config/doctor-versions.txt. For
every tool in SCOPE this runs its detect_command, requires the first line of
the output to match that tool's pattern in full, takes the pattern's first
group as the version, and compares it with minimum_version (missing parts
count as 0). A tool that is missing, has no pattern, prints a first line that
does not match, or is older than its minimum fails the run.

The config is read with a small strict reader for this file's own shape (no
PyYAML on maintainer hosts); anything it does not recognize is an error, not
a skipped tool.
"""

import re
import shlex
import shutil
import subprocess
import sys



def fail(message):
    raise ValueError(message)


def read_config(path):
    """Return (scopes, tools): scopes maps a name to a tool list; tools maps a
    name to its detect_command and minimum_version."""
    scopes, tools = {}, {}
    section = scope = tool = None
    with open(path, encoding="utf-8") as handle:
        for number, raw in enumerate(handle, 1):
            line = raw.split(" #", 1)[0].rstrip() if not raw.lstrip().startswith("#") else ""
            if not line.strip():
                continue
            indent = len(line) - len(line.lstrip(" "))
            text = line.strip()
            if indent == 0:
                if text not in ("scopes:", "tools:"):
                    fail(f"{path}:{number}: unexpected top-level key {text!r}")
                section, scope, tool = text[:-1], None, None
            elif indent == 2 and text.endswith(":") and " " not in text:
                name = text[:-1]
                if section == "scopes":
                    scope = name
                    scopes[scope] = None
                elif section == "tools":
                    tool = name
                    tools[tool] = {}
                else:
                    fail(f"{path}:{number}: entry outside a section")
            elif indent == 4 and ":" in text:
                key, _, value = text.partition(":")
                value = value.strip()
                if section == "scopes" and scope is not None and key == "tools":
                    match = re.fullmatch(r"\[([a-z0-9_, -]*)\]", value)
                    if not match:
                        fail(f"{path}:{number}: scope tools must be a [a, b] list")
                    scopes[scope] = [t.strip() for t in match.group(1).split(",") if t.strip()]
                elif section == "tools" and tool is not None:
                    if value.startswith('"') and value.endswith('"') and len(value) >= 2:
                        value = value[1:-1]
                    tools[tool][key] = value
            # Deeper or other keys (description, installer lists) are not needed.
    for name, listed in scopes.items():
        if not listed:
            fail(f"{path}: scope {name} lists no tools")
    return scopes, tools


def read_patterns(path):
    """Return a map of tool name to a compiled full-match version pattern."""
    patterns = {}
    with open(path, encoding="utf-8") as handle:
        for number, raw in enumerate(handle, 1):
            line = raw.rstrip("\n")
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            name, _, pattern = line.partition(" ")
            pattern = pattern.strip()
            if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", name) or not pattern or name in patterns:
                fail(f"{path}:{number}: malformed or duplicate pattern line")
            try:
                compiled = re.compile(pattern)
            except re.error as error:
                fail(f"{path}:{number}: invalid pattern: {error}")
            if compiled.groups < 1:
                fail(f"{path}:{number}: the pattern needs a version group")
            patterns[name] = compiled
    return patterns


def parse(version):
    return tuple(int(part) for part in version.split("."))


def at_least(have, want):
    a, b = parse(have), parse(want)
    width = max(len(a), len(b))
    return a + (0,) * (width - len(a)) >= b + (0,) * (width - len(b))


def check(name, spec, pattern):
    command = spec.get("detect_command", "")
    minimum = spec.get("minimum_version", "")
    if not command:
        return f"{name}: no detect_command declared"
    if not re.fullmatch(r"[0-9]+(?:\.[0-9]+)*", minimum):
        return f"{name}: no valid minimum_version declared"
    argv = shlex.split(command)
    if shutil.which(argv[0]) is None:
        return f"{name} is missing"
    try:
        result = subprocess.run(argv, capture_output=True, text=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        return f"{name}: {command} failed: {error}"
    output = (result.stdout or "") or (result.stderr or "")
    first = output.splitlines()[0].strip() if output.strip() else ""
    match = pattern.fullmatch(first) if result.returncode == 0 else None
    if match is None or not re.fullmatch(r"[0-9]+(?:\.[0-9]+)+", match.group(1) or ""):
        return f"{name}: cannot read a version from {command!r} (got {first[:60]!r})"
    have = match.group(1)
    if not at_least(have, minimum):
        return f"{name} {have} is below the recommended minimum {minimum}"
    print(f"[ok] {name} {have} (minimum {minimum})")
    return None


def main():
    if len(sys.argv) != 4:
        fail("usage: doctor.py CONFIG PATTERNS SCOPE")
    config, pattern_file, scope = sys.argv[1], sys.argv[2], sys.argv[3]
    scopes, tools = read_config(config)
    patterns = read_patterns(pattern_file)
    if scope not in scopes:
        fail(f"unknown scope {scope} ({', '.join(sorted(scopes))})")
    problems = []
    for name in scopes[scope]:
        if name not in tools:
            problems.append(f"{name}: listed in scope {scope} but not declared under tools")
            continue
        if name not in patterns:
            problems.append(f"{name}: no version pattern in {pattern_file}")
            continue
        problem = check(name, tools[name], patterns[name])
        if problem:
            problems.append(problem)
    for problem in problems:
        print(f"error: {problem}", file=sys.stderr)
    if problems:
        sys.exit(1)
    print(f"[ok] doctor ({scope}): every tool meets its recommended minimum")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError) as error:
        sys.exit(f"error: {error}")
