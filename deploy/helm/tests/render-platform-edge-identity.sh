#!/usr/bin/env bash
# render-platform-edge-identity.sh — identity-header handling at the edges the
# chart renders.
#
# elitea-main accepts an X-Auth-* identity only together with a valid
# X-Auth-Signature for the same request. Every edge in front of it therefore
# has to (1) delete caller-supplied X-Auth-* names before any forwardAuth runs,
# and (2) copy X-Auth-Signature from the auth response. This script reads the
# RENDERED YAML and asserts:
#
#   1. the worker platform edge ConfigMap: every router that reaches
#      elitea-main starts with the strip middleware, the strip middleware
#      deletes the identity floor, every forwardAuth copies the projection
#      (incl. X-Auth-Signature), and the catch-all excludes /internal;
#   2. that rendered `http` block equals deploy/runtime/platform-edge-dynamic.yml;
#   3. the HTTPRoute rendered from main.ingress removes the floor;
#   4. the two chart guards (plain Ingress with Form sign-in; Gateway API list
#      missing a required name) refuse to render, and the acknowledged or
#      compliant configurations render.
#
# The Go half is services/elitea-main/tests/deployedge/edge_platform_identity_test.go.
#
# Usage: deploy/helm/tests/render-platform-edge-identity.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
RUNTIME_EDGE="$REPO/deploy/runtime/platform-edge-dynamic.yml"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "render-platform-edge-identity.sh needs $tool on PATH" >&2
    exit 2
  }
done

GATEWAY_RENDER_POSTURE=(--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.ingressFrom[0].podSelector.matchLabels.render-only=gateway --set networkPolicies.main.noExternalIngress=false)
STANDALONE=(-f "$CHART/values-standalone.yaml")

# The identity floor the strip middleware must delete (lower case). It is the
# browser-edge floor of edge_identity_strip_test.go minus the names in
# EXEMPT below.
FLOOR="x-auth-type x-auth-id x-auth-user-id x-auth-reference x-auth-signature x-auth-avatar x-auth-avatar-state x-auth-session-id x-auth-session-name x-auth-session-endpoint x-elitea-identity-signature x-elitea-project-id x-elitea-user-id x-elitea-tenant-id"
# X-Elitea-Execution-Id: sent by the runtime worker on model calls through
# platform_origin (this edge), so it must NOT be deleted here.
EXEMPT="x-elitea-execution-id"
AUTH_PROJECTION="x-auth-type x-auth-id x-auth-user-id x-auth-signature"
# The names the Gateway API HTTPRoute must remove (the full browser-edge floor).
ROUTE_FLOOR="$FLOOR $EXEMPT"

lc() { tr '[:upper:]' '[:lower:]'; }

# check_dynamic LABEL FILE — assert (a)-(d) on a Traefik dynamic config file.
check_dynamic() {
  local label="$1" file="$2" name
  local strip="strip-caller-auth-context"

  # (a) every router reaching elitea-main starts with the strip middleware.
  local routers bad
  routers="$(yq -r '.http.routers | to_entries | .[] | select(.value.service == "elitea-main") | .key' "$file")"
  if [ -z "$routers" ]; then
    fail "$label: no router reaches elitea-main, so nothing was checked"
  fi
  for name in $routers; do
    bad="$(yq -r ".http.routers.\"$name\".middlewares[0] // \"\"" "$file")"
    if [ "${bad%@*}" = "$strip" ]; then
      pass "$label: router $name runs $strip first"
    else
      fail "$label: router $name does not run $strip first (first middleware: '${bad}')"
    fi
  done

  # (b) the strip middleware deletes the floor and never sets a value.
  local deleted
  deleted="$(yq -r ".http.middlewares.\"$strip\".headers.customRequestHeaders | to_entries | .[] | select(.value == \"\") | .key" "$file" | lc)"
  for name in $FLOOR; do
    if grep -qx "$name" <<<"$deleted"; then
      pass "$label: $strip deletes $name"
    else
      fail "$label: $strip does not delete $name"
    fi
  done
  for name in $EXEMPT; do
    if grep -qx "$name" <<<"$deleted"; then
      fail "$label: $strip deletes $name, which the runtime worker sends through this edge"
    else
      pass "$label: $strip leaves $name in place"
    fi
  done
  local nonempty
  nonempty="$(yq -r ".http.middlewares.\"$strip\".headers.customRequestHeaders | to_entries | .[] | select(.value != \"\") | .key" "$file")"
  [ -z "$nonempty" ] && pass "$label: $strip sets no header value" || fail "$label: $strip sets values for: $nonempty"

  # (c) every forwardAuth copies the projection.
  local auths
  auths="$(yq -r '.http.middlewares | to_entries | .[] | select(.value.forwardAuth) | .key' "$file")"
  [ -n "$auths" ] || fail "$label: no forwardAuth middleware, so nothing was checked"
  local have
  for name in $auths; do
    have="$(yq -r ".http.middlewares.\"$name\".forwardAuth.authResponseHeaders[]" "$file" | lc)"
    local h missing=""
    for h in $AUTH_PROJECTION; do
      grep -qx "$h" <<<"$have" || missing="$missing $h"
    done
    [ -z "$missing" ] && pass "$label: forwardAuth $name copies the projection incl. x-auth-signature" \
                      || fail "$label: forwardAuth $name does not copy:$missing"
  done

  # (d) the catch-all excludes /internal.
  local rule
  rule="$(yq -r '.http.routers.platform.rule' "$file")"
  case "$rule" in
    *'!PathPrefix(`/internal`)'*) pass "$label: the catch-all excludes /internal" ;;
    *) fail "$label: the catch-all rule '$rule' does not exclude /internal" ;;
  esac
}

# ---------------------------------------------------------------------------
# 1 + 2. The worker platform edge.
# ---------------------------------------------------------------------------
helm template "${GATEWAY_RENDER_POSTURE[@]}" "${STANDALONE[@]}" --set worker.enabled=true t "$CHART" >"$WORK/worker.yaml"
pass "the chart renders with the worker and its platform edge"

yq eval-all 'select(.kind == "ConfigMap" and .metadata.component != "x" and .metadata.name == "elitea-platform-edge") | .data["dynamic.yml"]' "$WORK/worker.yaml" >"$WORK/rendered-dynamic.yml"
if [ ! -s "$WORK/rendered-dynamic.yml" ] || [ "$(yq -r '.http.routers | length' "$WORK/rendered-dynamic.yml")" = "0" ]; then
  fail "no platform edge dynamic.yml was rendered"
else
  check_dynamic "rendered platform edge" "$WORK/rendered-dynamic.yml"
  check_dynamic "runtime platform edge" "$RUNTIME_EDGE"

  # Parity: the routers, middlewares and services blocks are identical.
  yq -o=json -I=0 -P '.http | sort_keys(..)' "$WORK/rendered-dynamic.yml" >"$WORK/rendered.json"
  yq -o=json -I=0 -P '.http | sort_keys(..)' "$RUNTIME_EDGE" >"$WORK/runtime.json"
  if cmp -s "$WORK/rendered.json" "$WORK/runtime.json"; then
    pass "the rendered http block equals deploy/runtime/platform-edge-dynamic.yml"
  else
    fail "the rendered http block differs from deploy/runtime/platform-edge-dynamic.yml"
    diff "$WORK/rendered.json" "$WORK/runtime.json" | head -5 >&2 || true
  fi
fi

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
  echo "render-platform-edge-identity.sh: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-platform-edge-identity.sh: all assertions passed"
