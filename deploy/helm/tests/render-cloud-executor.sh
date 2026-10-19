#!/usr/bin/env bash
# ADR-0034: the cloud executor on Kubernetes. The supervisor may read only
# the RuntimeClass its workspace runs use (it refuses any handler but runsc or
# kata*), and nothing cluster-scoped renders without workspace runs.
set -euo pipefail
cd "$(dirname "$0")/../../.."
chart=deploy/helm/elitea
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base=(-f "$chart/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
  --set worker.enabled=true
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=)
boundary=(--set sandboxKubernetes.enabled=true --set sandboxKubernetes.executionNamespace=code-execution)

# The whole cloud executor on a k3s cluster with one tainted sandbox node.
cp deploy/helm/tests/fixtures/cloud-executor-values.yaml "$work/k3s.yaml"
# Without workspace runs Main is told there is no executor; a stated value wins.
helm template sandbox "$chart" "${base[@]}" --show-only templates/main/configmap.yaml > "$work/main-off.yaml"
grep -q 'ELITEA_CLOUD_EXECUTOR_CONFIGURED: "false"' "$work/main-off.yaml"
helm template sandbox "$chart" "${base[@]}" --set-string main.env.ELITEA_CLOUD_EXECUTOR_CONFIGURED=true \
  --show-only templates/main/configmap.yaml > "$work/main-stated.yaml"
grep -q 'ELITEA_CLOUD_EXECUTOR_CONFIGURED: "true"' "$work/main-stated.yaml"
# Without workspace runs: no ClusterRole.
helm template sandbox "$chart" "${base[@]}" "${boundary[@]}" \
  --show-only templates/sandbox/kubernetes-boundary.yaml > "$work/off.yaml"
python3 - "$work/off.yaml" <<'PY'
import sys, yaml
docs = list(filter(None, yaml.safe_load_all(open(sys.argv[1]))))
assert not any(d['kind'] in ['ClusterRole', 'ClusterRoleBinding'] for d in docs)
PY

# With workspace runs: `get` on the one RuntimeClass, bound to the supervisor.
helm template sandbox "$chart" "${base[@]}" -f "$work/k3s.yaml" \
  --show-only templates/sandbox/kubernetes-boundary.yaml > "$work/rbac.yaml"
python3 - "$work/rbac.yaml" <<'PY'
import sys, yaml
docs = {d['kind']: d for d in filter(None, yaml.safe_load_all(open(sys.argv[1])))}
role, binding = docs['ClusterRole'], docs['ClusterRoleBinding']
assert role['metadata']['name'] == 'elitea-sandbox-runtimeclass-platform'
assert role['rules'] == [dict(apiGroups=['node.k8s.io'], resources=['runtimeclasses'],
                              resourceNames=['gvisor'], verbs=['get'])]
assert binding['roleRef'] == dict(apiGroup='rbac.authorization.k8s.io', kind='ClusterRole',
                                  name='elitea-sandbox-runtimeclass-platform')
assert binding['subjects'] == [dict(kind='ServiceAccount', name='elitea-sandbox-supervisor',
                                    namespace='platform')]
PY
for invalid in 'sandboxKubernetes.runtimeClass.name=' 'sandboxKubernetes.runtimeClass.name=Not_A_Name'; do
  if helm template sandbox "$chart" "${base[@]}" -f "$work/k3s.yaml" --set "$invalid" > "$work/invalid" 2>&1; then
    echo "Invalid RuntimeClass accepted: $invalid" >&2; exit 1
  fi
  grep -Fq 'sandboxKubernetes.runtimeClass.name' "$work/invalid"
done

helm template sandbox "$chart" "${base[@]}" -f "$work/k3s.yaml" > "$work/k3s-out.yaml"
python3 - "$work/k3s-out.yaml" <<'PY'
import json, sys, yaml
docs = list(filter(None, yaml.safe_load_all(open(sys.argv[1]))))
named = {(d['kind'], d['metadata']['name']): d for d in docs}
# The chart creates no RuntimeClass and touches no node.
assert not any(d['kind'] == 'RuntimeClass' for d in docs)
assert not any(d['kind'] == 'DaemonSet' and 'runtime' in d['metadata']['name'] for d in docs)
profile = json.loads(named['ConfigMap', 'elitea-sandbox-workspace-profile']['data']['workspace.json'])
# Exactly the fields WorkspaceProfileConfig (deny_unknown_fields) accepts.
assert set(profile) == {'revision', 'purpose', 'listen_address', 'owner', 'audience', 'ca_path',
    'certificate_path', 'private_key_path', 'verification_keyring_path', 'image', 'concurrency',
    'max_workspace_bytes', 'max_cpu_millis', 'max_memory_bytes', 'max_wall_seconds', 'backend'}, profile
assert profile['purpose'] == 'workspace' and profile['revision'] == 1
# The supervisor's own test parses this file (workspace_run_profile.rs), so
# the chart and the Rust schema cannot drift apart.
assert profile == json.load(open('deploy/runtime/sandbox-workspace.kubernetes.example.json')), 'regenerate deploy/runtime/sandbox-workspace.kubernetes.example.json'
assert profile['listen_address'] == '0.0.0.0:9470'
assert profile['audience'] == 'dns:elitea-sandbox-workspace'
assert profile['ca_path'] == '/run/elitea-sandbox/workspace-client-ca.pem'
assert profile['private_key_path'] == '/run/elitea-sandbox/workspace-server.key'
assert profile['max_workspace_bytes'] == 10737418240 and isinstance(profile['max_workspace_bytes'], int)
assert profile['backend'] == dict(kind='kubernetes', namespace='code-execution', runtime_class='gvisor',
    node_selector={'elitea.ai/sandbox': 'true'},
    tolerations=[dict(key='elitea.ai/sandbox', operator='Equal', value='true', effect='NoSchedule')])
# The supervisor runs it, from its private material, projected with the Secret.
supervisor = named['Deployment', 'elitea-sandbox-supervisor']['spec']['template']
pod = supervisor['spec']
assert pod['containers'][0]['args'] == ['--config', '/run/elitea-sandbox/workspace.json']
assert pod['containers'][0]['ports'] == [dict(name='profile-0', containerPort=9470)]
assert 'checksum/workspace-profile' in supervisor['metadata']['annotations']
sources = pod['volumes'][0]['projected']['sources']
assert pod['volumes'][0]['projected']['defaultMode'] == 0o440
assert sources == [dict(secret=dict(name='sandbox-material')),
                   dict(configMap=dict(name='elitea-sandbox-workspace-profile'))]
service = named['Service', 'elitea-sandbox-workspace']['spec']
assert service['ports'] == [dict(name='profile-0', port=9470, targetPort='profile-0')]
netpol = named['NetworkPolicy', 'elitea-sandbox-supervisor-netpol']['spec']
assert dict(port=9470, protocol='TCP') in netpol['ingress'][0]['ports']
assert ('ClusterRole', 'elitea-sandbox-runtimeclass-platform') in named
# The worker's scratch is its own sized disk.
worker = next(d for d in docs if d['kind'] == 'Deployment' and d['metadata']['name'].endswith('worker'))
spec = worker['spec']['template']['spec']
mounts = {m['name']: m for m in spec['containers'][0]['volumeMounts']}
assert mounts['workspace-scratch']['mountPath'] == '/var/lib/elitea-worker/workspace-scratch'
volumes = {v['name']: v for v in spec['volumes']}
assert volumes['workspace-scratch'] == dict(name='workspace-scratch', emptyDir=dict(sizeLimit='30Gi'))
# Main knows an executor runs (Send to cloud never reports no_executor here).
main_env = next(d for d in docs if d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'elitea-main-config')['data']
assert main_env['ELITEA_CLOUD_EXECUTOR_CONFIGURED'] == 'true', main_env.get('ELITEA_CLOUD_EXECUTOR_CONFIGURED')
PY

# Beside Code profiles the workspace profile takes the next index.
helm template sandbox "$chart" "${base[@]}" -f "$work/k3s.yaml" \
  --set sandboxKubernetes.supervisor.profiles[0].file=deno.json \
  --set sandboxKubernetes.supervisor.profiles[0].port=9446 \
  --show-only templates/sandbox/supervisor.yaml > "$work/mixed.yaml"
python3 - "$work/mixed.yaml" <<'PY'
import sys, yaml
docs = list(filter(None, yaml.safe_load_all(open(sys.argv[1]))))
pod = next(d for d in docs if d['kind'] == 'Deployment')['spec']['template']['spec']
assert pod['containers'][0]['args'] == ['--config', '/run/elitea-sandbox/deno.json', '--config', '/run/elitea-sandbox/workspace.json']
assert [p['name'] for p in pod['containers'][0]['ports']] == ['profile-0', 'profile-1']
services = {d['metadata']['name']: d for d in docs if d['kind'] == 'Service'}
assert services['elitea-sandbox-workspace']['spec']['ports'][0]['targetPort'] == 'profile-1'
PY

# Every half-enabled shape fails the render.
expect_refusal() {
  local needle=$1; shift
  if helm template sandbox "$chart" "${base[@]}" -f "$work/k3s.yaml" "$@" > "$work/invalid" 2>&1; then
    echo "Half-enabled cloud executor accepted: $*" >&2; exit 1
  fi
  grep -Fq -- "$needle" "$work/invalid" || { cat "$work/invalid" >&2; exit 1; }
}
expect_refusal 'requires sandboxKubernetes.supervisor.enabled' --set sandboxKubernetes.supervisor.enabled=false
expect_refusal 'requires sandboxKubernetes.enabled' --set sandboxKubernetes.enabled=false
expect_refusal 'requires worker.runtime.workspaceRuns' --set-json 'worker.runtime.workspaceRuns=null'
expect_refusal 'runtimeClass.name' --set sandboxKubernetes.runtimeClass.name=
expect_refusal 'profile.image' --set sandboxKubernetes.workspaceRuns.profile.image=runner:latest
expect_refusal 'nodeSelector' --set-json 'sandboxKubernetes.workspaceRuns.nodeSelector=null'
expect_refusal 'audience must equal' --set worker.runtime.workspaceRuns.audience=dns:other \
  --set-json 'main.runtime.sandboxAudiences=["dns:elitea-sandbox-workspace","dns:other"]'
expect_refusal 'main.runtime.sandboxAudiences' --set-json 'main.runtime.sandboxAudiences=["dns:other"]'
expect_refusal 'worker.runtime.workspaceRuns.target' --set worker.runtime.workspaceRuns.target=elsewhere:9470
expect_refusal 'taken by a supervisor profile' \
  --set sandboxKubernetes.supervisor.profiles[0].file=deno.json --set sandboxKubernetes.supervisor.profiles[0].port=9470
expect_refusal 'scratch_root' --set worker.runtime.workspaceRuns.scratch_root=/tmp/scratch
expect_refusal 'scratch_root' --set worker.runtime.workspaceRuns.scratch_root=/var/lib/elitea-worker/output-spool
echo 'Cloud executor rendering checks passed'
