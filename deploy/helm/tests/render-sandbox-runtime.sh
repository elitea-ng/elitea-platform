#!/usr/bin/env bash
# The elitea-sandbox-runtime chart: a privileged installer DaemonSet on the
# sandbox nodes only, the scripts from its ConfigMap, the gvisor RuntimeClass
# (handler runsc) whose scheduling lands Pods on those nodes, and the guards
# that refuse an unpinned release, image or checksum.
set -euo pipefail
cd "$(dirname "$0")/../../.."
chart=deploy/helm/elitea-sandbox-runtime
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
helm lint "$chart" >/dev/null
helm template rt "$chart" --namespace elitea-sandbox-runtime > "$work/k3s.yaml"
helm template rt "$chart" --namespace elitea-sandbox-runtime --set mode=containerd \
  --set runtimeClass.overhead.cpu=100m --set runtimeClass.overhead.memory=64Mi > "$work/containerd.yaml"
python3 - "$work/k3s.yaml" "$work/containerd.yaml" "$chart" <<'PY'
import sys, yaml
def load(path):
    return {(d['kind'], d['metadata']['name']): d for d in yaml.safe_load_all(open(path)) if d}
k3s, cd = load(sys.argv[1]), load(sys.argv[2])
ds = k3s['DaemonSet', 'rt-installer']
pod = ds['spec']['template']['spec']
assert pod['hostPID'] is True and pod['automountServiceAccountToken'] is False
assert pod['nodeSelector'] == {'elitea.ai/sandbox': 'true'}
assert pod['tolerations'] == [dict(key='elitea.ai/sandbox', operator='Equal', value='true', effect='NoSchedule')]
assert ds['spec']['updateStrategy']['rollingUpdate']['maxUnavailable'] == 1
init, ready = pod['initContainers'][0], pod['containers'][0]
assert init['command'] == ['/bin/bash', '/scripts/install.sh']
assert init['securityContext']['privileged'] is True
env = {e['name']: e['value'] for e in init['env']}
assert env['MODE'] == 'k3s' and env['GVISOR_RELEASE'] == '20261005.0'
assert len(env['GVISOR_SHA512_X86_64']) == 128 and len(env['GVISOR_SHA512_AARCH64']) == 128
assert env['GVISOR_BASE_URL'].startswith('https://')
assert ready['readinessProbe']['exec']['command'] == ['/bin/bash', '/scripts/check.sh']
for c in (init, ready):
    assert '@sha256:' in c['image']
mounts = {m['name']: m for m in ready['volumeMounts']}
assert mounts['host']['readOnly'] is True
assert {v['name']: v for v in pod['volumes']}['host']['hostPath'] == dict(path='/', type='Directory')
scripts = k3s['ConfigMap', 'rt-scripts']['data']
assert scripts['install.sh'] == open(f'{sys.argv[3]}/files/install.sh').read().rstrip('\n')
assert 'checksum/scripts' in ds['spec']['template']['metadata']['annotations']
rc = k3s['RuntimeClass', 'gvisor']
assert rc['handler'] == 'runsc'
assert rc['scheduling']['nodeSelector'] == pod['nodeSelector']
assert rc['scheduling']['tolerations'] == pod['tolerations']
assert 'overhead' not in rc
cd_pod = cd['DaemonSet', 'rt-installer']['spec']['template']['spec']
assert {e['name']: e['value'] for e in cd_pod['initContainers'][0]['env']}['MODE'] == 'containerd'
assert cd['RuntimeClass', 'gvisor']['overhead'] == dict(podFixed=dict(cpu='100m', memory='64Mi'))
# A mode change re-runs the installer.
assert cd['DaemonSet', 'rt-installer']['spec']['template']['metadata']['annotations']['checksum/env'] != \
    ds['spec']['template']['metadata']['annotations']['checksum/env']
PY
helm template rt "$chart" --set runtimeClass.create=false > "$work/norc.yaml"
! grep -q '^kind: RuntimeClass' "$work/norc.yaml"
refuse() {
  local needle=$1; shift
  if helm template rt "$chart" "$@" > "$work/invalid" 2>&1; then echo "accepted: $*" >&2; exit 1; fi
  grep -Fq "$needle" "$work/invalid" || { cat "$work/invalid" >&2; exit 1; }
}
refuse 'mode must be' --set mode=docker
refuse 'gvisor.release' --set gvisor.release=latest
refuse 'gvisor.baseUrl' --set gvisor.baseUrl=http://storage.googleapis.com/gvisor
refuse 'gvisor.sha512.x86_64' --set gvisor.sha512.x86_64=abc
refuse 'image must use' --set image=python:3.12-slim-trixie
refuse 'nodeSelector' --set-json 'nodeSelector=null'
refuse 'k3s.service' --set k3s.service=rke2-agent
refuse 'kata is a placeholder' --set kata.enabled=true
# Standalone: the release name and namespace by default, the overrides when set.
helm template rt "$chart" --namespace ns --set namespace=other --set fullnameOverride=x > "$work/over.yaml"
python3 - "$work/over.yaml" <<'PY'
import sys, yaml
got = sorted((d['kind'], d['metadata']['name'], d['metadata'].get('namespace')) for d in yaml.safe_load_all(open(sys.argv[1])) if d)
assert got == [('ConfigMap', 'x-scripts', 'other'), ('DaemonSet', 'x-installer', 'other'), ('RuntimeClass', 'gvisor', None)], got
PY

# As the elitea chart's `sandboxRuntime` dependency (file://../elitea-sandbox-runtime,
# built into charts/ by deploy/helm/elitea/build-deps.sh): off by default; on, the installer goes to its own privileged
# namespace under its own name, not the platform release's.
el=(elitea deploy/helm/elitea -n elitea
    --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
    --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)
# The archive build-deps.sh made must be THIS chart as it is now: a stale one
# would render an installer this test never read.
sv="$(awk '$1 == "version:" { print $2; exit }' deploy/helm/elitea-sandbox-runtime/Chart.yaml)"
arc="deploy/helm/elitea/charts/elitea-sandbox-runtime-${sv}.tgz"
[ -f "$arc" ] || { echo "$arc is missing: run deploy/helm/elitea/build-deps.sh" >&2; exit 1; }
mkdir -p "$work/arc" && tar -xzf "$arc" -C "$work/arc"
diff -r -x .helmignore "$work/arc/elitea-sandbox-runtime/templates" deploy/helm/elitea-sandbox-runtime/templates >/dev/null \
  && cmp -s "$work/arc/elitea-sandbox-runtime/values.yaml" deploy/helm/elitea-sandbox-runtime/values.yaml \
  || { echo "$arc differs from deploy/helm/elitea-sandbox-runtime: run deploy/helm/elitea/build-deps.sh" >&2; exit 1; }
helm template "${el[@]}" 2>/dev/null > "$work/el-off.yaml"
helm template "${el[@]}" --set sandboxRuntime.enabled=true 2>/dev/null > "$work/el-on.yaml"
helm template "${el[@]}" --set sandboxRuntime.enabled=true --set sandboxRuntime.mode=containerd \
  --set sandboxRuntime.runtimeClass.create=false 2>/dev/null > "$work/el-eks.yaml"
python3 - "$work" <<'PY'
import sys, yaml, pathlib
w = pathlib.Path(sys.argv[1])
def load(n): return {(d['kind'], d['metadata']['name']): d for d in yaml.safe_load_all((w / n).read_text()) if d}
off, on, eks = load('el-off.yaml'), load('el-on.yaml'), load('el-eks.yaml')
assert not [k for k in off if k[0] in ('DaemonSet', 'RuntimeClass')], 'sandboxRuntime renders while off'
ds = on['DaemonSet', 'elitea-sandbox-runtime-installer']
assert ds['metadata']['namespace'] == 'elitea-sandbox-runtime'
assert on['ConfigMap', 'elitea-sandbox-runtime-scripts']['metadata']['namespace'] == 'elitea-sandbox-runtime'
pod = ds['spec']['template']['spec']
assert pod['initContainers'][0]['securityContext']['privileged'] is True
assert on['RuntimeClass', 'gvisor']['handler'] == 'runsc'
# The installer is the only thing that moved: no platform object lands in its namespace.
moved = sorted(k for k, d in on.items() if d['metadata'].get('namespace') == 'elitea-sandbox-runtime')
assert moved == [('ConfigMap', 'elitea-sandbox-runtime-scripts'), ('DaemonSet', 'elitea-sandbox-runtime-installer')], moved
e = {x['name']: x['value'] for x in eks['DaemonSet', 'elitea-sandbox-runtime-installer']['spec']['template']['spec']['initContainers'][0]['env']}
assert e['MODE'] == 'containerd' and ('RuntimeClass', 'gvisor') not in eks
PY
echo 'sandbox runtime chart: ok'
