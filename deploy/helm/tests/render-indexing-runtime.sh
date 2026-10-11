#!/usr/bin/env bash
# render-indexing-runtime.sh — worker.indexingRuntime reaches elitea-main as
# ELITEA_INDEXING_RUNTIME, defaults to python, and the combinations that cannot
# work are refused at render time (ADR-0030 decision 6, ADR-0031 V1).
#
# elitea-main refuses an unrecognised value at startup, and selects the registry
# only when index ingest dispatch is composed (a dark plane leaves the setting
# dead). The chart owns the facts around that: the value is always written
# explicitly (as ELITEA_WORKER_IMPLEMENTATION is), "rust" needs the Rust worker,
# because only that worker searches the vector store the Rust runtime writes,
# and "rust" needs main.runtime.enabled and main.runtime.indexIngestDispatch.enabled,
# the values that set ELITEA_RUNTIME_ENABLED and
# ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED.
#
# Run: deploy/helm/tests/render-indexing-runtime.sh   (requires helm + python3 + PyYAML)
set -euo pipefail

DIR="$(cd "$(dirname "$0")/../.." && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"
# shellcheck source=../../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"

HELM="${HELM:-helm}"
CHART="$DIR/helm/elitea"
PASS=0
FAIL=0

ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])ok[[:space:]]+"'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN")"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()  { PASS=$((PASS+1)); echo "  ok: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1" >&2; }

RENDER=(
  -f "$CHART/values-standalone.yaml"
  --set worker.enabled=true
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
)

# Unrolled, like render-worker.sh: the floor counts accepting sites.
"$HELM" template t "$CHART" "${RENDER[@]}" >"$TMP/default.yaml" 2>"$TMP/default.err" \
  && ok "the default values render" \
  || bad "default does not render: $(tail -1 "$TMP/default.err")"
"$HELM" template t "$CHART" "${RENDER[@]}" --set worker.implementation=rust --set worker.indexingRuntime=rust \
  >"$TMP/rust.yaml" 2>"$TMP/rust.err" \
  && ok "implementation=rust with indexingRuntime=rust renders" \
  || bad "rust does not render: $(tail -1 "$TMP/rust.err")"

mainenv() {
  python3 - "$1" <<'PY'
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
values = [d["data"]["ELITEA_INDEXING_RUNTIME"] for d in docs
          if d["kind"] == "ConfigMap" and "ELITEA_INDEXING_RUNTIME" in (d.get("data") or {})]
assert len(values) == 1, f"expected exactly one ELITEA_INDEXING_RUNTIME, found {values}"
print(values[0])
PY
}

[ "$(mainenv "$TMP/default.yaml")" = "python" ] \
  && ok "the default is written explicitly as python" \
  || bad "the default render does not carry ELITEA_INDEXING_RUNTIME=python"
[ "$(mainenv "$TMP/rust.yaml")" = "rust" ] \
  && ok "indexingRuntime=rust is written as rust" \
  || bad "the rust render does not carry ELITEA_INDEXING_RUNTIME=rust"

refuses() {
  local expect="$1"; shift
  local out
  if out="$("$HELM" template t "$CHART" "${RENDER[@]}" "$@" 2>&1)"; then
    echo "    rendered instead of being refused" >&2
    return 1
  fi
  printf '%s' "$out" | grep -q "$expect" && return 0
  echo "    refused, but not for the stated reason: $(printf '%s' "$out" | tail -1)" >&2
  return 1
}

refuses 'worker.indexingRuntime must be' --set worker.indexingRuntime=golang \
  && ok "an unknown worker.indexingRuntime is refused" \
  || bad "an unknown worker.indexingRuntime is not refused"

refuses 'worker.indexingRuntime=rust needs' --set worker.implementation=python --set worker.indexingRuntime=rust \
  && ok "indexingRuntime=rust with the Python worker is refused" \
  || bad "indexingRuntime=rust with the Python worker is not refused"

# The runtime plane itself (main.runtime.enabled) is already required by the
# worker.enabled guard that indexingRuntime=rust implies, so only the dispatch
# plane is a new refusal. The stream is cleared so that the settings guard of the
# configmap does not refuse first.
refuses 'worker.indexingRuntime=rust needs main.runtime.enabled' \
  --set worker.implementation=rust --set worker.indexingRuntime=rust \
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream= \
  && ok "indexingRuntime=rust without index ingest dispatch is refused" \
  || bad "indexingRuntime=rust without index ingest dispatch is not refused"

echo
RAN=$((PASS+FAIL))
echo "indexing runtime render assertions: ${RAN} ran, ${PASS} passed, ${FAIL} failed"
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAIL: ${RAN} assertion(s) ran, and this file holds ${EXPECTED_ASSERTIONS} assertion site(s)." >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
