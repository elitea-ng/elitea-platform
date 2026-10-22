#!/usr/bin/env bash
# render-external-secrets.sh — the `externalSecrets` block (one
# external-secrets.io/v1 ExternalSecret per Secret the chart's pods read).
#
# The point of the block is PARITY: an ExternalSecret that fills a Secret no
# pod reads, or a pod reading a key no ExternalSecret fills, installs cleanly
# and then waits forever on CreateContainerConfigError. So the main assertion
# does not compare the ExternalSecrets with a list written in this file. It
# reads the RENDERED workloads — every Deployment, StatefulSet, Job and
# CronJob, containers and init containers — collects each secretKeyRef, each
# secret volume (with its `items`) and each projected secret source, removes
# the Secrets a rendered cert-manager Certificate or a rendered Secret owns,
# and requires:
#   * every remaining required reference to be an ExternalSecret target that
#     carries the referenced key;
#   * the whole-Secret material volumes to carry every file the consumer
#     opens: the worker's MATERIAL_FILES, and every elitea-main ConfigMap path
#     under main.runtime.material.mountPath;
#   * every ExternalSecret to be read by something (no orphans);
#   * no ExternalSecret to target a cert-manager or chart-rendered Secret.
# A template that starts reading a new Secret fails here until
# templates/externalSecrets/_inventory.tpl learns it.
#
# Also: off renders nothing; the one-remote-key and per-secret-ref modes and
# a per-key override produce the documented remote references; the
# ExternalSecrets are hooks ordered before the first Job hook; and each
# misconfiguration is refused with its message.
#
# Usage: deploy/helm/tests/render-external-secrets.sh
# Needs: helm, python3 + PyYAML. No cluster, no network.
set -euo pipefail

cd "$(dirname "$0")/../../.."

chart=deploy/helm/elitea
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

# Every component that reads a Secret, switched on at once: the runtime plane
# and worker (values-standalone.yaml), elitea-vector + Qdrant, DeepWiki,
# Inventory, PgBouncer and the egress proxy. The gateway postures and git
# allowlists are render-only values; `.invalid` never resolves.
full=(-f "$chart/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=allowlist --set llmGateway.env.GATEWAY_EGRESS_ALLOWLIST=elitea.invalid:8000
  --set networkPolicies.main.noExternalIngress=true
  --set worker.enabled=true
  --set vector.enabled=true --set vector.qdrant.apiKeySecret.name=elitea-qdrant-api-key
  --set deepwiki.enabled=true --set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com
  --set main.fileConfig.deepwikiClientMaterial.enabled=true
  --set main.fileConfig.deepwikiClientMaterial.secretName=elitea-main-deepwiki-client-tls
  --set main.env.ELITEA_DEEPWIKI_ENABLED=true --set main.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com
  --set main.env.ELITEA_DEEPWIKI_CLIENT_CERT_FILE=/run/elitea-deepwiki/tls.crt
  --set main.env.ELITEA_DEEPWIKI_CLIENT_KEY_FILE=/run/elitea-deepwiki/tls.key
  --set main.env.ELITEA_DEEPWIKI_CA_FILE=/run/elitea-deepwiki/ca.crt
  --set inventory.enabled=true --set inventory.env.ELITEA_INVENTORY_GIT_ALLOWLIST=github.com
  --set main.fileConfig.inventoryClientMaterial.enabled=true
  --set main.fileConfig.inventoryClientMaterial.secretName=elitea-main-inventory-client-tls
  --set main.env.ELITEA_INVENTORY_ENABLED=true --set main.env.ELITEA_INVENTORY_GIT_ALLOWLIST=github.com
  --set main.env.ELITEA_INVENTORY_CLIENT_CERT_FILE=/run/elitea-inventory/tls.crt
  --set main.env.ELITEA_INVENTORY_CLIENT_KEY_FILE=/run/elitea-inventory/tls.key
  --set main.env.ELITEA_INVENTORY_CA_FILE=/run/elitea-inventory/ca.crt
  --set pgbouncer.enabled=true --set pgbouncer.postgresHost=pg.internal --set pgbouncer.database=elitea_v2
  --set pgbouncer.credentialsSecretName=elitea-pgbouncer-credentials
  -f deploy/helm/tests/fixtures/egress-proxy-values.yaml)

es=(--set externalSecrets.enabled=true --set externalSecrets.secretStoreRef.name=platform-secrets)

# parity <manifest> <mode> — the rendered-workload comparison above, plus the
# remote-reference shape of <mode>: "single" (externalSecrets.remoteKey,
# property "<Secret>/<key>") or "per-secret" (secrets.<name>.remoteKey,
# property = key, with the one per-key override set below).
parity() {
  python3 - "$1" "$2" <<'PY'
import re, sys, posixpath, yaml
path, mode = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(path)) if d]
problems = []

issued = {d["spec"]["secretName"] for d in docs if d.get("kind") == "Certificate"}
rendered = {d["metadata"]["name"] for d in docs if d.get("kind") == "Secret"}
ext = {d["spec"]["target"]["name"]: d for d in docs if d.get("kind") == "ExternalSecret"}

def pod_specs():
    for d in docs:
        k, s = d.get("kind"), d.get("spec") or {}
        if k in ("Deployment", "StatefulSet", "Job", "DaemonSet"):
            yield d, s["template"]["spec"]
        elif k == "CronJob":
            yield d, s["jobTemplate"]["spec"]["template"]["spec"]

required, optional, whole, material_files = {}, set(), set(), {}
for d, spec in pod_specs():
    where = f'{d["kind"]}/{d["metadata"]["name"]}'
    for c in (spec.get("initContainers") or []) + spec.get("containers", []):
        for e in c.get("env") or []:
            ref = ((e.get("valueFrom") or {}).get("secretKeyRef"))
            if ref:
                if ref.get("optional"):
                    optional.add(ref["name"])
                else:
                    required.setdefault(ref["name"], set()).add(ref["key"])
            if e.get("name") == "MATERIAL_FILES":
                material_files.setdefault(where, e["value"].split())
    for v in spec.get("volumes") or []:
        srcs = []
        if "secret" in v:
            srcs.append((v["secret"]["secretName"], v["secret"].get("items")))
        for p in (v.get("projected") or {}).get("sources") or []:
            if "secret" in p:
                srcs.append((p["secret"]["name"], p["secret"].get("items")))
        for name, items in srcs:
            if items:
                required.setdefault(name, set()).update(i["key"] for i in items)
            else:
                required.setdefault(name, set())
                whole.add(name)
            if v["name"] == "material-source" and where in material_files:
                required[name].update(material_files[where])
            # A whole-Secret volume that a container's script opens by path
            # (PgBouncer's `cat /material/user`): each <mountPath>/<file> the
            # command or args name is a key the Secret must carry.
            if not items:
                for c in (spec.get("initContainers") or []) + spec.get("containers", []):
                    text = " ".join(map(str, (c.get("command") or []) + (c.get("args") or [])))
                    for m in c.get("volumeMounts") or []:
                        if m["name"] == v["name"]:
                            required[name].update(re.findall(re.escape(m["mountPath"].rstrip("/")) + r"/([A-Za-z0-9._-]+)", text))

# elitea-main's runtime material: every path the ConfigMap names under the
# mount, which is the list the init container copies.
for d in docs:
    if d.get("kind") == "Deployment" and d["metadata"]["name"].endswith("-main"):
        spec = d["spec"]["template"]["spec"]
        mount = next((m["mountPath"] for c in spec.get("initContainers") or []
                      if c["name"] == "runtime-material" for m in c["volumeMounts"]
                      if m["name"] == "runtime-material"), None)
        src = next((v["secret"]["secretName"] for v in spec["volumes"]
                    if v["name"] == "runtime-material-source"), None)
        cm = next(x for x in docs if x.get("kind") == "ConfigMap"
                  and x["metadata"]["name"] == d["metadata"]["name"] + "-config")
        if mount and src:
            files = {posixpath.basename(val) for val in cm["data"].values()
                     if isinstance(val, str) and val.startswith(mount + "/")}
            if not files:
                problems.append("main runtime material: no ConfigMap path under " + mount)
            required.setdefault(src, set()).update(files)

operator = {n: k for n, k in required.items() if n not in issued and n not in rendered}
for name, keys in sorted(operator.items()):
    if name not in ext:
        problems.append(f"pods read Secret {name} but no ExternalSecret fills it")
        continue
    have = {x["secretKey"] for x in ext[name]["spec"].get("data") or []}
    if not keys <= have:
        problems.append(f"{name}: pods read {sorted(keys - have)} that the ExternalSecret does not fill")
for name in sorted(ext):
    if name in issued or name in rendered:
        problems.append(f"ExternalSecret {name} targets a Secret the chart or cert-manager already owns")
    if name not in required and name not in optional:
        problems.append(f"ExternalSecret {name} fills a Secret no rendered workload reads")
for name, d in ext.items():
    for x in d["spec"].get("data") or []:
        r = x["remoteRef"]
        if mode == "single":
            want = ("elitea-all", f'{name}/{x["secretKey"]}')
        elif name == "elitea-main-db":
            want = ("projects/p/secrets/elitea-database-url", None)
        else:
            want = (f"es-{name}", x["secretKey"])
        if (r["key"], r.get("property")) != want:
            problems.append(f'{name}/{x["secretKey"]}: remoteRef {r} != {want}')
    if d["spec"]["target"].get("creationPolicy") != "Orphan":
        problems.append(f"{name}: target.creationPolicy must be Orphan (hook recreation must not GC the Secret)")
    ann = d["metadata"].get("annotations") or {}
    if ann.get("helm.sh/hook") != "pre-install,pre-upgrade" or ann.get("argocd.argoproj.io/hook") != "PreSync":
        problems.append(f"{name}: not a pre-install/PreSync hook")
    weight = int(ann.get("helm.sh/hook-weight", "0"))
    jobs = [int((j["metadata"].get("annotations") or {}).get("helm.sh/hook-weight", "0"))
            for j in docs if j.get("kind") == "Job" and (j["metadata"].get("annotations") or {}).get("helm.sh/hook")]
    if jobs and weight >= min(jobs):
        problems.append(f"{name}: hook weight {weight} is not before the first Job hook ({min(jobs)})")
    if int(ann.get("argocd.argoproj.io/sync-wave", "0")) != weight:
        problems.append(f"{name}: Argo sync-wave differs from the Helm hook weight")

print(f"  {len(operator)} operator Secrets read by the workloads, {len(ext)} ExternalSecrets, "
      f"{len(issued)} cert-manager Secrets excluded")
for p in problems:
    print("FAIL: " + p, file=sys.stderr)
sys.exit(1 if problems else 0)
PY
}

echo "== externalSecrets off (the default)"
helm template t "$chart" "${full[@]}" > "$work/off.yaml"
if grep -q '^kind: ExternalSecret' "$work/off.yaml"; then
  fail "externalSecrets is off, but an ExternalSecret rendered"
else
  note "no ExternalSecret"
fi

echo "== on, one remote key for the whole install"
helm template t "$chart" "${full[@]}" "${es[@]}" --set externalSecrets.remoteKey=elitea-all > "$work/single.yaml"
if parity "$work/single.yaml" single; then note "parity holds"; else fail "one-remote-key parity"; fi

echo "== on, one remote key per Secret, plus a per-key override"
names=$(python3 -c 'import sys,yaml; print(" ".join(d["metadata"]["name"] for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind")=="ExternalSecret"))' "$work/single.yaml")
per=()
for n in $names; do per+=(--set "externalSecrets.secrets.$n.remoteKey=es-$n"); done
helm template t "$chart" "${full[@]}" "${es[@]}" "${per[@]}" \
  --set externalSecrets.secrets.elitea-main-db.keys.database-url.remoteKey=projects/p/secrets/elitea-database-url \
  > "$work/per.yaml"
if parity "$work/per.yaml" per-secret; then note "parity holds"; else fail "per-secret parity"; fi

echo "== the default values, runtime plane off"
min=(--set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)
helm template t "$chart" "${min[@]}" "${es[@]}" --set externalSecrets.remoteKey=elitea-all > "$work/min.yaml"
if parity "$work/min.yaml" single; then note "parity holds"; else fail "default-values parity"; fi

# The ExternalSecret target names in a manifest, one per line: a grep for the
# name alone would also match the pods' (optional) references to it.
es_names() { yq eval-all 'select(.kind == "ExternalSecret") | .spec.target.name' - <<<"$1"; }

echo "== optional references follow the deployment"
gcs="$(helm template t "$chart" "${min[@]}" "${es[@]}" --set externalSecrets.remoteKey=elitea-all \
  --set main.env.STORAGE_BACKEND=gcs)"
if es_names "$gcs" | grep -x elitea-main-storage-secrets >/dev/null; then
  fail "STORAGE_BACKEND=gcs still renders the S3/Azure key ExternalSecret"
else
  note "gcs: no storage-key ExternalSecret"
fi
if es_names "$gcs" | grep -x elitea-main-mail-secrets >/dev/null; then
  fail "SMTP_USERNAME is empty, yet the SMTP password ExternalSecret rendered"
else
  note "no SMTP: no mail ExternalSecret"
fi
skip="$(helm template t "$chart" "${min[@]}" "${es[@]}" --set externalSecrets.remoteKey=elitea-all \
  --set externalSecrets.secrets.elitea-postgres-cluster-superuser.enabled=false)"
if es_names "$skip" | grep -x elitea-postgres-cluster-superuser >/dev/null; then
  fail "enabled: false still renders that ExternalSecret"
else
  note "enabled: false leaves the Secret to the operator"
fi
extra="$(helm template t "$chart" "${min[@]}" "${es[@]}" --set externalSecrets.remoteKey=elitea-all \
  --set externalSecrets.secrets.elitea-runtime-ca.extra=true \
  --set externalSecrets.secrets.elitea-runtime-ca.type=kubernetes.io/tls \
  --set 'externalSecrets.secrets.elitea-runtime-ca.keys.tls\.crt.property=runtime-ca/crt' \
  --set 'externalSecrets.secrets.elitea-runtime-ca.keys.tls\.key.property=runtime-ca/key')"
if grep -q 'type: kubernetes.io/tls' <<<"$extra" && grep -q 'property: "runtime-ca/key"' <<<"$extra"; then
  note "extra Secret rendered with its type and property overrides"
else
  fail "extra: true Secret did not render as declared"
fi

expect_refusal() {
  local description="$1" fragment="$2"
  shift 2
  local out
  if out="$(helm template t "$chart" "$@" 2>&1)"; then
    fail "$description: rendered, expected a refusal"
  elif ! printf '%s' "$out" | grep -qF -- "$fragment"; then
    fail "$description: refused for the wrong reason: $(printf '%s' "$out" | tail -1)"
  else
    note "refused: $description"
  fi
}

echo "== refusals"
expect_refusal "no secret store" "needs externalSecrets.secretStoreRef.name" \
  "${min[@]}" --set externalSecrets.enabled=true --set externalSecrets.remoteKey=x
expect_refusal "a key with no remote reference" 'no remote reference for key "hop-secret" of Secret "elitea-llm-gateway-secrets"' \
  "${min[@]}" "${es[@]}"
expect_refusal "a mapping for a cert-manager Secret" "a cert-manager Certificate in this chart issues that Secret" \
  "${full[@]}" "${es[@]}" --set externalSecrets.remoteKey=x \
  --set externalSecrets.secrets.elitea-main-gateway-client-tls.remoteKey=y
expect_refusal "a mapping for the chart-rendered egress keys" "this chart renders that Secret itself" \
  "${full[@]}" "${es[@]}" --set externalSecrets.remoteKey=x \
  --set externalSecrets.secrets.elitea-egress-proxy-keys.remoteKey=y
expect_refusal "a mapping for a Secret nothing reads" "names no Secret this release reads" \
  "${min[@]}" "${es[@]}" --set externalSecrets.remoteKey=x --set externalSecrets.secrets.elitea-main-dbb.remoteKey=y
expect_refusal "an operator-named-keys Secret with no keys" "are yours, not the chart's" \
  "${full[@]}" "${es[@]}" --set externalSecrets.remoteKey=x \
  --set egressProxy.publicKeys.keys=null --set egressProxy.publicKeys.existingSecret=elitea-egress-public
expect_refusal "a plain DSN in a ConfigMap" "main.env.DATABASE_URL is set while externalSecrets.enabled" \
  "${min[@]}" "${es[@]}" --set externalSecrets.remoteKey=x --set main.env.DATABASE_URL=postgres://u:p@h/db

echo "== an operator-named-keys Secret, whole remote secret extracted"
open="$(helm template t "$chart" "${full[@]}" "${es[@]}" --set externalSecrets.remoteKey=x \
  --set egressProxy.publicKeys.keys=null --set egressProxy.publicKeys.existingSecret=elitea-egress-public \
  --set externalSecrets.secrets.elitea-egress-public.remoteKey=egress-public-keys)"
if grep -q 'key: "egress-public-keys"' <<<"$open"; then
  note "dataFrom.extract for the operator-named keys"
else
  fail "open Secret with a remoteKey did not render dataFrom.extract"
fi

if [ "$failures" -gt 0 ]; then
  echo "render-external-secrets: $failures failure(s)" >&2
  exit 1
fi
echo "render-external-secrets: OK"
