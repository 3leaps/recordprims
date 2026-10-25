#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Install checksum-pinned CI tools into a directory and print it.
#
# Usage: ci-install-tools.sh <target-triple> <dest-dir> [actionlint]
#
# Installs goneat (the JSON Schema validator the vendored durable-record/v1
# interpreter runs) and cargo-deny. Windows arm64 uses the x86_64 builds under
# emulation; no native arm64 Windows build is published.
set -euo pipefail

target="$1"
dest="$2"
want_actionlint="${3:-}"
root="$(cd "$(dirname "$0")/.." && pwd)"
pins="${root}/config/ci-tools.sha256"
case "${dest}" in /*|[A-Za-z]:*) ;; *) dest="$(pwd)/${dest}" ;; esac

case "${target}" in
  x86_64-unknown-linux-gnu)   goneat=linux_amd64.tar.gz;   deny=x86_64-unknown-linux-musl ;;
  aarch64-unknown-linux-gnu)  goneat=linux_arm64.tar.gz;   deny=aarch64-unknown-linux-musl ;;
  aarch64-apple-darwin)       goneat=darwin_arm64.tar.gz;  deny=aarch64-apple-darwin ;;
  x86_64-pc-windows-msvc)     goneat=windows_amd64.zip;    deny=x86_64-pc-windows-msvc ;;
  aarch64-pc-windows-msvc)    goneat=windows_amd64.zip;    deny=x86_64-pc-windows-msvc ;;
  *) echo "unsupported target ${target}" >&2; exit 2 ;;
esac

sha256() {
  if command -v sha256sum > /dev/null; then sha256sum "$1" | cut -c1-64; else shasum -a 256 "$1" | cut -c1-64; fi
}

fetch() {
  local name="$1" url="$2" want got
  want="$(awk -v n="${name}" '$2 == n { print $1 }' "${pins}")"
  if [ -z "${want}" ]; then echo "no pin for ${name}" >&2; exit 1; fi
  curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
    --retry 3 --output "${name}" "${url}"
  got="$(sha256 "${name}" | tr 'A-F' 'a-f')"
  if [ "${got}" != "${want}" ]; then
    echo "checksum mismatch for ${name}: ${got} != ${want}" >&2
    exit 1
  fi
}

mkdir -p "${dest}"
# Work inside dest with relative names: GNU tar on Windows reads "D:\..."
# as a remote host specification.
cd "${dest}"

name="goneat_v0.6.1_${goneat}"
fetch "${name}" "https://github.com/fulmenhq/goneat/releases/download/v0.6.1/${name}"
case "${name}" in
  *.zip) unzip -q -o "${name}" ;;
  *) tar -xzf "${name}" goneat ;;
esac

name="cargo-deny-0.20.2-${deny}.tar.gz"
fetch "${name}" "https://github.com/EmbarkStudios/cargo-deny/releases/download/0.20.2/${name}"
tar -xzf "${name}" --strip-components=1

if [ "${want_actionlint}" = "actionlint" ]; then
  name="actionlint_1.7.12_linux_amd64.tar.gz"
  fetch "${name}" "https://github.com/rhysd/actionlint/releases/download/v1.7.12/${name}"
  tar -xzf "${name}" actionlint
fi

rm -f ./*.tar.gz ./*.zip
pwd
