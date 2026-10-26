#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# The doctor fails closed for every declared tool: each tool's real version
# format is read (two-part ones such as jq-1.6 and minisign 0.12 included), a
# version below the declared minimum, an unreadable version, a failing detect
# command and a missing tool each fail the run, and nothing prints [ok] for the
# run after a failure. Hermetic: PATH holds only stub tools and python3, so a
# tool installed on the host can never stand in for a missing one.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
python="$(command -v python3)"
mkdir -p "$scratch/py" "$scratch/stubs"
ln -s "$python" "$scratch/py/python3"
config="$root/.goneat/tools.yaml"
patterns="$root/config/doctor-versions.txt"

# Version output in each tool's own format, at or above its declared minimum.
# (Plain functions, not associative arrays: macOS ships bash 3.2.)
tools_stubbed='git gh jq gpg minisign go'
good() {
	case "$1" in
	git) echo 'git version 2.54.0 (Apple Git-157)' ;;
	gh) echo 'gh version 2.94.0 (2026-06-10)' ;;
	jq) echo 'jq-1.7.1' ;;
	gpg) echo 'gpg (GnuPG) 2.2.0' ;;
	minisign) echo 'minisign 0.11' ;;
	go) echo 'go version go1.26.0 linux/arm64' ;;
	esac
}
# Below the minimum, in the same formats; jq-1.6 and minisign 0.10 have two parts.
old() {
	case "$1" in
	git) echo 'git version 2.37.9' ;;
	gh) echo 'gh version 2.39.0 (2023-11-01)' ;;
	jq) echo 'jq-1.6' ;;
	gpg) echo 'gpg (GnuPG) 2.1.23' ;;
	minisign) echo 'minisign 0.10' ;;
	go) echo 'go version go1.25.9 linux/arm64' ;;
	esac
}
stub() {
	# stub NAME OUTPUT [STATUS]
	printf '#!/bin/sh\necho "%s"\nexit %s\n' "$2" "${3:-0}" >"$scratch/stubs/$1"
	chmod +x "$scratch/stubs/$1"
}
reset_stubs() {
	rm -f "$scratch/stubs/"*
	local tool
	# python3 is the real interpreter (the doctor runs on it); its own version
	# check is exercised below through a config pointing at a stub.
	for tool in $tools_stubbed; do stub "$tool" "$(good "$tool")"; done
}
doctor() {
	local scope="$1"
	env -i PATH="$scratch/stubs:$scratch/py" HOME="$scratch" \
		python3 -I -B "$root/scripts/doctor.py" "$config" "$patterns" "$scope" >"$scratch/out" 2>&1
}
direct() { env -i PATH="$scratch/stubs:$scratch/py" python3 -I -B "$root/scripts/doctor.py" "$1" "${3:-$patterns}" "$2" >"$scratch/out" 2>&1; }
passes() {
	doctor "$1" || { echo "error: refused scope $1" >&2; cat "$scratch/out" >&2; exit 1; }
	grep -q "^\[ok\] doctor ($1)" "$scratch/out"
}
refused() {
	local scope="$1" reason="$2"
	if doctor "$scope"; then
		echo "error: accepted, expected: $reason" >&2
		cat "$scratch/out" >&2
		exit 1
	fi
	grep -q -- "$reason" "$scratch/out" || { echo "error: refused for another reason than '$reason'" >&2; cat "$scratch/out" >&2; exit 1; }
	if grep -q '^\[ok\] doctor' "$scratch/out"; then
		echo 'error: printed an [ok] summary after a failure' >&2
		exit 1
	fi
}

reset_stubs
passes release
passes dev
# Each tool's version format is parsed to the right number.
doctor release
for line in 'git 2.54.0' 'gh 2.94.0' 'jq 1.7.1' 'gpg 2.2.0' 'minisign 0.11'; do
	grep -q "^\[ok\] $line (minimum" "$scratch/out" || { echo "error: did not read '$line'" >&2; cat "$scratch/out" >&2; exit 1; }
done
doctor dev
grep -q '^\[ok\] go 1.26.0 (minimum' "$scratch/out"

# Below the minimum, for every stubbed tool, in its own format.
for tool in $tools_stubbed; do
	reset_stubs
	stub "$tool" "$(old "$tool")"
	scope=release
	[[ "$tool" == go ]] && scope=dev
	refused "$scope" "$tool .* is below the recommended minimum"
done

# python3 below its minimum: the doctor itself runs on the real interpreter, so
# the detect command is pointed at a stub.
reset_stubs
stub old-python 'Python 3.10.14'
sed 's/detect_command: python3 --version/detect_command: old-python --version/' "$config" >"$scratch/oldpy.yaml"
grep -q 'old-python --version' "$scratch/oldpy.yaml"
if direct "$scratch/oldpy.yaml" dev; then echo 'error: accepted Python 3.10' >&2; exit 1; fi
grep -q 'python3 3.10.14 is below the recommended minimum 3.11.0' "$scratch/out"

# Unreadable version, failing detect command, missing tool.
reset_stubs
stub jq 'jq (development build)'
refused dev "jq: cannot read a version"
reset_stubs
stub gpg 'gpg (GnuPG) 2.5.22' 3
refused release "gpg: cannot read a version"
reset_stubs
rm -f "$scratch/stubs/minisign"
refused release "minisign is missing"
reset_stubs
rm -f "$scratch/stubs/go"
refused dev "go is missing"

# A malformed version is unreadable, never a numeric prefix (0.12garbage is
# not 0.12), and only the first output line counts.
for bad in 'minisign 0.12garbage' 'minisign 0.11..' 'minisign 0.11.' 'minisign 0.12 extra' 'minisign v0.12' 'minisign'; do
	reset_stubs
	stub minisign "$bad"
	refused release 'minisign: cannot read a version'
done
for bad in 'jq-1.6x' 'jq-1.7.1 trailing' 'jq-' 'jq-1'; do
	reset_stubs
	stub jq "$bad"
	refused dev 'jq: cannot read a version'
done
reset_stubs
printf '#!/bin/sh\necho "built from source"\necho "minisign 0.12"\n' >"$scratch/stubs/minisign"
chmod +x "$scratch/stubs/minisign"
refused release 'minisign: cannot read a version'
reset_stubs
stub go 'go version devel go1.28-abc123 linux/amd64'
refused dev 'go: cannot read a version'
# A tool without a version pattern fails rather than being skipped.
reset_stubs
grep -v '^gpg ' "$patterns" >"$scratch/nopat.txt"
if direct "$config" release "$scratch/nopat.txt"; then echo 'error: accepted a tool with no pattern' >&2; exit 1; fi
grep -q 'gpg: no version pattern' "$scratch/out"
printf 'jq   jq-[0-9.]+\n' >"$scratch/nogroup.txt"
if direct "$config" dev "$scratch/nogroup.txt"; then echo 'error: accepted a pattern without a group' >&2; exit 1; fi
grep -q 'needs a version group' "$scratch/out"

# A scope naming an undeclared tool, a tool without a minimum, and a
# malformed config are errors, not skips.
reset_stubs
sed 's/tools: \[git, jq, python3, go\]/tools: [git, jq, python3, go, rustc]/' "$config" >"$scratch/extra.yaml"
grep -q 'rustc\]' "$scratch/extra.yaml"
if direct "$scratch/extra.yaml" dev; then echo 'error: accepted an undeclared tool' >&2; exit 1; fi
grep -q 'rustc: listed in scope dev but not declared' "$scratch/out"
awk '!(/minimum_version: 1.7.0/)' "$config" >"$scratch/nomin.yaml"
if direct "$scratch/nomin.yaml" dev; then
	echo 'error: accepted a tool with no minimum' >&2; exit 1
fi
grep -q 'jq: no valid minimum_version declared' "$scratch/out"
printf 'bogus:\n  x: 1\n' >"$scratch/bad.yaml"
if direct "$scratch/bad.yaml" dev; then
	echo 'error: accepted a malformed config' >&2; exit 1
fi
grep -q "unexpected top-level key" "$scratch/out"
if "$root/scripts/doctor.sh" bogus >"$scratch/out" 2>&1; then
	echo 'error: accepted an unknown scope' >&2; exit 1
fi
grep -q 'unknown scope bogus' "$scratch/out"
echo '[ok] doctor: every declared tool compared, two-part versions, older, unreadable, malformed, failing, missing, unpatterned, undeclared and bad config all fail closed'
