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
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)
boundary=(--set sandboxKubernetes.enabled=true --set sandboxKubernetes.executionNamespace=code-execution)

# Without workspace runs: no ClusterRole.
helm template sandbox "$chart" "${base[@]}" "${boundary[@]}" \
  --show-only templates/sandbox/kubernetes-boundary.yaml > "$work/off.yaml"
python3 - "$work/off.yaml" <<'PY'
import sys, yaml
docs = list(filter(None, yaml.safe_load_all(open(sys.argv[1]))))
assert not any(d['kind'] in ['ClusterRole', 'ClusterRoleBinding'] for d in docs)
PY

# With workspace runs: `get` on the one RuntimeClass, bound to the supervisor.
helm template sandbox "$chart" "${base[@]}" "${boundary[@]}" \
  --set sandboxKubernetes.workspaceRuns.enabled=true \
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
  if helm template sandbox "$chart" "${base[@]}" "${boundary[@]}" \
      --set sandboxKubernetes.workspaceRuns.enabled=true --set "$invalid" > "$work/invalid" 2>&1; then
    echo "Invalid RuntimeClass accepted: $invalid" >&2; exit 1
  fi
  grep -Fq 'sandboxKubernetes.runtimeClass.name' "$work/invalid"
done
echo 'Cloud executor rendering checks passed'
