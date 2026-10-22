#!/usr/bin/env bash
# render-platform-listener.sh — the worker's platform_origin and the identity
# headers at the edges the chart renders.
#
# elitea-main serves the worker's platform_origin itself, on its internal mTLS
# "platform" listener (:9446, #1359). It replaced the platform-edge proxy
# this script used to check. The listener is gRPC-only; that it serves no HTTP
# and admits each service only to its allowlisted workload identities is gated
# by services/elitea-main/internal/transport/runtimegrpc/platform_server_test.go;
# the mTLS admission (runtime-CA client required) from the installed material
# by services/elitea-main/cmd/elitea-runtime-material/platform_listener_test.go.
# This script reads the RENDERED YAML and asserts:
#
#   1. the standalone + worker render wires the listener: elitea-main's
#      ConfigMap names the address, the content keypair and the runtime CA as
#      client CA; the Deployment and Service expose 9446; the worker's
#      platform_origin is https://elitea-main:9446; Main renders no client
#      identity of its own (its self call is gone); and no platform-edge
#      resource is rendered;
#   2. the NetworkPolicy admits the worker (and nothing else by default) to
#      9446, the chart default renders no platform client identity, and the
#      retired worker.platformEdge values are refused with a migration message;
#   3. the HTTPRoute rendered from main.ingress removes the floor;
#   4. the two chart guards (plain Ingress with Form sign-in; Gateway API list
#      missing a required name) refuse to render, and the acknowledged or
#      compliant configurations render;
#   5. ModelService's allowlist (ELITEA_MODEL_SERVICE_CLIENTS, #1359 U2) names
#      the worker, takes extraClients, and its guards refuse a worker without
#      a SPIFFE ID, an empty list, a hand-set env value, a malformed entry and
#      extraClients with the runtime off;
#   6. the ArtifactObjectService and InternalToolService allowlists (#1359 U5:
#      ELITEA_ARTIFACT_SERVICE_CLIENTS, ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS)
#      name the worker, the callback identity exactly when a provider needs
#      it, take extraClients, and refuse the same mistakes; the retired
#      internal MCP origin (currentMainBaseUrl, ELITEA_RUNTIME_CURRENT_MAIN_
#      BASE_URL) is refused with a migration message.
#
# Usage: deploy/helm/tests/render-platform-listener.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail
# Every `helm template` here starts from the pre-S10 feature switches
# (fixtures/features-off.yaml: runtime, worker, vector, Qdrant, DeepWiki and
# Inventory off), prepended so that any values file or --set a render names
# wins; each render turns on what it tests. render-default-on.sh covers the
# shipped defaults.
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
_helm_bin="${HELM:-helm}"
helm() {
  if [ "${1:-}" = template ]; then shift; command "$_helm_bin" template -f "$FEATURES_OFF" "$@"
  else command "$_helm_bin" "$@"; fi
}
HELM=helm

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "render-platform-listener.sh needs $tool on PATH" >&2
    exit 2
  }
done

GATEWAY_RENDER_POSTURE=(--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.ingressFrom[0].podSelector.matchLabels.render-only=gateway --set networkPolicies.main.noExternalIngress=false)
STANDALONE=(-f "$CHART/values-standalone.yaml")
WORKER=(--set worker.enabled=true --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=)

# The identity floor the edges must delete (lower case).
FLOOR="x-auth-type x-auth-id x-auth-user-id x-auth-reference x-auth-signature x-auth-avatar x-auth-avatar-state x-auth-session-id x-auth-session-name x-auth-session-endpoint x-elitea-identity-signature x-elitea-project-id x-elitea-user-id x-elitea-tenant-id"
EXEMPT="x-elitea-execution-id"
# The names the Gateway API HTTPRoute must remove (the full browser-edge floor).
ROUTE_FLOOR="$FLOOR $EXEMPT"

lc() { tr '[:upper:]' '[:lower:]'; }

# expect LABEL ACTUAL WANT
expect() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi
}

# ---------------------------------------------------------------------------
# 1. The platform listener, standalone + worker.
# ---------------------------------------------------------------------------
helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${WORKER[@]}" t "$CHART" >"$WORK/worker.yaml"
pass "the chart renders the worker against the platform listener"

cm() { yq eval-all "select(.kind == \"ConfigMap\" and .metadata.name == \"elitea-main-config\") | .data.$1" "$WORK/worker.yaml"; }
expect "listener address" "$(cm ELITEA_RUNTIME_PLATFORM_ADDRESS)" ":9446"
expect "listener certificate (content keypair, SAN elitea-main)" "$(cm ELITEA_RUNTIME_PLATFORM_TLS_CERT_FILE)" "/run/elitea-runtime/content-server.crt"
expect "listener key" "$(cm ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE)" "/run/elitea-runtime/content-server.key"
expect "listener client CA is the runtime CA" "$(cm ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE)" "/run/elitea-runtime/runtime-ca.crt"
# The worker's internal MCP is InternalToolService on this listener (#1359
# U5), so no HTTP origin for it is rendered any more.
expect "no internal MCP origin is rendered" "$(cm ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL)" "null"
# Main no longer calls itself (#1359 H7), so the chart renders no client
# identity of Main's own.
expect "no Main client identity is rendered" \
  "$(grep -c 'ELITEA_RUNTIME_PLATFORM_CLIENT_\|main-platform-client' "$WORK/worker.yaml" || true)" "0"
expect "Deployment exposes runtime-plat" \
  "$(yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-main") | .spec.template.spec.containers[].ports[] | select(.name == "runtime-plat") | .containerPort' "$WORK/worker.yaml")" "9446"
expect "Service exposes runtime-plat" \
  "$(yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-main") | .spec.ports[] | select(.name == "runtime-plat") | .port' "$WORK/worker.yaml")" "9446"
# Kubernetes refuses a port name longer than 15 characters (IANA_SVC_NAME)
# at apply time, which `helm template` never checks: "runtime-platform" (16)
# rendered here and failed only on the kind install. Every named port of
# every container and Service in this render must fit.
expect "every port name is at most 15 characters" \
  "$(yq eval-all '(.spec.template.spec.containers[]?.ports[]?.name, .spec.template.spec.initContainers[]?.ports[]?.name, .spec.ports[]?.name, .spec.ports[]?.targetPort) | select(tag == "!!str" and length > 15)' "$WORK/worker.yaml" | grep -v '^---$' | sort -u)" ""
expect "worker platform_origin" \
  "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "elitea-worker-runtime") | .data["runtime.json"]' "$WORK/worker.yaml" | yq -p=json '.platform_origin')" "https://elitea-main:9446"
edge="$(yq eval-all 'select(.metadata.name | test("platform-edge")) | .kind + "/" + .metadata.name' "$WORK/worker.yaml")"
expect "no platform-edge resource renders" "$edge" ""

# ---------------------------------------------------------------------------
# 2. NetworkPolicy, default render, retired values.
# ---------------------------------------------------------------------------
peers="$(yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-main-netpol") | .spec.ingress[] | select(.ports[].port == 9446) | .from[].podSelector.matchLabels["app.kubernetes.io/component"]' "$WORK/worker.yaml" | sort -u | tr '\n' ' ')"
expect "only the worker may reach 9446" "$peers" "worker "

helm template "${GATEWAY_RENDER_POSTURE[@]}" t "$CHART" >"$WORK/default.yaml"
expect "the default render names no platform client identity" \
  "$(grep -c 'ELITEA_RUNTIME_PLATFORM_CLIENT_' "$WORK/default.yaml" || true)" "0"

retired() {
  local label="$1" text="$2"; shift 2
  if helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${WORKER[@]}" t "$CHART" "$@" >/dev/null 2>"$WORK/err.txt"; then
    fail "$label: the chart rendered, but must refuse"
  elif grep -q -- "$text" "$WORK/err.txt"; then
    pass "$label: refused with the migration message"
  else
    fail "$label: refused for another reason: $(tail -1 "$WORK/err.txt")"
  fi
}
retired "worker.platformEdge.enabled=true" "worker.platformEdge was removed" --set worker.platformEdge.enabled=true
retired "platformOrigin naming the retired edge" "platform-edge Service was removed" --set-string worker.runtime.platformOrigin=https://elitea-platform-edge
retired "main.runtime.agentExecutionDispatch.currentMainBaseUrl" "currentMainBaseUrl was retired" \
  --set-string main.runtime.agentExecutionDispatch.currentMainBaseUrl=https://elitea-main:9446
retired "an EMPTY currentMainBaseUrl" "currentMainBaseUrl was retired" \
  --set-string main.runtime.agentExecutionDispatch.currentMainBaseUrl=
retired "main.env.ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL" "ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL was retired" \
  --set-string main.env.ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL=https://elitea-main:9446

# ---------------------------------------------------------------------------
# 3. The HTTPRoute rendered from main.ingress.
# ---------------------------------------------------------------------------
ONLY_MAIN=(--set web.enabled=false --set scheduler.enabled=false --set llmGateway.enabled=false --set otelCollector.enabled=false --set worker.enabled=false)
helm template "${GATEWAY_RENDER_POSTURE[@]}" "${ONLY_MAIN[@]}" t "$CHART" \
  --set main.ingress.enabled=true --set main.ingress.gatewayApi=true \
  --set-string main.ingress.gateway.name=elitea >"$WORK/httproute.yaml"
pass "the chart renders the HTTPRoute edge"
removed="$(yq eval-all 'select(.kind == "HTTPRoute") | .spec.rules[].filters[] | select(.type == "RequestHeaderModifier") | .requestHeaderModifier.remove[]' "$WORK/httproute.yaml" | lc)"
for name in $ROUTE_FLOOR; do
  if grep -qx "$name" <<<"$removed"; then
    pass "the HTTPRoute removes $name"
  else
    fail "the HTTPRoute does not remove $name"
  fi
done

# ---------------------------------------------------------------------------
# 4. The guards.
# ---------------------------------------------------------------------------
# refuses NAME EXPECTED_TEXT ARGS... — render must fail and say EXPECTED_TEXT.
refuses() {
  local label="$1" text="$2"; shift 2
  if helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${ONLY_MAIN[@]}" t "$CHART" "$@" >"$WORK/out.yaml" 2>"$WORK/err.txt"; then
    fail "$label: the chart rendered, but must refuse"
  elif grep -q -- "$text" "$WORK/err.txt"; then
    pass "$label: the chart refuses with the expected message"
  else
    fail "$label: the chart refused for another reason: $(tail -1 "$WORK/err.txt")"
  fi
}
renders() {
  local label="$1"; shift
  if helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${ONLY_MAIN[@]}" t "$CHART" "$@" >"$WORK/out.yaml" 2>"$WORK/err.txt"; then
    pass "$label: the chart renders"
  else
    fail "$label: the chart refused: $(tail -1 "$WORK/err.txt")"
  fi
}

# A plain Ingress renders with Form sign-in on (#1359 P3): elitea-main strips
# every identity header itself, so the old "the controller strips X-Auth-*"
# acknowledgement is gone.
renders "plain Ingress with Form sign-in" \
  --set main.ingress.enabled=true --set main.ingress.gatewayApi=false
# Chart defaults carry no Form sign-in (the standalone values file turns it on).
if helm template "${GATEWAY_RENDER_POSTURE[@]}" "${ONLY_MAIN[@]}" t "$CHART" \
  --set main.ingress.enabled=true --set main.ingress.gatewayApi=false >"$WORK/out.yaml" 2>"$WORK/err.txt"; then
  pass "plain Ingress without Form sign-in: the chart renders"
else
  fail "plain Ingress without Form sign-in: the chart refused: $(tail -1 "$WORK/err.txt")"
fi
for required in X-Auth-Type X-Auth-ID X-Auth-User-ID X-Auth-Signature; do
  refuses "Gateway API list without $required" "must name $required" \
    --set main.ingress.enabled=true --set main.ingress.gatewayApi=true \
    --set-string main.ingress.gateway.name=elitea \
    --set-json "main.ingress.stripIdentityHeaders=$(yq -o=json -I=0 "[.main.ingress.stripIdentityHeaders[] | select(downcase != \"$(echo "$required" | lc)\")]" "$CHART/values.yaml" 2>/dev/null || echo '[]')"
done
renders "Gateway API list in a different case" \
  --set main.ingress.enabled=true --set main.ingress.gatewayApi=true \
  --set-string main.ingress.gateway.name=elitea \
  --set-json 'main.ingress.stripIdentityHeaders=["x-auth-type","x-auth-id","x-auth-user-id","x-auth-signature"]'

# ---------------------------------------------------------------------------
# 5. ModelService's allowlist (#1359 U2): ELITEA_MODEL_SERVICE_CLIENTS.
# ---------------------------------------------------------------------------
# The standalone + worker render: the worker's SPIFFE ID (values-standalone
# states it twice, as expectedIdentity and as an extra client; it renders once).
expect "ModelService admits the worker" "$(cm ELITEA_MODEL_SERVICE_CLIENTS)" "spiffe://elitea.standalone/ns/default/sa/agent-worker"
expect "the default render (runtime off) names no ModelService client" \
  "$(grep -c 'ELITEA_MODEL_SERVICE_CLIENTS' "$WORK/default.yaml" || true)" "0"
helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${ONLY_MAIN[@]}" t "$CHART" \
  --set-json 'main.runtime.modelService.extraClients=["dns:elitea-subapp-callback","spiffe://x.test/ns/a/sa/worker","dns:elitea-subapp-callback"]' >"$WORK/extra.yaml"
expect "extraClients render as given, without repeats" \
  "$(yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "elitea-main-config") | .data.ELITEA_MODEL_SERVICE_CLIENTS' "$WORK/extra.yaml")" \
  "dns:elitea-subapp-callback,spiffe://x.test/ns/a/sa/worker"

refuses "the worker without its SPIFFE ID" "needs worker.runtimeSession.expectedIdentity" \
  --set worker.enabled=true --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream= \
  --set-string worker.runtimeSession.expectedIdentity=
refuses "a worker identity that is not spiffe://" "carries exactly one spiffe:// URI SAN" \
  --set worker.enabled=true --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream= \
  --set-string worker.runtimeSession.expectedIdentity=dns:agent-worker
refuses "the platform listener open with nobody admitted" "ELITEA_MODEL_SERVICE_CLIENTS would be empty" \
  --set-json 'main.runtime.modelService.extraClients=[]'
refuses "ELITEA_MODEL_SERVICE_CLIENTS set by hand" "main.env.ELITEA_MODEL_SERVICE_CLIENTS must not be set" \
  --set-string main.env.ELITEA_MODEL_SERVICE_CLIENTS=dns:someone
refuses "an extra client that is not a workload identity" "is not a workload identity" \
  --set-json 'main.runtime.modelService.extraClients=["agent-worker"]'
if helm template "${GATEWAY_RENDER_POSTURE[@]}" "${ONLY_MAIN[@]}" t "$CHART" \
  --set-json 'main.runtime.modelService.extraClients=["dns:someone"]' >/dev/null 2>"$WORK/err.txt"; then
  fail "extraClients with the runtime off: the chart rendered, but must refuse"
elif grep -q "main.runtime.enabled is false, so elitea-main opens no platform listener" "$WORK/err.txt"; then
  pass "extraClients with the runtime off: refused"
else
  fail "extraClients with the runtime off: refused for another reason: $(tail -1 "$WORK/err.txt")"
fi

# ---------------------------------------------------------------------------
# 6. ArtifactObjectService and InternalToolService (#1359 U5).
# ---------------------------------------------------------------------------
WORKER_ID="spiffe://elitea.standalone/ns/default/sa/agent-worker"
# values-standalone states the worker identity as extraClients too; it renders
# once. No provider runs here, so no callback identity.
expect "ArtifactObjectService admits the worker" "$(cm ELITEA_ARTIFACT_SERVICE_CLIENTS)" "$WORKER_ID"
expect "InternalToolService admits the worker" "$(cm ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS)" "$WORKER_ID"
expect "the default render (runtime off) names no artifact or internal-tool client" \
  "$(grep -c 'ELITEA_ARTIFACT_SERVICE_CLIENTS\|ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS' "$WORK/default.yaml" || true)" "0"
# The derivation alone (no extraClients): the worker from its runtime session,
# nothing else, and an EMPTY list when nothing calls (the service is then
# left unregistered, which elitea-main accepts).
helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${WORKER[@]}" t "$CHART" \
  --set-json 'main.runtime.artifactService.extraClients=[]' --set-json 'main.runtime.internalToolService.extraClients=[]' >"$WORK/derived.yaml"
dcm() { yq eval-all "select(.kind == \"ConfigMap\" and .metadata.name == \"elitea-main-config\") | .data.$2" "$1"; }
expect "derived: ArtifactObjectService = the worker" "$(dcm "$WORK/derived.yaml" ELITEA_ARTIFACT_SERVICE_CLIENTS)" "$WORKER_ID"
expect "derived: InternalToolService = the worker" "$(dcm "$WORK/derived.yaml" ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS)" "$WORKER_ID"
helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${ONLY_MAIN[@]}" t "$CHART" \
  --set-json 'main.runtime.artifactService.extraClients=[]' --set-json 'main.runtime.internalToolService.extraClients=[]' >"$WORK/nobody.yaml"
expect "nothing calls: ArtifactObjectService is left unregistered (empty)" "$(dcm "$WORK/nobody.yaml" ELITEA_ARTIFACT_SERVICE_CLIENTS)" ""
expect "nothing calls: InternalToolService is left unregistered (empty)" "$(dcm "$WORK/nobody.yaml" ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS)" ""
helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" "${ONLY_MAIN[@]}" t "$CHART" \
  --set-json 'main.runtime.artifactService.extraClients=["dns:a.test","spiffe://x.test/ns/a/sa/b","dns:a.test"]' \
  --set-json 'main.runtime.internalToolService.extraClients=["spiffe://x.test/ns/a/sa/b"]' >"$WORK/extra-u5.yaml"
expect "artifact extraClients render as given, without repeats" "$(dcm "$WORK/extra-u5.yaml" ELITEA_ARTIFACT_SERVICE_CLIENTS)" "dns:a.test,spiffe://x.test/ns/a/sa/b"
expect "internal-tool extraClients render as given" "$(dcm "$WORK/extra-u5.yaml" ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS)" "spiffe://x.test/ns/a/sa/b"
for pair in ELITEA_ARTIFACT_SERVICE_CLIENTS:artifactService ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS:internalToolService; do
  var="${pair%%:*}"; key="${pair#*:}"
  refuses "$var set by hand" "main.env.$var must not be set" --set-string "main.env.$var=dns:someone"
  refuses "a $key extra client that is not a workload identity" "main.runtime.$key.extraClients entry \"agent-worker\" is not a workload identity" \
    --set-json "main.runtime.$key.extraClients=[\"agent-worker\"]"
  refuses "$key extraClients that are not a list" "main.runtime.$key.extraClients must be a list" \
    --set-string "main.runtime.$key.extraClients=dns:someone"
  if helm template "${GATEWAY_RENDER_POSTURE[@]}" "${ONLY_MAIN[@]}" t "$CHART" \
    --set-json "main.runtime.$key.extraClients=[\"dns:someone\"]" >/dev/null 2>"$WORK/err.txt"; then
    fail "$key extraClients with the runtime off: the chart rendered, but must refuse"
  elif grep -q "main.runtime.$key.extraClients is set but main.runtime.enabled is false" "$WORK/err.txt"; then
    pass "$key extraClients with the runtime off: refused"
  else
    fail "$key extraClients with the runtime off: refused for another reason: $(tail -1 "$WORK/err.txt")"
  fi
done

if [ "$failures" -ne 0 ]; then
  echo "render-platform-listener.sh: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-platform-listener.sh: all assertions passed"
