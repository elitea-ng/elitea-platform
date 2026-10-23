#!/usr/bin/env bash
# ADR-0034 decision 9: the workspace-run egress NetworkPolicy renders only
# when enabled, selects only labelled workspace-run Pods, denies every
# private, metadata and cluster range, and leaves the deny-all policy and
# the Code Pods' boundary unchanged.
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
chart=deploy/helm/elitea
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base=(-f "$chart/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
  --set sandboxKubernetes.enabled=true --set sandboxKubernetes.executionNamespace=code-execution)
on=(-f deploy/helm/tests/fixtures/cloud-executor-values.yaml --set sandboxKubernetes.workspaceRuns.enabled=true
  --set sandboxKubernetes.workspaceRuns.egress.enabled=true
  --set 'sandboxKubernetes.workspaceRuns.egress.podCidrs[0]=10.244.0.0/16'
  --set 'sandboxKubernetes.workspaceRuns.egress.serviceCidrs[0]=10.96.0.0/12')

# Network off (the default, and egress disabled with runs enabled): no policy.
helm template sandbox "$chart" "${base[@]}" > "$work/off.yaml"
helm template sandbox "$chart" "${base[@]}" -f deploy/helm/tests/fixtures/cloud-executor-values.yaml > "$work/off-runs.yaml"
# Network on.
helm template sandbox "$chart" "${base[@]}" "${on[@]}" > "$work/on.yaml"
python3 - "$work/off.yaml" "$work/off-runs.yaml" "$work/on.yaml" <<'PY'
import sys, yaml
def docs(path):
    return [d for d in yaml.safe_load_all(open(path)) if d]
def policies(all_docs):
    return {d['metadata']['name']: d for d in all_docs if d['kind'] == 'NetworkPolicy'
            and d['metadata'].get('namespace') == 'code-execution'}
for path in sys.argv[1:3]:
    found = policies(docs(path))
    assert 'elitea-workspace-run-egress' not in found, path
    assert found['elitea-code-deny-network']['spec'] == dict(
        podSelector={}, policyTypes=['Ingress', 'Egress'], ingress=[], egress=[]), path
found = policies(docs(sys.argv[3]))
deny = found['elitea-code-deny-network']['spec']
assert deny == dict(podSelector={}, policyTypes=['Ingress', 'Egress'], ingress=[], egress=[]), 'deny-all unchanged'
policy = found['elitea-workspace-run-egress']['spec']
assert policy['podSelector'] == {'matchLabels': {'sandbox.elitea.ai/kind': 'workspace-run',
                                                 'sandbox.elitea.ai/egress': 'public'}}
assert policy['policyTypes'] == ['Egress'], 'no ingress is opened'
dns, internet = policy['egress']
assert dns['to'][0]['namespaceSelector']['matchLabels'] == {'kubernetes.io/metadata.name': 'kube-system'}
assert dns['to'][0]['podSelector']['matchLabels'] == {'k8s-app': 'kube-dns'}
assert sorted((p['protocol'], p['port']) for p in dns['ports']) == [('TCP', 53), ('UDP', 53)]
v4 = internet['to'][0]['ipBlock']
assert v4['cidr'] == '0.0.0.0/0'
for cidr in ['10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16', '169.254.0.0/16', '127.0.0.0/8',
             '100.64.0.0/10', '10.244.0.0/16', '10.96.0.0/12']:
    assert cidr in v4['except'], cidr
v6 = internet['to'][1]['ipBlock']
assert v6['cidr'] == '::/0'
for cidr in ['fc00::/7', 'fe80::/10', '::1/128']:
    assert cidr in v6['except'], cidr
assert 'ports' not in internet, 'any port on the public internet'
PY
# Missing cluster ranges are refused, not rendered open.
if helm template sandbox "$chart" "${base[@]}" -f deploy/helm/tests/fixtures/cloud-executor-values.yaml \
    --set sandboxKubernetes.workspaceRuns.egress.enabled=true > "$work/invalid" 2>&1; then
  echo "workspace egress without the cluster ranges was accepted" >&2; exit 1
fi
grep -Fq 'podCidrs and serviceCidrs' "$work/invalid"
# The worker's workspace runs (ADR-0034): written to runtime.json for the Rust
# worker, with the audience Main signs grants for; refused otherwise.
cat > "$work/runs.yaml" <<'YAML'
main:
  runtime:
    sandboxAudiences: [dns:elitea-sandbox-workspace]
worker:
  enabled: true
  implementation: rust
  runtime:
    workspaceRuns:
      target: elitea-sandbox-workspace:9470
      audience: dns:elitea-sandbox-workspace
      scratch_root: /var/lib/elitea-worker/workspaces
      cpu_millis: 4000
      memory_bytes: 8589934592
      max_disk_bytes: 10737418240
      turn_wall_seconds: 3600
YAML
worker=(-f "$chart/values-standalone.yaml" -f "$work/runs.yaml"
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
  --set vector.enabled=true --set qdrant.enabled=true)
helm template test "$chart" "${worker[@]}" --show-only templates/worker/configmap-runtime.yaml > "$work/runs-config.yaml"
python3 - "$work/runs-config.yaml" <<'PY'
import json, sys, yaml
config = json.loads(yaml.safe_load(open(sys.argv[1]))['data']['runtime.json'])
runs = config['workspace_runs']
assert runs == {'target': 'elitea-sandbox-workspace:9470', 'audience': 'dns:elitea-sandbox-workspace',
                'scratch_root': '/var/lib/elitea-worker/workspaces', 'cpu_millis': 4000,
                'memory_bytes': 8589934592, 'max_disk_bytes': 10737418240,
                'turn_wall_seconds': 3600}, runs
PY
# Absent by default.
helm template test "$chart" -f "$chart/values-standalone.yaml" \
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1 \
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true \
  --set worker.enabled=true --set worker.implementation=rust --set vector.enabled=true --set qdrant.enabled=true \
  --show-only templates/worker/configmap-runtime.yaml > "$work/default-config.yaml"
! grep -q workspace_runs "$work/default-config.yaml"
# An audience Main does not sign for is refused.
if helm template test "$chart" "${worker[@]}" --set 'main.runtime.sandboxAudiences[0]=dns:other' \
    --show-only templates/worker/configmap-runtime.yaml > "$work/bad" 2>&1; then
  echo "a workspace-run audience Main does not sign for was accepted" >&2; exit 1
fi
grep -Fq 'must be in main.runtime.sandboxAudiences' "$work/bad"
# The supervisor may list Pods (the workspace-run sweep).
helm template sandbox "$chart" "${base[@]}" "${on[@]}" > "$work/rbac.yaml"
python3 - "$work/rbac.yaml" <<'PY'
import sys, yaml
roles = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d['kind'] == 'Role'
         and d['metadata']['name'] == 'elitea-code-supervisor']
assert roles, 'the supervisor role renders'
pods = [r for r in roles[0]['rules'] if r['resources'] == ['pods']][0]
assert 'list' in pods['verbs'], pods
PY
echo "workspace egress render: ok"
