#!/usr/bin/env bash
# render-nats-bundled.sh — the upstream NATS chart is a dependency of the
# elitea chart (one chart, one release). Asserts, on rendered output:
#
#   locked     Chart.lock pins exactly the dependencies Chart.yaml declares
#              (name, version, repository); `helm dependency list` reports
#              every one `ok` (resolved into charts/ by build-deps.sh); and
#              charts/ is not committed
#   on         nats.enabled (the default) renders the server, its CA, the
#              bootstrap Job, and puts all of them in Argo CD wave -10 so the
#              workloads (wave 0) start after the streams exist
#   off        nats.enabled=false renders none of the server; the clients
#              point at natsClient.service; the bootstrap can still run
#              against your NATS, or be turned off
#   refusals   nats.enabled with natsClient naming another server; the old
#              client keys under `nats`
#   packaged   `helm package` carries every dependency, and a render
#              of the .tgz equals the render of the directory, so the
#              published OCI chart needs nothing built
#
# Run deploy/helm/elitea/build-deps.sh first (CI does). Needs helm, node,
# python3 + PyYAML.
set -euo pipefail
# Every `helm template` here starts from the pre-S10 feature switches
# (fixtures/features-off.yaml: runtime, worker, vector, Qdrant, DeepWiki and
# Inventory off), prepended so that any values file or --set a render names
# wins; each render turns on what it tests. render-default-on.sh covers the
# shipped defaults.
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
_helm_bin="${HELM:-helm}"
helm() {
  if [ "${1:-}" = template ]; then shift; command "$_helm_bin" template -f "$FEATURES_OFF" "$@"
  else command "$_helm_bin" "$@"; fi
}
HELM=helm

DIR="$(cd "$(dirname "$0")/../.." && pwd)"   # deploy/
CHART="$DIR/helm/elitea"
HELM="${HELM:-helm}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }

COMMON=(-n elitea
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)

# ── locked ──────────────────────────────────────────────────────────────────
python3 - "$CHART" <<'PY' || fails=$((fails + 1))
import sys, yaml, pathlib
c = pathlib.Path(sys.argv[1])
chart = yaml.safe_load((c / "Chart.yaml").read_text())
lock = yaml.safe_load((c / "Chart.lock").read_text())
key = lambda d: (d["name"], str(d["version"]), d["repository"])
want = [key(d) for d in chart.get("dependencies", [])]
got = [key(d) for d in lock.get("dependencies", [])]
ok = True
# keda and prometheus-adapter (S11) are optional and off by default; they are
# listed, locked and packaged like the rest.
WANT = ["nats", "qdrant", "elitea-sandbox-runtime", "keda", "prometheus-adapter"]
if [w[0] for w in want] != WANT:
    print(f"FAIL: Chart.yaml dependencies are {[w[0] for w in want]}, want {WANT}"); ok = False
if want != got:
    print(f"FAIL: Chart.lock pins {got}, Chart.yaml declares {want} (helm dependency update deploy/helm/elitea, commit Chart.lock)"); ok = False
for dep, cond in (("nats", "nats.enabled"), ("qdrant", "qdrant.enabled"), ("keda", "keda.enabled"),
                  ("prometheus-adapter", "prometheusAdapter.enabled")):
    found = [d for d in chart["dependencies"] if d["name"] == dep]
    if found and found[0].get("condition") != cond:
        print(f"FAIL: {dep} dependency condition is {found[0].get('condition')!r}, want {cond}"); ok = False
unpacked = [p.name for p in (c / "charts").iterdir() if p.is_dir() or p.is_symlink()] if (c / "charts").exists() else []
if unpacked:
    print(f"FAIL: charts/ holds unpacked or linked dependencies {unpacked}; build-deps.sh writes archives only"); ok = False
print("ok:   Chart.lock matches Chart.yaml", [w[0] + " " + w[1] for w in want]) if ok else None
sys.exit(0 if ok else 1)
PY
if node "$DIR/../scripts/ci/check-chart-deps.mjs" "$CHART" > "$TMP/deps.txt" 2>&1; then
  echo "ok:   every dependency resolves (helm dependency list: ok)"
else
  fail "a dependency does not resolve: $(grep '::error::' "$TMP/deps.txt" | head -3)"
fi
if [ -n "$(git -C "$DIR" ls-files -- helm/elitea/charts)" ]; then
  fail "deploy/helm/elitea/charts/ is committed; it is build output (.gitignore)"
fi

# ── on / off renders ────────────────────────────────────────────────────────
"$HELM" template elitea "$CHART" "${COMMON[@]}" > "$TMP/on.yaml"
"$HELM" template elitea "$CHART" "${COMMON[@]}" --set nats.enabled=false \
  --set-string natsClient.service=my-nats > "$TMP/off-bootstrap.yaml"
"$HELM" template elitea "$CHART" "${COMMON[@]}" --set nats.enabled=false \
  --set natsBootstrap.enabled=false --set-string natsClient.service=my-nats > "$TMP/off.yaml"

python3 - "$TMP" <<'PY' || fails=$((fails + 1))
import sys, yaml, pathlib
t = pathlib.Path(sys.argv[1])
def load(n): return [d for d in yaml.safe_load_all((t / n).read_text()) if d]
def find(docs, kind, name): return [d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name]
def ann(d): return d["metadata"].get("annotations") or {}
bad = []
on = load("on.yaml")
# The server and everything the bootstrap needs before it can connect.
for kind, name in [("StatefulSet", "elitea-nats"), ("Service", "elitea-nats"),
                   ("Service", "elitea-nats-headless"), ("ConfigMap", "elitea-nats-config"),
                   ("PodDisruptionBudget", "elitea-nats"), ("Issuer", "elitea-nats-ca"),
                   ("Certificate", "elitea-nats-server"), ("Job", "elitea-nats-bootstrap")]:
    got = find(on, kind, name)
    if not got:
        bad.append(f"on: no {kind}/{name}"); continue
    w = ann(got[0]).get("argocd.argoproj.io/sync-wave")
    if w != "-10":
        bad.append(f"on: {kind}/{name} sync-wave {w!r}, want \"-10\"")
job = find(on, "Job", "elitea-nats-bootstrap")
if job and ann(job[0]).get("argocd.argoproj.io/hook") != "Sync":
    bad.append("on: bootstrap Job is not an Argo CD Sync hook (a PostSync hook would wait on workloads that wait on it)")
if job and "post-install" not in ann(job[0]).get("helm.sh/hook", ""):
    bad.append("on: bootstrap Job is not a Helm post-install hook")
sts = find(on, "StatefulSet", "elitea-nats")
if sts:
    if sts[0]["spec"]["selector"]["matchLabels"].get("app.kubernetes.io/instance") != "elitea":
        bad.append("on: StatefulSet selector instance is not the release name")
    vct = [v["metadata"]["name"] for v in sts[0]["spec"].get("volumeClaimTemplates", [])]
    if vct != ["elitea-nats-js"]:
        bad.append(f"on: volumeClaimTemplates {vct}, want ['elitea-nats-js'] (the PVC an upgrade keeps)")
# Workloads stay at wave 0, after NATS.
for d in on:
    if d["kind"] == "Deployment" and d["metadata"]["name"] in ("elitea-main", "elitea-scheduler", "elitea-llm-gateway"):
        w = ann(d).get("argocd.argoproj.io/sync-wave", "0")
        if int(w) <= -10:
            bad.append(f"on: {d['metadata']['name']} wave {w} is not after NATS")

for f in ("off.yaml", "off-bootstrap.yaml"):
    docs = load(f)
    for d in docs:
        n = d["metadata"]["name"]
        if d["kind"] in ("StatefulSet", "Issuer", "PrometheusRule") and n.startswith("elitea-nats"):
            bad.append(f"{f}: {d['kind']}/{n} rendered with nats.enabled=false")
        if d["kind"] == "Certificate" and n in ("elitea-nats-server", "elitea-nats-route", "elitea-nats-ca"):
            bad.append(f"{f}: server Certificate/{n} rendered with nats.enabled=false")
    text = (t / f).read_text()
    if "tls://my-nats.elitea.svc.cluster.local:4222" not in text:
        bad.append(f"{f}: clients do not point at natsClient.service")
    if "elitea-nats.elitea.svc" in text:
        bad.append(f"{f}: something still points at the bundled server")
if not find(load("off-bootstrap.yaml"), "Job", "elitea-nats-bootstrap"):
    bad.append("off-bootstrap: the bootstrap Job is gone with nats.enabled=false (it serves your NATS too)")
if find(load("off.yaml"), "Job", "elitea-nats-bootstrap"):
    bad.append("off: bootstrap Job rendered with natsBootstrap.enabled=false")
for b in bad: print("FAIL:", b)
if not bad: print("ok:   on renders server+bootstrap in wave -10; off renders none and points clients at natsClient")
sys.exit(1 if bad else 0)
PY

# ── refusals ────────────────────────────────────────────────────────────────
refuse() {  # refuse "<what>" "<expected message fragment>" helm-args...
  local what="$1" want="$2"; shift 2
  local out
  if out="$("$HELM" template elitea "$CHART" "${COMMON[@]}" "$@" 2>&1)"; then
    fail "rendered: $what"
  elif ! grep -qF -- "$want" <<<"$out"; then
    fail "$what: refused, but not with \"$want\": $(tail -1 <<<"$out")"
  else
    echo "ok:   refuses $what"
  fi
}
refuse "nats.enabled with natsClient naming another server" "natsClient" \
  --set-string natsClient.service=my-nats
refuse "the pre-merge client keys under nats" "natsClient" \
  --set-string nats.namespace=elitea
refuse "nats.service as a string (the old client key)" "nats" \
  --set-string nats.service=elitea-nats

# ── packaged ────────────────────────────────────────────────────────────────
"$HELM" package "$CHART" -d "$TMP/pkg" >/dev/null
tgz=("$TMP"/pkg/elitea-*.tgz)
# The listing goes to a file: under pipefail, `tar -t | grep -q` fails when
# grep exits at its match and tar dies of SIGPIPE.
tar -tzf "${tgz[0]}" > "$TMP/pkg-files.txt"
# helm package unpacks each dependency into charts/<name>/. The names come
# from Chart.lock, so a new dependency needs no edit here.
python3 - "$CHART/Chart.lock" > "$TMP/dep-archives.txt" <<'PY'
import sys, yaml
for d in yaml.safe_load(open(sys.argv[1]))["dependencies"]:
    print(f"elitea/charts/{d['name']}/Chart.yaml")
PY
while read -r want; do
  grep -qx "$want" "$TMP/pkg-files.txt" || fail "the packaged chart does not carry ${want#elitea/}"
done < "$TMP/dep-archives.txt"
"$HELM" template elitea "${tgz[0]}" "${COMMON[@]}" > "$TMP/pkg-on.yaml"
"$HELM" template elitea "${tgz[0]}" "${COMMON[@]}" --set nats.enabled=false \
  --set natsBootstrap.enabled=false --set-string natsClient.service=my-nats > "$TMP/pkg-off.yaml"
if diff -q "$TMP/on.yaml" "$TMP/pkg-on.yaml" >/dev/null && diff -q "$TMP/off.yaml" "$TMP/pkg-off.yaml" >/dev/null; then
  echo "ok:   the packaged chart renders what the directory renders (on and off)"
else
  fail "the packaged chart renders differently from the directory"
  diff "$TMP/on.yaml" "$TMP/pkg-on.yaml" | head -20 || true
fi

if [ "$fails" -ne 0 ]; then echo "$fails check group(s) failed"; exit 1; fi
echo "render-nats-bundled: all checks pass"
