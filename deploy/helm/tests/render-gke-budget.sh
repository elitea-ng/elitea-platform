#!/usr/bin/env bash
# render-gke-budget.sh — deploy/helm/elitea/values-gke.yaml renders, and its
# steady-state REQUESTS fit the budget the file header promises: 4 vCPU and
# 10 GiB for the whole release.
#
# Why a test and not the header's arithmetic: the header is prose. A default
# that changes in values.yaml (a sidecar added to a pod, a replica floor
# raised by a guard) changes the real number without touching this file, and
# the cluster then fails to schedule the release on the reserved nodes — a
# pod Pending with "Insufficient cpu", far from the change that caused it.
#
# What is summed, from the RENDERED manifest:
#   * every Deployment and StatefulSet, every app container's requests, times
#     its replicas — or, when an HPA or a KEDA ScaledObject targets it (the
#     replicas field is then omitted), its minReplicas / minReplicaCount;
#   * NOT init containers (Kubernetes takes the larger of init and app, never
#     the sum), NOT hook Jobs or CronJobs (seconds long, never all at once);
#   * the bundled NATS server from nats.container.merge.resources when this
#     branch renders no NATS StatefulSet itself (the values block is the
#     integration contract; once the chart renders the StatefulSet, it is
#     summed like any other and this fallback stops applying).
#   * the PEAK during a rollout: the steady state plus the largest surge one
#     Deployment adds while it rolls (maxSurge pods of it; Deployments roll
#     one at a time under Argo CD's waves and Helm alike). The peak must fit
#     the budget too: a surge pod with no room is Pending, and the rollout
#     stalls on it.
#
# Also asserted: the ExternalSecrets render, gcs is keyless (no storage key
# ExternalSecret, the elitea-main KSA carries its Google service account), and
# objectInit is off.
#
# Usage: deploy/helm/tests/render-gke-budget.sh
# Needs: helm, python3 + PyYAML. No cluster, no network.
set -euo pipefail

cd "$(dirname "$0")/../../.."

chart=deploy/helm/elitea
values="$chart/values-gke.yaml"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "== values-gke.yaml renders"
if ! helm template elitea "$chart" --namespace elitea -f "$values" > "$work/gke.yaml" 2> "$work/err"; then
  cat "$work/err" >&2
  echo "FAIL: values-gke.yaml does not render" >&2
  exit 1
fi
echo "  rendered"

echo "== steady-state requests"
python3 - "$work/gke.yaml" "$values" <<'PY'
import math, sys, yaml

CPU_BUDGET_M, MEM_BUDGET_MI = 4000, 10 * 1024

def cpu_m(v):
    v = str(v)
    return float(v[:-1]) if v.endswith("m") else float(v) * 1000

def mem_mi(v):
    v = str(v)
    for unit, f in (("Ki", 1 / 1024), ("Mi", 1), ("Gi", 1024), ("Ti", 1024 * 1024)):
        if v.endswith(unit):
            return float(v[: -len(unit)]) * f
    return float(v) / (1024 * 1024)

def requests(containers):
    c = m = 0.0
    for x in containers:
        r = (x.get("resources") or {}).get("requests") or {}
        c += cpu_m(r.get("cpu", 0))
        m += mem_mi(r.get("memory", 0))
    return c, m

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
values = yaml.safe_load(open(sys.argv[2]))
problems = []

hpa_min = {(d["spec"]["scaleTargetRef"]["kind"], d["spec"]["scaleTargetRef"]["name"]): d["spec"].get("minReplicas", 1)
           for d in docs if d["kind"] == "HorizontalPodAutoscaler"}
# A KEDA ScaledObject owns its target the same way (its scaleTargetRef kind
# defaults to Deployment): count its minReplicaCount.
hpa_min.update({(d["spec"]["scaleTargetRef"].get("kind", "Deployment"), d["spec"]["scaleTargetRef"]["name"]): d["spec"].get("minReplicaCount", 0)
                for d in docs if d["kind"] == "ScaledObject"})
rows, worker, surges = [], None, []
for d in docs:
    if d["kind"] not in ("Deployment", "StatefulSet"):
        continue
    name = d["metadata"]["name"]
    replicas = hpa_min.get((d["kind"], name), d["spec"].get("replicas", 1))
    c, m = requests(d["spec"]["template"]["spec"]["containers"])
    rows.append((name, replicas, c, m))
    if name.endswith("-worker"):
        worker = (c, m)
    strat = d["spec"].get("strategy") or {}
    if d["kind"] == "Deployment" and strat.get("type", "RollingUpdate") == "RollingUpdate":
        ms = (strat.get("rollingUpdate") or {}).get("maxSurge", "25%")
        n = math.ceil(replicas * int(ms[:-1]) / 100) if isinstance(ms, str) and ms.endswith("%") else int(ms)
        if n:
            surges.append((name, n * c, n * m))

if not any("nats" in r[0] for r in rows):
    res = (((values.get("nats") or {}).get("container") or {}).get("merge") or {}).get("resources") or {}
    if not res.get("requests"):
        problems.append("no NATS StatefulSet rendered and no nats.container.merge.resources.requests to count")
    else:
        c, m = requests([{"resources": res}])
        rows.append(("nats (values: nats.container.merge.resources)", 1, c, m))

total_c = sum(r * c for _, r, c, _ in rows)
total_m = sum(r * m for _, r, _, m in rows)
for name, r, c, m in sorted(rows):
    print(f"  {name:48} x{r}  {c * r:7.0f}m  {m * r:7.0f}Mi")
print(f"  {'STEADY STATE':48}     {total_c:7.0f}m  {total_m:7.0f}Mi")
if not worker:
    problems.append("no worker Deployment rendered")
peak = max(surges, key=lambda x: (x[1] / CPU_BUDGET_M + x[2] / MEM_BUDGET_MI), default=("none", 0, 0))
peak_c = total_c + max((x[1] for x in surges), default=0)
peak_m = total_m + max((x[2] for x in surges), default=0)
print(f"  {'PEAK DURING A ROLLOUT (largest surge: ' + peak[0] + ')':48}     {peak_c:7.0f}m  {peak_m:7.0f}Mi")
if peak_c > CPU_BUDGET_M:
    problems.append(f"rollout peak CPU requests {peak_c:.0f}m exceed {CPU_BUDGET_M}m; give the largest surger maxSurge 0 / maxUnavailable 1")
if peak_m > MEM_BUDGET_MI:
    problems.append(f"rollout peak memory requests {peak_m:.0f}Mi exceed {MEM_BUDGET_MI}Mi; give the largest surger maxSurge 0 / maxUnavailable 1")

if total_c > CPU_BUDGET_M:
    problems.append(f"steady-state CPU requests {total_c:.0f}m exceed {CPU_BUDGET_M}m")
if total_m > MEM_BUDGET_MI:
    problems.append(f"steady-state memory requests {total_m:.0f}Mi exceed {MEM_BUDGET_MI}Mi")

# Keyless GCS and the secret source.
kinds = {}
for d in docs:
    kinds.setdefault(d["kind"], {})[d["metadata"]["name"]] = d
ext = kinds.get("ExternalSecret", {})
if "elitea-main-db" not in ext:
    problems.append("no ExternalSecret for the Cloud SQL DSN (elitea-main-db)")
if "elitea-main-storage-secrets" in ext:
    problems.append("gcs is keyless, yet a storage-key ExternalSecret rendered")
sa = kinds.get("ServiceAccount", {}).get("elitea-main") or {}
if not ((sa.get("metadata") or {}).get("annotations") or {}).get("iam.gke.io/gcp-service-account"):
    problems.append("the elitea-main KSA has no iam.gke.io/gcp-service-account annotation")
if any(n.endswith("object-init") for n in kinds.get("Job", {})):
    problems.append("objectInit rendered on gcs")

for p in problems:
    print("FAIL: " + p, file=sys.stderr)
sys.exit(1 if problems else 0)
PY

echo "== GCS guards"
refuse() {
  local description="$1" fragment="$2" out
  shift 2
  if out="$(helm template elitea "$chart" --namespace elitea -f "$values" "$@" 2>&1)"; then
    echo "FAIL: $description: rendered, expected a refusal" >&2; exit 1
  elif ! grep -qF -- "$fragment" <<<"$out"; then
    echo "FAIL: $description: refused for the wrong reason: $(tail -1 <<<"$out")" >&2; exit 1
  fi
  echo "  refused: $description"
}
refuse "gcs with no Google service account on the elitea-main KSA" "reach the bucket as the NODE's service account" \
  --set main.serviceAccount.annotations=null
refuse "objectInit on gcs" "objectInit creates S3-compatible buckets only" --set objectInit.enabled=true
if helm template elitea "$chart" --namespace elitea -f "$values" --set main.serviceAccount.annotations=null \
    --set main.env.GCS_ENDPOINT=http://gcs-emulator:4443 >/dev/null 2>&1; then
  echo "  an emulator (GCS_ENDPOINT) needs no Google service account"
else
  echo "FAIL: gcs with GCS_ENDPOINT set was refused" >&2; exit 1
fi

echo "render-gke-budget: OK"
