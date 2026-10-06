#!/usr/bin/env bash
# render-nats-security.sh — the NATS security posture (#1076), asserted on the
# RENDERED output of all three charts that have to agree about it:
#
#   deploy/helm/nats            the server: dedicated CA, server certificate,
#                               TLS + verify_and_map, the permission table,
#                               the ingress NetworkPolicy, its refusals
#   deploy/helm/nats-bootstrap  the bootstrap identity and its connection
#   deploy/helm/elitea          one client certificate per component, the
#                               mounts and env that point at it, tls:// URLs,
#                               and the URL refusals
#
# The agreement is the point. Each chart can be internally fine while the
# certificate a component presents names no user, is signed by an issuer the
# server does not trust, or comes from a pod the NetworkPolicy does not admit.
# Every one of those installs cleanly and then fails every connection.
#
# What this does NOT prove: that a grant in the permission table is the right
# one. libs/go/natsconn/natstest runs every service's real code against the
# rendered config for that (ci-go.yml, ci-gateway.yml).
#
# Needs helm, python3 + PyYAML, and network access the first time (the
# upstream nats subchart is vendored with `helm dependency build`).
set -euo pipefail

DIR="$(cd "$(dirname "$0")/../.." && pwd)"   # deploy/
HELM="${HELM:-helm}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if ! ls "$DIR"/helm/nats/charts/nats-*.tgz >/dev/null 2>&1; then
  "$HELM" dependency build "$DIR/helm/nats" >/dev/null
fi

NS=elitea
"$HELM" template elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-scale1.yaml" > "$TMP/nats-scale1.yaml"
"$HELM" template elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-ha.yaml"     > "$TMP/nats-ha.yaml"
"$HELM" template elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" -n "$NS"                    > "$TMP/bootstrap.yaml"
"$HELM" template elitea "$DIR/helm/elitea" -n "$NS" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted > "$TMP/elitea.yaml"

# Refusals: each render below MUST fail, with the reason named.
refuse() {
  local name="$1" want="$2"; shift 2
  if "$HELM" template "$@" > /dev/null 2> "$TMP/refusal.err"; then
    echo "REFUSAL-NOT-RAISED	$name" >> "$TMP/refusals"
  elif grep -q -- "$want" "$TMP/refusal.err"; then
    echo "ok	$name" >> "$TMP/refusals"
  else
    echo "WRONG-REASON	$name	$(tail -1 "$TMP/refusal.err")" >> "$TMP/refusals"
  fi
}
: > "$TMP/refusals"
EL=(elitea "$DIR/helm/elitea" -n "$NS"
    --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
    --set-string llmGateway.egressPosture=public-unrestricted)
NA=(elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-scale1.yaml")
refuse "nats: verify_and_map off"             "verify_and_map must be true"  "${NA[@]}" --set nats.config.nats.tls.merge.verify_and_map=false
refuse "nats: TLS off"                        "tls.enabled is false"         "${NA[@]}" --set nats.config.nats.tls.enabled=false
refuse "nats: no_auth_user"                   "no_auth_user"                 "${NA[@]}" --set nats.config.merge.no_auth_user=anyone
refuse "nats: allow_non_tls"                  "allow_non_tls"                "${NA[@]}" --set nats.config.merge.allow_non_tls=true
refuse "nats: NetworkPolicy off, unstated"    "networkPolicy.enabled is false" "${NA[@]}" --set networkPolicy.enabled=false
refuse "nats: HA routes in plaintext"         "cluster.tls.enabled false"    elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-ha.yaml" --set nats.config.cluster.tls.enabled=false
refuse "bootstrap: nats:// URL"               "is not tls://"                elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" --set natsUrl=nats://elitea-nats:4222
refuse "bootstrap: credential URL"            "user information"             elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" --set natsUrl=tls://u:p@elitea-nats:4222
refuse "elitea: plaintext unacknowledged"     "allowPlaintext"               "${EL[@]}" --set nats.tls.enabled=false
refuse "elitea: nats:// gateway URL"          "not tls://"                   "${EL[@]}" --set-string llmGateway.env.GATEWAY_NATS_URL=nats://elitea-nats:4222
refuse "elitea: nats:// main URL"             "not tls://"                   "${EL[@]}" --set-string main.env.ELITEA_EVENTS_NATS_URL=nats://elitea-nats:4222
refuse "elitea: credential in a scheduler URL" "user information"            "${EL[@]}" --set-string scheduler.env.GATEWAY_NATS_URL=tls://u:p@elitea-nats:4222
refuse "elitea: tls:// with TLS off"          "tls:// but nats.tls.enabled is false" "${EL[@]}" --set nats.tls.enabled=false --set nats.tls.allowPlaintext=true --set-string llmGateway.env.GATEWAY_NATS_URL=tls://elitea-nats:4222
# And the one acknowledged way out renders.
if "$HELM" template "${EL[@]}" --set nats.tls.enabled=false --set nats.tls.allowPlaintext=true > "$TMP/plain.yaml" 2> "$TMP/plain.err"; then
  echo "ok	elitea: acknowledged plaintext renders" >> "$TMP/refusals"
else
  echo "REFUSED-THE-ESCAPE-HATCH	elitea: acknowledged plaintext	$(tail -1 "$TMP/plain.err")" >> "$TMP/refusals"
fi

python3 - "$TMP" "$NS" "$DIR/argocd/applications" <<'PY'
import json, pathlib, re, sys, yaml

tmp, ns, apps = pathlib.Path(sys.argv[1]), sys.argv[2], pathlib.Path(sys.argv[3])
results = []

def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))

def docs(f):
    return [d for d in yaml.safe_load_all(open(tmp / f)) if d]

def conf_json(text):
    # The upstream chart renders JSON with unquoted `<< >>` values and
    # include directives; quote the one variable so json can read the rest.
    text = re.sub(r':\s*\$SERVER_NAME', ': "$SERVER_NAME"', text)
    text = re.sub(r':\s*(\d+)(Gi|Mi|Ki)\b', r': "\1\2"', text)
    return json.loads(text)

IDS = ["elitea-main", "elitea-llm-gateway", "elitea-scheduler", "elitea-nats-bootstrap", "elitea-worker"]
URI = lambda i: f"spiffe://elitea.internal/nats/{i}"

# ── the server ────────────────────────────────────────────────────────────
scale1, ha = docs("nats-scale1.yaml"), docs("nats-ha.yaml")
def nats_conf(d):
    cm = [x for x in d if x["kind"] == "ConfigMap" and "nats.conf" in x.get("data", {})]
    return conf_json(cm[0]["data"]["nats.conf"])
c1, c3 = nats_conf(scale1), nats_conf(ha)

for label, c in (("scale-1", c1), ("HA", c3)):
    tls = c.get("tls", {})
    check(f"{label}: client port TLS with verify_and_map and the NATS CA", tls.get("verify_and_map") is True and tls.get("ca_file", "").endswith("ca.crt") and tls.get("cert_file"))
    check(f"{label}: no no_auth_user, no allow_non_tls, no accounts", not any(k in c for k in ("no_auth_user", "allow_non_tls", "accounts")))
    check(f"{label}: http monitor stays on 8222 (exporter over localhost, kubelet probes)", c.get("http_port") == 8222)
check("both profiles carry the SAME permission table", c1["authorization"] == c3["authorization"])
users = {u["user"]: u.get("permissions", {}) for u in c1["authorization"]["users"]}
check("the permission table names exactly the five identities", set(users) == {URI(i) for i in IDS}, sorted(users))
for i in IDS:
    sub = users.get(URI(i), {}).get("subscribe", {}).get("allow", [])
    check(f"{i} subscribes to its own inbox prefix", f"_INBOX_{i}.>" in sub, sub)
    others = [s for s in sub if s.startswith("_INBOX")  and s != f"_INBOX_{i}.>"]
    check(f"{i} subscribes to no other inbox", not others, others)
admin = re.compile(r"^\$JS\.API\.(STREAM\.(CREATE|UPDATE|DELETE|PURGE)|ACCOUNT\.PURGE)")
for i in IDS:
    pub = users.get(URI(i), {}).get("publish", {}).get("allow", [])
    bad = [p for p in pub if admin.match(p) or p in (">", "$JS.API.>")]
    if i == "elitea-nats-bootstrap":
        check("only the bootstrap may create, update, delete or purge a stream", any(p.startswith("$JS.API.STREAM.CREATE") for p in pub))
    else:
        check(f"{i} may not administer a stream", not bad, bad)
cl = c3.get("cluster", {})
check("HA: routes are tls:// with peer verification", cl.get("tls", {}).get("verify") is True and all(r.startswith("tls://") for r in cl.get("routes", [])))

def kinds(d, k):
    return [x for x in d if x["kind"] == k]
issuers = {x["metadata"]["name"]: x["spec"] for x in kinds(scale1, "Issuer")}
check("the dedicated NATS CA chain renders (selfSigned -> CA cert -> CA Issuer)", "elitea-nats-ca-selfsigned" in issuers and "ca" in issuers.get("elitea-nats-ca", {}))
ca_certs = [x for x in kinds(scale1, "Certificate") if x["spec"].get("isCA")]
check("the CA is its own, not elitea-internal-ca", ca_certs and ca_certs[0]["spec"]["secretName"] == "elitea-nats-ca")
srv = [x for x in kinds(scale1, "Certificate") if not x["spec"].get("isCA")]
check("the server certificate is issued by the NATS CA Issuer", srv and srv[0]["spec"]["issuerRef"]["name"] == "elitea-nats-ca")
fqdn = f"elitea-nats.{ns}.svc.cluster.local"
check("the server certificate names the client URL host", srv and fqdn in srv[0]["spec"]["dnsNames"])
srv_ha = [x for x in kinds(ha, "Certificate") if not x["spec"].get("isCA")]
check("HA: the server certificate covers the routes and is usable as a route client", srv_ha and "*.elitea-nats-headless" in srv_ha[0]["spec"]["dnsNames"] and "client auth" in srv_ha[0]["spec"]["usages"])
sts = kinds(scale1, "StatefulSet")[0]
mounted = {v.get("secret", {}).get("secretName") for v in sts["spec"]["template"]["spec"]["volumes"]}
check("the server mounts the certificate Secret it is issued into", srv[0]["spec"]["secretName"] in mounted)

pod_labels = sts["spec"]["template"]["metadata"]["labels"]
for label, d in (("scale-1", scale1), ("HA", ha)):
    nps = kinds(d, "NetworkPolicy")
    check(f"{label}: an ingress NetworkPolicy renders", len(nps) == 1)
    if not nps:
        continue
    np = nps[0]["spec"]
    sel = np["podSelector"]["matchLabels"]
    check(f"{label}: it selects the NATS pods", all(pod_labels.get(k) == v for k, v in sel.items()))
    rules = {r["ports"][0]["port"]: r for r in np["ingress"]}
    names = {p["podSelector"]["matchLabels"]["app.kubernetes.io/name"] for p in rules.get(4222, {}).get("from", []) if "podSelector" in p}
    check(f"{label}: 4222 admits exactly the four clients", names == {"elitea-main", "elitea-llm-gateway", "elitea-scheduler", "nats-bootstrap"}, sorted(names))
    check(f"{label}: 8222 (monitoring) has no ingress rule", 8222 not in rules)
    if label == "HA":
        check("HA: 6222 is admitted only from the NATS pods", 6222 in rules and all("namespaceSelector" not in p for p in rules[6222]["from"]))

# ── the bootstrap ─────────────────────────────────────────────────────────
bs = docs("bootstrap.yaml")
bcert = kinds(bs, "Certificate")
check("the bootstrap's certificate carries the bootstrap user's URI SAN", bcert and bcert[0]["spec"]["uris"] == [URI("elitea-nats-bootstrap")])
check("the bootstrap's certificate comes from the NATS CA Issuer", bcert and bcert[0]["spec"]["issuerRef"]["name"] == "elitea-nats-ca")
job = kinds(bs, "Job")[0]["spec"]["template"]["spec"]
env = {e["name"]: e.get("value") for e in job["containers"][0]["env"]}
check("the bootstrap dials tls:// with no credential", env.get("NATS_URL", "").startswith("tls://") and "@" not in env.get("NATS_URL", ""))
check("the bootstrap uses its own inbox prefix", env.get("NATS_INBOX_PREFIX") == "_INBOX_elitea-nats-bootstrap")
check("the bootstrap Job pod is what the NetworkPolicy admits", kinds(bs, "Job")[0]["spec"]["template"]["metadata"]["labels"].get("app.kubernetes.io/name") == "nats-bootstrap")
bvols = {v["name"]: v for v in job["volumes"]}
check("the bootstrap mounts its certificate Secret", bvols.get("nats-client-tls", {}).get("secret", {}).get("secretName") == bcert[0]["spec"]["secretName"])

# ── the platform ──────────────────────────────────────────────────────────
el = docs("elitea.yaml")
certs = {c["spec"]["commonName"]: c for c in kinds(el, "Certificate") if c["spec"].get("uris")}
want = {"elitea-main", "elitea-llm-gateway", "elitea-scheduler"}
check("the platform issues a NATS certificate for each client", set(certs) == want, sorted(certs))
for ident, c in certs.items():
    check(f"{ident}: URI SAN names its user in the permission table", c["spec"]["uris"] == [URI(ident)] and URI(ident) in users)
    check(f"{ident}: issued by the NATS CA Issuer, not elitea-internal-ca", c["spec"]["issuerRef"]["name"] == "elitea-nats-ca")
deps = {d["metadata"]["name"]: d for d in kinds(el, "Deployment")}
cms = {d["metadata"]["name"]: d.get("data", {}) for d in kinds(el, "ConfigMap")}
prefix = {"elitea-main": ("ELITEA_EVENTS", "elitea-main-config", "ELITEA_EVENTS_NATS_URL"),
          "elitea-llm-gateway": ("GATEWAY", None, "GATEWAY_NATS_URL"),
          "elitea-scheduler": ("GATEWAY", "elitea-scheduler-config", "GATEWAY_NATS_URL")}
for ident, (pfx, cm, urlvar) in prefix.items():
    d = deps.get(ident)
    if not d:
        check(f"{ident}: Deployment renders", False)
        continue
    spec = d["spec"]["template"]["spec"]
    ctr = spec["containers"][0]
    check(f"{ident}: pod label is one the NetworkPolicy admits", d["spec"]["template"]["metadata"]["labels"].get("app.kubernetes.io/name") == ident)
    vol = [v for v in spec.get("volumes", []) if v["name"] == "nats-client-tls"]
    mnt = [m for m in ctr.get("volumeMounts", []) if m["name"] == "nats-client-tls"]
    check(f"{ident}: mounts the Secret its Certificate writes", vol and vol[0]["secret"]["secretName"] == certs[ident]["spec"]["secretName"] and mnt)
    envmap = {e["name"]: e.get("value") for e in ctr.get("env", [])}
    if cm:
        envmap.update(cms.get(cm, {}))
    files = [envmap.get(f"{pfx}_NATS_TLS_{k}_FILE") for k in ("CA", "CERT", "KEY")]
    base = mnt[0]["mountPath"] if mnt else "<unmounted>"
    check(f"{ident}: {pfx}_NATS_TLS_* point into the mount", files == [f"{base}/ca.crt", f"{base}/tls.crt", f"{base}/tls.key"], files)
    url = envmap.get(urlvar, "")
    check(f"{ident}: {urlvar} is tls://{fqdn}:4222, no credential", url == f"tls://{fqdn}:4222", url)

# ── the Argo CD sample ────────────────────────────────────────────────────
def app(f):
    return yaml.safe_load(open(apps / f))
nsset = {app(f)["spec"]["destination"]["namespace"] for f in ("nats.yaml", "nats-bootstrap.yaml", "elitea.yaml")}
check("argocd: NATS, its bootstrap and the platform share one namespace (the NATS CA Issuer is namespaced)", len(nsset) == 1, nsset)
bparams = {p["name"]: p["value"] for p in app("nats-bootstrap.yaml")["spec"]["source"]["helm"]["parameters"]}
check("argocd: the bootstrap dials tls://", bparams.get("natsUrl", "").startswith("tls://"))

# ── report ────────────────────────────────────────────────────────────────
for line in open(tmp / "refusals"):
    status, name, *rest = line.rstrip("\n").split("\t")
    results.append((f"refusal: {name}", status == "ok", " ".join(rest) or status))
failed = [r for r in results if not r[1]]
for name, ok, detail in results:
    print(("  ok: " if ok else "  FAIL: ") + name + ("" if ok or not detail else f" — {detail}"))
FLOOR = 60
print(f"render-nats-security: {len(results)} assertion(s), {len(failed)} failed")
if len(results) < FLOOR:
    sys.exit(f"only {len(results)} assertion(s) ran, under the floor of {FLOOR}: an assertion stopped running")
sys.exit(1 if failed else 0)
PY
