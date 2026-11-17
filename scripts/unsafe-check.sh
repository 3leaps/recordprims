#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Enforces ADR-0001 §7: workspace denies unsafe_code, and only the single
# Windows platform module (crates/recordprims/src/sys/windows.rs) may carry
# #![allow(unsafe_code)] or use unsafe blocks, each documented with // SAFETY:.
set -euo pipefail

root="${RECORDPRIMS_UNSAFE_CHECK_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"

python3 - "$root" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1]).resolve()
allowed_rel = "crates/recordprims/src/sys/windows.rs"

cargo_root = root / "Cargo.toml"
if not cargo_root.is_file():
    sys.exit("unsafe-check: missing root Cargo.toml")
cargo_text = cargo_root.read_text(encoding="utf-8")
if not re.search(r'^\s*unsafe_code\s*=\s*"deny"\s*$', cargo_text, re.MULTILINE):
    sys.exit('unsafe-check: root Cargo.toml must set unsafe_code = "deny"')

crate_cargo = root / "crates/recordprims/Cargo.toml"
if not crate_cargo.is_file():
    sys.exit("unsafe-check: missing crates/recordprims/Cargo.toml")
crate_cargo_text = crate_cargo.read_text(encoding="utf-8")
if not re.search(r"\[lints\]\s+workspace\s*=\s*true", crate_cargo_text):
    sys.exit("unsafe-check: crates/recordprims/Cargo.toml must inherit [lints] workspace = true")

rs_files = sorted((root / "crates").rglob("*.rs"))
if not rs_files:
    sys.exit("unsafe-check: no .rs files found under crates/")

win_path = root / allowed_rel
if not win_path.is_file():
    sys.exit(f"unsafe-check: expected {allowed_rel} to exist")


def is_ident_char(ch: str) -> bool:
    return ch.isalnum() or ch == "_"


def strip_comments_and_strings(src: str, rel: str) -> tuple[str, set[int]]:
    """
    Lexically blank out Rust comments, raw strings (with exact # delimiter counts),
    normal/byte/c strings, and character literals while preserving newlines.
    Also returns the 0-based line indices that carry a real `// SAFETY:` line comment.
    """
    n = len(src)
    i = 0
    out: list[str] = []
    safety_lines: set[int] = set()
    line_idx = 0

    def emit_blank(ch: str) -> None:
        nonlocal line_idx
        if ch == "\n":
            out.append("\n")
            line_idx += 1
        else:
            out.append(" ")

    def emit_char(ch: str) -> None:
        nonlocal line_idx
        if ch == "\n":
            line_idx += 1
        out.append(ch)

    while i < n:
        # 1. Line comment: //
        if src.startswith("//", i):
            end = src.find("\n", i + 2)
            if end == -1:
                comment_body = src[i + 2 :]
                if "SAFETY:" in comment_body:
                    safety_lines.add(line_idx)
                for ch in src[i:]:
                    emit_blank(ch)
                break
            else:
                comment_body = src[i + 2 : end]
                if "SAFETY:" in comment_body:
                    safety_lines.add(line_idx)
                for ch in src[i:end]:
                    emit_blank(ch)
                i = end
                continue

        # 2. Nested block comment: /* ... */
        if src.startswith("/*", i):
            depth = 1
            emit_blank("/")
            emit_blank("*")
            i += 2
            while i < n and depth > 0:
                if src.startswith("/*", i):
                    depth += 1
                    emit_blank("/")
                    emit_blank("*")
                    i += 2
                elif src.startswith("*/", i):
                    depth -= 1
                    emit_blank("*")
                    emit_blank("/")
                    i += 2
                else:
                    emit_blank(src[i])
                    i += 1
            if depth > 0:
                sys.exit(f"unsafe-check: unclosed block comment in {rel}")
            continue

        prev_is_ident = i > 0 and (is_ident_char(src[i - 1]) or src[i - 1] == "'")

        # 3. Raw string literal: r#*"..."#*, br#*"..."#*, cr#*"..."#*
        if not prev_is_ident:
            prefix_len = 0
            if src.startswith(("br", "cr"), i):
                prefix_len = 2
            elif src[i] == "r":
                prefix_len = 1
            if prefix_len > 0:
                j = i + prefix_len
                hashes = 0
                while j < n and src[j] == "#":
                    hashes += 1
                    j += 1
                if j < n and src[j] == '"':
                    # Consume opening prefix + hashes + quote
                    while i <= j:
                        emit_blank(src[i])
                        i += 1
                    closing = '"' + ("#" * hashes)
                    close_pos = src.find(closing, i)
                    if close_pos == -1:
                        sys.exit(f"unsafe-check: unclosed raw string in {rel}")
                    end_pos = close_pos + len(closing)
                    while i < end_pos:
                        emit_blank(src[i])
                        i += 1
                    continue

        # 4. Normal / byte / C string literal: "...", b"...", c"..."
        if src[i] == '"' or (
            not prev_is_ident
            and src[i] in ("b", "c")
            and i + 1 < n
            and src[i + 1] == '"'
        ):
            if src[i] in ("b", "c"):
                emit_blank(src[i])
                i += 1
            emit_blank(src[i])  # opening "
            i += 1
            closed = False
            while i < n:
                if src[i] == "\\":
                    emit_blank(src[i])
                    i += 1
                    if i < n:
                        emit_blank(src[i])
                        i += 1
                elif src[i] == '"':
                    emit_blank(src[i])
                    i += 1
                    closed = True
                    break
                else:
                    emit_blank(src[i])
                    i += 1
            if not closed:
                sys.exit(f"unsafe-check: unclosed string literal in {rel}")
            continue

        # 5. Character / byte-character literal vs lifetime: 'x', '\...', b'x', b'\...'
        if src[i] == "'" or (
            not prev_is_ident and src.startswith("b'", i)
        ):
            q_start = i + 1 if src[i] == "b" else i
            after_q = q_start + 1
            if after_q < n:
                if src[after_q] == "\\":
                    # Escaped character literal: consume until closing ' on same line
                    k = after_q + 2
                    while k < n and src[k] not in ("'", "\n"):
                        k += 1
                    if k < n and src[k] == "'":
                        while i <= k:
                            emit_blank(src[i])
                            i += 1
                        continue
                elif after_q + 1 < n and src[after_q + 1] == "'" and src[after_q] != "\n":
                    end_k = after_q + 1
                    while i <= end_k:
                        emit_blank(src[i])
                        i += 1
                    continue

        emit_char(src[i])
        i += 1

    return "".join(out), safety_lines


for path in rs_files:
    rel = path.relative_to(root).as_posix()
    raw = path.read_text(encoding="utf-8")
    code, safety_lines = strip_comments_and_strings(raw, rel)

    if rel != allowed_rel:
        if re.search(r"\bunsafe_code\b", code):
            sys.exit(f"unsafe-check: forbidden unsafe_code lint reference in {rel}")
        if re.search(r"\bunsafe\b", code):
            sys.exit(f"unsafe-check: forbidden unsafe keyword in {rel}")
    else:
        code_lines = code.splitlines()
        unsafe_count = 0
        for idx, cline in enumerate(code_lines):
            if re.search(r"\bunsafe\b", cline):
                unsafe_count += 1
                window_start = max(0, idx - 6)
                if not any(ln in safety_lines for ln in range(window_start, idx + 1)):
                    sys.exit(
                        f"unsafe-check: {rel}:{idx + 1}: unsafe usage lacks preceding '// SAFETY:' comment"
                    )
        if unsafe_count == 0:
            sys.exit(f"unsafe-check: {rel} carries allow(unsafe_code) but contains no unsafe blocks")

print("[ok] unsafe-check")
PY
