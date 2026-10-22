#!/usr/bin/env bash
# render-gke-ingress.sh — elitea-main behind the GKE Ingress (main.ingress.gke).
#
# The standard Google Cloud load balancer, optionally behind a proxy of your
# own, needs three things the chart did not render: container-native load
# balancing (the NEG annotation), a BackendConfig whose timeout does not cut
# SSE streams at the GCP default of 30 seconds and whose health check asks
# /healthz (the readiness probe is an exec probe GKE cannot copy), and a
# FrontendConfig that redirects plain HTTP. TLS comes from a Secret that
# cert-manager may issue.
#
# Asserted on the RENDERED YAML, on and off:
#   1. off (the default, and a non-GKE Ingress): no BackendConfig, no
#      FrontendConfig, no GKE annotation anywhere;
#   2. gke.enabled: the class annotation, NEG + backend-config on the Service's
#      http port only, the BackendConfig (timeout 3600, draining, HTTP
#      /healthz on 8080, optional Cloud Armor), the FrontendConfig (redirect,
#      optional SSL policy) and the binding annotation on the Ingress;
#   3. className gce turns it on too, and sets no second class annotation;
#   4. create=false renders neither object and binds the operator's names;
#   5. TLS secretName and the cert-manager annotations (issuer, kind, group);
#   6. the refusals: gke with gatewayApi, issuer with clusterIssuer.
#
# Usage: deploy/helm/tests/render-gke-ingress.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-gke-ingress.sh needs $tool on PATH" >&2; exit 2; }
done

# The two Google front-end ranges the load balancer and its health checks use,
# as the values comment tells an operator to admit them.
POSTURE=(--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted
  --set 'networkPolicies.main.ingressFrom[0].ipBlock.cidr=35.191.0.0/16'
  --set 'networkPolicies.main.ingressFrom[1].ipBlock.cidr=130.211.0.0/22')
INGRESS=(--set main.ingress.enabled=true --set 'main.ingress.hosts[0]=elitea.example.invalid')

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }
expect() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi; }
render() {
  local name="$1"; shift
  helm template t "$CHART" "${POSTURE[@]}" "$@" >"$WORK/$name.yaml" 2>"$WORK/$name.err" \
    || { fail "$name does not render: $(tail -1 "$WORK/$name.err")"; return 1; }
}
refuses() {
  local label="$1" text="$2"; shift 2
  if helm template t "$CHART" "${POSTURE[@]}" "$@" >/dev/null 2>"$WORK/refuse.err"; then
    fail "$label: rendered, but must refuse"
  elif grep -q -- "$text" "$WORK/refuse.err"; then
    pass "$label: refused"
  else
    fail "$label: refused for another reason: $(tail -1 "$WORK/refuse.err")"
  fi
}
q() { yq eval-all "$1" "$2"; }
svc_ann() { q "select(.kind == \"Service\" and .metadata.name == \"elitea-main\") | .metadata.annotations[\"$1\"] // \"\"" "$2"; }
ing_ann() { q "select(.kind == \"Ingress\") | .metadata.annotations[\"$1\"] // \"\"" "$2"; }
count() { q "[select(.kind == \"$1\")] | length" "$2"; }

# ── 1. Off ───────────────────────────────────────────────────────────────────
for variant in default plain; do
  case "$variant" in default) args=();; plain) args=("${INGRESS[@]}" --set main.ingress.className=nginx);; esac
  if render "off-$variant" ${args[@]+"${args[@]}"}; then
    f="$WORK/off-$variant.yaml"
    expect "$variant: no BackendConfig" "$(count BackendConfig "$f")" "0"
    expect "$variant: no FrontendConfig" "$(count FrontendConfig "$f")" "0"
    expect "$variant: no NEG annotation" "$(svc_ann cloud.google.com/neg "$f")" ""
    expect "$variant: no backend-config annotation" "$(svc_ann cloud.google.com/backend-config "$f")" ""
  fi
done

# ── 2. gke.enabled ───────────────────────────────────────────────────────────
if render on "${INGRESS[@]}" --set main.ingress.gke.enabled=true \
    --set-string main.ingress.gke.backendConfig.securityPolicy=edge-armor \
    --set-string main.ingress.gke.frontendConfig.sslPolicy=modern-tls; then
  f="$WORK/on.yaml"
  expect "the Ingress class is gce" "$(ing_ann kubernetes.io/ingress.class "$f")" "gce"
  expect "container-native load balancing" "$(svc_ann cloud.google.com/neg "$f")" '{"ingress":true}'
  expect "the BackendConfig binds the http port only" "$(svc_ann cloud.google.com/backend-config "$f")" '{"ports":{"http":"elitea-main"}}'
  expect "the Ingress names the FrontendConfig" "$(ing_ann networking.gke.io/v1beta1.FrontendConfig "$f")" "elitea-main"
  bc='select(.kind == "BackendConfig")'
  expect "one BackendConfig, cloud.google.com/v1" "$(q "[$bc | .apiVersion] | join(\",\")" "$f")" "cloud.google.com/v1"
  expect "timeoutSec keeps streams alive" "$(q "$bc | .spec.timeoutSec" "$f")" "3600"
  expect "connection draining" "$(q "$bc | .spec.connectionDraining.drainingTimeoutSec" "$f")" "30"
  expect "health check: HTTP /healthz on 8080" "$(q "$bc | .spec.healthCheck | .type + \" \" + .requestPath + \" \" + (.port | tostring)" "$f")" "HTTP /healthz 8080"
  expect "the health check port is the container's http port" "$(q 'select(.kind == "Deployment" and .metadata.name == "elitea-main") | .spec.template.spec.containers[0].ports[] | select(.name == "http") | .containerPort' "$f")" "8080"
  expect "Cloud Armor policy" "$(q "$bc | .spec.securityPolicy.name" "$f")" "edge-armor"
  fc='select(.kind == "FrontendConfig")'
  expect "one FrontendConfig, networking.gke.io/v1beta1" "$(q "[$fc | .apiVersion] | join(\",\")" "$f")" "networking.gke.io/v1beta1"
  expect "HTTPS redirect" "$(q "$fc | .spec.redirectToHttps.enabled" "$f")" "true"
  expect "SSL policy" "$(q "$fc | .spec.sslPolicy" "$f")" "modern-tls"
  expect "the Ingress is still one / rule to elitea-main" "$(q 'select(.kind == "Ingress") | [.spec.rules[].http.paths[] | .path + " " + .backend.service.name] | join(",")' "$f")" "/ elitea-main"
fi
if render on-defaults "${INGRESS[@]}" --set main.ingress.gke.enabled=true; then
  expect "no Cloud Armor policy by default" "$(q 'select(.kind == "BackendConfig") | .spec.securityPolicy // "none"' "$WORK/on-defaults.yaml")" "none"
  expect "no SSL policy by default" "$(q 'select(.kind == "FrontendConfig") | .spec.sslPolicy // "none"' "$WORK/on-defaults.yaml")" "none"
fi

# ── 3. className gce ─────────────────────────────────────────────────────────
if render classname "${INGRESS[@]}" --set main.ingress.className=gce; then
  f="$WORK/classname.yaml"
  expect "className gce turns the BackendConfig on" "$(count BackendConfig "$f")" "1"
  expect "className gce: no second class annotation" "$(ing_ann kubernetes.io/ingress.class "$f")" ""
  expect "className gce: ingressClassName kept" "$(q 'select(.kind == "Ingress") | .spec.ingressClassName' "$f")" "gce"
fi
if render annotation "${INGRESS[@]}" --set-string 'main.ingress.annotations.kubernetes\.io/ingress\.class=gce-internal'; then
  expect "the gce-internal annotation turns it on" "$(count FrontendConfig "$WORK/annotation.yaml")" "1"
  expect "the operator's class annotation is kept" "$(ing_ann kubernetes.io/ingress.class "$WORK/annotation.yaml")" "gce-internal"
fi

# ── 4. Bring your own objects ────────────────────────────────────────────────
if render own "${INGRESS[@]}" --set main.ingress.gke.enabled=true \
    --set main.ingress.gke.backendConfig.create=false --set main.ingress.gke.backendConfig.name=my-backend \
    --set main.ingress.gke.frontendConfig.create=false --set main.ingress.gke.frontendConfig.name=my-frontend \
    --set-string 'main.service.annotations.example\.invalid/team=edge'; then
  f="$WORK/own.yaml"
  expect "create=false renders no BackendConfig" "$(count BackendConfig "$f")" "0"
  expect "create=false renders no FrontendConfig" "$(count FrontendConfig "$f")" "0"
  expect "the Service names the operator's BackendConfig" "$(svc_ann cloud.google.com/backend-config "$f")" '{"ports":{"http":"my-backend"}}'
  expect "the Ingress names the operator's FrontendConfig" "$(ing_ann networking.gke.io/v1beta1.FrontendConfig "$f")" "my-frontend"
  expect "main.service.annotations merge in" "$(svc_ann example.invalid/team "$f")" "edge"
fi

# ── 5. TLS from cert-manager ─────────────────────────────────────────────────
if render tls "${INGRESS[@]}" --set main.ingress.gke.enabled=true \
    --set 'main.ingress.tls[0].secretName=elitea-origin-tls' --set 'main.ingress.tls[0].hosts[0]=elitea.example.invalid' \
    --set main.ingress.certManager.issuer=origin-ca --set main.ingress.certManager.issuerKind=OriginIssuer \
    --set main.ingress.certManager.issuerGroup=issuers.example.invalid; then
  f="$WORK/tls.yaml"
  expect "the TLS secret" "$(q 'select(.kind == "Ingress") | .spec.tls[0].secretName' "$f")" "elitea-origin-tls"
  expect "cert-manager issuer" "$(ing_ann cert-manager.io/issuer "$f")" "origin-ca"
  expect "cert-manager issuer kind" "$(ing_ann cert-manager.io/issuer-kind "$f")" "OriginIssuer"
  expect "cert-manager issuer group" "$(ing_ann cert-manager.io/issuer-group "$f")" "issuers.example.invalid"
fi
if render cluster-issuer "${INGRESS[@]}" --set main.ingress.certManager.clusterIssuer=letsencrypt; then
  expect "cert-manager cluster issuer" "$(ing_ann cert-manager.io/cluster-issuer "$WORK/cluster-issuer.yaml")" "letsencrypt"
fi

# ── 6. Refusals ──────────────────────────────────────────────────────────────
refuses "gke with gatewayApi" "main.ingress.gke.enabled=true needs main.ingress.gatewayApi=false" \
  --set main.ingress.enabled=true --set main.ingress.gatewayApi=true --set-string main.ingress.gateway.name=elitea \
  --set main.ingress.gke.enabled=true
refuses "issuer with clusterIssuer" "set issuer or clusterIssuer, not both" \
  "${INGRESS[@]}" --set main.ingress.certManager.issuer=a --set main.ingress.certManager.clusterIssuer=b

if [ "$failures" -ne 0 ]; then
  echo "render-gke-ingress.sh: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-gke-ingress.sh: all assertions passed"
