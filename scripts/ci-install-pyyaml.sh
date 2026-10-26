#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Install PyYAML's pure-Python package into the given interpreter for the CI
# gates, without pip: download the source archive named in
# config/ci-python-requirements.txt, check its SHA-256, and copy lib/yaml/ into
# the interpreter's site-packages. Nothing is built and nothing else is fetched.
# Usage: ci-install-pyyaml.sh <python> [--user]
#   --user  install into the interpreter's user site-packages (for a system
#           interpreter whose own site-packages is not writable)
set -euo pipefail
python="${1:?python interpreter required}"
scope=purelib
[[ "${2:-}" != --user ]] || scope=user
root="$(cd "$(dirname "$0")/.." && pwd)"
line="$(grep -E '^pyyaml==' "$root/config/ci-python-requirements.txt")"
version="$(sed -n 's/^pyyaml==\([0-9.]*\) .*/\1/p' <<<"$line")"
want="$(sed -n 's/.*--hash=sha256:\([0-9a-f]\{64\}\).*/\1/p' <<<"$line")"
[[ -n "$version" && -n "$want" ]] || { echo 'error: malformed PyYAML pin' >&2; exit 1; }
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
archive="pyyaml-$version.tar.gz"
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --retry 3 \
	--output "$work/$archive" "https://files.pythonhosted.org/packages/source/P/PyYAML/$archive"
if command -v sha256sum >/dev/null; then got="$(sha256sum "$work/$archive" | cut -c1-64)"; else got="$(shasum -a 256 "$work/$archive" | cut -c1-64)"; fi
[[ "$got" == "$want" ]] || { echo "error: $archive checksum $got != $want" >&2; exit 1; }
(cd "$work" && tar -xzf "$archive" "pyyaml-$version/lib/yaml")
# The interpreter copies the package itself, so its own (possibly Windows)
# path form is used for site-packages.
src="$work/pyyaml-$version/lib/yaml"
if command -v cygpath >/dev/null; then src="$(cygpath -w "$src")"; fi
"$python" -I - "$src" "$scope" <<'PY'
import os, shutil, site as sitemod, sys, sysconfig
site = sitemod.getusersitepackages() if sys.argv[2] == "user" else sysconfig.get_paths()["purelib"]
if sys.argv[2] == "user":
    os.makedirs(site, exist_ok=True)
target = os.path.join(site, "yaml")
if not os.path.isdir(site) or os.path.exists(target):
    sys.exit(f"error: {site} is missing or already has yaml")
shutil.copytree(sys.argv[1], target)
PY
# The gates run Python with -B -E (user site-packages stays visible).
"$python" -B -E -c 'import yaml; assert not yaml.__with_libyaml__; print("PyYAML", yaml.__version__, "pure Python")'
