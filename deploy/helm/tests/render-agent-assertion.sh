#!/usr/bin/env bash
# The Elitea-native agent assertion keys (#1153) render as a whole-directory
# Secret volume on Main only, with the egress signing key's mode (0444: the
# pod sets no fsGroup and the kubelet's files are root-owned), and render
# nothing when main.agentAssertion.keysSecret is empty. Main reads the files
# through the kubelet's symlinks (eliteanative.ReadMountedKey).
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
main_pod='select(.kind == "Deployment" and .metadata.name == "elitea-main") | .spec.template.spec'

echo "== off (the default)"
helm template t "$chart" "${base[@]}" > "$work/off.yaml"
[ "$(yq "$main_pod | .volumes[] | select(.name == \"agent-assertion-keys\") | .name" "$work/off.yaml")" = "" ] \
  || fail "a keys volume renders with keysSecret empty"

echo "== on"
helm template t "$chart" "${base[@]}" --set main.agentAssertion.keysSecret=agent-keys > "$work/on.yaml"
volume="$main_pod | .volumes[] | select(.name == \"agent-assertion-keys\")"
[ "$(yq "$volume | .secret.secretName" "$work/on.yaml")" = "agent-keys" ] || fail "the volume does not name the Secret"
[ "$(yq "$volume | .secret.defaultMode" "$work/on.yaml")" = "292" ] || [ "$(yq "$volume | .secret.defaultMode" "$work/on.yaml")" = "0444" ] \
  || fail "defaultMode is not 0444"
[ "$(yq "$volume | .secret.items" "$work/on.yaml")" = "null" ] || fail "the volume projects items; every key of the Secret should mount"
mount="$main_pod | .containers[] | select(.name == \"elitea-main\") | .volumeMounts[] | select(.name == \"agent-assertion-keys\")"
[ "$(yq "$mount | .mountPath" "$work/on.yaml")" = "/etc/elitea/agent-assertion" ] || fail "the mount path is not the default"
[ "$(yq "$mount | .readOnly" "$work/on.yaml")" = "true" ] || fail "the mount is writable"
[ "$(yq "$mount | .subPath" "$work/on.yaml")" = "null" ] || fail "the mount uses subPath"
others=$(yq 'select(.kind == "Deployment" and .metadata.name != "elitea-main") | (.spec.template.spec.volumes // [])[] | select(.name == "agent-assertion-keys") | .name' "$work/on.yaml" | grep -v "^---$" || true)
[ "$others" = "" ] || fail "another workload mounts the keys"

if [ "$failures" -gt 0 ]; then
  echo "render-agent-assertion: $failures failure(s)" >&2
  exit 1
fi
echo "render-agent-assertion: OK"
