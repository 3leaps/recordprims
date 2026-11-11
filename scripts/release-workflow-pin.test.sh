#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
# Every workflow and local action matches its reviewed digest and is
# read-only; each listed mutation of a workflow, an action or the pin is
# refused, and the ceremony reads the committed tree, not the working tree.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd -P)"
py="${PYTHON:-python3}"
"$py" -c 'import yaml' 2>/dev/null || {
	echo 'error: the workflow pin test needs PyYAML' >&2
	exit 1
}
(cd "$root" && "$py" -B -E scripts/release-workflow-pin.py check) >/dev/null
# Pin controls, on a scratch repository holding the real workflows and pin.
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
repo="$scratch/repo"
mkdir -p "$repo/.github/workflows" "$repo/.github/actions/make-check" "$repo/config/release"
cp "$root"/.github/workflows/*.yml "$repo/.github/workflows/"
cp "$root/.github/actions/make-check/action.yml" "$repo/.github/actions/make-check/"
cp "$root/config/release/release-workflow.sha256" "$repo/config/release/"
git -C "$repo" init -q -b main
git -C "$repo" config user.name fixture
git -C "$repo" config user.email fixture@example.invalid
git -C "$repo" config commit.gpgsign false
git -C "$repo" config core.autocrlf false
git -C "$repo" add -A
git -C "$repo" commit -qm base
# Edit in place without leaving a backup file among the workflows.
sed_in_place() {
	local script="$1" file="$2"
	sed "$script" "$file" >"$file.tmp" && mv "$file.tmp" "$file"
}
pin() { (cd "$repo" && PYTHONDONTWRITEBYTECODE=1 "$py" -B -E "$root/scripts/release-workflow-pin.py" "$@") >"$scratch/out" 2>&1; }
refused() {
	if pin "${@:2}"; then
		echo "error: workflow pin accepted: $1" >&2
		exit 1
	fi
	grep -q "$1" "$scratch/out" || { cat "$scratch/out" >&2; exit 1; }
}
pin_with() {
	(cd "$repo" && for f in .github/actions/make-check/action.yml .github/workflows/ci.yml; do
		printf '%s  %s\n' "$("$py" -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$f")" "$f"
	done) >"$repo/config/release/release-workflow.sha256"
}
pin check
pin verify-commit HEAD
# A workflow edited without its pin fails, however small the edit.
sed_in_place 's/timeout-minutes: 15/timeout-minutes: 16/' "$repo/.github/workflows/ci.yml"
refused 'differs from its reviewed digest' check
git -C "$repo" checkout -q -- .github
# A local action edited without its pin fails: it changes what a workflow runs.
printf '# changed\n' >>"$repo/.github/actions/make-check/action.yml"
refused 'action.yml differs from its reviewed digest' check
git -C "$repo" checkout -q -- .github
printf 'name: x\n' >"$repo/.github/actions/make-check/extra.yml"
refused 'extra.yml is not pinned' check
rm "$repo/.github/actions/make-check/extra.yml"
# A pinned workflow that is deleted fails.
rm "$repo/.github/workflows/ci.yml"
refused 'pinned workflow .github/workflows/ci.yml is missing' check
git -C "$repo" checkout -q -- .github
# The pin edited without the workflow fails.
git -C "$repo" checkout -q -- .github
# Flip the first hex digit (to one it is not), so the edit always changes it.
sed_in_place '1{s/^0/1/;t
s/^./0/;}' "$repo/config/release/release-workflow.sha256"
refused 'differs from its reviewed digest' check
git -C "$repo" checkout -q -- config
# Both edited together, as one reviewed change, passes.
sed_in_place 's/timeout-minutes: 15/timeout-minutes: 16/' "$repo/.github/workflows/ci.yml"
pin_with
pin check
git -C "$repo" checkout -q -- .
# Every workflow file is pinned, whatever its triggers (a plain push, push
# with no filter, create, or anything else can run on a tag).
for body in 'on: push' 'on: [push]' 'on:\n  push:\n' 'on:\n  push: {}\n' 'on:\n  push:\n    paths: [x]\n' 'on: create' 'on:\n  workflow_dispatch:\n'; do
	printf "$body\npermissions:\n  contents: read\njobs: {}\n" >"$repo/.github/workflows/extra.yml"
	refused 'extra.yml is not pinned' check
done
rm "$repo/.github/workflows/extra.yml"
# A pinned workflow must be read-only, at the top and in every job.
sed_in_place 's/^  contents: read$/  contents: write/' "$repo/.github/workflows/ci.yml"
pin_with
refused 'ci.yml must declare top-level permissions: contents: read' check
git -C "$repo" checkout -q -- .
"$py" -c 'import sys; p = sys.argv[1]; s = open(p).read(); open(p, "w").write(s.replace("    timeout-minutes: 15\n", "    timeout-minutes: 15\n    permissions:\n      id-token: write\n", 1))' "$repo/.github/workflows/ci.yml"
pin_with
refused 'grants more than read' check
git -C "$repo" checkout -q -- .
pin check
# A yaml.py beside the checker is never imported (a script's own directory
# is the one path Python adds by itself).
mkdir "$scratch/shadow"
cp "$root/scripts/release-workflow-pin.py" "$scratch/shadow/"
printf 'raise SystemExit("shadow yaml imported")\n' >"$scratch/shadow/yaml.py"
(cd "$repo" && "$py" -B -E "$scratch/shadow/release-workflow-pin.py" check) >"$scratch/out" 2>&1 || true
if grep -q 'shadow' "$scratch/out"; then
	echo 'error: a yaml.py beside the pin checker was imported' >&2
	exit 1
fi
grep -q 'every workflow matches' "$scratch/out" || { cat "$scratch/out" >&2; exit 1; }
# The ceremony reads the tagged commit, not the working tree: an uncommitted
# fix does not rescue a bad commit, and an uncommitted edit does not break a
# good one.
sed_in_place 's/timeout-minutes: 15/timeout-minutes: 16/' "$repo/.github/workflows/ci.yml"
pin verify-commit HEAD
git -C "$repo" add .github/workflows/ci.yml
git -C "$repo" commit -qm unreviewed
git -C "$repo" checkout -q HEAD~1 -- .github/workflows/ci.yml
refused 'differs from its reviewed digest' verify-commit HEAD
echo '[ok] read-only workflow boundary and mutation controls'
