#!/usr/bin/env bash
# render-bf0-2b.sh — render/config assertions for BF0.2b (NATS JetStream cluster
# + gateway Helm/ArgoCD app). Deterministic, no live cluster required: it renders
# the Helm charts with `helm template ${GATEWAY_RENDER_POSTURE}` and greps the profile/config files.
#
# Run: deploy/helm/tests/render-bf0-2b.sh   (requires helm + python3 + PyYAML)
#
# It was `deploy/test_bf0_2b.sh`, and no workflow, Taskfile task or script
# called it (#485). It sits beside render-capabilities.sh and render-llm-path.sh
# now, under deploy/helm/, so helm-lint.yml's `deploy/helm/**` trigger path
# covers the script itself as well as the charts it reads. A gate outside the
# path that starts it is the same false green in another costume (#409, #429).
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

# The single chart contains the LLM gateway, which REFUSES to render until an
# operator states its two postures. Every render below therefore supplies them:
# they are render-only values (.invalid is reserved by RFC 2606), and a chart
# that rendered without them would be the defect that refusal exists to stop.
GATEWAY_RENDER_POSTURE="--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true"

# deploy/helm/tests -> deploy. The chart and ArgoCD paths below are relative to
# it, so this must follow the file if it ever moves again.
DIR="$(cd "$(dirname "$0")/../.." && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"
# The floor helper, shared with the other honest validators (issue #534). It
# sits outside this script's own trigger path, so an edit to it does not start
# helm-lint.yml. That is safe here and it is not a second gate: the helper only
# COUNTS, and a helper that counts wrong makes this script red on its next run
# instead of quietly passing.
# shellcheck source=../../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"

# The gateway chart REFUSES to render until the operator states two postures
# (#467, #473): the self-referential-credential origins and the egress posture.
# Neither can have a chart default, because both name addresses that only the
# operator knows. So this file supplies them exactly as helm-lint.yml's template
# matrix does, and for the same reason: a render that skips them measures the
# refusal, not the Service. render-llm-path.sh owns the assertion that the
# refusal still fires on an empty guard.
#
# RENDER-ONLY values. `.invalid` is reserved by RFC 2606 and never resolves, and
# the label says what the value is for, so no reader can mistake either one for
# a shipped default.
# Component-scoped: the gateway is a component of the single elitea chart, so
# its values live under `llmGateway`.
GATEWAY_RENDER_VALUES=(
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
)
HELM="${HELM:-helm}"
PASS=0
FAIL=0
# Every assertion this file is meant to make. A run that makes fewer has
# skipped one, and a skipped assertion proves nothing. Same shape as
# deploy/scripts/embedding-path-check.sh, which is the reference honest
# validator in this repository.
#
# DERIVED, not written down (issue #534). A number stated here is true only
# when the pull request merges, so an assertion added later, with the number
# left alone, made the floor under-count in silence for ever. Each assertion
# below holds exactly one accepting arm, so the accepting arms are the
# assertions. Read scripts/lib/assertion-floor.sh.
ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])ok[[:space:]]+"'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN")"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()   { PASS=$((PASS+1)); echo "  ok: $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  FAIL: $1" >&2; }

echo "== NATS profile values (design §8.1.1) =="
# scale-1 (the chart's own nats block): single node, HA waived, file storage, NATS 2.12.0+
python3 - "$DIR/helm/elitea/values.yaml" <<'PY' && ok "scale-1: bundled by default, cluster disabled, file store, image 2.12.0" || bad "scale-1 profile shape"
import sys, yaml
v = yaml.safe_load(open(sys.argv[1]))
d = v["nats"]
assert d["enabled"] is True, "NATS is bundled by default"
assert d["config"]["cluster"]["enabled"] is False, "cluster must be disabled at scale-1"
assert d["config"]["jetstream"]["fileStore"]["enabled"] is True, "file storage required"
assert d["config"]["jetstream"]["memoryStore"]["enabled"] is False, "memory store must be off"
assert d["container"]["image"]["tag"].startswith("2.12"), "NATS Server must be 2.12.0+ (Nats-Incr)"
assert v["natsBootstrap"]["replicas"] == 1, "scale-1 assets are created at replicas=1"
PY
# HA: 3 nodes, replicas>=3 (RAFT quorum), and the bootstrap at the same count
python3 - "$DIR/helm/elitea/values-nats-ha.yaml" <<'PY' && ok "HA: cluster enabled, replicas>=3, spread constraints, bootstrap replicas match" || bad "HA profile shape"
import sys, yaml
v = yaml.safe_load(open(sys.argv[1]))
d = v["nats"]
assert d["config"]["cluster"]["enabled"] is True, "HA must enable clustering"
assert d["config"]["cluster"]["replicas"] >= 3, "HA needs replicas>=3 for quorum"
assert d["container"]["image"]["tag"].startswith("2.12"), "NATS Server must be 2.12.0+"
assert "topologySpreadConstraints" in d["podTemplate"], "HA must spread the quorum across nodes"
assert v["natsBootstrap"]["replicas"] == d["config"]["cluster"]["replicas"], "assets must be created at the cluster's replica count"
PY

echo "== KV/stream bootstrap owns every asset (design §8.6, §9.5; #1076) =="
BS="$DIR/helm/elitea/files/nats-bootstrap.sh"
grep -q  'ensure_stream GATEWAY_BUDGET file'        "$BS" && ok "creates the GATEWAY_BUDGET counter stream"     || bad "GATEWAY_BUDGET stream"
grep -q  'ensure_stream GATEWAY_RATELIMIT file'     "$BS" && ok "creates the GATEWAY_RATELIMIT counter stream"  || bad "GATEWAY_RATELIMIT stream"
grep -q  'ensure_stream GATEWAY_BUDGET_DELTAS file' "$BS" && ok "creates the GATEWAY_BUDGET_DELTAS stream"       || bad "GATEWAY_BUDGET_DELTAS stream"
grep -q  'ensure_kv GATEWAY_ALERT_COOLDOWN'         "$BS" && ok "creates the GATEWAY_ALERT_COOLDOWN KV"         || bad "GATEWAY_ALERT_COOLDOWN KV"
grep -q  'ensure_kv ELITEA_CANVAS_PRESENCE'         "$BS" && ok "creates the ELITEA_CANVAS_PRESENCE KV"         || bad "ELITEA_CANVAS_PRESENCE KV"
grep -q  'ensure_kv ELITEA_PLATFORM_MCP_REPLAY'     "$BS" && ok "creates the ELITEA_PLATFORM_MCP_REPLAY KV"     || bad "ELITEA_PLATFORM_MCP_REPLAY KV"
[ "$(grep -cE -- '^ +--allow-counter \\$' "$BS")" -eq 2 ]       && ok "both counter streams are AllowMsgCounter"      || bad "counter streams must set --allow-counter"
grep -q  'gateway.budget.delta'             "$BS" && ok "stream subject gateway.budget.delta" || bad "stream subject"
grep -q  -- '--dupe-window "${DELTAS_DUPE_WINDOW}"' "$BS" && ok "sets the deltas duplicate_window" || bad "duplicate_window"
grep -q  -- '--max-age "${DELTAS_MAX_AGE}"'  "$BS" && ok "sets retention MaxAge"               || bad "MaxAge retention"
grep -q  -- '--max-bytes "${DELTAS_MAX_BYTES}"' "$BS" && ok "sets retention MaxBytes"          || bad "MaxBytes retention"
grep -q  -- '--max-msgs "${DELTAS_MAX_MSGS}"' "$BS" && ok "sets retention MaxMsgs"             || bad "MaxMsgs retention"
grep -q  -- '--replicas "${REPLICAS}"'      "$BS" && ok "replicas parameterised per profile"  || bad "replicas param"
grep -q  'stream edit'                      "$BS" && ok "reconciles an existing stream instead of skipping it" || bad "existing streams must be edited, not skipped"
# big-bang migration => NO cutover bucket is ever *created* (a comment saying so is fine)
if grep -qE 'kv add +GATEWAY_CUTOVER' "$BS"; then bad "must NOT create GATEWAY_CUTOVER (big-bang)"; else ok "no GATEWAY_CUTOVER bucket created"; fi
# The dead GATEWAY_BUDGET KV (stream KV_GATEWAY_BUDGET) nothing read.
if grep -qE '(ensure_kv|kv add) +GATEWAY_BUDGET( |$)' "$BS"; then bad "must NOT create the dead GATEWAY_BUDGET KV bucket"; else ok "no GATEWAY_BUDGET KV bucket"; fi

echo "== gateway Services: infra ClusterIP:8083 + headless gRPC:8443 (#1359) =="
"$HELM" template gw "$DIR/helm/elitea" "${GATEWAY_RENDER_VALUES[@]}" > "$TMP/gw.yaml"
python3 - "$TMP/gw.yaml" <<'PY' && ok "elitea-llm-gateway-svc ClusterIP:8083 (infra); elitea-llm-gateway-grpc headless:8443; server+client certs" || bad "gateway service/certs render"
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
svc  = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Service"}
# The plaintext listener serves /healthz, /readyz and /metrics only.
s = svc.get("elitea-llm-gateway-svc")
assert s, "Service elitea-llm-gateway-svc missing"
assert s["spec"]["type"] == "ClusterIP", "must be ClusterIP (not public)"
assert [p["port"] for p in s["spec"]["ports"]] == [8083], "infra Service must expose 8083 only"
# /llm is the GatewayService gRPC listener. Headless, so elitea-main balances
# round_robin across replicas instead of pinning one HTTP/2 connection.
g = svc.get("elitea-llm-gateway-grpc")
assert g, "headless gRPC Service elitea-llm-gateway-grpc missing"
assert g["spec"].get("clusterIP") == "None", "gRPC Service must be headless"
ports = g["spec"]["ports"]
assert [(p["name"], p["port"], p.get("appProtocol")) for p in ports] == [("grpc", 8443, "grpc")], ports
dep = [d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-llm-gateway"]
assert dep, "gateway Deployment missing"
c = dep[0]["spec"]["template"]["spec"]["containers"][0]
cports = {p["name"]: p["containerPort"] for p in c["ports"]}
assert cports == {"http": 8083, "grpc": 8443}, cports
env = {e["name"]: e.get("value") for e in c.get("env", [])}
assert env.get("GATEWAY_GRPC_ADDR", ":8443") == ":8443", env.get("GATEWAY_GRPC_ADDR")
assert c["readinessProbe"]["httpGet"]["port"] == "http", "probes use the plaintext infra listener"
certs = [d for d in docs if d["kind"] == "Certificate" and d["spec"].get("usages")]
usages = {u for c in certs for u in c["spec"]["usages"]}
assert "server auth" in usages and "client auth" in usages, "need both server + client mTLS certs"
server = [c for c in certs if "server auth" in c["spec"]["usages"]]
assert any("elitea-llm-gateway-grpc" in c["spec"].get("dnsNames", []) for c in server), "server cert must name the gRPC Service"
client = [c for c in certs if "client auth" in c["spec"]["usages"] and "server auth" not in c["spec"]["usages"]]
assert any("elitea-main" in c["spec"].get("dnsNames", []) for c in client), "client cert must carry the elitea-main SAN the gateway allowlist admits"
PY

echo "== gateway HPA: custom /llm SSE metric (design §9.5) =="
"$HELM" template gw "$DIR/helm/elitea" "${GATEWAY_RENDER_VALUES[@]}" \
  --set llmGateway.autoscaling.enabled=true > "$TMP/gw-hpa.yaml"
python3 - "$TMP/gw-hpa.yaml" <<'PY' && ok "HPA scales on gateway_llm_sse_active_connections Pods metric" || bad "HPA custom metric"
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
# Selected BY NAME: the single chart renders every component, so "the first
# HPA in the document" is whichever component happens to sort first — it used to
# be the only one.
hpa = [d for d in docs if d["kind"] == "HorizontalPodAutoscaler"
       and d["metadata"]["name"] == "elitea-llm-gateway"]
assert hpa, "gateway HPA missing when llmGateway.autoscaling.enabled=true"
m = hpa[0]["spec"]["metrics"][0]
assert m["type"] == "Pods", "SSE metric must be a Pods metric, not Resource/CPU"
assert m["pods"]["metric"]["name"] == "gateway_llm_sse_active_connections", "wrong SSE metric name"
PY

echo "== nats-bootstrap Job: a Helm post hook, an Argo CD Sync hook in the NATS wave =="
"$HELM" template nb "$DIR/helm/elitea" "${GATEWAY_RENDER_VALUES[@]}" > "$TMP/nb.yaml"
python3 - "$TMP/nb.yaml" <<'PY' && ok "post-install/upgrade hook Job mounts bootstrap.sh; Argo CD Sync hook in wave -10" || bad "bootstrap Job hook"
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
job = [d for d in docs if d["kind"] == "Job" and d["metadata"]["name"] == "elitea-nats-bootstrap"][0]
ann = job["metadata"]["annotations"]
assert "post-install" in ann["helm.sh/hook"] and "post-upgrade" in ann["helm.sh/hook"], "Job must be a post-install/upgrade hook"
# Argo CD would map post-install to PostSync, which waits for healthy
# workloads that wait for these streams.
assert ann.get("argocd.argoproj.io/hook") == "Sync", "Argo CD must run it as a Sync hook, not PostSync"
assert ann.get("argocd.argoproj.io/sync-wave") == "-10", "the bootstrap runs in the NATS wave, ahead of the workloads"
c = job["spec"]["template"]["spec"]["containers"][0]
assert "bootstrap.sh" in " ".join(c["command"]), "Job must run bootstrap.sh"
PY

# deploy/argocd/applications/, NOT deploy/argocd/. The children moved into
# the applications/ directory with the app-of-apps layout (#265), and this
# assertion went on reading the old path. It raised FileNotFoundError, and no
# caller ran it, so nothing printed the failure anywhere (#485).
echo "== ArgoCD: one Application, NATS inside it =="
python3 - "$DIR/argocd/applications" <<'PY' && ok "elitea is the only child Application (NATS and its bootstrap are bundled)" || bad "argocd application layout"
import sys, pathlib
d = pathlib.Path(sys.argv[1])
assert d.is_dir(), f"{d} does not exist; the ArgoCD layout moved and this assertion stopped measuring"
names = sorted(f.name for f in d.glob("*.yaml"))
assert names == ["elitea.yaml"], names
PY

# Issue #475. The Application used to name "the values file this Application
# points at" and carry no `helm:` block at all, so it pointed at nothing and
# ArgoCD rendered the chart from its defaults alone. The chart REFUSES those
# defaults — the two gateway postures have none — so the committed Application
# could not sync. No gate said so, because every render in this repository
# supplies the postures on its own command line.
#
# The two assertions below read the Application instead. The first says each
# Application states where its values come from. The second renders the chart
# the way ArgoCD renders it, from that statement, and reads DATABASE_URL back
# out of the manifest.
echo "== ArgoCD apps state where their values come from (#475) =="
python3 - "$DIR/argocd/applications" <<'PYA' && ok "each Application that syncs an in-repo chart declares spec.source.helm" || bad "argocd application values source"
import sys, yaml, pathlib
apps = pathlib.Path(sys.argv[1])
found = 0
for f in sorted(apps.glob("*.yaml")):
    a = yaml.safe_load(open(f))
    src = a["spec"]["source"]
    path = src.get("path", "")
    if not path.startswith("deploy/helm/"):
        continue
    found += 1
    helm = src.get("helm")
    assert helm, (
        f"{f.name} syncs {path} and declares no spec.source.helm. ArgoCD then "
        "renders the chart from its own defaults, and no reader of this file "
        "can say which values the release gets."
    )
    assert helm.get("valueFiles") or helm.get("parameters"), (
        f"{f.name} has an empty spec.source.helm block, which states nothing."
    )
assert found, "no Application syncs an in-repo chart; this assertion measured nothing"
PYA

echo "== the elitea app renders, and DATABASE_URL comes from the Secret it names =="
python3 - "$DIR/argocd/applications/elitea.yaml" "$DIR/helm/elitea" "$HELM" <<'PYB' && ok "DATABASE_URL is a secretKeyRef on the Secret and key the Application names" || bad "elitea application render / DATABASE_URL"
import subprocess, sys, yaml

app_file, chart, helm_bin = sys.argv[1], sys.argv[2], sys.argv[3]
src = yaml.safe_load(open(app_file))["spec"]["source"]
helm = src.get("helm") or {}
params = {p["name"]: p.get("value", "") for p in helm.get("parameters", [])}
# Argo CD passes a parameter with --set unless it says forceString: true, so a
# "false" here is the boolean false, as the switches below rely on.
forced = {p["name"] for p in helm.get("parameters", []) if p.get("forceString")}

# The values an OPERATOR supplies, and only those. Both are empty in git on
# purpose, so this stands in for the operator. `.invalid` is reserved by
# RFC 2606 and never resolves, so no reader mistakes it for a shipped default.
operator = {
    "llmGateway.env.GATEWAY_SELF_LLM_ORIGINS": "https://render-only.example.invalid/llm/v1",
    "llmGateway.egressPosture": "public-unrestricted",
    # The gateway peer admitted to elitea-main:8080 (networkPolicies).
    "networkPolicies.main.ingressFrom[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name": "gateway-system",
}
for name in operator:
    assert name in params, (
        f"{name} has no chart default and no slot in the Application. An "
        "operator has nowhere in the committed files to put it."
    )
    assert params[name] == "", (
        f"{name} carries the committed value {params[name]!r}. Only the "
        "operator knows this address; a value here guards a name nobody uses."
    )

argv = [helm_bin, "template", "elitea", chart]
for vf in helm.get("valueFiles", []):
    argv += ["-f", f"{chart}/{vf}"]
for name, value in params.items():
    argv += ["--set-string" if name in forced else "--set", f"{name}={operator.get(name, value)}"]

done = subprocess.run(argv, capture_output=True, text=True)
assert done.returncode == 0, (
    "the Application does not render with the values it supplies plus the two "
    f"the operator supplies:\n{done.stderr.strip()}"
)

secret = params["postgresql.existingSecret"]
key = params["postgresql.key"]
assert secret and key, "the Application names no database Secret"

seen = 0
for doc in yaml.safe_load_all(done.stdout):
    if not doc:
        continue
    spec = doc.get("spec", {})
    pod = spec.get("template", {}).get("spec") or {}
    for container in (pod.get("containers", []) + pod.get("initContainers", [])):
        for env in container.get("env", []):
            if env.get("name") != "DATABASE_URL":
                continue
            seen += 1
            ref = env.get("valueFrom", {}).get("secretKeyRef")
            where = f"{doc['kind']}/{doc['metadata']['name']}:{container['name']}"
            assert ref, f"{where} takes DATABASE_URL in plaintext, not from a Secret"
            assert ref["name"] == secret, (
                f"{where} reads Secret {ref['name']!r}; the Application names {secret!r}"
            )
            assert ref["key"] == key, (
                f"{where} reads key {ref['key']!r}; the Application names {key!r}"
            )
# A render that carried no DATABASE_URL at all would pass every check above.
assert seen >= 3, (
    f"only {seen} container(s) read DATABASE_URL. elitea-main, its migration "
    "Job and the scheduler all read the database."
)
PYB

echo
RAN=$((PASS+FAIL))
echo "BF0.2b render assertions: ${RAN} ran, ${PASS} passed, ${FAIL} failed"
# -ne, not -lt. A count that is too LOW means an assertion stopped running. A
# count that is too HIGH means an assertion site ran more than once, so the
# floor no longer describes the file. Both are errors, and neither is a pass.
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAIL: ${RAN} assertion(s) ran, and this file holds ${EXPECTED_ASSERTIONS} assertion site(s)." >&2
  echo "      A run that skips an assertion proves nothing, and a site that runs" >&2
  echo "      twice is a site the floor cannot describe." >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
