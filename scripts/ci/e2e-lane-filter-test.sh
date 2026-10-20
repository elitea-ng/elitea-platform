#!/usr/bin/env bash
# Drives every arm of scripts/ci/e2e-lane-filter.sh. A skip rule that skips
# too much is a lane that silently stops gating, so each "reaches" case is as
# load-bearing as each "skipped" case.
set -euo pipefail
cd "$(dirname "$0")/../.."
filter=scripts/ci/e2e-lane-filter.sh
fails=0
ran=0
expect() {
  local want_stack="$1" want_model="$2" label="$3"; shift 3
  local out
  out="$(printf '%s\n' "$@" | bash "$filter" 2>/dev/null)"
  ran=$((ran + 1))
  if [ "$out" != "$(printf 'reaches-stack=%s\nreaches-model=%s' "$want_stack" "$want_model")" ]; then
    echo "FAIL: $label — got: $(echo "$out" | tr '\n' ' ')"
    fails=$((fails + 1))
  else
    echo "ok:   $label"
  fi
}
# Skipped.
expect false false "a root README only" "README.md"
expect false false "docs/ prose only" "docs/ARCHITECTURE.md" "docs/plans/x.md"
expect false false "a .github markdown only" ".github/PULL_REQUEST_TEMPLATE.md"
expect false false "helm only" "deploy/helm/elitea/values.yaml" "deploy/helm/elitea/templates/a.yaml"
expect false false "desktop host only" "apps/elitea-desktop/src-tauri/src/main.rs"
expect false false "docs + helm + desktop mixed" "README.md" "deploy/helm/x.yaml" "apps/elitea-desktop/a.ts"
# Reaches the stack.
expect true true  "empty list reaches everything"
expect true false "a web source file" "apps/elitea-web/src/pages/a.tsx"
expect true false "the embedded docs SPA (vite.config imports it, the image ships it)" "apps/elitea-web/src/entries/docs/content/a.mdx"
expect true false "a raw-imported .md under web src" "apps/elitea-web/src/entries/desktop/agents/local-coding-agent.md"
expect true false "a desktop web entry file (not separable)" "apps/elitea-web/src/entries/desktop/launchApp.tsx"
expect true false "a go:embed'ed .md under services" "services/elitea-main/internal/application/brandpackage/templates/README.md"
expect true false "a service README (services/** is never prose-only)" "services/elitea-main/README.md"
expect true false "a .md under libs" "libs/proto/contracts/node-recovery-v1.md"
expect true false "a .md test fixture" "services/elitea-deepwiki-engine/tests/fixtures/a.md"
expect true false "a compose file next to the chart" "deploy/docker-compose.e2e-standalone.yml" "deploy/helm/x.yaml"
expect true false "a helm file a compose stack bind-mounts" "deploy/helm/nats-bootstrap/files/bootstrap.sh"
expect true false "docs plus one Go file" "README.md" "services/elitea-main/cmd/x.go"
expect true true  "the gateway" "services/elitea-llm-gateway/main.go"
expect true true  "the e2e workflow" ".github/workflows/ci-web-e2e.yml"
expect true true  "worker docs (under services/, so not prose-only)" "services/elitea-worker-rust/docs/x.md"

# The compose-referenced helm list is READ, not written: against a fixture
# compose dir that references a different helm file, that file reaches the
# stack and the real one no longer does.
fixture="$(mktemp -d "${TMPDIR:-/tmp}/lane-filter.XXXXXX")"
trap 'rm -rf "$fixture"' EXIT
printf 'services:\n  x:\n    volumes:\n      - ./helm/other/files/run.sh:/run.sh:ro\n      - "./helm/somedir:/d"\n' > "$fixture/docker-compose.yml"
COMPOSE_DIR="$fixture" expect true false "fixture: a referenced helm file" "deploy/helm/other/files/run.sh"
COMPOSE_DIR="$fixture" expect true false "fixture: a file under a referenced helm dir" "deploy/helm/somedir/a/b.conf"
COMPOSE_DIR="$fixture" expect false false "fixture: the file no compose references now" "deploy/helm/nats-bootstrap/files/bootstrap.sh"
# And the real compose files do reference something, or the arm above is vacuous.
grep -qE '\./helm/' deploy/*.yml || { echo "FAIL: no deploy/*.yml references ./helm/ — the compose-reference arm tests nothing"; fails=$((fails + 1)); }

if [ "$ran" -lt 24 ]; then echo "ran only $ran cases"; exit 1; fi
if [ "$fails" -ne 0 ]; then echo "$fails of $ran case(s) failed"; exit 1; fi
echo "e2e-lane-filter: all $ran cases pass"
