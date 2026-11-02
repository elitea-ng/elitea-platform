#!/usr/bin/env bash
# render-shared-settings.sh — the release-wide settings of #1460 render what
# they promise, and an operator's own value still wins.
#
#   * certManager.issuers: every mTLS Certificate takes its issuer from the
#     shared setting; create=true renders the internal CA chain and a runtime
#     CA Issuer over the EXISTING runtime CA Secret (never a new runtime CA);
#     a block's own issuerRef wins; create with a ClusterIssuer is refused.
#   * global.publicUrl: every derived value, each overridable; a URL with a
#     path is refused.
#   * global.trustedProxyCidrs: the env variable and the auth document.
#   * OTel: the bundled collector by default, OTEL_SDK_DISABLED=true with no
#     collector, global.otelEndpoint, and an explicit env value wins.
#   * the placeholder guard: values-gke.yaml alone is refused with every
#     placeholder listed by path; the shipped defaults are not.
#   * externalSecrets.sharedProperties: one flat property per shared value.
#   * no container or init container requests less than 50m CPU (GKE
#     Autopilot's minimum).
#   * the db-init Job reads both DSNs; the NATS bootstrap Job restarts in place.
#
# Every assertion reads the RENDERED objects.
#
# Usage: deploy/helm/tests/render-shared-settings.sh
# Needs: helm, python3 + PyYAML. No cluster, no network.
set -euo pipefail

cd "$(dirname "$0")/../../.."
CHART=deploy/helm/elitea
BASE=(-f deploy/helm/tests/fixtures/operator-values.yaml)
GKE=(-f "$CHART/values-gke.yaml" -f deploy/helm/tests/fixtures/gke-operator-values.yaml)
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

render() {
  local out="$1"
  shift
  if ! helm template elitea "$CHART" -n elitea "$@" >"$WORK/$out.yaml" 2>"$WORK/$out.err"; then
    cat "$WORK/$out.err" >&2
    fail "$out: did not render"
    return 0
  fi
}

refuse() {
  local description="$1" fragment="$2" out
  shift 2
  if out="$(helm template elitea "$CHART" -n elitea "$@" 2>&1)"; then
    fail "$description: rendered, expected a refusal"
  elif ! grep -qF -- "$fragment" <<<"$out"; then
    fail "$description: refused for the wrong reason: $(tail -3 <<<"$out")"
  else
    echo "ok: refused: $description"
  fi
}

# check NAME FILE PYTHON — run PYTHON with `docs` (every rendered object) and
# helpers in scope; it raises AssertionError with a message on failure.
check() {
  local name="$1" file="$2" code="$3"
  if python3 - "$WORK/$file.yaml" "$code" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]

def objs(kind):
    return [d for d in docs if d.get("kind") == kind]

def one(kind, name):
    found = [d for d in objs(kind) if d["metadata"]["name"] == name]
    assert len(found) == 1, f"{kind}/{name}: {len(found)} rendered"
    return found[0]

def cm(name):
    return one("ConfigMap", name).get("data") or {}

def pods():
    for d in docs:
        spec = (d.get("spec") or {})
        if d.get("kind") == "CronJob":
            spec = spec["jobTemplate"]["spec"]
        tpl = (spec.get("template") or {}).get("spec")
        if tpl:
            yield d, tpl

def env_of(kind, name, container):
    for d, pod in pods():
        if d["kind"] == kind and d["metadata"]["name"] == name:
            for c in (pod.get("containers") or []) + (pod.get("initContainers") or []):
                if c["name"] == container:
                    return {e["name"]: e for e in c.get("env") or []}
    raise AssertionError(f"{kind}/{name} container {container} not rendered")

def issuers():
    return {d["metadata"]["name"]: (d["spec"]["issuerRef"]["name"], d["spec"]["issuerRef"]["kind"])
            for d in objs("Certificate")}

try:
    exec(sys.argv[2])
except AssertionError as e:
    print(e, file=sys.stderr)
    sys.exit(1)
PY
  then echo "ok: $name"; else fail "$name"; fi
}

INTERNAL='["elitea-llm-gateway-server", "elitea-llm-gateway-edge-client", "elitea-deepwiki-server", "elitea-deepwiki-facade-client", "elitea-inventory-server", "elitea-inventory-facade-client"]'
RUNTIME='["elitea-vector-server", "elitea-vector-client", "elitea-main-vector-client", "elitea-vectordb", "elitea-deepwiki-callback-client"]'

echo "== certManager.issuers"
render issuers-default "${BASE[@]}"
check "default: every mTLS Certificate names the shared ClusterIssuers, as before" issuers-default "
got = issuers()
for n in $INTERNAL:
    assert got.get(n) == ('elitea-internal-ca', 'ClusterIssuer'), f'{n}: {got.get(n)}'
for n in $RUNTIME:
    assert got.get(n) == ('elitea-runtime-ca', 'ClusterIssuer'), f'{n}: {got.get(n)}'
assert not [i for i in objs('Issuer') if i['metadata']['name'].startswith(('elitea-internal-ca', 'elitea-runtime-ca'))], 'issuers rendered without create'
"
render issuers-create "${BASE[@]}" --set certManager.issuers.create=true \
  --set-string llmGateway.mtls.issuerRef.name=my-gateway-ca --set-string llmGateway.mtls.issuerRef.kind=ClusterIssuer
check "create: internal CA chain, runtime CA Issuer over the existing Secret, kind Issuer everywhere, a block override wins" issuers-create "
got = issuers()
for n in $INTERNAL:
    want = ('my-gateway-ca', 'ClusterIssuer') if n.startswith('elitea-llm-gateway') else ('elitea-internal-ca', 'Issuer')
    assert got.get(n) == want, f'{n}: {got.get(n)} != {want}'
for n in $RUNTIME:
    assert got.get(n) == ('elitea-runtime-ca', 'Issuer'), f'{n}: {got.get(n)}'
ca = one('Certificate', 'elitea-internal-ca')['spec']
assert ca['isCA'] and ca['issuerRef']['name'] == 'elitea-internal-ca-selfsigned', ca
assert one('Issuer', 'elitea-internal-ca-selfsigned')['spec'] == {'selfSigned': {}}
assert one('Issuer', 'elitea-internal-ca')['spec'] == {'ca': {'secretName': 'elitea-internal-ca'}}
assert one('Issuer', 'elitea-runtime-ca')['spec'] == {'ca': {'secretName': 'elitea-runtime-ca'}}
assert 'elitea-runtime-ca' not in [c['spec']['secretName'] for c in objs('Certificate')], 'the chart must never issue the runtime CA'
"
refuse "create with a ClusterIssuer kind" "creates NAMESPACED Issuers" "${BASE[@]}" \
  --set certManager.issuers.create=true --set-string certManager.issuers.runtime.kind=ClusterIssuer
refuse "an issuer kind that is not Issuer or ClusterIssuer" "/certManager/issuers/internal/kind" "${BASE[@]}" \
  --set-string certManager.issuers.internal.kind=Isuer

echo "== global.publicUrl and global.trustedProxyCidrs"
# The operator fixture states every one of these values; null removes them
# so the derivation has empty targets to fill.
CLEAR=(--set main.env.DEPLOYMENT_URL=null
  --set main.fileConfig.authConfig.document.public_origin=null
  --set main.fileConfig.authConfig.document.trusted_proxy_cidrs=null
  --set worker.runtimeSession.expectedIdentity=null
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=null)
render public "${BASE[@]}" "${CLEAR[@]}" --set-string global.publicUrl=https://elitea.example.test/ \
  --set-string main.env.OIDC_ISSUER_URL=https://idp.example.test \
  --set main.ingress.enabled=true --set-string main.ingress.className=nginx \
  --set networkPolicies.main.noExternalIngress=false \
  --set 'networkPolicies.main.ingressFrom[0].ipBlock.cidr=10.0.0.0/8' \
  --set 'main.ingress.tls[0].secretName=elitea-tls' \
  --set 'global.trustedProxyCidrs={10.0.0.0/8,35.191.0.0/16}'
check "publicUrl fills every derived value; the CIDR list fills both places" public "
m = cm('elitea-main-config')
assert m['DEPLOYMENT_URL'] == 'https://elitea.example.test', m['DEPLOYMENT_URL']
assert m['OIDC_REDIRECT_URI'] == 'https://elitea.example.test/auth/oidc/callback', m['OIDC_REDIRECT_URI']
assert m['ELITEA_TRUSTED_PROXY_CIDRS'] == '10.0.0.0/8,35.191.0.0/16', m['ELITEA_TRUSTED_PROXY_CIDRS']
import json
auth = [v for d in objs('ConfigMap') for k, v in (d.get('data') or {}).items() if k == 'auth-config.json']
assert auth, 'no auth-config.json rendered'
doc = json.loads(auth[0])
assert doc['public_origin'] == 'https://elitea.example.test', doc['public_origin']
assert doc['trusted_proxy_cidrs'] == ['10.0.0.0/8', '35.191.0.0/16'], doc['trusted_proxy_cidrs']
gw = env_of('Deployment', 'elitea-llm-gateway', 'elitea-llm-gateway')
assert gw['GATEWAY_SELF_LLM_ORIGINS']['value'] == 'https://elitea.example.test/llm,http://elitea-main:8080/llm', gw['GATEWAY_SELF_LLM_ORIGINS']
spiffe = 'spiffe://elitea.example.test/ns/elitea/sa/agent-worker'
assert spiffe in m['ELITEA_MODEL_SERVICE_CLIENTS'], m['ELITEA_MODEL_SERVICE_CLIENTS']
rt = json.loads(cm('elitea-worker-runtime')['runtime.json'])
assert rt.get('public_base_url') == 'https://elitea.example.test', rt.get('public_base_url')
ing = one('Ingress', 'elitea-main')['spec']
assert [r['host'] for r in ing['rules']] == ['elitea.example.test'], ing['rules']
assert ing['tls'] == [{'secretName': 'elitea-tls', 'hosts': ['elitea.example.test']}], ing['tls']
"
render public-explicit "${BASE[@]}" "${CLEAR[@]}" --set-string global.publicUrl=https://elitea.example.test \
  --set-string main.env.DEPLOYMENT_URL=https://other.example.test \
  --set-string worker.runtimeSession.expectedIdentity=spiffe://custom.test/ns/x/sa/agent-worker \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://other.example.test/llm/v1
check "an explicit value wins over global.publicUrl" public-explicit "
m = cm('elitea-main-config')
assert m['DEPLOYMENT_URL'] == 'https://other.example.test', m['DEPLOYMENT_URL']
assert 'spiffe://custom.test/ns/x/sa/agent-worker' in m['ELITEA_MODEL_SERVICE_CLIENTS'], m['ELITEA_MODEL_SERVICE_CLIENTS']
gw = env_of('Deployment', 'elitea-llm-gateway', 'elitea-llm-gateway')
assert gw['GATEWAY_SELF_LLM_ORIGINS']['value'] == 'https://other.example.test/llm/v1'
"
refuse "a publicUrl with a path" "/global/publicUrl" "${BASE[@]}" --set-string global.publicUrl=https://elitea.example.test/app

echo "== OpenTelemetry defaults"
render otel-on "${BASE[@]}" --set-string scheduler.env.OTEL_EXPORTER_OTLP_ENDPOINT=http://mine:4318
check "otelCollector on: every component exports to it; an explicit value wins" otel-on "
want = 'http://elitea-otel-collector:4318'
assert cm('elitea-main-config')['OTEL_EXPORTER_OTLP_ENDPOINT'] == want
assert cm('elitea-scheduler-config')['OTEL_EXPORTER_OTLP_ENDPOINT'] == 'http://mine:4318'
for kind, name, c in [('Deployment', 'elitea-llm-gateway', 'elitea-llm-gateway'), ('Deployment', 'elitea-worker', 'worker'),
                      ('Deployment', 'elitea-vector', 'elitea-vector')]:
    e = env_of(kind, name, c)
    assert e.get('OTEL_EXPORTER_OTLP_ENDPOINT', {}).get('value') == want, f'{name}: {e.get(\"OTEL_EXPORTER_OTLP_ENDPOINT\")}'
"
render otel-off "${BASE[@]}" --set otelCollector.enabled=false
check "otelCollector off and no endpoint: OTEL_SDK_DISABLED=true, never localhost" otel-off "
m = cm('elitea-main-config')
assert m['OTEL_SDK_DISABLED'] == 'true' and not m.get('OTEL_EXPORTER_OTLP_ENDPOINT'), m.get('OTEL_SDK_DISABLED')
assert env_of('Deployment', 'elitea-worker', 'worker')['OTEL_SDK_DISABLED']['value'] == 'true'
"
render otel-global "${BASE[@]}" --set otelCollector.enabled=false --set-string global.otelEndpoint=http://collector.obs:4318
check "global.otelEndpoint reaches every component" otel-global "
assert cm('elitea-main-config')['OTEL_EXPORTER_OTLP_ENDPOINT'] == 'http://collector.obs:4318'
assert env_of('Deployment', 'elitea-worker', 'worker')['OTEL_EXPORTER_OTLP_ENDPOINT']['value'] == 'http://collector.obs:4318'
"

echo "== placeholders"
refuse "values-gke.yaml with its placeholders" "main.env.OIDC_CLIENT_ID: \"<client id>\"" -f "$CHART/values-gke.yaml"
refuse "a placeholder inside a list" "global.trustedProxyCidrs[0]: \"<pod CIDR>\"" "${BASE[@]}" --set 'global.trustedProxyCidrs={<pod CIDR>}'

echo "== externalSecrets.sharedProperties and the GKE example"
render gke "${GKE[@]}"
check "GKE: one flat property per shared value; the runtime CA Secret is an ExternalSecret; chart-made issuers" gke "
props = {}
for es in objs('ExternalSecret'):
    for item in es['spec'].get('data', []):
        props[(es['metadata']['name'], item['secretKey'])] = item['remoteRef'].get('property')
want = {
    ('elitea-runtime-material', 'runtime-ca.crt'): 'runtime-ca.crt',
    ('elitea-worker-material', 'runtime-ca.crt'): 'runtime-ca.crt',
    ('elitea-runtime-ca', 'tls.crt'): 'runtime-ca.crt',
    ('elitea-runtime-ca', 'tls.key'): 'runtime-ca.key',
    ('elitea-runtime-material', 'command-signing-keyring.json'): 'command-signing-keyring.json',
    ('elitea-worker-material', 'command-signing-keyring.json'): 'command-signing-keyring.json',
    ('elitea-llm-gateway-secrets', 'identity-secret'): 'gateway-identity-secret',
    ('elitea-main-llm-gateway-secrets', 'gateway-identity-secret'): 'gateway-identity-secret',
    ('elitea-main-sso', 'oidc-client-secret'): 'elitea-main-sso/oidc-client-secret',
}
for k, v in want.items():
    assert props.get(k) == v, f'{k}: {props.get(k)} != {v}'
assert one('ExternalSecret', 'elitea-runtime-ca')['spec']['target']['template']['type'] == 'kubernetes.io/tls'
assert set(issuers().values()) <= {('elitea-internal-ca', 'Issuer'), ('elitea-runtime-ca', 'Issuer'), ('elitea-internal-ca-selfsigned', 'Issuer'), ('elitea-nats-ca', 'Issuer'), ('elitea-nats-ca-selfsigned', 'Issuer')}, set(issuers().values())
"

echo "== resources, db-init, NATS bootstrap"
check "no container or init container requests less than 50m CPU (GKE Autopilot)" issuers-default "
def millis(v):
    v = str(v)
    return float(v[:-1]) if v.endswith('m') else float(v) * 1000
low = []
for d, pod in pods():
    for c in (pod.get('containers') or []) + (pod.get('initContainers') or []):
        cpu = ((c.get('resources') or {}).get('requests') or {}).get('cpu')
        if cpu is not None and millis(cpu) < 50:
            low.append(f'{d[\"kind\"]}/{d[\"metadata\"][\"name\"]}/{c[\"name\"]}={cpu}')
assert not low, low
"
check "db-init reads the platform and agent-state DSNs; NATS bootstrap restarts in place" issuers-default "
e = env_of('Job', 'elitea-db-init', 'db-init')
assert e['APP_DSN']['valueFrom']['secretKeyRef'] == {'name': 'elitea-main-db', 'key': 'database-url', 'optional': True}, e['APP_DSN']
assert e['AGENT_DSN']['valueFrom']['secretKeyRef'] == {'name': 'elitea-worker-material', 'key': 'agent-checkpoint-connection', 'optional': True}, e['AGENT_DSN']
assert e['AGENT_STATE_DB']['value'] == '' and e['APP_ROLE']['value'] == ''
for d, pod in pods():
    if d['kind'] == 'Job' and d['metadata']['name'] == 'elitea-nats-bootstrap':
        assert pod['restartPolicy'] == 'OnFailure', pod['restartPolicy']
        break
else:
    raise AssertionError('no nats-bootstrap Job')
"

if [ "$failures" -ne 0 ]; then
  echo "render-shared-settings: $failures failure(s)" >&2
  exit 1
fi
echo "render-shared-settings: OK"
