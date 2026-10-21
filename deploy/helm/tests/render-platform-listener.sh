#!/usr/bin/env bash
# render-platform-listener.sh — the worker's platform_origin and the identity
# headers at the edges the chart renders.
#
# elitea-main serves the worker's platform_origin itself, on its internal mTLS
# "platform" listener (:9446, #1359). It replaced the platform-edge Traefik
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
#      platform_origin is https://elitea-main:9446; Main's self call presents
#      its own client identity; and no platform-edge resource is rendered;
#   2. the NetworkPolicy admits the worker (and nothing else by default) to
#      9446, the chart default renders no platform client identity, and the
#      retired worker.platformEdge values are refused with a migration message;
#   3. the HTTPRoute rendered from main.ingress removes the floor;
#   4. the two chart guards (plain Ingress with Form sign-in; Gateway API list
#      missing a required name) refuse to render, and the acknowledged or
#      compliant configurations render.
#
# Usage: deploy/helm/tests/render-platform-listener.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

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
expect "Main's self call names the listener" "$(cm ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL)" "https://elitea-main:9446"
expect "Main's self call presents its client certificate" "$(cm ELITEA_RUNTIME_PLATFORM_CLIENT_CERT_FILE)" "/run/elitea-runtime/main-platform-client.crt"
expect "Main's self call presents its client key" "$(cm ELITEA_RUNTIME_PLATFORM_CLIENT_KEY_FILE)" "/run/elitea-runtime/main-platform-client.key"
expect "Deployment exposes runtime-platform" \
  "$(yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-main") | .spec.template.spec.containers[].ports[] | select(.name == "runtime-platform") | .containerPort' "$WORK/worker.yaml")" "9446"
expect "Service exposes runtime-platform" \
  "$(yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-main") | .spec.ports[] | select(.name == "runtime-platform") | .port' "$WORK/worker.yaml")" "9446"
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

refuses "plain Ingress with Form sign-in" "identityHeadersStrippedByController" \
  --set main.ingress.enabled=true --set main.ingress.gatewayApi=false
renders "plain Ingress acknowledged" \
  --set main.ingress.enabled=true --set main.ingress.gatewayApi=false \
  --set main.ingress.identityHeadersStrippedByController=true
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

if [ "$failures" -ne 0 ]; then
  echo "render-platform-listener.sh: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-platform-listener.sh: all assertions passed"
