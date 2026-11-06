#!/usr/bin/env bash
# ADR-0049 (#1595): the cloud browser's `browser` supervisor profile.
#
#   * off (the default) renders no profile, no port, no egress policy;
#   * on, the rendered profile equals deploy/runtime/sandbox-browser.kubernetes.example.json
#     (the supervisor's own test parses that file, so chart and Rust schema
#     cannot drift), the supervisor gets the --config, port, Service and
#     checksum, its NetworkPolicy admits the port, and the RuntimeClass
#     ClusterRole renders even without workspace runs;
#   * with the egress proxy, session Pods get exactly one route out (the
#     proxy pods on the proxy port, no DNS, no ingress) and the proxy admits them;
#   * every half-enabled shape is refused.
set -euo pipefail
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
  --set worker.enabled=true
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=)
executor=(-f deploy/helm/tests/fixtures/cloud-executor-values.yaml)
browser=(-f deploy/helm/tests/fixtures/browser-sessions-values.yaml)
proxy=(-f deploy/helm/tests/fixtures/egress-proxy-values.yaml)

helm template sandbox "$chart" "${base[@]}" "${executor[@]}" > "$work/off.yaml"
helm template sandbox "$chart" "${base[@]}" "${executor[@]}" "${browser[@]}" > "$work/on.yaml"
helm template sandbox "$chart" "${base[@]}" "${executor[@]}" "${browser[@]}" "${proxy[@]}" > "$work/proxy.yaml"
# Browser sessions alone (no workspace runs): still the RuntimeClass grant.
helm template sandbox "$chart" "${base[@]}" "${executor[@]}" "${browser[@]}" \
  --set sandboxKubernetes.workspaceRuns.enabled=false > "$work/alone.yaml"

python3 - "$work/off.yaml" "$work/on.yaml" "$work/proxy.yaml" "$work/alone.yaml" <<'PY'
import json, sys, yaml
def load(path):
    docs = [d for d in yaml.safe_load_all(open(path)) if d]
    return docs, {(d['kind'], d['metadata']['name']): d for d in docs}
off, off_named = load(sys.argv[1])
on, named = load(sys.argv[2])
proxy, proxy_named = load(sys.argv[3])
alone, alone_named = load(sys.argv[4])

# Off: nothing of the browser profile.
assert ('ConfigMap', 'elitea-sandbox-browser-profile') not in off_named
assert ('Service', 'elitea-sandbox-browser') not in off_named
assert ('NetworkPolicy', 'elitea-browser-session-egress') not in off_named
sup = off_named['Deployment', 'elitea-sandbox-supervisor']
assert '/run/elitea-sandbox/browser.json' not in sup['spec']['template']['spec']['containers'][0]['args']

# On: the profile is the checked-in example, field for field.
profile = json.loads(named['ConfigMap', 'elitea-sandbox-browser-profile']['data']['browser.json'])
expected = json.load(open('deploy/runtime/sandbox-browser.kubernetes.example.json'))
assert profile == expected, ('regenerate deploy/runtime/sandbox-browser.kubernetes.example.json', profile)
assert profile['purpose'] == 'browser' and profile['audience'] == 'dns:elitea-sandbox-browser'
assert profile['engine_sandbox'] is False and 'lite' not in profile
assert profile['full'] == dict(max_cpu_millis=2000, max_memory_bytes=2147483648)

sup = named['Deployment', 'elitea-sandbox-supervisor']
tpl = sup['spec']['template']
c = tpl['spec']['containers'][0]
assert c['args'] == ['--config', '/run/elitea-sandbox/workspace.json',
                     '--config', '/run/elitea-sandbox/browser.json'], c['args']
assert {p['name']: p['containerPort'] for p in c['ports']} == {'profile-0': 9470, 'profile-1': 9471}
assert 'checksum/browser-profile' in tpl['metadata']['annotations']
assert 'checksum/workspace-profile' in tpl['metadata']['annotations']
sources = next(v for v in tpl['spec']['volumes'] if v['name'] == 'projected-material')['projected']['sources']
assert [s['configMap']['name'] for s in sources if 'configMap' in s] == \
    ['elitea-sandbox-workspace-profile', 'elitea-sandbox-browser-profile']
svc = named['Service', 'elitea-sandbox-browser']
assert svc['spec']['ports'] == [dict(name='profile-1', port=9471, targetPort='profile-1')]
netpol = named['NetworkPolicy', 'elitea-sandbox-supervisor-netpol']
assert {p['port'] for p in netpol['spec']['ingress'][0]['ports']} == {9470, 9471}
# Without the egress proxy there is no allow rule: a session has no network.
assert ('NetworkPolicy', 'elitea-browser-session-egress') not in named

# Alone: the profile is index 0 and the RuntimeClass grant still renders.
c = alone_named['Deployment', 'elitea-sandbox-supervisor']['spec']['template']['spec']['containers'][0]
assert c['args'] == ['--config', '/run/elitea-sandbox/browser.json'], c['args']
assert c['ports'] == [dict(name='profile-0', containerPort=9471)]
role = alone_named['ClusterRole', 'elitea-sandbox-runtimeclass-platform']
assert role['rules'][0]['resourceNames'] == ['gvisor']
assert ('ConfigMap', 'elitea-sandbox-workspace-profile') not in alone_named

# With the egress proxy: exactly one route out for session Pods.
eg = proxy_named['NetworkPolicy', 'elitea-browser-session-egress']
assert eg['metadata']['namespace'] == 'code-execution'
assert eg['spec']['podSelector'] == {'matchLabels': {'sandbox.elitea.ai/kind': 'browser-session'}}
assert eg['spec']['policyTypes'] == ['Ingress', 'Egress']
assert eg['spec']['ingress'] == []
assert len(eg['spec']['egress']) == 1, eg['spec']['egress']
rule = eg['spec']['egress'][0]
proxy_port = proxy_named['Service', 'elitea-egress-proxy']['spec']['ports'][0]['port']
assert rule['ports'] == [dict(protocol='TCP', port=proxy_port)], rule['ports']
peer, = rule['to']
assert peer['namespaceSelector'] == {'matchLabels': {'kubernetes.io/metadata.name': 'platform'}}
assert 'ipBlock' not in peer and peer['podSelector']['matchLabels']
pp = proxy_named['NetworkPolicy', 'elitea-egress-proxy-netpol']
peers = pp['spec']['ingress'][0]['from']
assert {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'code-execution'}},
        'podSelector': {'matchLabels': {'sandbox.elitea.ai/kind': 'browser-session'}}} in peers, peers
print('browser sessions: rendered as expected')
PY

# The worker's side (#1596): browser_sessions in runtime.json when set, and
# absent by default.
helm template sandbox "$chart" "${base[@]}" "${executor[@]}" "${browser[@]}" \
  --show-only templates/worker/configmap-runtime.yaml > "$work/worker-on.yaml"
helm template sandbox "$chart" "${base[@]}" "${executor[@]}" \
  --show-only templates/worker/configmap-runtime.yaml > "$work/worker-off.yaml"
python3 - "$work/worker-on.yaml" "$work/worker-off.yaml" <<'PY'
import json, sys, yaml
on = json.loads(yaml.safe_load(open(sys.argv[1]))['data']['runtime.json'])
assert on['browser_sessions'] == {'target': 'elitea-sandbox-browser:9471',
    'audience': 'dns:elitea-sandbox-browser', 'cpu_millis': 2000,
    'memory_bytes': 2147483648, 'wall_seconds': 3600, 'max_pages': 3}, on['browser_sessions']
off = json.loads(yaml.safe_load(open(sys.argv[2]))['data']['runtime.json'])
assert 'browser_sessions' not in off, off
PY

refuse() {
  local why=$1; shift
  if helm template sandbox "$chart" "${base[@]}" "${executor[@]}" "${browser[@]}" "$@" > "$work/refused" 2>&1; then
    echo "accepted: $why" >&2; exit 1
  fi
  grep -Fq "$why" "$work/refused" || { cat "$work/refused" >&2; exit 1; }
}
refuse 'profile.image must be the browser session image' --set sandboxKubernetes.browserSessions.profile.image=registry/browser:latest
refuse 'nodeSelector must place session Pods' --set sandboxKubernetes.browserSessions.nodeSelector=null
refuse 'audience must be in main.runtime.sandboxAudiences' --set 'main.runtime.sandboxAudiences={dns:elitea-sandbox-workspace}'
refuse 'port is taken by the workspace profile' --set sandboxKubernetes.browserSessions.profile.port=9470
refuse 'owner is taken by the workspace profile' --set sandboxKubernetes.browserSessions.profile.owner=workspace-runs
refuse 'requires sandboxKubernetes.supervisor.enabled' --set sandboxKubernetes.supervisor.enabled=false
refuse 'engineSandbox must be true or false' --set-string sandboxKubernetes.browserSessions.profile.engineSandbox=maybe
refuse 'maxPages must be a positive integer' --set sandboxKubernetes.browserSessions.profile.maxPages=0
refuse 'lite.maxCpuMillis must be a positive integer' --set sandboxKubernetes.browserSessions.profile.lite.maxMemoryBytes=536870912
refuse 'worker.runtime.browserSessions.audience must be in main.runtime.sandboxAudiences' --set worker.runtime.browserSessions.audience=dns:other
refuse 'worker.runtime.browserSessions.max_pages is required' --set worker.runtime.browserSessions.max_pages=null
echo "render-browser-sessions: ok"
