#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Unit tests for scripts/unsafe-check.sh.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
check_script="$repo_root/scripts/unsafe-check.sh"

# 1. Live repository must pass.
"$check_script" >/dev/null

tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/recordprims-unsafe-check-XXXXXX")"
trap 'rm -rf "$tmpdir"' EXIT

setup_fixture() {
  local dir="$1"
  rm -rf "$dir"
  mkdir -p "$dir/crates/recordprims/src/sys"
  cat >"$dir/Cargo.toml" <<'EOF'
[workspace]
members = ["crates/recordprims"]
[workspace.lints.rust]
unsafe_code = "deny"
EOF
  cat >"$dir/crates/recordprims/Cargo.toml" <<'EOF'
[package]
name = "recordprims"
version = "0.0.2"
[lints]
workspace = true
EOF
  cat >"$dir/crates/recordprims/src/lib.rs" <<'EOF'
pub fn safe_fn() -> u32 { 42 }
EOF
  cat >"$dir/crates/recordprims/src/sys/windows.rs" <<'EOF'
#![allow(unsafe_code)]
pub fn win_fn() -> u32 {
    // SAFETY: trivial test block.
    unsafe { 1 }
}
EOF
}

# 2. Clean fixture passes.
setup_fixture "$tmpdir/clean"
RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/clean" "$check_script" >/dev/null

# 3. allow(unsafe_code) outside windows.rs fails.
setup_fixture "$tmpdir/bad-allow"
cat >"$tmpdir/bad-allow/crates/recordprims/src/lib.rs" <<'EOF'
#[allow(unsafe_code)]
pub fn bad() {}
EOF
if RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/bad-allow" "$check_script" >/dev/null 2>&1; then
  echo "FAIL: expected allow(unsafe_code) in lib.rs to fail" >&2
  exit 1
fi

# 4. unsafe block outside windows.rs fails.
setup_fixture "$tmpdir/bad-unsafe"
cat >"$tmpdir/bad-unsafe/crates/recordprims/src/lib.rs" <<'EOF'
pub fn bad() { unsafe {} }
EOF
if RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/bad-unsafe" "$check_script" >/dev/null 2>&1; then
  echo "FAIL: expected unsafe in lib.rs to fail" >&2
  exit 1
fi

# 5. unsafe block in windows.rs without // SAFETY: fails.
setup_fixture "$tmpdir/no-safety"
cat >"$tmpdir/no-safety/crates/recordprims/src/sys/windows.rs" <<'EOF'
#![allow(unsafe_code)]
pub fn win_fn() -> u32 {
    unsafe { 1 }
}
EOF
if RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/no-safety" "$check_script" >/dev/null 2>&1; then
  echo "FAIL: expected undocumented unsafe in windows.rs to fail" >&2
  exit 1
fi

# 6. Cargo.toml without unsafe_code = "deny" fails.
setup_fixture "$tmpdir/bad-cargo"
cat >"$tmpdir/bad-cargo/Cargo.toml" <<'EOF'
[workspace]
members = ["crates/recordprims"]
[workspace.lints.rust]
unsafe_code = "forbid"
EOF
if RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/bad-cargo" "$check_script" >/dev/null 2>&1; then
  echo "FAIL: expected missing unsafe_code = \"deny\" in Cargo.toml to fail" >&2
  exit 1
fi

# 7. Raw string with embedded quote followed by unsafe/allow(unsafe_code) must fail (secrev/devrev GR-01).
setup_fixture "$tmpdir/raw-string-smuggle"
cat >"$tmpdir/raw-string-smuggle/crates/recordprims/src/lib.rs" <<'EOF'
const A: &str = r#" raw " "#;
#[allow(unsafe_code)]
pub fn smuggled() {
    unsafe { core::ptr::null::<u8>().read() };
}
const B: &str = r#" end "#;
EOF
if RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/raw-string-smuggle" "$check_script" >/dev/null 2>&1; then
  echo "FAIL: expected unsafe after raw string with embedded quote to fail" >&2
  exit 1
fi

# 8. Nested block comment followed by unsafe must fail, and unsafe inside nested block comment must pass.
setup_fixture "$tmpdir/nested-comment-smuggle"
cat >"$tmpdir/nested-comment-smuggle/crates/recordprims/src/lib.rs" <<'EOF'
/* outer /* inner */ still comment "# */
pub fn smuggled() {
    unsafe {}
}
/* "# */
EOF
if RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/nested-comment-smuggle" "$check_script" >/dev/null 2>&1; then
  echo "FAIL: expected unsafe after nested block comment to fail" >&2
  exit 1
fi

setup_fixture "$tmpdir/nested-comment-harmless"
cat >"$tmpdir/nested-comment-harmless/crates/recordprims/src/lib.rs" <<'EOF'
/* outer /* inner */ still inside comment: unsafe { allow(unsafe_code) } */
pub fn safe_fn() -> &'static str {
    r##"contains " and "# and unsafe inside raw string"##
}
EOF
RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/nested-comment-harmless" "$check_script" >/dev/null

# 9. Fake // SAFETY: inside a string literal in windows.rs must not satisfy the SAFETY comment check.
setup_fixture "$tmpdir/fake-safety-in-string"
cat >"$tmpdir/fake-safety-in-string/crates/recordprims/src/sys/windows.rs" <<'EOF'
#![allow(unsafe_code)]
pub fn win_fn() -> u32 {
    let _ = "// SAFETY: not a real comment";
    unsafe { 1 }
}
EOF
if RECORDPRIMS_UNSAFE_CHECK_ROOT="$tmpdir/fake-safety-in-string" "$check_script" >/dev/null 2>&1; then
  echo "FAIL: expected fake // SAFETY: inside string literal in windows.rs to fail" >&2
  exit 1
fi

echo "[ok] unsafe-check.test"
