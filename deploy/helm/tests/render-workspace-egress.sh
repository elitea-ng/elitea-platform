#!/usr/bin/env bash
# ADR-0034 decision 9: the workspace-run egress NetworkPolicy renders only
# when enabled, selects only labelled workspace-run Pods, denies every
# private, metadata and cluster range, and leaves the deny-all policy and
# the Code Pods' boundary unchanged.
set -euo pipefail
cd "$(dirname "$0")/../../.."
chart=deploy/helm/elitea
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base=(-f "$chart/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
  --set sandboxKubernetes.enabled=true --set sandboxKubernetes.executionNamespace=code-execution)
on=(--set sandboxKubernetes.workspaceRuns.enabled=true
  --set sandboxKubernetes.workspaceRuns.egress.enabled=true
  --set 'sandboxKubernetes.workspaceRuns.egress.podCidrs[0]=10.244.0.0/16'
  --set 'sandboxKubernetes.workspaceRuns.egress.serviceCidrs[0]=10.96.0.0/12')

# Network off (the default, and egress disabled with runs enabled): no policy.
helm template sandbox "$chart" "${base[@]}" > "$work/off.yaml"
helm template sandbox "$chart" "${base[@]}" --set sandboxKubernetes.workspaceRuns.enabled=true > "$work/off-runs.yaml"
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
if helm template sandbox "$chart" "${base[@]}" --set sandboxKubernetes.workspaceRuns.enabled=true \
    --set sandboxKubernetes.workspaceRuns.egress.enabled=true > "$work/invalid" 2>&1; then
  echo "workspace egress without the cluster ranges was accepted" >&2; exit 1
fi
grep -Fq 'podCidrs and serviceCidrs' "$work/invalid"
echo "workspace egress render: ok"
