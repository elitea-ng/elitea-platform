#!/usr/bin/env bash
# render-security-context.sh — elitea-web, elitea-scheduler and otel-collector
# carry a pod and a container securityContext.
#
# These three templates rendered none, so the containers inherited the
# runtime default allowPrivilegeEscalation=true, which GKE Security Posture
# (and any restricted Pod Security admission) reports as "Pod container
# allows privilege escalation on exec". The values come from
# <component>.podSecurityContext / <component>.securityContext, the same keys
# main, worker, inventory and pgbouncer already read.
#
# What is asserted:
#   * the shipped defaults set allowPrivilegeEscalation=false and a
#     RuntimeDefault seccomp profile on all three;
#   * elitea-scheduler and otel-collector also drop ALL capabilities, run as
#     non-root and with a read-only root filesystem, and so does elitea-web
#     (#2954: unprivileged nginx on 8080, adding no capability back, with
#     an emptyDir at /tmp);
#   * otelCollector.extraVolumes / extraVolumeMounts reach the pod, so a file
#     exporter can get a writable path under the read-only root;
#   * an operator value reaches the rendered pod, and null renders no
#     securityContext at all (the key is opt-out, not mandatory).
#
# Usage: deploy/helm/tests/render-security-context.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
_helm_bin="${HELM:-helm}"
helm() {
  if [ "${1:-}" = template ]; then shift; command "$_helm_bin" template -f "$FEATURES_OFF" "$@"
  else command "$_helm_bin" "$@"; fi
}

CHART="deploy/helm/elitea"
GATEWAY_POSTURES="--set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1"

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

render() { helm template test "$CHART" $GATEWAY_POSTURES "$@" 2>&1; }

select_one() {
  # manifest, kind, name, expression
  printf '%s' "$1" | yq eval-all "select(.kind == \"$2\" and .metadata.name == \"$3\") | $4" -
}

expect() {
  # description, got, want
  if [ "$2" = "$3" ]; then note "$1"; else fail "$1: got '$2', want '$3'"; fi
}

echo "== shipped defaults"
out="$(render)"

for d in elitea-web elitea-scheduler elitea-otel-collector; do
  expect "$d: allowPrivilegeEscalation=false" \
    "$(select_one "$out" Deployment "$d" '.spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation')" "false"
  expect "$d: seccomp RuntimeDefault" \
    "$(select_one "$out" Deployment "$d" '.spec.template.spec.securityContext.seccompProfile.type')" "RuntimeDefault"
done

for d in elitea-web elitea-scheduler elitea-otel-collector; do
  expect "$d: drops ALL capabilities" \
    "$(select_one "$out" Deployment "$d" '.spec.template.spec.containers[0].securityContext.capabilities.drop | join(",")')" "ALL"
  expect "$d: read-only root filesystem" \
    "$(select_one "$out" Deployment "$d" '.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem')" "true"
  expect "$d: runAsNonRoot" \
    "$(select_one "$out" Deployment "$d" '.spec.template.spec.securityContext.runAsNonRoot')" "true"
done

# The image runs nginx as its `nginx` user on 8080 (#2954): nothing is added
# back, and /tmp is the one writable path.
expect "elitea-web: adds back no capability" \
  "$(select_one "$out" Deployment elitea-web '.spec.template.spec.containers[0].securityContext.capabilities.add // "none"')" "none"
expect "elitea-web: listens on the unprivileged port" \
  "$(select_one "$out" Deployment elitea-web '.spec.template.spec.containers[0].ports[0].containerPort')" "8080"
expect "elitea-web: /tmp is an emptyDir" \
  "$(select_one "$out" Deployment elitea-web '.spec.template.spec.volumes[] | select(.name == "tmp") | has("emptyDir")')" "true"
expect "elitea-web: /tmp is mounted" \
  "$(select_one "$out" Deployment elitea-web '.spec.template.spec.containers[0].volumeMounts[] | select(.name == "tmp") | .mountPath')" "/tmp"

echo "== operator values"
out="$(render --set web.securityContext.runAsUser=101 --set scheduler.podSecurityContext.fsGroup=65532)"
expect "web.securityContext reaches the container" \
  "$(select_one "$out" Deployment elitea-web '.spec.template.spec.containers[0].securityContext.runAsUser')" "101"
expect "scheduler.podSecurityContext reaches the pod" \
  "$(select_one "$out" Deployment elitea-scheduler '.spec.template.spec.securityContext.fsGroup')" "65532"

echo "== otel-collector extra volumes"
out="$(render --set 'otelCollector.extraVolumes[0].name=otel-data' --set 'otelCollector.extraVolumes[0].emptyDir.medium=' \
  --set 'otelCollector.extraVolumeMounts[0].name=otel-data' --set 'otelCollector.extraVolumeMounts[0].mountPath=/var/lib/otelcol')"
expect "otel-collector: extra volume reaches the pod" \
  "$(select_one "$out" Deployment elitea-otel-collector '.spec.template.spec.volumes[] | select(.name == "otel-data") | has("emptyDir")')" "true"
expect "otel-collector: extra mount reaches the container" \
  "$(select_one "$out" Deployment elitea-otel-collector '.spec.template.spec.containers[0].volumeMounts[] | select(.name == "otel-data") | .mountPath')" "/var/lib/otelcol"
expect "otel-collector: the config mount stays" \
  "$(select_one "$out" Deployment elitea-otel-collector '.spec.template.spec.containers[0].volumeMounts[0].name')" "config"

# Helm deep-merges maps, so `{}` keeps the defaults; null removes the key.
echo "== a null value renders no securityContext"
out="$(render --set otelCollector.securityContext=null --set otelCollector.podSecurityContext=null)"
expect "otel-collector: no container securityContext" \
  "$(select_one "$out" Deployment elitea-otel-collector '.spec.template.spec.containers[0] | has("securityContext")')" "false"
expect "otel-collector: no pod securityContext" \
  "$(select_one "$out" Deployment elitea-otel-collector '.spec.template.spec | has("securityContext")')" "false"

if [ "$failures" -ne 0 ]; then
  echo "render-security-context: $failures failure(s)" >&2
  exit 1
fi
echo "render-security-context: OK"
