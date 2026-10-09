#!/usr/bin/env bash
set -euo pipefail
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
YAML
args=(-f deploy/helm/elitea/values-standalone.yaml -f "$work/profiles.yaml"
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)
helm template test deploy/helm/elitea "${args[@]}" \
  --show-only templates/worker/configmap-runtime.yaml > "$work/config.yaml"
python3 - "$work/config.yaml" "$work/profiles.yaml" <<'PY'
import json,sys,yaml
config=json.loads(yaml.safe_load(open(sys.argv[1]))['data']['runtime.json'])
values=yaml.safe_load(open(sys.argv[2]))['worker']['runtime']
assert config['sandbox_runtimes']==values['sandboxRuntimes']
assert config['sandbox_runtimes'][0]['preparation']==values['sandboxRuntimes'][0]['preparation']
assert 'preparation' not in config['sandbox_runtimes'][1]
assert config['agent_model_checkpoint_recovery'] is True
assert config['agent_node_recovery'] is True
assert config['consumer_id'].endswith('__ELITEA_POD_NAME__')
PY
if helm template test deploy/helm/elitea "${args[@]}" --set worker.implementation=python > "$work/error" 2>&1; then
  echo 'Python worker silently accepted Rust-only runtime options' >&2
  exit 1
fi
grep -Fq 'require the Rust worker' "$work/error"
if helm template test deploy/helm/elitea "${args[@]}" --set main.runtime.codeOwnerRecovery.enabled=false \
  --set main.runtime.codeOwnerRecovery.mainWorkloadIdentity= --set 'main.runtime.codeOwnerRecovery.supervisors=null' \
  > "$work/error" 2>&1; then
  echo 'Node recovery with sandbox runtimes rendered without original-Code owner recovery' >&2
  exit 1
fi
grep -Fq 'Code nodes would be refused without original-Code owner recovery' "$work/error"
echo 'Worker sandbox and recovery rendering checks passed'
