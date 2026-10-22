#!/usr/bin/env bash
# render-edge-route.sh — one edge route (#1359 P3).
#
# elitea-main is the browser edge in a cluster too: it redirects `/` to
# `/app/`, relays `/app…` and `/docs…` to elitea-web (ELITEA_WEB_UPSTREAM) and
# keeps the MCP server and its own file servers under `/app`. So the chart's
# Ingress or HTTPRoute must be ONE `/` rule to elitea-main, with no
# RegularExpression match (the MCP carve-out it once needed is Go now) and no
# rule naming elitea-web.
#
# Asserted on the RENDERED YAML:
#   1. the HTTPRoute: one route, one rule, PathPrefix `/`, backend elitea-main,
#      no RegularExpression, no redirect filter, the identity strip kept;
#   2. the Ingress: one path `/` to elitea-main, nothing to elitea-web;
#   3. ELITEA_WEB_UPSTREAM names the web Service when web is rendered, and is
#      absent when it is not;
#   4. with the edge on, the startup and readiness probes are the binary's
#      loopback -healthcheck (the edge refuses /readyz and /startupz to a
#      kubelet httpGet), liveness stays an httpGet on /healthz;
#   5. a probe path -healthcheck cannot read is refused while rendering;
#   6. no plain-Ingress identity-header acknowledgement is demanded any more.
#
# Usage: deploy/helm/tests/render-edge-route.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-edge-route.sh needs $tool on PATH" >&2; exit 2; }
done

POSTURE=(--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted
  --set 'networkPolicies.main.ingressFrom[0].podSelector.matchLabels.render-only=gateway')
HTTPROUTE=(--set main.ingress.enabled=true --set main.ingress.gatewayApi=true --set-string main.ingress.gateway.name=elitea)
INGRESS=(--set main.ingress.enabled=true --set main.ingress.gatewayApi=false --set 'main.ingress.hosts[0]=elitea.example.invalid')

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }
expect() { # expect LABEL GOT WANT
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi
}
render() { # render NAME ARGS... -> $WORK/NAME.yaml
  local name="$1"; shift
  helm template t "$CHART" "${POSTURE[@]}" "$@" >"$WORK/$name.yaml" 2>"$WORK/$name.err" \
    || { fail "$name does not render: $(tail -1 "$WORK/$name.err")"; return 1; }
}
main_container() { # main_container FILE EXPR
  yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-main\") | .spec.template.spec.containers[0] | $2" "$1"
}

# ── 1. HTTPRoute ─────────────────────────────────────────────────────────────
if render httproute "${HTTPROUTE[@]}"; then
  f="$WORK/httproute.yaml"
  expect "one HTTPRoute" "$(yq eval-all '[select(.kind == "HTTPRoute")] | length' "$f")" "1"
  expect "one rule" "$(yq eval-all 'select(.kind == "HTTPRoute") | .spec.rules | length' "$f")" "1"
  expect "one match, PathPrefix /" "$(yq eval-all 'select(.kind == "HTTPRoute") | [.spec.rules[0].matches[].path | .type + " " + .value] | join(",")' "$f")" "PathPrefix /"
  expect "no RegularExpression match" "$(yq eval-all '[select(.kind == "HTTPRoute") | .spec.rules[].matches[].path | select(.type == "RegularExpression")] | length' "$f")" "0"
  expect "the only backend is elitea-main:8080" "$(yq eval-all 'select(.kind == "HTTPRoute") | [.spec.rules[].backendRefs[] | .name + ":" + (.port | tostring)] | join(",")' "$f")" "elitea-main:8080"
  expect "no filter but the header strip (the redirect is Go's)" "$(yq eval-all 'select(.kind == "HTTPRoute") | [.spec.rules[].filters[].type] | join(",")' "$f")" "RequestHeaderModifier"
  expect "the strip still removes X-Auth-Type" "$(yq eval-all 'select(.kind == "HTTPRoute") | [.spec.rules[0].filters[0].requestHeaderModifier.remove[] | select(. == "X-Auth-Type")] | length' "$f")" "1"
  expect "nothing routes to elitea-web" "$(grep -c 'name: elitea-web$' <(yq eval-all 'select(.kind == "HTTPRoute")' "$f") || true)" "0"
fi

# ── 2. Ingress ───────────────────────────────────────────────────────────────
if render ingress "${INGRESS[@]}"; then
  f="$WORK/ingress.yaml"
  expect "one Ingress" "$(yq eval-all '[select(.kind == "Ingress")] | length' "$f")" "1"
  expect "one path / to elitea-main" "$(yq eval-all 'select(.kind == "Ingress") | [.spec.rules[].http.paths[] | .path + " " + .pathType + " " + .backend.service.name] | join(",")' "$f")" "/ Prefix elitea-main"
fi

# ── 3 + 4. ELITEA_WEB_UPSTREAM and the probes ───────────────────────────────
if render edge-on "${HTTPROUTE[@]}"; then
  f="$WORK/edge-on.yaml"
  expect "ELITEA_WEB_UPSTREAM names the web Service" "$(main_container "$f" '.env[] | select(.name == "ELITEA_WEB_UPSTREAM") | .value')" "http://elitea-web:80"
  expect "the web Service exists on that port" "$(yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-web") | .spec.ports[0].port' "$f")" "80"
  expect "startup probe is the loopback -healthcheck" "$(main_container "$f" '.startupProbe.exec.command | join(" ")')" "/elitea-main -healthcheck /startupz"
  expect "readiness probe is the loopback -healthcheck" "$(main_container "$f" '.readinessProbe.exec.command | join(" ")')" "/elitea-main -healthcheck /readyz"
  expect "liveness stays an httpGet on /healthz" "$(main_container "$f" '.livenessProbe.httpGet.path')" "/healthz"
fi
if render edge-port "${HTTPROUTE[@]}" --set web.service.port=8081; then
  expect "the upstream follows web.service.port" "$(main_container "$WORK/edge-port.yaml" '.env[] | select(.name == "ELITEA_WEB_UPSTREAM") | .value')" "http://elitea-web:8081"
fi
if render edge-off "${HTTPROUTE[@]}" --set web.enabled=false; then
  f="$WORK/edge-off.yaml"
  expect "no ELITEA_WEB_UPSTREAM without elitea-web" "$(main_container "$f" '[.env[] | select(.name == "ELITEA_WEB_UPSTREAM")] | length')" "0"
  expect "readiness is an httpGet without the edge" "$(main_container "$f" '.readinessProbe.httpGet.path')" "/readyz"
  expect "startup is an httpGet without the edge" "$(main_container "$f" '.startupProbe.httpGet.path')" "/startupz"
fi

# ── 5. A probe path -healthcheck cannot read ─────────────────────────────────
if helm template t "$CHART" "${POSTURE[@]}" --set main.probes.readiness.path=/custom >/dev/null 2>"$WORK/probe.err"; then
  fail "a readiness path -healthcheck cannot read rendered with the edge on"
elif grep -q "main.probes.readiness.path" "$WORK/probe.err"; then
  pass "a readiness path -healthcheck cannot read is refused"
else
  fail "the custom probe path was refused for another reason: $(tail -1 "$WORK/probe.err")"
fi
render probe-no-edge --set web.enabled=false --set main.probes.readiness.path=/custom >/dev/null \
  && pass "any probe path renders when the edge is off (it stays an httpGet)"

# ── 6. A plain Ingress needs no identity-header acknowledgement ──────────────
# values-standalone.yaml turns Form sign-in on, which is what the old guard
# keyed on.
if helm template t "$CHART" "${POSTURE[@]}" -f "$CHART/values-standalone.yaml" "${INGRESS[@]}" \
    --set networkPolicies.main.noExternalIngress=false >/dev/null 2>"$WORK/plain-form.err"; then
  pass "a plain Ingress with Form sign-in renders with no identity-header acknowledgement"
else
  fail "a plain Ingress with Form sign-in is refused: $(tail -1 "$WORK/plain-form.err")"
fi

if [ "$failures" -ne 0 ]; then
  echo "render-edge-route.sh: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-edge-route.sh: all assertions passed"
