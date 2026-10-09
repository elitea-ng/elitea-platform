#!/usr/bin/env bash
# render-gateway-master-key.sh — elitea-llm-gateway must be given SECRETS_MASTER_KEY.
#
# The gateway refuses to start without the key (cmd/elitea-llm-gateway/
# master_key_gate.go), unless the development opt-out is set — the rule
# elitea-main enforces too (render-main-master-key.sh). The chart's part:
#
#   * it hands the key to the Deployment from a REQUIRED Secret reference — the
#     one elitea-main also reads by default, so both carry one value;
#   * it refuses, while rendering, a values set that leaves the gateway with no
#     key source and no opt-out, and a key written as a plain env value.
#
# Every assertion reads the RENDERED Deployment.
#
# Usage: deploy/helm/tests/render-gateway-master-key.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
# The gateway's own guards (GATEWAY_SELF_LLM_ORIGINS, egressPosture) refuse a
# render that does not decide them, and guards.yaml demands the elitea-main
# ingress decision whenever network policies are on. These values settle them
# so every refusal below can only come from the master-key logic; each guard is
# asserted by its own script (render-llm-path.sh, render-network-policies.sh).
ONLY_GATEWAY=(--set main.enabled=false --set web.enabled=false --set scheduler.enabled=false
  --set otelCollector.enabled=false --set worker.enabled=false
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted
  --set networkPolicies.main.noExternalIngress=true)
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-gateway-master-key.sh needs $tool on PATH" >&2; exit 2; }
done

# render <out file> [helm args...]
render() {
  local out="$1"
  shift
  helm template test-release "$CHART" "${ONLY_GATEWAY[@]}" "$@" >"$out" 2>"$WORK/err.txt"
}

# masterKeyEnv <field> <file> — a field of the SECRETS_MASTER_KEY env entry on
# the elitea-llm-gateway Deployment's first container.
masterKeyEnv() {
  yq "select(.kind == \"Deployment\" and .metadata.name == \"elitea-llm-gateway\") | .spec.template.spec.containers[0].env[] | select(.name == \"SECRETS_MASTER_KEY\") | $1" "$2"
}

refuses() {
  local description="$1" expected="$2"
  shift 2
  local output
  if output="$(helm template test-release "$CHART" "${ONLY_GATEWAY[@]}" "$@" 2>&1)"; then
    fail "the chart rendered $description, and the gateway would then refuse to start"
  elif grep -qE "$expected" <<<"$output"; then
    pass "the chart refuses $description while it renders"
  else
    fail "the chart refuses $description but the message does not name '$expected':
$output"
  fi
}

# 1. Defaults: the key comes from the shared Secret, and it is required
# (optional: false) so a missing Secret stops the pod legibly
# (CreateContainerConfigError) instead of starting a crash-looping binary.
if render "$WORK/default.yaml"; then
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.name "$WORK/default.yaml")" = "elitea-llm-gateway-secrets" ] \
    && [ "$(masterKeyEnv .valueFrom.secretKeyRef.key "$WORK/default.yaml")" = "secrets-master-key" ] \
    && pass "the gateway reads SECRETS_MASTER_KEY from the shared Secret key" \
    || fail "the gateway does not read SECRETS_MASTER_KEY from the shared Secret key"
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.optional "$WORK/default.yaml")" = "false" ] \
    && pass "the default reference is not optional" \
    || fail "the default SECRETS_MASTER_KEY reference is optional; a missing Secret must stop the pod"
  [ "$(masterKeyEnv .name "$WORK/default.yaml" | grep -c 'SECRETS_MASTER_KEY')" = "1" ] \
    && pass "the reference is rendered once" \
    || fail "SECRETS_MASTER_KEY is rendered more or less than once"
  [ -z "$(masterKeyEnv .value "$WORK/default.yaml" | grep -v '^null$' || true)" ] \
    && ! yq 'select(.kind == "ConfigMap") | .data | has("SECRETS_MASTER_KEY")' "$WORK/default.yaml" | grep -q true \
    && pass "the key is never a plain value or ConfigMap entry" \
    || fail "SECRETS_MASTER_KEY is rendered as a plain value"
else
  fail "the default render failed: $(cat "$WORK/err.txt")"
fi

# 2. A stale `optional: true` on the entry does not loosen it: the opt-out
# alone decides.
if render "$WORK/stale-optional.yaml" --set llmGateway.secrets.SECRETS_MASTER_KEY.optional=true; then
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.optional "$WORK/stale-optional.yaml")" = "false" ] \
    && pass "an explicit optional: true without the opt-out is ignored" \
    || fail "an explicit optional: true made the reference optional without the opt-out"
else
  fail "a reference carrying optional: true was refused: $(cat "$WORK/err.txt")"
fi

# 3. A different Secret is honoured.
if render "$WORK/explicit.yaml" \
  --set llmGateway.secrets.SECRETS_MASTER_KEY.secretName=my-vault \
  --set llmGateway.secrets.SECRETS_MASTER_KEY.key=fernet; then
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.name "$WORK/explicit.yaml")" = "my-vault" ] \
    && [ "$(masterKeyEnv .valueFrom.secretKeyRef.key "$WORK/explicit.yaml")" = "fernet" ] \
    && pass "an operator-named Secret is honoured" \
    || fail "an operator-named Secret was not honoured"
else
  fail "an operator-named Secret was refused: $(cat "$WORK/err.txt")"
fi

# 4. The development opt-out keeps the reference but makes it optional, so a
# local install with no Secret still starts (the binary then warns).
if render "$WORK/optout.yaml" --set-string llmGateway.env.ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true; then
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.optional "$WORK/optout.yaml")" = "true" ] \
    && pass "with the development opt-out the reference is optional" \
    || fail "with the development opt-out the reference is still required"
  yq 'select(.kind == "Deployment" and .metadata.name == "elitea-llm-gateway") | .spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS") | .value' "$WORK/optout.yaml" | grep -qx true \
    && pass "the opt-out reaches the gateway container" \
    || fail "the opt-out does not reach the gateway container, which would then refuse to start"
else
  fail "the development opt-out was refused: $(cat "$WORK/err.txt")"
fi

# 5. Refusals.
refuses "a plaintext SECRETS_MASTER_KEY in llmGateway.env" 'llmGateway\.env\.SECRETS_MASTER_KEY is refused' \
  --set-string llmGateway.env.SECRETS_MASTER_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
refuses "a master key reference with no Secret name" 'needs both secretName and key' \
  --set llmGateway.secrets.SECRETS_MASTER_KEY.secretName=
refuses "a master key reference with no key" 'needs both secretName and key' \
  --set llmGateway.secrets.SECRETS_MASTER_KEY.key=
refuses "no master key source at all" 'elitea-llm-gateway has no SECRETS_MASTER_KEY source' \
  --set llmGateway.secrets.SECRETS_MASTER_KEY=null

# 6. With no key source the opt-out is the one way through.
if render "$WORK/nokey-optout.yaml" --set llmGateway.secrets.SECRETS_MASTER_KEY=null \
  --set-string llmGateway.env.ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true; then
  [ -z "$(masterKeyEnv .name "$WORK/nokey-optout.yaml")" ] \
    && pass "no key source plus the opt-out renders, with no key reference" \
    || fail "a key reference was rendered although none exists"
else
  fail "no key source plus the opt-out was refused: $(cat "$WORK/err.txt")"
fi

echo
if [ "$failures" -eq 0 ]; then
  echo "render-gateway-master-key: all checks passed"
else
  echo "render-gateway-master-key: $failures check(s) failed" >&2
  exit 1
fi
