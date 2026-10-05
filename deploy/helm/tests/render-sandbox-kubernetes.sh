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
assert not any(d.get('metadata', {}).get('name') == 'elitea-python-preparation-network' for d in baseline)
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
cat > "$work/preparation-values.yaml" <<'YAML'
sandboxKubernetes:
  enabled: true
  executionNamespace: code-execution
  preparation:
    enabled: true
    namespace: python-preparation
    resolverCidrs: [203.0.113.0/24, "2001:db8::/64"]
  supervisor:
    enabled: true
    image: registry/supervisor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    materialSecret: sandbox-material
    profiles:
      - file: deno.json
        port: 9446
        serviceName: elitea-sandbox-deno
      - file: rust.json
        port: 9447
        serviceName: elitea-sandbox-rust
      - file: preparation.json
        port: 9448
        serviceName: elitea-sandbox-preparation
    contentStaging:
      - {name: deno, sizeMiB: 256}
      - {name: preparation, sizeMiB: 256}
    contentStagingInitImage: registry/staging-init@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    resources:
      limits: {cpu: "1", memory: 1Gi}
sandboxImageWarmup:
  enabled: true
  nodeSelector: {elitea.ai/sandbox: "true"}
  images:
    - name: deno
      image: registry/deno@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
    - name: preparation
      image: registry/preparation@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
YAML
helm template sandbox "$chart" "${base[@]}" -f "$work/preparation-values.yaml" \
  --show-only templates/sandbox/kubernetes-boundary.yaml \
  --show-only templates/sandbox/preparation-boundary.yaml \
  --show-only templates/sandbox/supervisor.yaml \
  --show-only templates/sandbox/image-warmup.yaml > "$work/preparation.yaml"
python3 - "$work/preparation.yaml" <<'PYTEST'
import sys, yaml
from collections import Counter
docs = list(filter(None, yaml.safe_load_all(open(sys.argv[1]))))
execution = {d['kind']: d for d in docs if d.get('metadata', {}).get('namespace') == 'code-execution'}
assert execution['NetworkPolicy']['spec'] == dict(podSelector={}, policyTypes=['Ingress','Egress'], ingress=[], egress=[])
preparation = {d['kind']: d for d in docs if d.get('metadata', {}).get('namespace') == 'python-preparation'}
namespace = next(d for d in docs if d['kind']=='Namespace' and d['metadata']['name']=='python-preparation')
assert namespace['metadata']['labels']['pod-security.kubernetes.io/enforce'] == 'restricted'
for doc in [namespace, preparation['NetworkPolicy'], preparation['ResourceQuota']]:
    assert doc['metadata']['annotations']['helm.sh/resource-policy'] == 'keep'
network = preparation['NetworkPolicy']['spec']
assert network['podSelector'] == {}
assert network['policyTypes'] == ['Ingress','Egress']
assert network['ingress'] == []
assert network['egress'] == [
    dict(to=[dict(namespaceSelector=dict(matchLabels={'kubernetes.io/metadata.name':'kube-system'}),
                  podSelector=dict(matchLabels={'k8s-app':'kube-dns'}))],
         ports=[dict(protocol='UDP',port=53),dict(protocol='TCP',port=53)]),
    dict(to=[dict(ipBlock=dict(cidr='203.0.113.0/24')),dict(ipBlock=dict(cidr='2001:db8::/64'))],
         ports=[dict(protocol='TCP',port=443)]),
]
assert preparation['ResourceQuota']['spec']['hard']['count/pods'] == '4'
assert preparation['ServiceAccount']['metadata']['name'] == 'elitea-code'
assert preparation['ServiceAccount']['automountServiceAccountToken'] is False
assert preparation['Role']['rules'] == execution['Role']['rules']
assert preparation['RoleBinding']['subjects'] == [dict(kind='ServiceAccount',name='elitea-sandbox-supervisor',namespace='platform')]
assert not any(d['kind'] in ['ClusterRole','ClusterRoleBinding'] for d in docs)
assert Counter(d['kind'] for d in docs)['Deployment'] == 1
pod = next(d for d in docs if d['kind']=='Deployment')['spec']['template']['spec']
assert len(pod['containers']) == 1
runtime = pod['containers'][0]
assert runtime['args'] == ['--config','/run/elitea-sandbox/deno.json','--config','/run/elitea-sandbox/rust.json','--config','/run/elitea-sandbox/preparation.json']
assert runtime['resources']['limits']['memory'] == '1Gi'
assert runtime['volumeMounts'] == [
    dict(name='private-material',mountPath='/run/elitea-sandbox',readOnly=True),
    dict(name='content-deno',mountPath='/run/elitea-sandbox-content/deno'),
    dict(name='content-preparation',mountPath='/run/elitea-sandbox-content/preparation'),
]
material, staging = pod['initContainers']
assert material['image'] == runtime['image']
assert staging['image'] == 'registry/staging-init@sha256:'+'b'*64
assert staging['command'] == ['/bin/sh','-c']
assert staging['args'] == [
    'set -eu; umask 077; for directory; do mkdir -p "$directory"; chmod 0700 "$directory"; done',
    'prepare-content-staging','/run/elitea-sandbox-content/deno/private','/run/elitea-sandbox-content/preparation/private',
]
assert staging['volumeMounts'] == runtime['volumeMounts'][1:]
assert pod['securityContext']['runAsUser'] == 10001
for container in [material,staging,runtime]:
    assert container['securityContext']['allowPrivilegeEscalation'] is False
    assert container['securityContext']['readOnlyRootFilesystem'] is True
    assert container['securityContext']['capabilities']['drop'] == ['ALL']
assert not any('hostPath' in v for v in pod['volumes'])
assert pod['volumes'][2:] == [
    dict(name='content-deno',emptyDir=dict(medium='Memory',sizeLimit='256Mi')),
    dict(name='content-preparation',emptyDir=dict(medium='Memory',sizeLimit='256Mi')),
]
services = {d['metadata']['name']:d for d in docs if d['kind']=='Service'}
assert [p['port'] for p in services['elitea-sandbox-supervisor']['spec']['ports']] == [9446,9447,9448]
for index,name in enumerate(['elitea-sandbox-deno','elitea-sandbox-rust','elitea-sandbox-preparation']):
    service = services[name]['spec']
    assert service['type'] == 'ClusterIP'
    assert service['selector'] == services['elitea-sandbox-supervisor']['spec']['selector']
    assert service['ports'] == [dict(name=f'profile-{index}',port=9446+index,targetPort=f'profile-{index}')]
warmer = next(d for d in docs if d['kind']=='DaemonSet')['spec']['template']['spec']
assert warmer['automountServiceAccountToken'] is False
assert warmer['nodeSelector'] == {'elitea.ai/sandbox':'true'}
assert [c['name'] for c in warmer['containers']] == ['deno','preparation']
assert all('@sha256:' in c['image'] for c in warmer['containers'])
PYTEST
for invalid in 'sandboxKubernetes.preparation.namespace=platform' \
    'sandboxKubernetes.preparation.namespace=code-execution' \
    'sandboxKubernetes.preparation.namespace=default' \
    'sandboxKubernetes.preparation.namespace=kube-system' \
    'sandboxKubernetes.preparation.namespace=' \
    'sandboxKubernetes.preparation.maxPods=0' \
    'sandboxKubernetes.preparation.maxPods=4097' \
    'sandboxKubernetes.preparation.maxPods=1.5' \
    'sandboxKubernetes.supervisor.enabled=false' \
    'sandboxKubernetes.enabled=false' \
    'sandboxKubernetes.preparation.resolverCidrs[0]=0.0.0.0/0' \
    'sandboxKubernetes.preparation.resolverCidrs[0]=::/0' \
    'sandboxKubernetes.preparation.resolverCidrs[0]=registry.example' \
    'sandboxKubernetes.preparation.resolverCidrs[0]=203.0.113.0/33' \
    'sandboxKubernetes.preparation.resolverCidrs[1]=203.0.113.0/24' \
    'sandboxKubernetes.supervisor.contentStaging[0].name=../escape' \
    'sandboxKubernetes.supervisor.contentStaging[1].name=deno' \
    'sandboxKubernetes.supervisor.contentStaging[0].sizeMiB=0' \
    'sandboxKubernetes.supervisor.contentStaging[0].sizeMiB=1025' \
    'sandboxKubernetes.supervisor.contentStaging[0].sizeMiB=1.5' \
    'sandboxKubernetes.supervisor.contentStagingInitImage=init:latest' \
    'sandboxKubernetes.supervisor.contentStagingInitImage=' \
    'sandboxKubernetes.supervisor.profiles[2].serviceName=elitea-sandbox-deno' \
    'sandboxKubernetes.supervisor.profiles[2].serviceName=elitea-sandbox-supervisor' \
    'sandboxKubernetes.supervisor.profiles[2].serviceName=elitea-main' \
    'sandboxKubernetes.supervisor.profiles[2].serviceName=../escape'; do
  if helm template sandbox "$chart" "${base[@]}" -f "$work/preparation-values.yaml" \
      --set "$invalid" > "$work/invalid" 2>&1; then
    echo "Unsafe preparation configuration accepted: $invalid" >&2; exit 1
  fi
  rg -q 'Kubernetes|sandboxKubernetes' "$work/invalid"
done
for invalid in 'sandboxKubernetes.preparation.resolverCidrs=[]' \
    'sandboxKubernetes.preparation.dnsNamespaceLabels=null' \
    'sandboxKubernetes.preparation.dnsPodLabels=null'; do
  if helm template sandbox "$chart" "${base[@]}" -f "$work/preparation-values.yaml" \
      --set-json "$invalid" > "$work/invalid" 2>&1; then
    echo "Unscoped preparation configuration accepted" >&2; exit 1
  fi
  rg -q 'Kubernetes' "$work/invalid"
done
python3 - "$work" <<'PYTEST'
import pathlib, sys, yaml
work = pathlib.Path(sys.argv[1])
names = ['mixed-execution', 'rust-execution', 'native-deno-execution',
         'python-preparation', 'javascript-preparation', 'typescript-preparation',
         'rust-preparation', 'extra-profile']
for count in [7, 8]:
    supervisor = dict(
        enabled=True, materialSecret='sandbox-material',
        image='registry/supervisor@sha256:'+'a'*64,
        profiles=[dict(file=f'{name}.json', port=9446+index,
                       serviceName=f'elitea-sandbox-{name}')
                  for index, name in enumerate(names[:count])],
        contentStaging=[dict(name=name, sizeMiB=1) for name in names[:count]],
        contentStagingInitImage='registry/staging-init@sha256:'+'b'*64,
    )
    values = dict(sandboxKubernetes=dict(enabled=True, executionNamespace='code-execution',
                                         supervisor=supervisor))
    (work/f'profiles-{count}.yaml').write_text(yaml.safe_dump(values))
PYTEST
for count in 7 8; do
  helm template sandbox "$chart" "${base[@]}" -f "$work/profiles-$count.yaml" \
    --show-only templates/sandbox/supervisor.yaml > "$work/rendered-profiles-$count.yaml"
  python3 - "$work/rendered-profiles-$count.yaml" "$count" <<'PYTEST'
import sys, yaml
docs = list(yaml.safe_load_all(open(sys.argv[1])))
count = int(sys.argv[2])
pod = next(d for d in docs if d['kind']=='Deployment')['spec']['template']['spec']
runtime = pod['containers'][0]
assert len(runtime['args']) == 2*count
assert runtime['args'][::2] == ['--config']*count
assert len(set(runtime['args'][1::2])) == count
assert [p['containerPort'] for p in runtime['ports']] == list(range(9446, 9446+count))
staging = runtime['volumeMounts'][1:]
assert len(staging) == count
assert len(set(m['name'] for m in staging)) == count
assert len(set(m['mountPath'] for m in staging)) == count
assert pod['initContainers'][1]['volumeMounts'] == staging
assert pod['initContainers'][1]['args'][2:] == [m['mountPath']+'/private' for m in staging]
assert len(pod['volumes']) == 2+count
assert all(v['emptyDir']==dict(medium='Memory',sizeLimit='1Mi') for v in pod['volumes'][2:])
services = [d for d in docs if d['kind']=='Service']
assert len(services) == 1+count
assert len(set(d['metadata']['name'] for d in services)) == 1+count
assert [p['port'] for p in services[0]['spec']['ports']] == list(range(9446, 9446+count))
for index, service in enumerate(services[1:]):
    assert service['spec']['ports'] == [dict(name=f'profile-{index}',port=9446+index,targetPort=f'profile-{index}')]
PYTEST
done
for invalid in 'sandboxKubernetes.supervisor.profiles[8].file=ninth.json' \
    'sandboxKubernetes.supervisor.contentStaging[8].name=ninth' \
    'sandboxKubernetes.supervisor.profiles[7].file=mixed-execution.json' \
    'sandboxKubernetes.supervisor.profiles[7].port=9446' \
    'sandboxKubernetes.supervisor.profiles[7].serviceName=elitea-sandbox-mixed-execution' \
    'sandboxKubernetes.supervisor.contentStaging[7].name=mixed-execution'; do
  if helm template sandbox "$chart" "${base[@]}" -f "$work/profiles-8.yaml" \
      --set "$invalid" > "$work/invalid" 2>&1; then
    echo "Unsafe eight-profile configuration accepted: $invalid" >&2; exit 1
  fi
  rg -q 'Kubernetes' "$work/invalid"
done
if helm template sandbox "$chart" "${base[@]}" -f "$work/profiles-8.yaml" \
    --set-json 'sandboxKubernetes.supervisor.profiles=[]' > "$work/invalid" 2>&1; then
  echo 'Empty supervisor profile list accepted' >&2; exit 1
fi
rg -q 'one to eight profiles' "$work/invalid"
echo 'Kubernetes sandbox and optional Python preparation rendering checks passed'
