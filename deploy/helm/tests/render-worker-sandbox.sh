#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat > "$work/profiles.yaml" <<'YAML'
# An explicit list replaces the derived Code backends. The node attempt journal
# (always on) runs Code as original-Code visits, which Main admits only with
# original-Code owner recovery for the same supervisor audiences; a preparation
# audience needs only to be a sandbox audience.
main:
  runtime:
    sandboxAudiences: [dns:sandbox-python, dns:sandbox-rust, dns:elitea-sandbox-preparation]
    codeOwnerRecovery:
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
args=(-f deploy/helm/elitea/values-standalone.yaml -f deploy/helm/tests/fixtures/code-material-values.yaml -f "$work/profiles.yaml"
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
  --set vector.enabled=true --set main.autoscaling.maxReplicas=3
  --set vector.qdrant.apiKeySecret.name=elitea-qdrant-api-key)
helm template test deploy/helm/elitea "${args[@]}" \
  --show-only templates/worker/configmap-runtime.yaml > "$work/config.yaml"
python3 - "$work/config.yaml" "$work/profiles.yaml" <<'PY'
import json,sys,yaml
config=json.loads(yaml.safe_load(open(sys.argv[1]))['data']['runtime.json'])
values=yaml.safe_load(open(sys.argv[2]))['worker']['runtime']
assert config['sandbox_runtimes']==values['sandboxRuntimes']
assert config['sandbox_runtimes'][0]['preparation']==values['sandboxRuntimes'][0]['preparation']
assert 'preparation' not in config['sandbox_runtimes'][1]
# Recovery is not a setting: never written, always on in the worker.
assert 'agent_model_checkpoint_recovery' not in config and 'agent_node_recovery' not in config
assert config['agent_checkpoint_connection_path'] == '/run/elitea-runtime/agent-checkpoint-connection'
assert config['consumer_id'].endswith('__ELITEA_POD_NAME__')
PY
if helm template test deploy/helm/elitea "${args[@]}" --set worker.implementation=python > "$work/error" 2>&1; then
  echo 'the retired Python worker rendered' >&2
  exit 1
fi
grep -Fq 'is no longer supported' "$work/error"
# Owner recovery is always on with Agent dispatch (no enable bit); without
# Agent dispatch the worker's explicit Code backends are refused.
if helm template test deploy/helm/elitea "${args[@]}" \
  --set main.runtime.codeOwnerRecovery.mainWorkloadIdentity= --set 'main.runtime.codeOwnerRecovery.supervisors=null' \
  --set main.runtime.agentExecutionDispatch.enabled=false --set-string main.runtime.agentExecutionDispatch.commandStream= \
  > "$work/error" 2>&1; then
  echo 'Node recovery with sandbox runtimes rendered without original-Code owner recovery' >&2
  exit 1
fi
grep -Fq 'Code nodes would be refused without original-Code owner recovery' "$work/error"
if helm template test deploy/helm/elitea "${args[@]}" \
  --set-json 'main.runtime.sandboxAudiences=["dns:sandbox-python","dns:sandbox-rust"]' > "$work/error" 2>&1; then
  echo 'A preparation audience Main does not sign for rendered' >&2
  exit 1
fi
grep -Fq 'preparation audience "dns:elitea-sandbox-preparation" is not in main.runtime.sandboxAudiences' "$work/error"
echo 'Worker sandbox and recovery rendering checks passed'
