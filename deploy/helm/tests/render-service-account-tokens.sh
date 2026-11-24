#!/usr/bin/env bash
# render-service-account-tokens.sh — no Kubernetes API token in a pod that does
# not call the Kubernetes API (#2953).
#
# Renders the chart with its defaults (plus the operator's values) and with
# every feature on, and fails when any Pod, Deployment, StatefulSet, DaemonSet,
# Job or CronJob template of THIS chart (templates/, not a subchart) leaves
# automountServiceAccountToken unset or true, except the allowlist below: the
# workloads that do call the API and hold a Role for it.
#
#   Deployment/elitea-sandbox-supervisor   creates and watches the sandbox pods
#   Job/<release>-keda-cleanup             deletes the ScaledObjects at uninstall
#
# (The NATS bootstrap Job also calls the API, from one init container, through
# a projected token it mounts itself: its pod-level automount stays false.)
#
# Also asserted: every ServiceAccount the chart creates for a workload that
# does not call the API sets automountServiceAccountToken false; the worker
# pod names the chart-created elitea-worker ServiceAccount, not `default`; the
# per-component escape hatch (<component>.serviceAccount.automountToken=true)
# turns the token on for that pod and its ServiceAccount only.
#
# Usage: deploy/helm/tests/render-service-account-tokens.sh
# Needs: helm, python3 with PyYAML. Run deploy/helm/elitea/build-deps.sh first.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
FIX="$REPO/deploy/helm/tests/fixtures"
HELM="${HELM:-helm}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for tool in "$HELM" python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-service-account-tokens.sh needs $tool on PATH" >&2; exit 2; }
done

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

render() { # render <name> <args...> -> $WORK/<name>.yaml
  local name="$1"; shift
  "$HELM" template elitea "$CHART" --namespace elitea -f "$FIX/operator-values.yaml" "$@" \
    > "$WORK/$name.yaml" 2> "$WORK/$name.err" \
    || { fail "$name does not render: $(grep -v '^$' "$WORK/$name.err" | head -5 | tr '\n' ' ')"; return 1; }
}

# Every feature this chart can render a pod for: the defaults (main, web,
# scheduler, the LLM gateway, the worker and its workload-session Jobs,
# elitea-vector, DeepWiki, Inventory, the OTel collector, NATS and its
# bootstrap), plus the cloud executor and cloud browser with the sandbox
# supervisor and image warm-up, the egress proxy, PgBouncer, the object-store
# and agent-state Jobs, and the KEDA uninstall hook.
ALL=(-f "$FIX/cloud-executor-values.yaml" -f "$FIX/browser-sessions-values.yaml" -f "$FIX/egress-proxy-values.yaml"
  --set pgbouncer.enabled=true --set-string pgbouncer.postgresHost=postgres.example.invalid
  --set-string pgbouncer.database=elitea --set-string pgbouncer.credentialsSecretName=elitea-pgbouncer
  --set objectInit.enabled=true --set-string main.env.STORAGE_BACKEND=s3 --set-string main.env.STORAGE_CONTAINER=elitea
  --set-string objectInit.credentials.secretName=elitea-object-store
  --set-string objectInit.credentials.accessKeyKey=access-key --set-string objectInit.credentials.secretKeyKey=secret-key
  --set agentStateMigrations.enabled=true --set-string agentStateMigrations.databaseSecret.name=elitea-agentstate-db
  --set keda.enabled=true)

check() { # check <name> [<component with the escape hatch on>]
  python3 - "$WORK/$1.yaml" "$1" "${2:-}" <<'PY'
import re, sys, yaml

path, name, hatch = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
docs = []
for chunk in re.split(r"^---\s*$", text, flags=re.M):
    m = re.search(r"^# Source: (\S+)", chunk, re.M)
    d = yaml.safe_load(chunk)
    if d and m:
        docs.append((m.group(1), d))

ALLOW = {"Deployment/elitea-sandbox-supervisor", "Job/elitea-keda-cleanup"}
# The ServiceAccounts of the allowlisted workloads keep their token too.
ALLOW_SA = {"elitea-sandbox-supervisor", "elitea-keda-cleanup"}
HATCH = {"main": ("Deployment/elitea-main", "elitea-main"),
         "worker": ("Deployment/elitea-worker", "elitea-worker"),
         "llmGateway": ("Deployment/elitea-llm-gateway", "elitea-llm-gateway")}

bad, count = [], 0
def check(cond, msg):
    global count
    count += 1
    if not cond:
        bad.append(msg)

def pod_spec(d):
    k = d["kind"]
    if k in ("Deployment", "StatefulSet", "DaemonSet", "Job"):
        return d["spec"]["template"]["spec"]
    if k == "CronJob":
        return d["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    if k == "Pod":
        return d["spec"]
    return None

own = [(src, d) for src, d in docs if src.startswith("elitea/templates/")]
pods, sas = {}, {}
for src, d in own:
    if d["kind"] == "ServiceAccount":
        sas[d["metadata"]["name"]] = d
    spec = pod_spec(d)
    if spec is not None:
        pods[f'{d["kind"]}/{d["metadata"]["name"]}'] = spec

check(len(pods) > 5, f"only {len(pods)} pod templates rendered")
if name == "all-features":
    # The allowlist is exercised, not just declared.
    for key in sorted(ALLOW):
        check(key in pods, f"{key} did not render with every feature on")
want_on = set(ALLOW)
if hatch:
    want_on.add(HATCH[hatch][0])
for key, spec in sorted(pods.items()):
    mount = spec.get("automountServiceAccountToken")
    if key in want_on:
        check(mount is not False, f"{key}: calls the Kubernetes API (or opted in) but automountServiceAccountToken is false")
    else:
        check(mount is False, f"{key}: automountServiceAccountToken is {mount!r}; a pod that does not call the Kubernetes API must set false")

want_sa_on = set(ALLOW_SA) | ({HATCH[hatch][1]} if hatch else set())
for sa, d in sorted(sas.items()):
    if d["metadata"].get("namespace") not in (None, "elitea"):
        continue  # the sandbox pods' account lives in the execution namespace
    mount = d.get("automountServiceAccountToken")
    if sa in want_sa_on:
        check(mount is not False, f"ServiceAccount/{sa}: automount is false, but its workload calls the API (or opted in)")
    else:
        check(mount is False, f"ServiceAccount/{sa}: automountServiceAccountToken is {mount!r}, want false")

# The bundled NATS and Qdrant subcharts: their pods set no automount of their
# own (or the chart merges one in), so what holds is the pod's value, else its
# ServiceAccount's, else Kubernetes' default of mounting. Neither server calls
# the Kubernetes API. KEDA and prometheus-adapter are controllers that do, and
# are not asserted here.
all_sas = {d["metadata"]["name"]: d for _, d in docs if d["kind"] == "ServiceAccount"}
sub = 0
for src, d in docs:
    if not (src.startswith("elitea/charts/nats/") or src.startswith("elitea/charts/qdrant/")):
        continue
    spec = pod_spec(d)
    if spec is None:
        continue
    sub += 1
    mount = spec.get("automountServiceAccountToken")
    if mount is None:
        mount = all_sas.get(spec.get("serviceAccountName", "default"), {}).get("automountServiceAccountToken", True)
    check(mount is False, f'{d["kind"]}/{d["metadata"]["name"]} ({src}): the pod mounts a service-account token')
if name == "defaults":
    check(sub >= 2, f"only {sub} NATS/Qdrant pod templates rendered; the subchart check measured nothing")

w = pods.get("Deployment/elitea-worker")
check(w is not None, "the worker Deployment did not render")
if w is not None:
    check(w.get("serviceAccountName") == "elitea-worker", f"worker serviceAccountName is {w.get('serviceAccountName')!r}, want the chart's elitea-worker")
    check("elitea-worker" in sas, "no ServiceAccount/elitea-worker rendered")

if bad:
    print(f"{name}: {len(bad)} failed of {count}")
    for b in bad:
        print("  FAIL:", b)
    sys.exit(1)
print(f"{name}: {count} assertions over {len(pods)} pod templates and {len(sas)} ServiceAccounts")
PY
}

run() { # run <name> [hatch component] -- <helm args>
  local name="$1" hatch="$2"; shift 3
  render "$name" "$@" || return 0
  if out="$(check "$name" "$hatch" 2>&1)"; then pass "$out"; else fail "$out"; fi
}

run defaults "" --
run all-features "" -- "${ALL[@]}"
# The escape hatch, one component at a time.
run hatch-main main -- --set main.serviceAccount.automountToken=true
run hatch-worker worker -- --set worker.serviceAccount.automountToken=true
run hatch-gateway llmGateway -- --set llmGateway.serviceAccount.automountToken=true

# worker.serviceAccount.create=false: the operator's account (or `default`), still no token.
if render worker-byo --set worker.serviceAccount.create=false --set worker.serviceAccount.name=my-worker; then
  if python3 - "$WORK/worker-byo.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
w = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")["spec"]["template"]["spec"]
assert w.get("serviceAccountName") == "my-worker", w.get("serviceAccountName")
assert w.get("automountServiceAccountToken") is False, w.get("automountServiceAccountToken")
assert not any(d["kind"] == "ServiceAccount" and d["metadata"]["name"] == "elitea-worker" for d in docs), "elitea-worker SA rendered with create=false"
PY
  then pass "worker.serviceAccount.create=false: the named account, no chart ServiceAccount, no token"
  else fail "worker.serviceAccount.create=false renders the wrong account"; fi
fi

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)" >&2
  exit 1
fi
echo "all service-account token checks passed"
