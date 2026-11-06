#!/usr/bin/env bash
# ADR-0049 decision 6 (#1597): Main's cloud browser view relay.
#
#   * off (the default) renders no ELITEA_RUNTIME_BROWSER_VIEW_CONFIG and no
#     browser-view-client material;
#   * on, the ConfigMap carries the relay config (Main's client identity from
#     the runtime material, the browser supervisor's audience and origin), the
#     runtime material Secret's key list gains browser-view-client.crt/.key,
#     and the ExternalSecret asks for them;
#   * a null value is missing (Helm 4 keeps `--set key=null`), and every
#     half-enabled shape is refused.
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
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=
  -f deploy/helm/tests/fixtures/cloud-executor-values.yaml
  -f deploy/helm/tests/fixtures/browser-sessions-values.yaml)
cat > "$work/view.yaml" <<'YAML'
main:
  runtime:
    browserView:
      enabled: true
      mainWorkloadIdentity: spiffe://elitea.invalid/main
      supervisors:
        - audience: dns:elitea-sandbox-browser
          httpsOrigin: https://elitea-sandbox-browser:9471
YAML
view=(-f "$work/view.yaml")
es=(--set externalSecrets.enabled=true --set externalSecrets.secretStoreRef.name=platform-secrets --set externalSecrets.remoteKey=elitea
  --set externalSecrets.secrets.sandbox-material.enabled=false)

helm template elitea "$chart" "${base[@]}" > "$work/off.yaml"
helm template elitea "$chart" "${base[@]}" "${view[@]}" > "$work/on.yaml"
helm template elitea "$chart" "${base[@]}" "${view[@]}" --set main.runtime.browserView.maxFps=5 > "$work/fps.yaml"
helm template elitea "$chart" "${base[@]}" --set main.runtime.browserView=null > "$work/null.yaml"
helm template elitea "$chart" "${base[@]}" "${view[@]}" "${es[@]}" > "$work/es.yaml"

python3 - "$work" <<'PY'
import json, sys, yaml
work = sys.argv[1]
def load(name):
    return [d for d in yaml.safe_load_all(open(f'{work}/{name}.yaml')) if d]
def main_env(docs):
    return next(d for d in docs if d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'elitea-main-config')['data']
def main_pod(docs):
    return next(d for d in docs if d['kind'] == 'Deployment' and d['metadata']['name'] == 'elitea-main')['spec']['template']['spec']

for name in ('off', 'null'):
    docs = load(name)
    assert 'ELITEA_RUNTIME_BROWSER_VIEW_CONFIG' not in main_env(docs), name
    assert 'browser-view-client' not in yaml.safe_dump_all(docs), name

on = load('on')
cfg = json.loads(main_env(on)['ELITEA_RUNTIME_BROWSER_VIEW_CONFIG'])
mount = next(m['mountPath'] for c in main_pod(on)['containers'] for m in c.get('volumeMounts', [])
             if m['name'] == 'runtime-material').rstrip('/')
assert cfg == {
    'main_workload_identity': 'spiffe://elitea.invalid/main',
    'certificate_chain_path': f'{mount}/browser-view-client.crt',
    'private_key_path': f'{mount}/browser-view-client.key',
    'server_ca_path': f'{mount}/runtime-ca.crt',
    'supervisors': [{'audience': 'dns:elitea-sandbox-browser', 'https_origin': 'https://elitea-sandbox-browser:9471'}],
}, cfg
fps = json.loads(main_env(load('fps'))['ELITEA_RUNTIME_BROWSER_VIEW_CONFIG'])
assert fps['max_fps'] == 5, fps

# The ExternalSecret for Main's runtime material asks for the client pair.
es = load('es')
keys = set()
src = next(v['secret']['secretName'] for v in main_pod(es)['volumes'] if v['name'] == 'runtime-material-source')
for d in es:
    if d['kind'] == 'ExternalSecret' and d['spec']['target']['name'] == src:
        keys |= {x['secretKey'] for x in d['spec']['data']}
assert {'browser-view-client.crt', 'browser-view-client.key', 'runtime-ca.crt'} <= keys, sorted(keys)
print('browser view: rendered as expected')
PY

refuse() {
  local why=$1; shift
  if helm template elitea "$chart" "${base[@]}" "$@" > "$work/refused" 2>&1; then
    echo "accepted: $why" >&2; exit 1
  fi
  grep -Fq -- "$why" "$work/refused" || { cat "$work/refused" >&2; exit 1; }
}
refuse 'settings require main.runtime.browserView.enabled=true' --set main.runtime.browserView.mainWorkloadIdentity=spiffe://elitea.invalid/main
refuse 'must be in main.runtime.sandboxAudiences' "${view[@]}" --set 'main.runtime.sandboxAudiences={dns:elitea-sandbox-workspace}'
refuse 'requires mainWorkloadIdentity' "${view[@]}" --set main.runtime.browserView.mainWorkloadIdentity=null
refuse 'one to sixteen supervisors' "${view[@]}" --set main.runtime.browserView.supervisors=null
refuse 'fixed https://host:port origins' "${view[@]}" --set 'main.runtime.browserView.supervisors[0].httpsOrigin=http://elitea-sandbox-browser:9471'
refuse 'maxFps is a whole number of at most 10' "${view[@]}" --set main.runtime.browserView.maxFps=11
refuse 'unknown field' "${view[@]}" --set main.runtime.browserView.fps=5
refuse 'requires the runtime, agent dispatch' "${view[@]}" --set main.runtime.agentExecutionDispatch.enabled=false
refuse 'env.ELITEA_RUNTIME_BROWSER_VIEW_CONFIG must not be set' --set main.env.ELITEA_RUNTIME_BROWSER_VIEW_CONFIG=x
echo "render-browser-view: ok"
