#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
set -euo pipefail

# Read-only preflight for the GitHub-hosted settings that are not represented in
# git: workflow token permissions, the v* tag ruleset, and immutable releases.
# Run with `make release-github-preflight` before cutting a release tag.

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

root="$(cd "$(dirname "$0")/.." && pwd -P)"
# shellcheck source=scripts/release-gh.sh
source "$root/scripts/release-gh.sh"

for cmd in gh jq; do
	command -v "$cmd" >/dev/null 2>&1 || die "required command not found: $cmd"
done

repository="3leaps/recordprims"
ruleset_name="Tag Publish Protection"

workflow_permissions="$(release_gh api "repos/$repository/actions/permissions/workflow")" ||
	die "could not read workflow permissions for $repository"
jq -e '
	.default_workflow_permissions == "read" and
	.can_approve_pull_request_reviews == false
' >/dev/null <<<"$workflow_permissions" ||
	die "$repository workflow permissions must be default_workflow_permissions=read and can_approve_pull_request_reviews=false"

rulesets="$(release_gh api --paginate "repos/$repository/rulesets?per_page=100" | jq -s 'add // []')" ||
	die "could not list rulesets for $repository"
ruleset_ids="$(jq -r --arg name "$ruleset_name" '
	[.[] | select(.name == $name and .target == "tag" and .enforcement == "active") | .id]
	| if length == 1 then .[0] else empty end
' <<<"$rulesets")"
[ -n "$ruleset_ids" ] ||
	die "$repository must have a single active tag ruleset named '$ruleset_name'"

ruleset="$(release_gh api "repos/$repository/rulesets/$ruleset_ids")" ||
	die "could not read ruleset $ruleset_ids for $repository"
jq -e --arg repo "$repository" --arg name "$ruleset_name" '
	.name == $name and
	.target == "tag" and
	.enforcement == "active" and
	.source_type == "Repository" and
	.source == $repo and
	(.conditions.ref_name.include // []) == ["refs/tags/v*"] and
	(.conditions.ref_name.exclude // []) == [] and
	((.rules // []) | map(.type) | sort) == ["creation", "deletion", "non_fast_forward", "update"] and
	((.bypass_actors // []) | map({actor_id, actor_type, bypass_mode})) == [
		{"actor_id": null, "actor_type": "OrganizationAdmin", "bypass_mode": "always"}
	]
' >/dev/null <<<"$ruleset" ||
	die "$repository ruleset '$ruleset_name' does not match the v* tag protection contract"

immutable="$(release_gh api "repos/$repository/immutable-releases")" ||
	die "could not read immutable-releases setting for $repository"
jq -e '.enabled == true' >/dev/null <<<"$immutable" ||
	die "$repository must have immutable releases enabled"

printf '[ok] %s GitHub release settings verified (read-only GITHUB_TOKEN, active %s on refs/tags/v*, immutable releases)\n' \
	"$repository" "$ruleset_name"
