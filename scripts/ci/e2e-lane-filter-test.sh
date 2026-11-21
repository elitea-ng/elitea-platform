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
  out="$(printf '%s\n' "$@" | bash "$filter" 2>/dev/null | grep -E '^reaches-(stack|model)=')"
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
expect false false "docs site only" "apps/elitea-docs/src/content/docs/a.mdx" "apps/elitea-docs/astro.config.ts"
expect true false "docs site + a web source file" "apps/elitea-docs/src/content/docs/a.mdx" "apps/elitea-web/src/pages/a.tsx"
expect false false "docs + helm + desktop mixed" "README.md" "deploy/helm/x.yaml" "apps/elitea-desktop/a.ts"
# Reaches the stack.
expect true true  "empty list reaches everything"
expect true false "a web source file" "apps/elitea-web/src/pages/a.tsx"
expect true false "a raw-imported .md under web src" "apps/elitea-web/src/entries/desktop/agents/local-coding-agent.md"
expect true false "a desktop web entry file (not separable)" "apps/elitea-web/src/entries/desktop/launchApp.tsx"
expect true false "a go:embed'ed .md under services" "services/elitea-main/internal/application/brandpackage/templates/README.md"
expect true false "a service README (services/** is never prose-only)" "services/elitea-main/README.md"
expect true false "a .md under libs" "libs/proto/contracts/node-recovery-v1.md"
expect true false "a .md test fixture" "services/elitea-deepwiki-engine/tests/fixtures/a.md"
expect true false "a compose file next to the chart" "deploy/docker-compose.e2e-standalone.yml" "deploy/helm/x.yaml"
expect true false "a helm file a compose stack bind-mounts" "deploy/helm/elitea/files/nats-bootstrap.sh"
expect true false "docs plus one Go file" "README.md" "services/elitea-main/cmd/x.go"
expect true true  "the gateway" "services/elitea-llm-gateway/main.go"
expect true true  "the e2e workflow" ".github/workflows/ci-web-e2e.yml"
expect true true  "the build-once workflow" ".github/workflows/ci-build-once.yml"
expect true true  "worker docs (under services/, so not prose-only)" "services/elitea-worker-rust/docs/x.md"

# The handoff journey's own signal (third line): decided on every changed
# path, the desktop host's included.
expect_handoff() {
  local want="$1" label="$2"; shift 2
  local out
  out="$(printf '%s\n' "$@" | bash "$filter" 2>/dev/null | grep '^reaches-handoff=')"
  ran=$((ran + 1))
  if [ "$out" != "reaches-handoff=$want" ]; then
    echo "FAIL: $label — got: $out"
    fails=$((fails + 1))
  else
    echo "ok:   $label"
  fi
}
expect_handoff true "the desktop handoff host (skipped by the stack lanes)" "apps/elitea-desktop/src-tauri/src/handoff/mod.rs"
expect_handoff true "the live tests" "apps/elitea-desktop/src-tauri/src/handoff_live_tests.rs"
expect_handoff true "the build-once workflow" ".github/workflows/ci-build-once.yml"
expect_handoff true "the cloud-turn simulator" "services/elitea-main/cmd/handoff-live-cloudturn/main.go"
expect_handoff true "the store service" "services/elitea-main/internal/application/workspacestore/service.go"
expect_handoff true "the shared workspace store crate" "libs/rust/workspace-store/src/sync.rs"
expect_handoff true "the lane's script" "deploy/scripts/handoff-live.sh"
expect_handoff true "empty list" 
expect_handoff false "a web page" "apps/elitea-web/src/pages/a.tsx"
expect_handoff true "any desktop host source (the lane builds the whole crate)" "apps/elitea-desktop/src-tauri/src/tray.rs"
expect_handoff true "a shared crate the host links" "libs/rust/agent-runtime/src/lib.rs"
expect_handoff true "the pinned toolchain" "rust-toolchain.toml"
expect_handoff true "the toolchain assertion" "scripts/ci/assert-rust-toolchain.sh"
expect_handoff true "the stack wrapper" "deploy/scripts/standalone-stack.sh"
expect_handoff true "the rust-agent overlay" "deploy/docker-compose.standalone-rust-agent.yml"
expect_handoff true "the lane's overlay" "deploy/docker-compose.handoff-live.yml"
expect_handoff true "the image action" ".github/actions/stack-images/action.yml"
expect_handoff true "the executor adapter" "services/elitea-main/internal/application/workspaceexecutor/adapter.go"
expect_handoff true "the compose detection helper" "apps/elitea-web/scripts/lib/compose-detect.sh"
expect_handoff false "an unrelated desktop-adjacent file" "apps/elitea-desktop/README.md"
expect_handoff false "docs only" "README.md"

# The code-sandbox lane's own signal (fourth line, #2206/#2208).
expect_sandbox() {
  local want="$1" label="$2"; shift 2
  local out
  out="$(printf '%s\n' "$@" | bash "$filter" 2>/dev/null | grep '^reaches-sandbox=')"
  ran=$((ran + 1))
  if [ "$out" != "reaches-sandbox=$want" ]; then
    echo "FAIL: $label — got: $out"
    fails=$((fails + 1))
  else
    echo "ok:   $label"
  fi
}
expect_sandbox true "empty list"
expect_sandbox true "the run_code tool" "libs/rust/agent-runtime/src/toolkits/families/data_analysis/sandbox_tool.rs"
expect_sandbox true "the worker's code tool" "services/elitea-worker-rust/src/agents/graph/code_tool.rs"
expect_sandbox true "the runner image" "services/elitea-code-runner/Containerfile"
expect_sandbox true "the sandbox overlay" "deploy/docker-compose.sandbox.yml"
expect_sandbox true "the Postgres TLS overlay" "deploy/docker-compose.sandbox-postgres-tls.yml"
expect_sandbox true "the material script" "deploy/scripts/sandbox-compose-material.sh"
expect_sandbox true "the lane's journey" "apps/elitea-web/e2e/streaming/code-sandbox.run-code.spec.ts"
expect_sandbox true "the lane's wrapper" "apps/elitea-web/scripts/code-sandbox-e2e.sh"
expect_sandbox true "the workflow" ".github/workflows/ci-web-e2e.yml"
expect_sandbox true "the build-once workflow" ".github/workflows/ci-build-once.yml"
expect_sandbox false "a web page" "apps/elitea-web/src/pages/a.tsx"
expect_sandbox false "docs only" "README.md"
expect_sandbox false "the DeepWiki overlay" "deploy/docker-compose.deepwiki-native-real-engine.yml"

# The webkit legs' own signal (fifth line): the web app, the harness, the
# package set and the shared client, never the backend or the chart.
expect_webkit() {
  local want="$1" label="$2"; shift 2
  local out
  out="$(printf '%s\n' "$@" | bash "$filter" 2>/dev/null | grep '^reaches-webkit=')"
  ran=$((ran + 1))
  if [ "$out" != "reaches-webkit=$want" ]; then
    echo "FAIL: $label — got: $out"
    fails=$((fails + 1))
  else
    echo "ok:   $label"
  fi
}
expect_webkit true "empty list"
expect_webkit true "a web source file" "apps/elitea-web/src/pages/a.tsx"
expect_webkit true "an e2e journey" "apps/elitea-web/e2e/chat/a.spec.ts"
expect_webkit true "the Playwright config" "apps/elitea-web/playwright.config.ts"
expect_webkit true "the package lock (Playwright's version)" "apps/elitea-web/package-lock.json"
expect_webkit true "the shared client package" "libs/ts/client-core/src/index.ts"
expect_webkit true "the workflow" ".github/workflows/ci-web-e2e.yml"
expect_webkit true "the build-once workflow" ".github/workflows/ci-build-once.yml"
expect_webkit true "the nginx CSP and headers" "apps/elitea-web/nginx/spa.conf"
expect_webkit true "the web image" "apps/elitea-web/Containerfile"
expect_webkit false "a Go service" "services/elitea-main/cmd/x.go"
expect_webkit false "a Rust crate" "libs/rust/agent-runtime/src/lib.rs"
expect_webkit false "the chart" "deploy/helm/elitea/values.yaml"
expect_webkit false "docs only" "README.md" "docs/ARCHITECTURE.md"
expect_webkit false "the desktop host" "apps/elitea-desktop/src-tauri/src/main.rs"
expect_webkit true "docs plus one web source file" "README.md" "apps/elitea-web/src/a.ts"

# The compose smoke leg's own signal (sixth line): what only the container
# stack proves. Decided on every changed path.
expect_images() {
  local want="$1" label="$2"; shift 2
  local out
  out="$(printf '%s\n' "$@" | bash "$filter" 2>/dev/null | grep '^reaches-images=')"
  ran=$((ran + 1))
  if [ "$out" != "reaches-images=$want" ]; then
    echo "FAIL: $label — got: $out"
    fails=$((fails + 1))
  else
    echo "ok:   $label"
  fi
}
expect_images true "empty list"
expect_images true "elitea-main's Containerfile" "services/elitea-main/Containerfile"
expect_images true "the web image" "apps/elitea-web/Containerfile"
expect_images true "a suffixed Containerfile" "deploy/docker/Containerfile.elitea-ui"
expect_images true "a .dockerignore" "services/elitea-main/.dockerignore"
expect_images true "the e2e compose file" "deploy/docker-compose.e2e-standalone.yml"
expect_images true "an overlay compose file" "deploy/docker-compose.standalone-rust-agent.yml"
expect_images true "the nginx config" "apps/elitea-web/nginx/spa.conf"
expect_images true "the web entrypoint" "apps/elitea-web/docker-entrypoint.sh"
expect_images true "the stack script (its compose half)" "apps/elitea-web/scripts/e2e-stack.sh"
expect_images true "the stack backend (its compose half)" "apps/elitea-web/scripts/lib/stack-backend.sh"
expect_images true "the bake file" "docker-bake.hcl"
expect_images true "the image action" ".github/actions/pull-stack-images/action.yml"
expect_images true "the compose workflow" ".github/workflows/ci-web-e2e.yml"
expect_images true "the build-once workflow" ".github/workflows/ci-build-once.yml"
expect_images true "the lane filter" "scripts/ci/e2e-lane-filter.sh"
expect_images true "docs plus one Containerfile" "README.md" "services/elitea-subapp-host/Containerfile"
expect_images false "a web source file" "apps/elitea-web/src/pages/a.tsx"
expect_images false "a Go service source" "services/elitea-main/cmd/elitea-main/main.go"
expect_images false "a journey" "apps/elitea-web/e2e/journeys/a.spec.ts"
expect_images false "the native table" "deploy/native/e2e-standalone.mjs"
expect_images false "the supervisor" "scripts/e2e/native-stack.mjs"
expect_images false "the native workflow" ".github/workflows/ci-web-e2e-native.yml"
expect_images false "a look-alike of the nginx dir" "apps/elitea-web/nginxish/a.conf"
expect_images false "the chart" "deploy/helm/elitea/values.yaml"
expect_images false "docs only" "README.md" "docs/ARCHITECTURE.md"

# The compose-referenced helm list is READ, not written: against a fixture
# compose dir that references a different helm file, that file reaches the
# stack and the real one no longer does.
fixture="$(mktemp -d "${TMPDIR:-/tmp}/lane-filter.XXXXXX")"
trap 'rm -rf "$fixture"' EXIT
printf 'services:\n  x:\n    volumes:\n      - ./helm/other/files/run.sh:/run.sh:ro\n      - "./helm/somedir:/d"\n' > "$fixture/docker-compose.yml"
COMPOSE_DIR="$fixture" expect true false "fixture: a referenced helm file" "deploy/helm/other/files/run.sh"
COMPOSE_DIR="$fixture" expect true false "fixture: a file under a referenced helm dir" "deploy/helm/somedir/a/b.conf"
COMPOSE_DIR="$fixture" expect false false "fixture: the file no compose references now" "deploy/helm/elitea/files/nats-bootstrap.sh"
# And the real compose files do reference something, or the arm above is vacuous.
grep -qE '\./helm/' deploy/*.yml || { echo "FAIL: no deploy/*.yml references ./helm/ — the compose-reference arm tests nothing"; fails=$((fails + 1)); }

if [ "$ran" -lt 24 ]; then echo "ran only $ran cases"; exit 1; fi
if [ "$fails" -ne 0 ]; then echo "$fails of $ran case(s) failed"; exit 1; fi
echo "e2e-lane-filter: all $ran cases pass"
