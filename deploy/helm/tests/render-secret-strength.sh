#!/usr/bin/env bash
# render-secret-strength.sh — the chart refuses weak signing secrets (#2958).
#
# The services refuse an empty, short, placeholder or burned signing secret
# at start-up (libs/go/secretstrength). The chart applies the same rule to a
# value written into an env map, while it renders
# (templates/_secret-strength.tpl), and it refuses the vault-key development
# opt-out next to a sign-in configuration (elitea-main.masterKeyRef). Every
# refusal names the variable and never the value.
#
# Usage: deploy/helm/tests/render-secret-strength.sh
# Needs: helm. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
# The same reduced install render-main-master-key.sh uses.
ONLY_MAIN=(-f "$REPO/deploy/helm/tests/fixtures/features-off.yaml"
  --set web.enabled=false --set scheduler.enabled=false --set llmGateway.enabled=false
  --set otelCollector.enabled=false --set worker.enabled=false
  --set networkPolicies.main.noExternalIngress=true)
BASE=("${ONLY_MAIN[@]}")

command -v helm >/dev/null 2>&1 || { echo "render-secret-strength.sh needs helm on PATH" >&2; exit 2; }

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

# A strong value: 32 random bytes, base64.
STRONG="$(openssl rand -base64 32)"

# refuses <description> <message fragment> <value that must not appear> [helm args...]
refuses() {
  local description="$1" expected="$2" secret="$3"
  shift 3
  local output
  if output="$(helm template test-release "$CHART" "${BASE[@]}" "$@" 2>&1)"; then
    fail "$description: the render succeeded"
    return
  fi
  if ! grep -qF -- "$expected" <<<"$output"; then
    fail "$description: the error does not say \"$expected\": $(head -c 400 <<<"$output")"
    return
  fi
  if [ -n "$secret" ] && grep -qF -- "$secret" <<<"$output"; then
    fail "$description: the error prints the value"
    return
  fi
  pass "$description"
}

renders() {
  local description="$1"
  shift
  local output
  if output="$(helm template test-release "$CHART" "${BASE[@]}" "$@" 2>&1)"; then
    pass "$description"
  else
    fail "$description: $(head -c 400 <<<"$output")"
  fi
}

refuses "a placeholder APPLICATION_SECRET_KEY" "main.env.APPLICATION_SECRET_KEY is a well-known placeholder" "" \
  --set-string main.env.APPLICATION_SECRET_KEY=changeme
refuses "a placeholder marker inside a long value" "main.env.APPLICATION_SECRET_KEY contains a placeholder marker" "changeme-but-padded-to-more-than-32-bytes" \
  --set-string main.env.APPLICATION_SECRET_KEY=changeme-but-padded-to-more-than-32-bytes
refuses "a 10-byte APPLICATION_SECRET_KEY" "main.env.APPLICATION_SECRET_KEY is shorter than 32 bytes" "0123456789" \
  --set-string main.env.APPLICATION_SECRET_KEY=0123456789
refuses "a blank APPLICATION_SECRET_KEY" "main.env.APPLICATION_SECRET_KEY is empty or blank" "" \
  --set-string "main.env.APPLICATION_SECRET_KEY=   "
refuses "a value once committed to this repository" "main.env.APPLICATION_SECRET_KEY holds a value that was published in this repository" "staging-secret-for-local-dev" \
  --set-string main.env.APPLICATION_SECRET_KEY=staging-secret-for-local-dev
refuses "a short DeepWiki identity secret on the provider" "deepwiki.env.ELITEA_DEEPWIKI_IDENTITY_SECRET is shorter than 32 bytes" "short-identity" \
  --set-string deepwiki.env.ELITEA_DEEPWIKI_IDENTITY_SECRET=short-identity
refuses "a short gateway hop secret" "llmGateway.env.GATEWAY_HOP_SECRET is shorter than 32 bytes" "short-hop" \
  --set-string llmGateway.env.GATEWAY_HOP_SECRET=short-hop
refuses "a short value padded with spaces" "main.env.APPLICATION_SECRET_KEY is shorter than 32 bytes" "" \
  --set-string "main.env.APPLICATION_SECRET_KEY=   abcdefghijklmnopqrstuvwxyz012"
refuses "a long value of one repeated character" "main.env.APPLICATION_SECRET_KEY repeats too few characters" "" \
  --set-string "main.env.APPLICATION_SECRET_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
renders "an exactly empty optional value renders (unset, as the services read it)" --set-string "main.env.APPLICATION_SECRET_KEY="
renders "a strong APPLICATION_SECRET_KEY renders" --set-string "main.env.APPLICATION_SECRET_KEY=${STRONG}"
renders "no signing secret in env renders (the Secret references carry them)"

# The vault-key opt-out next to a sign-in configuration (#2958).
refuses "the unwrapped opt-out next to OIDC_ISSUER_URL" "ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=\"true\" is refused next to a sign-in configuration" "" \
  --set-string main.env.ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true \
  --set-string main.env.OIDC_ISSUER_URL=https://idp.example.invalid
# The kind values are a whole install of their own (Form authentication,
# the runtime plane, DeepWiki and Inventory), so they render on their own.
BASE=(-f "$REPO/deploy/kind/values-kind.yaml")
refuses "the unwrapped opt-out next to the Form auth configuration" "ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=\"true\" is refused next to a sign-in configuration" "" \
  --set-string main.env.ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true
renders "the kind values render (generated Secrets, no opt-out)"
# An enabled provider needs its identity secret on both sides (#2959).
refuses "DeepWiki on with no identity secret on elitea-main" "deepwiki.enabled needs ELITEA_DEEPWIKI_IDENTITY_SECRET on main" "" \
  --set main.secrets.ELITEA_DEEPWIKI_IDENTITY_SECRET=null
refuses "Inventory on with no identity secret on the host" "inventory.enabled needs ELITEA_INVENTORY_IDENTITY_SECRET on inventory" "" \
  --set inventory.secrets.ELITEA_INVENTORY_IDENTITY_SECRET=null

if [ "$failures" -gt 0 ]; then
  echo "render-secret-strength: ${failures} check(s) failed" >&2
  exit 1
fi
echo
echo "render-secret-strength: all checks passed"
