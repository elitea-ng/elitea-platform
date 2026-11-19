#!/usr/bin/env bash
# The Microsoft Default app seed (ADR-0057 decision 5, issue 2267): `microsoftOAuthApps`
# renders ONE JSON env var that carries no secret, and one secretKeyRef env var per
# entry whose Secret is named, on elitea-main only. The default empty list renders
# nothing: no env var, so nothing is seeded and the credential forms offer only
# Custom app. Main reads the names back with microsoftapps.SeedSecretEnv, so the
# env var name rule here and there must agree.
#
# Needs: helm, yq. No cluster, no network.
set -euo pipefail
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
cd "$(dirname "$0")/../../.."

chart=deploy/helm/elitea
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base=(-f "$FEATURES_OFF" -f "$chart/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)

failures=0
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }
main_env='select(.kind == "Deployment" and .metadata.name == "elitea-main") | .spec.template.spec.containers[] | select(.name == "elitea-main") | .env[]'

echo "== default: nothing is seeded"
helm template t "$chart" "${base[@]}" > "$work/off.yaml"
[ "$(yq "$main_env | select(.name == \"ELITEA_MICROSOFT_OAUTH_APPS\") | .name" "$work/off.yaml")" = "" ] \
  || fail "the seed env var renders with the default empty list"
grep -q "ELITEA_MICROSOFT_OAUTH_APP_SECRET_" "$work/off.yaml" && fail "a secret env var renders with the default empty list"

cat > "$work/apps.yaml" <<'EOF'
microsoftOAuthApps:
  - key: corp-prod
    name: Company Microsoft
    clientId: 7f8d0c1e-0000-4000-8000-000000000001
    authority: https://login.microsoftonline.com/organizations
    credentialTypes: [outlook, teams]
    scopeCeiling:
      outlook: [Mail.Read, Mail.Send]
    secretExpiresAt: "2027-06-30"
    secret: {existingSecret: microsoft-app-corp, key: client-secret}
  - key: crm
    name: CRM
    clientId: 7f8d0c1e-0000-4000-8000-000000000002
    authority: https://login.microsoftonline.com/11111111-2222-4333-8444-555555555555
    credentialTypes: [dataverse]
EOF

echo "== two entries, one with a Secret"
helm template t "$chart" "${base[@]}" -f "$work/apps.yaml" > "$work/on.yaml"
json=$(yq "$main_env | select(.name == \"ELITEA_MICROSOFT_OAUTH_APPS\") | .value" "$work/on.yaml")
[ "$(printf '%s' "$json" | yq '. | length' -p=json)" = "2" ] || fail "the JSON does not hold both entries"
[ "$(printf '%s' "$json" | yq '.[0].key' -p=json)" = "corp-prod" ] || fail "the first entry's key is wrong"
[ "$(printf '%s' "$json" | yq '.[0].credentialTypes | join(",")' -p=json)" = "outlook,teams" ] || fail "credentialTypes were not rendered"
[ "$(printf '%s' "$json" | yq '.[0].scopeCeiling.outlook | join(",")' -p=json)" = "Mail.Read,Mail.Send" ] || fail "the scope ceiling was not rendered"
printf '%s' "$json" | grep -q "microsoft-app-corp" && fail "the Secret reference is in the JSON"
printf '%s' "$json" | grep -qi "client-secret" && fail "the Secret key is in the JSON"

secret="$main_env | select(.name == \"ELITEA_MICROSOFT_OAUTH_APP_SECRET_CORP_PROD\") | .valueFrom.secretKeyRef"
[ "$(yq "$secret | .name" "$work/on.yaml")" = "microsoft-app-corp" ] || fail "the secretKeyRef does not name the Secret"
[ "$(yq "$secret | .key" "$work/on.yaml")" = "client-secret" ] || fail "the secretKeyRef does not name the key"
[ "$(yq "$main_env | select(.name == \"ELITEA_MICROSOFT_OAUTH_APP_SECRET_CRM\") | .name" "$work/on.yaml")" = "" ] \
  || fail "an entry with no Secret got a secret env var"
others=$(yq 'select(.kind == "Deployment" and .metadata.name != "elitea-main") | .spec.template.spec.containers[].env[]? | select(.name | test("ELITEA_MICROSOFT_OAUTH_APP")) | .name' "$work/on.yaml" | grep -v "^---$" || true)
[ "$others" = "" ] || fail "another workload receives the seed"

echo "== a duplicate key is refused"
printf 'microsoftOAuthApps:\n  - {key: a, name: A}\n  - {key: a, name: B}\n' > "$work/dup.yaml"
if helm template t "$chart" "${base[@]}" -f "$work/dup.yaml" > /dev/null 2> "$work/dup.err"; then
  fail "a duplicate key rendered"
elif ! grep -q "listed twice" "$work/dup.err"; then
  fail "the duplicate key error does not say why"
fi

if [ "$failures" -gt 0 ]; then
  echo "render-microsoft-apps: $failures failure(s)" >&2
  exit 1
fi
echo "render-microsoft-apps: OK"
