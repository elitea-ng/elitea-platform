#!/usr/bin/env bash
# render-indexing-runtime.sh — worker.indexingRuntime reaches elitea-main as
# ELITEA_INDEXING_RUNTIME, defaults to python, the Rust worker is given its
# vector store, and the combinations that cannot work are refused at render
# time (ADR-0030 decision 6 and I1, ADR-0031 V1).
#
# elitea-main refuses an unrecognised value at startup, and selects the registry
# only when index ingest dispatch is composed (a dark plane leaves the setting
# dead). The chart owns the facts around that: the value is always written
# explicitly (as ELITEA_WORKER_IMPLEMENTATION is), "rust" needs the Rust worker,
# because only that worker searches the vector store the Rust runtime writes,
# "rust" needs main.runtime.enabled and main.runtime.indexIngestDispatch.enabled,
# the values that set ELITEA_RUNTIME_ENABLED and
# ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED, and "rust" needs a vector store
# the worker can be pointed at: runtime.json gains `vector_target` (this chart's
# elitea-vector by default, or worker.runtime.vectorTarget) and
# `index_egress_allowlist`, and ONLY then — the python-indexing runtime.json
# stays the document render-worker.sh asserts.
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
# A Rust-indexing deployment: the Rust worker, the registry, and this chart's
# elitea-vector (whose introspection pool costs the fourth main replica, as
# render-vector.sh explains).
RUST=(
  --set worker.implementation=rust --set worker.indexingRuntime=rust
  --set vector.enabled=true --set main.autoscaling.maxReplicas=3
  --set vector.qdrant.apiKeySecret.name=elitea-qdrant-api-key
)

# Unrolled, like render-worker.sh: the floor counts accepting sites.
"$HELM" template t "$CHART" "${RENDER[@]}" >"$TMP/default.yaml" 2>"$TMP/default.err" \
  && ok "the default values render" \
  || bad "default does not render: $(tail -1 "$TMP/default.err")"
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" \
  >"$TMP/rust.yaml" 2>"$TMP/rust.err" \
  && ok "implementation=rust with indexingRuntime=rust and vector.enabled renders" \
  || bad "rust does not render: $(tail -1 "$TMP/rust.err")"
# An elitea-vector deployed elsewhere: the worker is pointed at it, and so is
# elitea-main's own client (ADR-0031 decision 4; render-vector.sh covers the
# shape), because rust indexing without elitea-main's client is refused.
"$HELM" template t "$CHART" "${RENDER[@]}" \
  --set worker.implementation=rust --set worker.indexingRuntime=rust \
  --set-string worker.runtime.vectorTarget=vector.platform.example:9470 \
  --set-string worker.runtime.indexEgressAllowlist= \
  --set-string main.env.ELITEA_VECTOR_GRPC_TARGET=vector.platform.example:9470 \
  --set-string main.env.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE=/run/elitea-vector-client/tls.crt \
  --set-string main.env.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE=/run/elitea-vector-client/tls.key \
  --set-string main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/elitea-vector-client/ca.crt \
  --set main.fileConfig.vectorClientMaterial.enabled=true \
  --set main.fileConfig.vectorClientMaterial.secretName=elitea-main-vector-client-tls \
  >"$TMP/external.yaml" 2>"$TMP/external.err" \
  && ok "indexingRuntime=rust with an external worker.runtime.vectorTarget renders" \
  || bad "external vector target does not render: $(tail -1 "$TMP/external.err")"

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

# runtime.json's vector keys, as `vector_target|index_egress_allowlist`, with
# `-` for an absent key.
vectorkeys() {
  python3 - "$1" <<'PY'
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
runtime = json.loads(cm["data"]["runtime.json"])
print(f"{runtime.get('vector_target', '-')}|{runtime.get('index_egress_allowlist', '-')}")
PY
}

[ "$(mainenv "$TMP/default.yaml")" = "python" ] \
  && ok "the default is written explicitly as python" \
  || bad "the default render does not carry ELITEA_INDEXING_RUNTIME=python"
[ "$(mainenv "$TMP/rust.yaml")" = "rust" ] \
  && ok "indexingRuntime=rust is written as rust" \
  || bad "the rust render does not carry ELITEA_INDEXING_RUNTIME=rust"

echo "== the Rust worker is given its vector store only under rust indexing =="
# Python indexing writes neither key: runtime.json must stay the document the
# Rust struct (deny_unknown_fields) and render-worker.sh both expect.
[ "$(vectorkeys "$TMP/default.yaml")" = "-|-" ] \
  && ok "python indexing writes no vector_target and no index_egress_allowlist" \
  || bad "python indexing leaked vector keys into runtime.json: $(vectorkeys "$TMP/default.yaml")"
# The chart's own elitea-vector: <vector.service.name>:<vector.service.port>,
# the SAN its server certificate carries and the port its Service exposes, and
# the default allowlist naming the I1 families' public hosts.
[ "$(vectorkeys "$TMP/rust.yaml")" = "elitea-vector:9470|github.com api.github.com gitlab.com api.bitbucket.org dev.azure.com *.visualstudio.com api.figma.com" ] \
  && ok "rust indexing points the worker at this chart's elitea-vector with the default egress allowlist" \
  || bad "rust indexing vector keys: $(vectorkeys "$TMP/rust.yaml")"
# An elitea-vector deployed elsewhere, named explicitly; an emptied allowlist
# is NOT written, which the worker reads as fail-closed (every host refused).
[ "$(vectorkeys "$TMP/external.yaml")" = "vector.platform.example:9470|-" ] \
  && ok "worker.runtime.vectorTarget names an external elitea-vector and an empty allowlist is omitted" \
  || bad "external vector keys: $(vectorkeys "$TMP/external.yaml")"
# The worker reaches elitea-vector with the runtime identity it already has:
# no vector client certificate is mounted into the worker pod.
python3 - "$TMP/rust.yaml" <<'PY' && ok "the worker mounts no separate vector client certificate (its runtime mTLS identity is the client)" || bad "the worker pod gained a vector TLS volume"
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
dep = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
volumes = [v.get("secret", {}).get("secretName", "") for v in dep["spec"]["template"]["spec"]["volumes"]]
assert not any("vector" in name for name in volumes), volumes
PY

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

# No vector store anywhere: neither this chart's elitea-vector nor an external
# target. The worker would start without a vector_target and refuse every
# index run while elitea-main keeps admitting them.
refuses 'worker.indexingRuntime=rust needs a vector store' \
  --set worker.implementation=rust --set worker.indexingRuntime=rust \
  && ok "indexingRuntime=rust without a vector store is refused" \
  || bad "indexingRuntime=rust without a vector store is not refused"

# The runtime plane itself (main.runtime.enabled) is already required by the
# worker.enabled guard that indexingRuntime=rust implies, so only the dispatch
# plane is a new refusal. The stream is cleared so that the settings guard of the
# configmap does not refuse first.
refuses 'worker.indexingRuntime=rust needs main.runtime.enabled' \
  "${RUST[@]}" \
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
