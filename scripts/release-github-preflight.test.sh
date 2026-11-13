#!/usr/bin/env bash
# SPDX-License-Identifier: MIT OR Apache-2.0
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd -P)"
script="$root/scripts/release-github-preflight.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

bindir="$scratch/bin"
fixtures="$scratch/fixtures"
mkdir -p "$bindir" "$fixtures"

cat >"$bindir/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "api" ]; then
	printf 'unexpected gh subcommand: %s\n' "${1:-}" >&2
	exit 2
fi
shift
if [ "${1:-}" = "--paginate" ]; then
	shift
fi
endpoint="${1:-}"
shift || true
if [ "${1:-}" != "--hostname" ] || [ "${2:-}" != "github.com" ] || [ "${GH_HOST:-}" != "github.com" ]; then
	printf 'expected release_gh --hostname github.com pinning, got GH_HOST=%s args=%s\n' "${GH_HOST:-}" "$*" >&2
	exit 2
fi
case "$endpoint" in
repos/3leaps/recordprims/actions/permissions/workflow)
	cat "$GH_FIXTURE_DIR/workflow.json"
	;;
"repos/3leaps/recordprims/rulesets?per_page=100")
	cat "$GH_FIXTURE_DIR/rulesets.json"
	;;
repos/3leaps/recordprims/rulesets/25834904)
	cat "$GH_FIXTURE_DIR/ruleset.json"
	;;
repos/3leaps/recordprims/immutable-releases)
	cat "$GH_FIXTURE_DIR/immutable.json"
	;;
*)
	printf 'unexpected gh api endpoint: %s\n' "$endpoint" >&2
	exit 2
	;;
esac
EOF
chmod +x "$bindir/gh"

reset_fixtures() {
	cat >"$fixtures/workflow.json" <<'EOF'
{"default_workflow_permissions":"read","can_approve_pull_request_reviews":false}
EOF
	cat >"$fixtures/rulesets.json" <<'EOF'
[{"id":25834904,"name":"Tag Publish Protection","target":"tag","enforcement":"active"}]
EOF
	cat >"$fixtures/ruleset.json" <<'EOF'
{
  "id": 25834904,
  "name": "Tag Publish Protection",
  "target": "tag",
  "source_type": "Repository",
  "source": "3leaps/recordprims",
  "enforcement": "active",
  "conditions": {
    "ref_name": {
      "include": ["refs/tags/v*"],
      "exclude": []
    }
  },
  "rules": [
    {"type": "creation"},
    {"type": "update"},
    {"type": "deletion"},
    {"type": "non_fast_forward"}
  ],
  "bypass_actors": [
    {
      "actor_id": null,
      "actor_type": "OrganizationAdmin",
      "bypass_mode": "always"
    }
  ]
}
EOF
	cat >"$fixtures/immutable.json" <<'EOF'
{"enabled":true,"enforced_by_owner":false}
EOF
}

run_preflight() {
	PATH="$bindir:$PATH" GH_FIXTURE_DIR="$fixtures" "$script" >"$scratch/out" 2>&1
}

refused() {
	local expected="$1"
	if run_preflight; then
		printf 'error: preflight accepted %s\n' "$expected" >&2
		exit 1
	fi
	grep -Fq "$expected" "$scratch/out" || {
		cat "$scratch/out" >&2
		exit 1
	}
}

reset_fixtures
run_preflight
grep -Fq '[ok] 3leaps/recordprims GitHub release settings verified' "$scratch/out"

# Paginated gh api output is one JSON array per page; jq -s add merges pages.
reset_fixtures
cat >"$fixtures/rulesets.json" <<'EOF'
[{"id":1,"name":"Other","target":"branch","enforcement":"active"}]
[{"id":25834904,"name":"Tag Publish Protection","target":"tag","enforcement":"active"}]
EOF
run_preflight

reset_fixtures
cat >"$fixtures/workflow.json" <<'EOF'
{"default_workflow_permissions":"write","can_approve_pull_request_reviews":false}
EOF
refused 'workflow permissions must be default_workflow_permissions=read'

reset_fixtures
cat >"$fixtures/workflow.json" <<'EOF'
{"default_workflow_permissions":"read","can_approve_pull_request_reviews":true}
EOF
refused 'can_approve_pull_request_reviews=false'

reset_fixtures
cat >"$fixtures/rulesets.json" <<'EOF'
[{"id":25834904,"name":"Tag Publish Protection","target":"tag","enforcement":"disabled"}]
EOF
refused "single active tag ruleset named 'Tag Publish Protection'"

reset_fixtures
jq '.conditions.ref_name.exclude = ["refs/tags/v0.0.1"]' "$fixtures/ruleset.json" >"$fixtures/ruleset.tmp"
mv "$fixtures/ruleset.tmp" "$fixtures/ruleset.json"
refused 'does not match the v* tag protection contract'

reset_fixtures
jq '.rules = [{"type":"creation"},{"type":"deletion"},{"type":"non_fast_forward"}]' "$fixtures/ruleset.json" >"$fixtures/ruleset.tmp"
mv "$fixtures/ruleset.tmp" "$fixtures/ruleset.json"
refused 'does not match the v* tag protection contract'

reset_fixtures
jq '.bypass_actors += [{"actor_id":1,"actor_type":"RepositoryRole","bypass_mode":"always"}]' "$fixtures/ruleset.json" >"$fixtures/ruleset.tmp"
mv "$fixtures/ruleset.tmp" "$fixtures/ruleset.json"
refused 'does not match the v* tag protection contract'

reset_fixtures
cat >"$fixtures/immutable.json" <<'EOF'
{"enabled":false,"enforced_by_owner":false}
EOF
refused 'immutable releases enabled'

reset_fixtures
if PATH="$bindir:$PATH" GH_FIXTURE_DIR="$fixtures" GH_HOST=other.example.invalid "$script" >"$scratch/out" 2>&1; then
	printf 'error: preflight accepted inherited GH_HOST\n' >&2
	exit 1
fi
grep -Fq 'GH_HOST is set; release steps talk to github.com only' "$scratch/out"

printf '[ok] GitHub release preflight controls\n'
