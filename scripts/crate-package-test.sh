#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Packages the Rust crate and runs its tests from the extracted archive,
# outside the checkout, as a downstream user of the published crate would.
# Tests that read the checkout's shared fixtures skip there; everything else
# must pass. The workspace Cargo.lock pins the same dependency versions.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
PYTHON_BIN="${PYTHON:-python3}"
cd "$root"
version="$("$PYTHON_BIN" -I -B -c 'import tomllib,sys; print(tomllib.load(open(sys.argv[1],"rb"))["workspace"]["package"]["version"])' Cargo.toml)"
cargo package --locked --allow-dirty -p recordprims --quiet
archive="target/package/recordprims-$version.crate"
[ -f "$archive" ] || { echo "error: no package at $archive" >&2; exit 1; }
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
tar -xzf "$archive" -C "$scratch"
pkg="$scratch/recordprims-$version"
cp Cargo.lock "$pkg/Cargo.lock"
(cd "$pkg" && env -u RECORDPRIMS_CHECKOUT_TESTS CARGO_TARGET_DIR="$root/target/crate-package-test" cargo test --locked --quiet)
echo "[ok] crate-package: the packaged crate builds and its tests pass outside the checkout"
