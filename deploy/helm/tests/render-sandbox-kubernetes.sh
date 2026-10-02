#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
chart=deploy/helm/elitea
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base=(-f "$chart/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted)
helm template sandbox "$chart" "${base[@]}" > "$work/default.yaml"
helm template sandbox "$chart" "${base[@]}" \
  --set sandboxKubernetes.enabled=true \
  --set sandboxKubernetes.executionNamespace=code-execution \
  --show-only templates/sandbox/kubernetes-boundary.yaml > "$work/boundary.yaml"
python3 - "$work/default.yaml" "$work/boundary.yaml" <<'PY'
import sys, yaml
baseline = list(filter(None, yaml.safe_load_all(open(sys.argv[1]))))
assert not any(d.get('metadata', {}).get('name') == 'elitea-code-supervisor' for d in baseline)
docs = list(yaml.safe_load_all(open(sys.argv[2])))
assert len(docs) == 7
by_kind = {d['kind']: d for d in docs if d['kind'] != 'ServiceAccount'}
ns = by_kind['Namespace']
assert ns['metadata']['name'] == 'code-execution'
assert ns['metadata']['labels']['pod-security.kubernetes.io/enforce'] == 'restricted'
for kind in ['Namespace', 'NetworkPolicy', 'ResourceQuota']:
    assert by_kind[kind]['metadata']['annotations']['helm.sh/resource-policy'] == 'keep'
network = by_kind['NetworkPolicy']['spec']
assert network == dict(podSelector={}, policyTypes=['Ingress', 'Egress'], ingress=[], egress=[])
assert by_kind['ResourceQuota']['spec']['hard']['count/pods'] == '128'
accounts = {d['metadata']['name']: d for d in docs if d['kind'] == 'ServiceAccount'}
assert accounts['elitea-code']['automountServiceAccountToken'] is False
assert accounts['elitea-code']['metadata']['namespace'] == 'code-execution'
assert accounts['elitea-sandbox-supervisor']['metadata']['namespace'] == 'platform'
assert accounts['elitea-sandbox-supervisor']['automountServiceAccountToken'] is True
assert by_kind['Role']['rules'] == [
    dict(apiGroups=[''], resources=['pods'], verbs=['create', 'get', 'patch', 'delete']),
    dict(apiGroups=[''], resources=['pods/exec'], verbs=['create', 'get']),
    dict(apiGroups=[''], resources=['pods/log'], verbs=['get']),
]
assert by_kind['RoleBinding']['subjects'] == [dict(kind='ServiceAccount', name='elitea-sandbox-supervisor', namespace='platform')]
for doc in docs:
    assert doc['kind'] not in ['ClusterRole', 'ClusterRoleBinding']
    if doc['kind'] not in ['Namespace', 'ServiceAccount']:
        assert doc['metadata']['namespace'] == 'code-execution'
PY
for namespace in platform default kube-system ''; do
  if helm template sandbox "$chart" "${base[@]}" --set sandboxKubernetes.enabled=true \
      --set "sandboxKubernetes.executionNamespace=$namespace" > "$work/invalid" 2>&1; then
    echo "Unsafe namespace accepted: $namespace" >&2; exit 1
  fi
  rg -q 'sandboxKubernetes.executionNamespace' "$work/invalid"
done
for count in 0 4097 1.5; do
  if helm template sandbox "$chart" "${base[@]}" --set sandboxKubernetes.enabled=true \
      --set sandboxKubernetes.executionNamespace=code-execution \
      --set "sandboxKubernetes.maxPods=$count" > "$work/invalid" 2>&1; then
    echo "Invalid capacity accepted: $count" >&2; exit 1
  fi
  rg -q 'sandboxKubernetes.maxPods' "$work/invalid"
done
supervisor=(--set sandboxKubernetes.enabled=true
  --set sandboxKubernetes.executionNamespace=code-execution
  --set sandboxKubernetes.supervisor.enabled=true
  --set sandboxKubernetes.supervisor.materialSecret=sandbox-material
  --set sandboxKubernetes.supervisor.profiles[0].file=deno.json
  --set sandboxKubernetes.supervisor.profiles[0].port=9446
  --set sandboxKubernetes.supervisor.profiles[1].file=rust.json
  --set sandboxKubernetes.supervisor.profiles[1].port=9447
  --set sandboxKubernetes.supervisor.image=registry/supervisor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa)
helm template sandbox "$chart" "${base[@]}" "${supervisor[@]}" \
  --show-only templates/sandbox/supervisor.yaml > "$work/supervisor.yaml"
python3 - "$work/supervisor.yaml" <<'PYTEST'
import sys, yaml
docs = {d['kind']:d for d in yaml.safe_load_all(open(sys.argv[1]))}
deployment, service = docs['Deployment'], docs['Service']
assert deployment['spec']['replicas'] == 1
pod = deployment['spec']['template']['spec']
assert pod['serviceAccountName'] == 'elitea-sandbox-supervisor'
assert pod['securityContext']['runAsUser'] == 10001
assert len(pod['containers']) == 1
init, runtime = pod['initContainers'][0], pod['containers'][0]
assert init['image'] == runtime['image']
assert init['args'] == ['--prepare-material','/run/projected-material','/run/elitea-sandbox']
assert runtime['args'] == ['--config','/run/elitea-sandbox/deno.json','--config','/run/elitea-sandbox/rust.json']
assert runtime['volumeMounts'] == [{'name':'private-material','mountPath':'/run/elitea-sandbox','readOnly':True}]
for container in [init, runtime]:
    assert container['securityContext']['allowPrivilegeEscalation'] is False
    assert container['securityContext']['readOnlyRootFilesystem'] is True
    assert container['securityContext']['capabilities']['drop'] == ['ALL']
assert not any('hostPath' in v for v in pod['volumes'])
assert pod['volumes'][0]['secret']['defaultMode'] == 0o440
assert pod['volumes'][1]['emptyDir']['medium'] == 'Memory'
assert service['spec']['type'] == 'ClusterIP'
assert [p['port'] for p in service['spec']['ports']] == [9446,9447]
PYTEST
for invalid in 'sandboxKubernetes.supervisor.image=runtime:latest' \
    'sandboxKubernetes.supervisor.profiles[1].port=9446' \
    'sandboxKubernetes.supervisor.profiles[0].file=../escape' \
    'sandboxKubernetes.enabled=false'; do
  if helm template sandbox "$chart" "${base[@]}" "${supervisor[@]}" --set "$invalid" > "$work/invalid" 2>&1; then
    echo "Unsafe supervisor configuration accepted: $invalid" >&2; exit 1
  fi
  rg -q 'Kubernetes' "$work/invalid"
done
echo 'Kubernetes sandbox boundary rendering checks passed'
