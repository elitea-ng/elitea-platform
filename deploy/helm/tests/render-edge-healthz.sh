#!/usr/bin/env bash
# render-edge-healthz.sh — issue #569.
#
# Assert that the chart's own browser edge PUBLISHES /healthz, and publishes it
# with no authenticating filter in front of it.
#
# The defect this closes: on the cluster, /healthz sat in the HTTPRoute rule
# that carries the forward-auth ExtensionRef, and the forward-auth policy in
# internal/api/main_public_rules.go did not name it. Every probe of the public
# hostname answered a 302 to the login form. The decision on the issue is to
# publish /healthz at the edge with a public rule and keep the body at the
# minimal {"status":"ok"}.
#
# Every assertion below reads the RENDERED YAML, never values.yaml or the
# template. The chart renders two edge shapes for elitea-main
# (templates/main/ingress.yaml): an HTTPRoute when main.ingress.gatewayApi is
# true, and a networking.k8s.io Ingress otherwise. Both must reach /healthz,
# and the HTTPRoute must carry no ExtensionRef — an authenticating filter on
# the one rule that also carries the liveness path is exactly the #569 shape.
# The Go half of the same contract is
# services/elitea-main/tests/deployedge/edge_public_policy_test.go, which
# drives the real forward-auth handler with the real public-rule catalog.
#
# Usage: deploy/helm/tests/render-edge-healthz.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

# The single chart contains the LLM gateway, which REFUSES to render until an
# operator states its two postures (render-only values, .invalid reserved by
# RFC 2606). The render is narrowed to elitea-main so that "the HTTPRoute"
# below means one document.
ONLY_MAIN="--set web.enabled=false --set scheduler.enabled=false --set llmGateway.enabled=false --set otelCollector.enabled=false --set worker.enabled=false --set runtimeRedis.enabled=false"
GATEWAY_RENDER_POSTURE="--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 --set-string llmGateway.egressPosture=public-unrestricted"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
PUBLIC_RULES_GO="$REPO/services/elitea-main/internal/api/main_public_rules.go"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "render-edge-healthz.sh needs $tool on PATH" >&2
    exit 2
  }
done

# path_covers PATH_TYPE PATH_VALUE — does one Gateway API / Ingress path match
# reach /healthz? Gateway API PathPrefix matches on element boundaries, so `/`
# covers everything and `/healthz` covers itself; an Ingress `Prefix` path
# behaves the same way. `Exact` must name the path itself.
path_covers() {
  local kind="$1" value="$2"
  case "$kind" in
    Exact) [ "$value" = "/healthz" ] ;;
    PathPrefix|Prefix|ImplementationSpecific)
      value="${value%/}"
      [ -z "$value" ] || [ "$value" = "/healthz" ] ;;
    *) return 1 ;;
  esac
}

# ---------------------------------------------------------------------------
# 1. The HTTPRoute shape (main.ingress.gatewayApi: true).
# ---------------------------------------------------------------------------
helm template ${GATEWAY_RENDER_POSTURE} ${ONLY_MAIN} test-release "$CHART" \
  --set main.ingress.enabled=true \
  --set main.ingress.gatewayApi=true \
  --set-string main.ingress.gateway.name=elitea \
  >"$WORK/httproute.yaml"
pass "the chart renders the HTTPRoute edge"

routes="$(yq eval-all 'select(.kind == "HTTPRoute") | .metadata.name' "$WORK/httproute.yaml" | grep -c . || true)"
if [ "$routes" -ne 1 ]; then
  fail "expected exactly one HTTPRoute for elitea-main, found $routes; the assertions below read one document"
fi

covered=0
rule_index=0
while IFS= read -r line; do
  kind="${line%%|*}"; value="${line#*|}"
  if path_covers "$kind" "$value"; then covered=1; fi
done < <(yq eval-all 'select(.kind == "HTTPRoute") | .spec.rules[].matches[].path | (.type + "|" + .value)' "$WORK/httproute.yaml")
if [ "$covered" -eq 1 ]; then
  pass "the HTTPRoute forwards /healthz to elitea-main"
else
  fail "no HTTPRoute path match reaches /healthz; a probe of the public hostname gets the Gateway's own 404"
fi

# The rule that reaches /healthz must carry no ExtensionRef. An authenticating
# filter there asks a policy the liveness path is not part of, and the probe
# gets a login redirect. The chart authenticates nothing at this edge:
# elitea-main answers /healthz itself, and its session middleware never sees
# the path.
extension_refs="$(yq eval-all 'select(.kind == "HTTPRoute") | [.spec.rules[].filters[]? | select(.type == "ExtensionRef")] | length' "$WORK/httproute.yaml")"
if [ "$extension_refs" = "0" ]; then
  pass "the HTTPRoute carries no ExtensionRef filter in front of /healthz"
else
  fail "the HTTPRoute carries $extension_refs ExtensionRef filter(s); a forward-auth filter on the rule that reaches /healthz answers a probe with a 302 to the login form (#569)"
fi

backend="$(yq eval-all 'select(.kind == "HTTPRoute") | .spec.rules[0].backendRefs[0].name' "$WORK/httproute.yaml")"
if [ "$backend" = "test-release-elitea-main" ] || [ "$backend" = "elitea-main" ]; then
  pass "the HTTPRoute backend is elitea-main ($backend)"
else
  fail "the HTTPRoute backend is $backend, not elitea-main; /healthz would reach another Service"
fi

# ---------------------------------------------------------------------------
# 2. The Ingress shape (main.ingress.gatewayApi: false).
# ---------------------------------------------------------------------------
helm template ${GATEWAY_RENDER_POSTURE} ${ONLY_MAIN} test-release "$CHART" \
  --set main.ingress.enabled=true \
  --set main.ingress.gatewayApi=false \
  --set 'main.ingress.hosts[0]=elitea.example.invalid' \
  >"$WORK/ingress.yaml"
pass "the chart renders the Ingress edge"

covered=0
while IFS= read -r line; do
  kind="${line%%|*}"; value="${line#*|}"
  if path_covers "$kind" "$value"; then covered=1; fi
done < <(yq eval-all 'select(.kind == "Ingress") | .spec.rules[].http.paths[] | (.pathType + "|" + .path)' "$WORK/ingress.yaml")
if [ "$covered" -eq 1 ]; then
  pass "the Ingress forwards /healthz to elitea-main"
else
  fail "no Ingress path reaches /healthz"
fi

# ---------------------------------------------------------------------------
# 3. The policy half exists. The chart publishes the path; the forward-auth
#    policy must admit it, or an install that adds an authenticating edge in
#    front of this chart lands back on #569. The Go gate proves the rule
#    works; this line proves the chart and the policy speak of the same path.
# ---------------------------------------------------------------------------
if grep -q '"go.health.healthz"' "$PUBLIC_RULES_GO"; then
  pass "main_public_rules.go carries the go.health.healthz public rule"
else
  fail "main_public_rules.go carries no go.health.healthz rule; a forward-auth edge in front of this chart answers /healthz with a login redirect"
fi

if [ "$failures" -ne 0 ]; then
  echo "render-edge-healthz.sh: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-edge-healthz.sh: all assertions passed"
