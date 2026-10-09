#!/usr/bin/env bash
# render-main-master-key.sh — elitea-main must be given SECRETS_MASTER_KEY.
#
# elitea-main refuses to start without the key (cmd/elitea-main/
# master_key_gate.go), unless the development opt-out is set. The chart's part:
#
#   * it hands the key to the Deployment from a Secret reference — by default
#     the one the LLM gateway reads, because both services read the same
#     centry.secrets_key rows and must carry the SAME value;
#   * it refuses, while rendering, a values set that leaves Main with no key
#     source and no opt-out, and a key written as plaintext into the ConfigMap.
#
# Every assertion reads the RENDERED Deployment.
#
# Usage: deploy/helm/tests/render-main-master-key.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
# noExternalIngress=true is the decision guards.yaml demands of every render
# with networkPolicies.enabled (the default) about who may reach elitea-main.
# Without it every render stops at that guard before any master-key logic runs.
# It changes only elitea-main's NetworkPolicy, never its env; the guard itself
# is asserted by render-network-policies.sh.
ONLY_MAIN=(--set web.enabled=false --set scheduler.enabled=false --set llmGateway.enabled=false
  --set otelCollector.enabled=false --set worker.enabled=false
  --set networkPolicies.main.noExternalIngress=true)
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-main-master-key.sh needs $tool on PATH" >&2; exit 2; }
done

# render <out file> [helm args...]
render() {
  local out="$1"
  shift
  helm template test-release "$CHART" "${ONLY_MAIN[@]}" "$@" >"$out" 2>"$WORK/err.txt"
}

# masterKeyEnv <field> <file> — a field of the SECRETS_MASTER_KEY env entry on
# the elitea-main Deployment's first container.
masterKeyEnv() {
  yq "select(.kind == \"Deployment\" and .metadata.name == \"elitea-main\") | .spec.template.spec.containers[0].env[] | select(.name == \"SECRETS_MASTER_KEY\") | $1" "$2"
}

refuses() {
  local description="$1" expected="$2"
  shift 2
  local output
  if output="$(helm template test-release "$CHART" "${ONLY_MAIN[@]}" "$@" 2>&1)"; then
    fail "the chart rendered $description, and elitea-main would then refuse to start"
  elif grep -q 'networkPolicies\.' <<<"$output"; then
    # A network-policy guard stopped the render before the master-key guard
    # ran, so this refusal proves nothing about the master key.
    fail "the chart refuses $description, but at a network-policy guard:
$output"
  elif grep -qE "$expected" <<<"$output"; then
    pass "the chart refuses $description while it renders"
  else
    fail "the chart refuses $description but the message does not name '$expected':
$output"
  fi
}

# 1. Defaults: the key comes from the Secret the gateway reads, and it is
# required (optional: false) so a missing Secret stops the pod instead of
# starting a service that cannot store keys.
if render "$WORK/default.yaml"; then
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.name "$WORK/default.yaml")" = "elitea-llm-gateway-secrets" ] \
    && [ "$(masterKeyEnv .valueFrom.secretKeyRef.key "$WORK/default.yaml")" = "secrets-master-key" ] \
    && pass "elitea-main reads SECRETS_MASTER_KEY from the Secret key the gateway reads" \
    || fail "elitea-main does not read SECRETS_MASTER_KEY from the gateway's Secret key"
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.optional "$WORK/default.yaml")" = "false" ] \
    && pass "the default reference is not optional" \
    || fail "the default SECRETS_MASTER_KEY reference is optional; a missing Secret must stop the pod"
  if [ "$(grep -c 'SECRETS_MASTER_KEY' "$WORK/default.yaml")" -ge 1 ] \
    && ! yq 'select(.kind == "ConfigMap") | .data | has("SECRETS_MASTER_KEY")' "$WORK/default.yaml" | grep -q true; then
    pass "the key is not rendered into a ConfigMap"
  else
    fail "SECRETS_MASTER_KEY appears in a ConfigMap"
  fi
else
  fail "the default render failed: $(cat "$WORK/err.txt")"
fi

# 2. An explicit main.secrets entry wins and is taken whole.
if render "$WORK/explicit.yaml" \
  --set main.secrets.SECRETS_MASTER_KEY.secretName=my-vault \
  --set main.secrets.SECRETS_MASTER_KEY.key=fernet; then
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.name "$WORK/explicit.yaml")" = "my-vault" ] \
    && [ "$(masterKeyEnv .valueFrom.secretKeyRef.key "$WORK/explicit.yaml")" = "fernet" ] \
    && [ "$(masterKeyEnv . "$WORK/explicit.yaml" | grep -c 'name: SECRETS_MASTER_KEY')" = "1" ] \
    && pass "an explicit main.secrets.SECRETS_MASTER_KEY wins, once" \
    || fail "an explicit main.secrets.SECRETS_MASTER_KEY did not win"
else
  fail "an explicit key reference was refused: $(cat "$WORK/err.txt")"
fi

# 3. The development opt-out keeps the reference but makes it optional, so a
# local install with no Secret still starts (the binary then warns).
if render "$WORK/optout.yaml" --set-string main.env.ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true; then
  [ "$(masterKeyEnv .valueFrom.secretKeyRef.optional "$WORK/optout.yaml")" = "true" ] \
    && pass "with the development opt-out the reference is optional" \
    || fail "with the development opt-out the reference is still required"
else
  fail "the development opt-out was refused: $(cat "$WORK/err.txt")"
fi

# 4. Refusals.
refuses "a plaintext SECRETS_MASTER_KEY in main.env" 'main\.env\.SECRETS_MASTER_KEY' \
  --set-string main.env.SECRETS_MASTER_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
refuses "a master key reference with no Secret name" 'needs both secretName and key' \
  --set main.secrets.SECRETS_MASTER_KEY.secretName= --set main.secrets.SECRETS_MASTER_KEY.key=k
refuses "no master key source at all" 'has no SECRETS_MASTER_KEY source' \
  --set llmGateway.secrets.SECRETS_MASTER_KEY=null

# 5. With no key source the opt-out is the one way through.
if render "$WORK/nokey-optout.yaml" --set llmGateway.secrets.SECRETS_MASTER_KEY=null \
  --set-string main.env.ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true; then
  [ -z "$(masterKeyEnv .name "$WORK/nokey-optout.yaml")" ] \
    && pass "no key source plus the opt-out renders, with no key reference" \
    || fail "a key reference was rendered although none exists"
else
  fail "no key source plus the opt-out was refused: $(cat "$WORK/err.txt")"
fi

echo
if [ "$failures" -eq 0 ]; then
  echo "render-main-master-key: all checks passed"
else
  echo "render-main-master-key: $failures check(s) failed" >&2
  exit 1
fi
