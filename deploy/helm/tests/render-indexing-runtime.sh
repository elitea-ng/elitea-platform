#!/usr/bin/env bash
# render-indexing-runtime.sh — the indexing runtime is rust, the only one
# (ADR-0030 decision 6 and I1, ADR-0031 V1): it reaches elitea-main as
# ELITEA_INDEXING_RUNTIME=rust, the retired python is refused at render time,
# the worker is given its vector store whenever the release indexes, and the
# combinations that cannot work are refused.
#
# elitea-main refuses any other value at startup and selects the registry
# whenever index ingest dispatch is composed. The chart owns the facts around
# that: the value is always written explicitly, and indexing with the worker
# needs a vector store the worker can be pointed at: runtime.json gains
# `vector_target` (this chart's elitea-vector by default, or
# worker.runtime.vectorTarget) and `index_egress_allowlist`, and ONLY then —
# a release without indexing keeps the document render-worker.sh asserts.
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
# No indexing: index ingest dispatch off (values-standalone.yaml turns it on).
NOINDEX=(--set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=)
# An indexing deployment: the registry, and this chart's elitea-vector.
RUST=(
  --set vector.enabled=true
  --set vector.qdrant.apiKeySecret.name=elitea-qdrant-api-key
)

# Unrolled, like render-worker.sh: the floor counts accepting sites.
"$HELM" template t "$CHART" "${RENDER[@]}" "${NOINDEX[@]}" >"$TMP/default.yaml" 2>"$TMP/default.err" \
  && ok "the worker without indexing renders" \
  || bad "the worker without indexing does not render: $(tail -1 "$TMP/default.err")"
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" \
  >"$TMP/rust.yaml" 2>"$TMP/rust.err" \
  && ok "the worker with indexing and vector.enabled renders" \
  || bad "rust does not render: $(tail -1 "$TMP/rust.err")"
# An elitea-vector deployed elsewhere: the worker is pointed at it, and so is
# elitea-main's own client (ADR-0031 decision 4; render-vector.sh covers the
# shape), because rust indexing without elitea-main's client is refused.
"$HELM" template t "$CHART" "${RENDER[@]}" \
  --set-string worker.runtime.vectorTarget=vector.platform.example:9470 \
  --set-string worker.runtime.indexEgressAllowlist= \
  --set-string main.env.ELITEA_VECTOR_GRPC_TARGET=vector.platform.example:9470 \
  --set-string main.env.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE=/run/elitea-vector-client/tls.crt \
  --set-string main.env.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE=/run/elitea-vector-client/tls.key \
  --set-string main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/elitea-vector-client/ca.crt \
  --set main.fileConfig.vectorClientMaterial.enabled=true \
  --set main.fileConfig.vectorClientMaterial.secretName=elitea-main-vector-client-tls \
  >"$TMP/external.yaml" 2>"$TMP/external.err" \
  && ok "indexing with an external worker.runtime.vectorTarget renders" \
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

[ "$(mainenv "$TMP/default.yaml")" = "rust" ] \
  && ok "without indexing ELITEA_INDEXING_RUNTIME is still written explicitly as rust" \
  || bad "the render without indexing does not carry ELITEA_INDEXING_RUNTIME=rust"
python3 - "$CHART/values.yaml" <<'PY' && ok "the shipped default is worker.indexingRuntime=rust and no worker.implementation key" || bad "the shipped worker defaults are not rust-only"
import sys, yaml
worker = yaml.safe_load(open(sys.argv[1]))["worker"]
assert worker["indexingRuntime"] == "rust" and "implementation" not in worker, worker
PY
"$HELM" template t "$CHART" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true \
  >"$TMP/dark.yaml" 2>"$TMP/dark.err" \
  && [ "$(mainenv "$TMP/dark.yaml")" = "rust" ] \
  && ok "the chart default (no worker, no runtime plane) renders with ELITEA_INDEXING_RUNTIME=rust" \
  || bad "the worker-less default does not render with ELITEA_INDEXING_RUNTIME=rust: $(tail -1 "$TMP/dark.err")"
[ "$(mainenv "$TMP/rust.yaml")" = "rust" ] \
  && ok "the indexing render is written as rust" \
  || bad "the rust render does not carry ELITEA_INDEXING_RUNTIME=rust"

echo "== the worker is given its vector store only while the release indexes =="
# Without indexing neither key is written: runtime.json must stay the document
# the Rust struct (deny_unknown_fields) and render-worker.sh both expect.
[ "$(vectorkeys "$TMP/default.yaml")" = "-|-" ] \
  && ok "without indexing no vector_target and no index_egress_allowlist" \
  || bad "the render without indexing leaked vector keys into runtime.json: $(vectorkeys "$TMP/default.yaml")"
# The chart's own elitea-vector: <vector.service.name>:<vector.service.port>,
# the SAN its server certificate carries and the port its Service exposes, and
# the default allowlist naming the I1 families' public hosts.
[ "$(vectorkeys "$TMP/rust.yaml")" = "elitea-vector:9470|github.com api.github.com gitlab.com api.bitbucket.org dev.azure.com *.visualstudio.com api.zephyrscale.smartbear.com eu.api.zephyrscale.smartbear.com prod-api.zephyr4jiracloud.com xray.cloud.getxray.app us.xray.cloud.getxray.app eu.xray.cloud.getxray.app au.xray.cloud.getxray.app api.figma.com" ] \
  && ok "rust indexing points the worker at this chart's elitea-vector with the default egress allowlist" \
  || bad "rust indexing vector keys: $(vectorkeys "$TMP/rust.yaml")"
# Artifact-toolkit indexing reads the platform's own object API at
# runtime.json's `platform_origin`. The shipped defaults must support it: the
# origin is a bare https origin (the shape the artifact source accepts), and
# the external-source allowlist neither names it nor opens everything, because
# the artifact source is pinned to that exact origin and never consults the
# allowlist (live defect: the allowlist, public SaaS hosts only, refused it).
platformorigin() {
  python3 - "$1" <<'PY'
import json, sys, yaml
from urllib.parse import urlsplit

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
runtime = json.loads(cm["data"]["runtime.json"])
origin = urlsplit(runtime["platform_origin"])
bare = (origin.scheme == "https" and origin.hostname and not origin.username
        and origin.path in ("", "/") and not origin.query and not origin.fragment)
entries = runtime.get("index_egress_allowlist", "").replace(",", " ").split()
pinned_only = origin.hostname not in entries and "*" not in entries
print("pinned" if bare and pinned_only else f"unexpected:{runtime['platform_origin']}|{entries}")
PY
}
[ "$(platformorigin "$TMP/rust.yaml")" = "pinned" ] \
  && ok "the default platform_origin is a bare https origin the artifact source reaches without the egress allowlist" \
  || bad "artifact indexing is not supported by the shipped defaults: $(platformorigin "$TMP/rust.yaml")"
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

# The model-call cap of a run's model steps (ADR-0030 I4): written under rust
# indexing (the chart default, or what the operator sets), never without it.
modelcap() {
  python3 - "$1" <<'PY'
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
print(json.loads(cm["data"]["runtime.json"]).get("index_model_call_cap", "-"))
PY
}
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" \
  --set worker.runtime.indexModelCallCap=500 \
  >"$TMP/cap.yaml" 2>"$TMP/cap.err" \
  || bad "an indexModelCallCap does not render: $(tail -1 "$TMP/cap.err")"
[ "$(modelcap "$TMP/default.yaml")" = "-" ] \
  && ok "without indexing no index_model_call_cap" \
  || bad "the render without indexing leaked index_model_call_cap: $(modelcap "$TMP/default.yaml")"
[ "$(modelcap "$TMP/rust.yaml")" = "2000" ] \
  && ok "rust indexing writes the default model-call cap of 2000" \
  || bad "rust indexing model-call cap: $(modelcap "$TMP/rust.yaml")"
[ "$(modelcap "$TMP/cap.yaml")" = "500" ] \
  && ok "worker.runtime.indexModelCallCap sets the cap" \
  || bad "indexModelCallCap=500 rendered: $(modelcap "$TMP/cap.yaml")"

# The memory toolkit's embedding model (worker unit M9): written under rust
# indexing when set, never otherwise, and refused where nothing could use it.
memorymodel() {
  python3 - "$1" <<'PY'
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
print(json.loads(cm["data"]["runtime.json"]).get("memory_embedding_model", "-"))
PY
}
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" \
  --set-string worker.runtime.memoryEmbeddingModel=text-embedding-3-small \
  >"$TMP/memory.yaml" 2>"$TMP/memory.err" \
  || bad "a memoryEmbeddingModel does not render: $(tail -1 "$TMP/memory.err")"
# elitea-main is told the same model, so it can withhold memory from the
# catalogue when none is configured.
mainmemory() {
  python3 - "$1" <<'PY'
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
values = [d["data"].get("ELITEA_MEMORY_EMBEDDING_MODEL", "-") for d in docs
          if d["kind"] == "ConfigMap" and "ELITEA_INDEXING_RUNTIME" in (d.get("data") or {})]
assert len(values) == 1, values
print(values[0])
PY
}
[ "$(mainmemory "$TMP/memory.yaml")" = "text-embedding-3-small" ] \
  && ok "elitea-main receives the memory embedding model as ELITEA_MEMORY_EMBEDDING_MODEL" \
  || bad "main memory model: $(mainmemory "$TMP/memory.yaml")"
[ "$(mainmemory "$TMP/rust.yaml")" = "-" ] \
  && ok "elitea-main gets no ELITEA_MEMORY_EMBEDDING_MODEL while none is configured" \
  || bad "main memory model leaked: $(mainmemory "$TMP/rust.yaml")"
[ "$(mainmemory "$TMP/default.yaml")" = "-" ] \
  && ok "without indexing elitea-main gets no memory model" \
  || bad "the render without indexing main memory model: $(mainmemory "$TMP/default.yaml")"
[ "$(memorymodel "$TMP/rust.yaml")" = "-" ] \
  && ok "rust indexing writes no memory_embedding_model by default (the memory toolkit stays unserved)" \
  || bad "rust indexing leaked a memory model: $(memorymodel "$TMP/rust.yaml")"
[ "$(memorymodel "$TMP/memory.yaml")" = "text-embedding-3-small" ] \
  && ok "worker.runtime.memoryEmbeddingModel is written as memory_embedding_model" \
  || bad "memoryEmbeddingModel rendered: $(memorymodel "$TMP/memory.yaml")"
[ "$(memorymodel "$TMP/default.yaml")" = "-" ] \
  && ok "without indexing no memory_embedding_model" \
  || bad "the render without indexing leaked a memory model: $(memorymodel "$TMP/default.yaml")"

# The toolkit completion cap of a claim: absent by default (worker default
# 200), written when the operator sets it.
toolkitcap() {
  python3 - "$1" <<'PY'
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
print(json.loads(cm["data"]["runtime.json"]).get("toolkit_model_call_cap", "-"))
PY
}
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" \
  --set worker.runtime.toolkitModelCallCap=50 \
  >"$TMP/tcap.yaml" 2>"$TMP/tcap.err" \
  || bad "a toolkitModelCallCap does not render: $(tail -1 "$TMP/tcap.err")"
[ "$(toolkitcap "$TMP/rust.yaml")" = "-" ] \
  && ok "no toolkit_model_call_cap by default (the worker's 200 applies)" \
  || bad "default toolkit_model_call_cap leaked: $(toolkitcap "$TMP/rust.yaml")"
[ "$(toolkitcap "$TMP/tcap.yaml")" = "50" ] \
  && ok "worker.runtime.toolkitModelCallCap sets toolkit_model_call_cap" \
  || bad "toolkitModelCallCap=50 rendered: $(toolkitcap "$TMP/tcap.yaml")"

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

refuses 'toolkitModelCallCap must be an integer between 1' "${RUST[@]}" --set worker.runtime.toolkitModelCallCap=0 \
  && ok "toolkitModelCallCap=0 is refused" \
  || bad "toolkitModelCallCap=0 is not refused"

refuses 'worker.indexingRuntime=golang is no longer supported' --set worker.indexingRuntime=golang \
  && ok "an unknown worker.indexingRuntime is refused" \
  || bad "an unknown worker.indexingRuntime is not refused"

# The retired python indexing runtime is refused with what to do, and so is the
# retired Python worker beside it: an old values file must not silently switch
# index stores.
refuses 'worker.indexingRuntime=python is no longer supported' --set worker.indexingRuntime=python \
  && ok "the retired worker.indexingRuntime=python is refused" \
  || bad "the retired worker.indexingRuntime=python is not refused"
refuses 'worker.implementation=python is no longer supported' --set worker.implementation=python "${RUST[@]}" \
  && ok "the retired worker.implementation=python is refused" \
  || bad "the retired worker.implementation=python is not refused"
# One source of truth: an override that disagrees with the resolver is refused.
refuses 'disagrees with worker.indexingRuntime' "${RUST[@]}" --set-string main.env.ELITEA_INDEXING_RUNTIME=python \
  && ok "a main.env.ELITEA_INDEXING_RUNTIME override that disagrees with the resolver is refused" \
  || bad "a disagreeing ELITEA_INDEXING_RUNTIME override is not refused"
# The worker-off note names the Rust worker.
"$HELM" install t "$CHART" --dry-run=client "${RENDER[@]}" "${NOINDEX[@]}" --set worker.enabled=false 2>/dev/null \
  | grep 'assumes the Rust worker' >/dev/null \
  && ok "the worker-off note names the Rust worker" \
  || bad "the worker-off note does not name the Rust worker"

# No vector store anywhere: neither this chart's elitea-vector nor an external
# target. The worker would start without a vector_target and refuse every
# index run while elitea-main keeps admitting them.
refuses 'a vector store' \
  && ok "the worker indexing without a vector store is refused" \
  || bad "the worker indexing without a vector store is not refused"

# A memory model with nothing to use it: without indexing there is no
# vector_target and no claim vector token.
refuses 'worker.runtime.memoryEmbeddingModel needs' "${NOINDEX[@]}" \
  --set-string worker.runtime.memoryEmbeddingModel=text-embedding-3-small \
  && ok "memoryEmbeddingModel without indexing is refused" \
  || bad "memoryEmbeddingModel without indexing is not refused"

# The extractor guard follows the resolver and says what is actually wrong.
refuses 'worker.extractor.enabled needs indexing' "${NOINDEX[@]}" --set worker.extractor.enabled=true \
  && ok "the extractor without indexing is refused with an accurate message" \
  || bad "the extractor guard message is wrong or missing"

# The cap is validated: 0 must not silently become the default, and a
# negative, fractional or non-numeric value is refused with the key's name.
refuses 'worker.runtime.indexModelCallCap must be an integer' \
  "${RUST[@]}" --set "worker.runtime.indexModelCallCap=0" \
  && ok "indexModelCallCap=0 is refused" \
  || bad "indexModelCallCap=0 is not refused"
refuses 'worker.runtime.indexModelCallCap must be an integer' \
  "${RUST[@]}" --set "worker.runtime.indexModelCallCap=-5" \
  && ok "indexModelCallCap=-5 is refused" \
  || bad "indexModelCallCap=-5 is not refused"
refuses 'worker.runtime.indexModelCallCap must be an integer' \
  "${RUST[@]}" --set "worker.runtime.indexModelCallCap=1.5" \
  && ok "indexModelCallCap=1.5 is refused" \
  || bad "indexModelCallCap=1.5 is not refused"
refuses 'worker.runtime.indexModelCallCap must be an integer' \
  "${RUST[@]}" --set-string "worker.runtime.indexModelCallCap=many" \
  && ok "a non-numeric indexModelCallCap is refused" \
  || bad "a non-numeric indexModelCallCap is not refused"

echo
RAN=$((PASS+FAIL))
echo "indexing runtime render assertions: ${RAN} ran, ${PASS} passed, ${FAIL} failed"
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAIL: ${RAN} assertion(s) ran, and this file holds ${EXPECTED_ASSERTIONS} assertion site(s)." >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
