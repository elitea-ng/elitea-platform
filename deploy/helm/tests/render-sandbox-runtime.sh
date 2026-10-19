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
echo 'sandbox runtime chart: ok'
