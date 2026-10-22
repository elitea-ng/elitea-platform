#!/usr/bin/env bash
# render-default-on.sh — the shipped defaults (S10): the runtime plane, the
# agent worker, elitea-vector with the bundled Qdrant, DeepWiki and Inventory
# are ON, each is ONE switch, and an install that is missing what only the
# operator can supply is refused with ONE message listing all of it.
#
# Asserts, on rendered output:
#
#   default    values.yaml plus fixtures/operator-values.yaml (the operator's
#              values and nothing else) renders every feature; elitea-main's
#              DeepWiki/Inventory/vector wiring is DERIVED from the provider
#              blocks; the callback client and Qdrant certificates come from
#              the runtime-CA issuer (no hand-minted certificate); the bundled
#              Qdrant serves TLS; every Secret has its fixed default name
#   secrets    with externalSecrets on, an ExternalSecret renders for each
#              fixed Secret name and none for a cert-manager-issued one
#   one-switch each feature turns off with its own switch (worker, DeepWiki,
#              Inventory; vector together with what stands on it; the
#              runtime together with what stands on it), and turning off a
#              prerequisite alone is refused with ONE message naming each
#              dependant and its switch
#   missing    the chart's defaults alone are refused with ONE message that
#              lists every value only the operator can supply, including the
#              runtime authentication's public origin
#   qdrant     qdrant.enabled=false with an external URL; values-qdrant-ha.yaml
#
# Run deploy/helm/elitea/build-deps.sh first (CI does). Needs helm, python3 +
# PyYAML.
set -euo pipefail

DIR="$(cd "$(dirname "$0")/../.." && pwd)"   # deploy/
CHART="$DIR/helm/elitea"
OPERATOR="$DIR/helm/tests/fixtures/operator-values.yaml"
HELM="${HELM:-helm}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }
ok() { echo "ok:   $*"; }

render() {  # render <name> helm-args... -> $TMP/<name>.yaml, or $TMP/<name>.err
  local name="$1"; shift
  "$HELM" template elitea "$CHART" -n elitea "$@" > "$TMP/$name.yaml" 2> "$TMP/$name.err"
}

# ── default ─────────────────────────────────────────────────────────────────
if render default -f "$OPERATOR"; then
  ok "the defaults plus the operator's values render"
else
  fail "the defaults plus the operator's values do not render: $(grep -v '^$' "$TMP/default.err" | head -5)"
fi
render secrets -f "$OPERATOR" --set externalSecrets.enabled=true \
  --set-string externalSecrets.secretStoreRef.name=store --set-string externalSecrets.remoteKey=elitea-platform \
  || fail "the defaults with externalSecrets do not render: $(grep -v '^$' "$TMP/secrets.err" | head -5)"

python3 - "$TMP" <<'PY' || fails=$((fails + 1))
import sys, yaml, pathlib, re
t = pathlib.Path(sys.argv[1])
def load(n):
    p = t / n
    return [d for d in yaml.safe_load_all(p.read_text()) if d] if p.exists() and p.stat().st_size else []
docs = load("default.yaml")
idx = {(d["kind"], d["metadata"]["name"]): d for d in docs}
bad = []
def need(kind, name):
    d = idx.get((kind, name))
    if d is None:
        bad.append(f"default: no {kind}/{name}")
    return d or {}

# Every feature renders.
for kind, name in [("Deployment", "elitea-main"), ("Deployment", "elitea-worker"),
                   ("Deployment", "elitea-vector"), ("StatefulSet", "elitea-vectordb"),
                   ("Deployment", "elitea-deepwiki"), ("Deployment", "elitea-inventory")]:
    need(kind, name)

# elitea-main's wiring is derived from the provider blocks (one switch each).
cm = need("ConfigMap", "elitea-main-config").get("data", {})
want_env = {
    "ELITEA_RUNTIME_ENABLED": "true",
    "ELITEA_DEEPWIKI_ENABLED": "true",
    "ELITEA_DEEPWIKI_CLIENT_CERT_FILE": "/run/elitea-deepwiki/tls.crt",
    "ELITEA_DEEPWIKI_CLIENT_KEY_FILE": "/run/elitea-deepwiki/tls.key",
    "ELITEA_DEEPWIKI_CA_FILE": "/run/elitea-deepwiki/ca.crt",
    "ELITEA_DEEPWIKI_GIT_ALLOWLIST": "github.com,*.github.com",
    "ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET": "elitea-deepwiki-svc:9443",
    "ELITEA_INVENTORY_ENABLED": "true",
    "ELITEA_INVENTORY_CLIENT_CERT_FILE": "/run/elitea-inventory/tls.crt",
    "ELITEA_INVENTORY_CLIENT_KEY_FILE": "/run/elitea-inventory/tls.key",
    "ELITEA_INVENTORY_CA_FILE": "/run/elitea-inventory/ca.crt",
    "ELITEA_INVENTORY_GIT_ALLOWLIST": "github.com,*.github.com",
    "ELITEA_VECTOR_GRPC_TARGET": "elitea-vector:9470",
    "ELITEA_VECTOR_INTROSPECTION_CLIENTS": "dns:elitea-vector",
}
for k, v in want_env.items():
    if cm.get(k) != v:
        bad.append(f"default: elitea-main-config {k} is {cm.get(k)!r}, want {v!r}")

# The main pod mounts the derived client material from the facade Certificates.
main_pod = need("Deployment", "elitea-main").get("spec", {}).get("template", {}).get("spec", {})
vols = {v["name"]: (v.get("secret") or {}).get("secretName") for v in main_pod.get("volumes", [])}
for vol, secret in [("deepwiki-client-material", "elitea-main-deepwiki-client-tls"),
                    ("inventory-client-material", "elitea-main-inventory-client-tls"),
                    ("vector-client-material", "elitea-main-vector-client-tls")]:
    if vols.get(vol) != secret:
        bad.append(f"default: elitea-main volume {vol} mounts {vols.get(vol)!r}, want {secret!r}")

# No hand-minted certificate: the callback clients and Qdrant come from the
# runtime-CA issuer.
for name, secret in [("elitea-deepwiki-callback-client", "elitea-deepwiki-callback-client"),
                     ("elitea-inventory-callback-client", "elitea-inventory-callback-client"),
                     ("elitea-vectordb", "elitea-qdrant-tls")]:
    c = need("Certificate", name).get("spec", {})
    if c and (c.get("secretName") != secret or c.get("issuerRef", {}).get("name") != "elitea-runtime-ca"):
        bad.append(f"default: Certificate/{name} secret {c.get('secretName')!r} issuer {c.get('issuerRef')!r}")
cb = idx.get(("Certificate", "elitea-deepwiki-callback-client"), {}).get("spec", {})
if cb and (cb.get("commonName") != "elitea-subapp-callback" or cb.get("dnsNames") != ["elitea-subapp-callback"]
           or "client auth" not in cb.get("usages", [])):
    bad.append(f"default: the callback client certificate is not the dns:elitea-subapp-callback client identity: {cb}")

# The bundled Qdrant serves TLS, reads the fixed API-key Secret, and
# elitea-vector dials it over https trusting the runtime CA.
sts = need("StatefulSet", "elitea-vectordb").get("spec", {}).get("template", {}).get("spec", {})
qc = (sts.get("containers") or [{}])[0]
key = {e["name"]: e for e in qc.get("env", [])}.get("QDRANT__SERVICE__API_KEY", {})
if (key.get("valueFrom") or {}).get("secretKeyRef") != {"name": "elitea-qdrant-api", "key": "api-key"}:
    bad.append(f"default: Qdrant's API key is not elitea-qdrant-api/api-key: {key}")
if (qc.get("readinessProbe") or {}).get("httpGet", {}).get("scheme") != "HTTPS":
    bad.append("default: Qdrant's probes are not HTTPS (config.service.enable_tls off?)")
qconf = yaml.safe_load(need("ConfigMap", "elitea-vectordb").get("data", {}).get("production.yaml", "{}") or "{}")
if not (qconf.get("service") or {}).get("enable_tls") or (qconf.get("tls") or {}).get("cert") != "/qdrant/tls/tls.crt":
    bad.append(f"default: Qdrant's config is not TLS: {qconf}")
vec = need("Deployment", "elitea-vector").get("spec", {}).get("template", {}).get("spec", {})
venv = {e["name"]: e.get("value") for e in (vec.get("containers") or [{}])[0].get("env", [])}
if venv.get("ELITEA_VECTOR_QDRANT_URL") != "https://elitea-vectordb.elitea.svc:6334":
    bad.append(f"default: ELITEA_VECTOR_QDRANT_URL is {venv.get('ELITEA_VECTOR_QDRANT_URL')!r}")
if venv.get("SSL_CERT_FILE") != "/etc/elitea-vector/tls/ca.crt":
    bad.append(f"default: elitea-vector's SSL_CERT_FILE is {venv.get('SSL_CERT_FILE')!r}")

# The fixed Secret names, as the workloads mount them.
text = (t / "default.yaml").read_text()
for secret in ("elitea-runtime-material", "elitea-worker-material", "elitea-main-auth-material",
               "elitea-qdrant-api", "elitea-deepwiki-secrets", "elitea-inventory-secrets", "elitea-main-db"):
    if not re.search(rf"\b{re.escape(secret)}\b", text):
        bad.append(f"default: nothing reads the fixed Secret {secret}")

# externalSecrets: one ExternalSecret per fixed name, none for what cert-manager issues.
es = {d["metadata"]["name"] for d in load("secrets.yaml") if d["kind"] == "ExternalSecret"}
for secret in ("elitea-runtime-material", "elitea-worker-material", "elitea-main-auth-material",
               "elitea-qdrant-api", "elitea-deepwiki-secrets", "elitea-inventory-secrets",
               "elitea-main-db", "elitea-main-llm-gateway-secrets"):
    if secret not in es:
        bad.append(f"secrets: no ExternalSecret for {secret}")
for issued in ("elitea-qdrant-tls", "elitea-deepwiki-callback-client", "elitea-inventory-callback-client",
               "elitea-main-deepwiki-client-tls", "elitea-vector-server-tls"):
    if issued in es:
        bad.append(f"secrets: an ExternalSecret for {issued}, which cert-manager issues")

for b in bad: print("FAIL:", b)
if not bad:
    print("ok:   every feature renders; elitea-main's wiring is derived; certificates come from the runtime CA; Qdrant is TLS")
    print("ok:   an ExternalSecret for every fixed Secret name, none for a cert-manager-issued one")
sys.exit(1 if bad else 0)
PY

# ── one switch per feature ──────────────────────────────────────────────────
renders() {  # renders "<what>" helm-args...
  local what="$1"; shift
  if render off -f "$OPERATOR" "$@"; then ok "renders: $what"; else
    fail "does not render: $what: $(grep -v '^$' "$TMP/off.err" | head -4)"; fi
}
absent() {  # absent "<what>" <kind> <name>: not in the last `renders`
  python3 - "$TMP/off.yaml" "$2" "$3" <<'PY' && ok "$1" || fail "$1"
import sys, yaml
sys.exit(1 if any(d and d["kind"] == sys.argv[2] and d["metadata"]["name"] == sys.argv[3]
                  for d in yaml.safe_load_all(open(sys.argv[1]))) else 0)
PY
}
renders "worker.enabled=false" --set worker.enabled=false
absent "  ... and no worker Deployment" Deployment elitea-worker
renders "deepwiki.enabled=false" --set deepwiki.enabled=false
absent "  ... and no DeepWiki Deployment" Deployment elitea-deepwiki
# (main.env ships the DeepWiki keys EMPTY; off, the chart fills none of them.)
grep -qE 'ELITEA_DEEPWIKI_(ENABLED: "true"|CLIENT_CERT_FILE: "/|GIT_ALLOWLIST: "[^"])' "$TMP/off.yaml" \
  && fail "  ... but elitea-main still gets DeepWiki wiring" || ok "  ... and elitea-main gets no DeepWiki wiring"
renders "inventory.enabled=false" --set inventory.enabled=false
absent "  ... and no Inventory Deployment" Deployment elitea-inventory
renders "vector off with what stands on it" --set vector.enabled=false --set qdrant.enabled=false \
  --set main.runtime.indexIngestDispatch.enabled=false --set deepwiki.enabled=false --set inventory.enabled=false
absent "  ... and no Qdrant" StatefulSet elitea-vectordb
renders "the runtime off with what stands on it" --set main.runtime.enabled=false --set worker.enabled=false \
  --set vector.enabled=false --set qdrant.enabled=false --set deepwiki.enabled=false --set inventory.enabled=false
grep -q 'ELITEA_RUNTIME_ENABLED: "true"' "$TMP/off.yaml" && fail "  ... but the runtime env still renders" || ok "  ... and the runtime plane is dark"

refused() {  # refused "<what>" "<fragment>"... -- helm-args...
  local what="$1"; shift
  local frags=()
  while [ "$1" != "--" ]; do frags+=("$1"); shift; done; shift
  if render refused -f "$OPERATOR" "$@"; then fail "rendered: $what"; return; fi
  local f missing=""
  for f in "${frags[@]}"; do grep -qF -- "$f" "$TMP/refused.err" || missing="$missing [$f]"; done
  if [ -n "$missing" ]; then fail "$what: refused, but the message lacks$missing: $(head -8 "$TMP/refused.err")"
  elif [ "$(grep -c 'this install cannot render yet' "$TMP/refused.err")" != 1 ]; then fail "$what: not ONE message"
  else ok "refuses $what, naming each dependant"; fi
}
refused "the runtime off alone" \
  "worker.enabled needs main.runtime.enabled" "vector.enabled needs main.runtime.enabled" \
  "deepwiki.enabled needs main.runtime.enabled" "inventory.enabled needs main.runtime.enabled" \
  -- --set main.runtime.enabled=false
refused "vector off alone" \
  "main.runtime.indexIngestDispatch.enabled=false" "set deepwiki.enabled=false" "set inventory.enabled=false" \
  "set qdrant.enabled=false too" \
  -- --set vector.enabled=false

# ── missing values: one message ─────────────────────────────────────────────
if render missing; then
  fail "the chart's defaults alone rendered; an install without the operator's values must be refused"
else
  missing=""
  for f in "this install cannot render yet." \
           "main.fileConfig.authConfig:" "public_origin" "platform.aiProjectId:" "main.env.DEPLOYMENT_URL:" \
           "worker.runtimeSession.expectedIdentity:" "deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST:" \
           "inventory.env.ELITEA_INVENTORY_GIT_ALLOWLIST:" "networkPolicies.main.ingressFrom:" \
           "llmGateway.env.GATEWAY_SELF_LLM_ORIGINS:" "llmGateway.egressPosture must be one of" \
           "docs/UPGRADING.md lists them"; do
    grep -qF -- "$f" "$TMP/missing.err" || missing="$missing [$f]"
  done
  if [ -n "$missing" ]; then fail "the missing-values message lacks$missing"
  elif [ "$(grep -c '^  - ' "$TMP/missing.err")" != 9 ]; then
    fail "the missing-values message lists $(grep -c '^  - ' "$TMP/missing.err") values, want 9: $(cat "$TMP/missing.err")"
  else ok "the chart's defaults alone are refused with ONE message listing all 9 operator values"; fi
fi
# With authentication but no public origin, the origin is named on its own.
if render noorigin -f "$OPERATOR" --set main.fileConfig.authConfig.document.public_origin=; then
  fail "an authentication document without public_origin rendered"
elif grep -qF "main.fileConfig.authConfig.document.public_origin:" "$TMP/noorigin.err"; then
  ok "an authentication document without public_origin is refused by name"
else
  fail "no public_origin: wrong message: $(head -5 "$TMP/noorigin.err")"
fi

# ── qdrant: bring your own, and HA ──────────────────────────────────────────
if render ext -f "$OPERATOR" --set qdrant.enabled=false \
     --set-string vector.qdrant.external.url=https://qdrant.example.invalid:6334; then
  if grep -q 'elitea-vectordb' "$TMP/ext.yaml"; then fail "qdrant.enabled=false still renders the bundled Qdrant"
  elif grep -q 'https://qdrant.example.invalid:6334' "$TMP/ext.yaml"; then ok "qdrant.enabled=false: no bundled Qdrant, elitea-vector dials the external URL"
  else fail "qdrant.enabled=false: the external URL does not reach elitea-vector"; fi
else
  fail "qdrant.enabled=false with an external URL does not render: $(head -4 "$TMP/ext.err")"
fi
if render ha -f "$OPERATOR" -f "$CHART/values-qdrant-ha.yaml"; then
  if grep -q 'enable_tls: true' "$TMP/ha.yaml" && grep -q '^  replicas: 3$' "$TMP/ha.yaml"; then
    ok "values-qdrant-ha.yaml: three nodes with peer TLS"
  else fail "values-qdrant-ha.yaml does not render three nodes with TLS"; fi
else
  fail "values-qdrant-ha.yaml does not render: $(head -4 "$TMP/ha.err")"
fi

if [ "$fails" -ne 0 ]; then echo "$fails check(s) failed"; exit 1; fi
echo "render-default-on: all checks pass"
