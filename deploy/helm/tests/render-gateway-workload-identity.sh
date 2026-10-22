#!/usr/bin/env bash
# render-gateway-workload-identity.sh — keyless platform credentials on GKE.
#
# llmGateway.workloadIdentity.enabled lets a PLATFORM provider (vertex_ai or
# ai_dial) sign in with the gateway pod's Google identity instead of a key
# (services/elitea-llm-gateway/internal/account/workload_identity.go). The
# chart's part:
#
#   * it renders GATEWAY_WORKLOAD_IDENTITY_ENABLED from that ONE value, "false"
#     by default, and refuses the same name in llmGateway.env;
#   * it puts llmGateway.serviceAccount.annotations (the GKE binding) on the
#     gateway's ServiceAccount;
#   * the gateway NetworkPolicy opens the GKE metadata server ONLY when the mode
#     is on, as two /32 rules, and keeps 169.254.0.0/16 out of the public rule.
#
# Every assertion reads the RENDERED objects.
#
# Usage: deploy/helm/tests/render-gateway-workload-identity.sh
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
# The same settled guards render-gateway-master-key.sh uses, so every result
# below can only come from the workload-identity logic.
ONLY_GATEWAY=(--set main.enabled=false --set web.enabled=false --set scheduler.enabled=false
  --set otelCollector.enabled=false --set worker.enabled=false
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted
  --set networkPolicies.main.noExternalIngress=true
  --set llmGateway.networkPolicy.enabled=true)
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-gateway-workload-identity.sh needs $tool on PATH" >&2; exit 2; }
done

render() {
  local out="$1"
  shift
  helm template test-release "$CHART" "${ONLY_GATEWAY[@]}" "$@" >"$out" 2>"$WORK/err.txt" || {
    cat "$WORK/err.txt" >&2
    return 1
  }
}

# wiEnv <file> — the value of GATEWAY_WORKLOAD_IDENTITY_ENABLED on the gateway.
wiEnv() {
  yq 'select(.kind == "Deployment" and .metadata.name == "elitea-llm-gateway") | .spec.template.spec.containers[0].env[] | select(.name == "GATEWAY_WORKLOAD_IDENTITY_ENABLED") | .value' "$1"
}

# metadataRules <file> — the egress peers of the gateway policy that name a
# 169.254.x address as their CIDR.
metadataRules() {
  yq 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-llm-gateway-egress") | .spec.egress[].to[].ipBlock.cidr | select(. == "169.254.*")' "$1"
}

refuses() {
  local description="$1" expected="$2"
  shift 2
  local output
  if output="$(helm template test-release "$CHART" "${ONLY_GATEWAY[@]}" "$@" 2>&1)"; then
    fail "the chart rendered $description"
  elif grep -qE "$expected" <<<"$output"; then
    pass "the chart refuses $description while it renders"
  else
    fail "the chart refuses $description but the message does not name '$expected':
$output"
  fi
}

# 1. Default: the mode is stated as off, and the metadata server stays closed.
if render "$WORK/default.yaml"; then
  [ "$(wiEnv "$WORK/default.yaml")" = "false" ] \
    && pass "the default renders GATEWAY_WORKLOAD_IDENTITY_ENABLED=false" \
    || fail "the default does not render GATEWAY_WORKLOAD_IDENTITY_ENABLED=false"
  [ -z "$(metadataRules "$WORK/default.yaml")" ] \
    && pass "the default policy opens no metadata address" \
    || fail "the default policy opens a metadata address: $(metadataRules "$WORK/default.yaml")"
  yq 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-llm-gateway-egress") | .spec.egress[].to[] | select(.ipBlock.except) | .ipBlock.except[]' \
    "$WORK/default.yaml" | grep -cE '^169[.]254[.]0[.]0/16( |$)' >/dev/null \
    && pass "the public rule excludes 169.254.0.0/16" \
    || fail "the public rule does not exclude 169.254.0.0/16"
  [ "$(yq 'select(.kind == "ServiceAccount" and .metadata.name == "elitea-llm-gateway") | .metadata.annotations' "$WORK/default.yaml")" = "null" ] \
    && pass "the default ServiceAccount carries no annotations" \
    || fail "the default ServiceAccount carries annotations"
else
  fail "the default values do not render"
fi

# 2. Enabled with a GKE binding: the env says true, the ServiceAccount carries
# the binding, and the policy opens exactly the two metadata /32s.
if render "$WORK/enabled.yaml" --set llmGateway.workloadIdentity.enabled=true \
  --set-string 'llmGateway.serviceAccount.annotations.iam\.gke\.io/gcp-service-account=gw@render.iam.gserviceaccount.com'; then
  [ "$(wiEnv "$WORK/enabled.yaml")" = "true" ] \
    && pass "enabled renders GATEWAY_WORKLOAD_IDENTITY_ENABLED=true" \
    || fail "enabled does not render GATEWAY_WORKLOAD_IDENTITY_ENABLED=true"
  [ "$(yq 'select(.kind == "ServiceAccount" and .metadata.name == "elitea-llm-gateway") | .metadata.annotations["iam.gke.io/gcp-service-account"]' "$WORK/enabled.yaml")" = "gw@render.iam.gserviceaccount.com" ] \
    && pass "the gateway ServiceAccount carries the GKE binding" \
    || fail "the gateway ServiceAccount does not carry the GKE binding"
  [ "$(metadataRules "$WORK/enabled.yaml" | sort | tr '\n' ' ')" = "169.254.169.252/32 169.254.169.254/32 " ] \
    && pass "enabled opens exactly the two GKE metadata /32s" \
    || fail "enabled opens these metadata addresses: $(metadataRules "$WORK/enabled.yaml" | tr '\n' ' ')"
  yq 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-llm-gateway-egress") | .spec.egress[] | select(.to[0].ipBlock.cidr == "169.254.169.254/32") | .ports[].port' \
    "$WORK/enabled.yaml" | grep -cx '80' >/dev/null \
    && pass "the metadata rule opens port 80 only" \
    || fail "the metadata rule does not open port 80"
  yq 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-llm-gateway-egress") | .spec.egress[].to[] | select(.ipBlock.except) | .ipBlock.except[]' \
    "$WORK/enabled.yaml" | grep -cE '^169[.]254[.]0[.]0/16( |$)' >/dev/null \
    && pass "the public rule still excludes 169.254.0.0/16 when enabled" \
    || fail "enabling the mode removed 169.254.0.0/16 from the public exclusions"
else
  fail "the enabled values do not render"
fi

# 3. One source only, and a boolean only.
refuses "GATEWAY_WORKLOAD_IDENTITY_ENABLED set in llmGateway.env" "owned by the chart" \
  --set-string llmGateway.env.GATEWAY_WORKLOAD_IDENTITY_ENABLED=true
refuses "a non-boolean llmGateway.workloadIdentity.enabled" "must be true or false" \
  --set-string llmGateway.workloadIdentity.enabled=yes

if [ "$failures" -gt 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all workload-identity render checks passed"
