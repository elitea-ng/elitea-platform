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
expect false false "a README only" "services/elitea-main/README.md"
expect false false "worker docs only (inside the model path tree)" "services/elitea-worker-rust/docs/x.md"
expect false false "helm only" "deploy/helm/elitea/values.yaml" "deploy/helm/elitea/templates/a.yaml"
expect false false "desktop host only" "apps/elitea-desktop/src-tauri/src/main.rs"
expect false false "embedded docs page only" "apps/elitea-web/src/entries/docs/content/a.mdx"
expect false false "docs + helm + desktop mixed" "README.md" "deploy/helm/x.yaml" "apps/elitea-desktop/a.ts"
# Reaches the stack.
expect true true  "empty list reaches everything"
expect true false "a web source file" "apps/elitea-web/src/pages/a.tsx"
expect true false "a raw-imported .md under web src" "apps/elitea-web/src/entries/desktop/agents/local-coding-agent.md"
expect true false "a desktop web entry file (not separable)" "apps/elitea-web/src/entries/desktop/launchApp.tsx"
expect true false "a .md test fixture" "services/elitea-deepwiki-engine/tests/fixtures/a.md"
expect true false "a .md under conformance" "conformance/provider/fixtures/a.md"
expect true false "a compose file next to the chart" "deploy/docker-compose.e2e-standalone.yml" "deploy/helm/x.yaml"
expect true false "docs plus one Go file" "README.md" "services/elitea-main/cmd/x.go"
expect true true  "the gateway" "services/elitea-llm-gateway/main.go"
expect true true  "the e2e workflow" ".github/workflows/ci-web-e2e.yml"
expect true true  "a worker source next to its docs" "services/elitea-worker-rust/docs/x.md" "services/elitea-worker-rust/src/lib.rs"
if [ "$ran" -lt 17 ]; then echo "ran only $ran cases"; exit 1; fi
if [ "$fails" -ne 0 ]; then echo "$fails of $ran case(s) failed"; exit 1; fi
echo "e2e-lane-filter: all $ran cases pass"
