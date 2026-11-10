#!/usr/bin/env bash
set -euo pipefail
# Every `helm template` here starts from the pre-S10 feature switches
# (fixtures/features-off.yaml: runtime, worker, vector, Qdrant, DeepWiki and
# Inventory off), prepended so that any values file or --set a render names
# wins; each render turns on what it tests. render-default-on.sh covers the
# shipped defaults.
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
_helm_bin="${HELM:-helm}"
helm() {
  if [ "${1:-}" = template ]; then shift; command "$_helm_bin" template -f "$FEATURES_OFF" "$@"
  else command "$_helm_bin" "$@"; fi
}
HELM=helm
cd "$(dirname "$0")/../../.."
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat > "$work/profiles.yaml" <<'YAML'
# The node attempt journal runs Code as original-Code visits, which Main admits
# only with original-Code owner recovery for the same supervisor audiences.
main:
  runtime:
    sandboxAudiences: [dns:sandbox-python, dns:sandbox-rust]
    codeOwnerRecovery:
      enabled: true
      mainWorkloadIdentity: spiffe://elitea.invalid/main
      supervisors:
        - audience: dns:sandbox-python
          httpsOrigin: https://sandbox-python:9446
        - audience: dns:sandbox-rust
          httpsOrigin: https://sandbox-rust:9447
worker:
  enabled: true
  implementation: rust
  runtime:
    agentModelCheckpointRecovery: true
    agentNodeRecovery: true
    sandboxRuntimes:
      - language: python
        target: sandbox-python:9446
        audience: dns:sandbox-python
        image_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        policy_revision: python-js-v1
        timeout_seconds: 120
        preparation:
          target: elitea-sandbox-preparation:9448
          audience: dns:elitea-sandbox-preparation
          image_digest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
          policy_revision: python-preparation-v1
          timeout_seconds: 120
      - language: rust
        target: sandbox-rust:9447
        audience: dns:sandbox-rust
        image_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
        policy_revision: rust-v1
        timeout_seconds: 120
        codeSlots: 4
YAML
args=(-f deploy/helm/elitea/values-standalone.yaml -f "$work/profiles.yaml"
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
  --set vector.enabled=true --set qdrant.enabled=true)
helm template test deploy/helm/elitea "${args[@]}" \
  --show-only templates/worker/configmap-runtime.yaml > "$work/config.yaml"
python3 - "$work/config.yaml" "$work/profiles.yaml" <<'PY'
import json,sys,yaml
config=json.loads(yaml.safe_load(open(sys.argv[1]))['data']['runtime.json'])
values=yaml.safe_load(open(sys.argv[2]))['worker']['runtime']
expected=[{k: v for k, v in entry.items() if k != 'codeSlots'} for entry in values['sandboxRuntimes']]
expected[1]['code_slots']=4
assert config['sandbox_runtimes']==expected, config['sandbox_runtimes']
assert config['sandbox_runtimes'][0]['preparation']==values['sandboxRuntimes'][0]['preparation']
assert 'preparation' not in config['sandbox_runtimes'][1]
# W3-1: codeSlots is written only when set, as an integer code_slots.
assert 'code_slots' not in config['sandbox_runtimes'][0]
assert type(config['sandbox_runtimes'][1]['code_slots']) is int
assert 'codeSlots' not in config['sandbox_runtimes'][1]
assert config['agent_model_checkpoint_recovery'] is True
assert config['agent_node_recovery'] is True
assert config['consumer_id'].endswith('__ELITEA_POD_NAME__')
PY
if helm template test deploy/helm/elitea "${args[@]}" --set worker.implementation=python > "$work/error" 2>&1; then
  echo 'the retired Python worker rendered' >&2
  exit 1
fi
grep -Fq 'is no longer supported' "$work/error"
# W3-1: codeSlots outside 1..1024, or not a whole number, refuses the render.
# (A values file, not --set-json on a list index, which helm v4 cannot coalesce.)
slots_case() {
  python3 - "$work/profiles.yaml" "$1" > "$work/slots.yaml" <<'PY'
import sys,yaml
values=yaml.safe_load(open(sys.argv[1]))
values['worker']['runtime']['sandboxRuntimes'][1]['codeSlots']=yaml.safe_load(sys.argv[2])
print(yaml.safe_dump({'worker':{'runtime':{'sandboxRuntimes':values['worker']['runtime']['sandboxRuntimes']}}}))
PY
}
for bad in 0 1025 2.5 '"four"'; do
  slots_case "$bad"
  if helm template test deploy/helm/elitea "${args[@]}" -f "$work/slots.yaml" \
    --show-only templates/worker/configmap-runtime.yaml > "$work/error" 2>&1; then
    echo "codeSlots=$bad rendered" >&2
    exit 1
  fi
  grep -Fq 'codeSlots must be a whole number from 1 to 1024' "$work/error"
done
slots_case 1024
helm template test deploy/helm/elitea "${args[@]}" -f "$work/slots.yaml" \
  --show-only templates/worker/configmap-runtime.yaml > "$work/edge.yaml"
python3 - "$work/edge.yaml" <<'PY'
import json,sys,yaml
config=json.loads(yaml.safe_load(open(sys.argv[1]))['data']['runtime.json'])
assert config['sandbox_runtimes'][1]['code_slots']==1024
PY'
import json,sys,yaml
config=json.loads(yaml.safe_load(open(sys.argv[1]))['data']['runtime.json'])
assert config['sandbox_runtimes'][1]['code_slots']==1024
PY
if helm template test deploy/helm/elitea "${args[@]}" --set main.runtime.codeOwnerRecovery.enabled=false \
  --set main.runtime.codeOwnerRecovery.mainWorkloadIdentity= --set 'main.runtime.codeOwnerRecovery.supervisors=null' \
  > "$work/error" 2>&1; then
  echo 'Node recovery with sandbox runtimes rendered without original-Code owner recovery' >&2
  exit 1
fi
grep -Fq 'Code nodes would be refused without original-Code owner recovery' "$work/error"
echo 'Worker sandbox and recovery rendering checks passed'
