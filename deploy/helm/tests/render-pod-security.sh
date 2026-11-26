#!/usr/bin/env bash
# render-pod-security.sh — every container the chart renders runs non-root,
# without privilege escalation, with every capability dropped and on a
# read-only root filesystem (#2954, ADR-0011 trust zones).
#
# For each container and init container of every Deployment, StatefulSet,
# DaemonSet, Job, CronJob and Pod in two renders (a default install with the
# operator-only values of fixtures/operator-values.yaml, and the same with
# the optional workloads that take a test fixture turned on), it
# asserts:
#   * runAsNonRoot: true, at the container or the pod level (a container-level
#     false overrides the pod);
#   * allowPrivilegeEscalation: false;
#   * capabilities.drop contains ALL;
#   * readOnlyRootFilesystem: true.
#
# Third-party subcharts (NATS, Qdrant) keep their own values and are not
# checked here. Every exception is listed in ALLOWED below, per container and
# per rule, with the reason; an entry that no longer matches a rendered
# container fails the test, so the list cannot go stale.
#
# Usage: deploy/helm/tests/render-pod-security.sh
# Needs: helm, python3 with PyYAML. No cluster, no network.
set -euo pipefail
cd "$(dirname "$0")/../../.."
chart=deploy/helm/elitea
work=$(mktemp -d)
trap 'rm -rf "${work:?}"' EXIT

base=(--namespace platform -f deploy/helm/tests/fixtures/operator-values.yaml
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)

helm template test "$chart" "${base[@]}" > "$work/defaults.yaml"
# The optional first-party workloads with a fixture: PgBouncer, the egress
# proxy and the Kubernetes code sandbox (supervisor, image warm-up).
helm template test "$chart" "${base[@]}" \
  -f deploy/helm/tests/fixtures/egress-proxy-values.yaml \
  --set pgbouncer.enabled=true --set pgbouncer.postgresHost=db.invalid \
  --set pgbouncer.database=elitea --set pgbouncer.credentialsSecretName=pgbouncer-credentials \
  > "$work/optional.yaml"

python3 - "$work/defaults.yaml" "$work/optional.yaml" <<'PY'
import sys
import yaml

# (workload name, container name) -> {rule: reason}. Rules: nonroot,
# escalation, capabilities, readonly.
ALLOWED = {
    # The worker's material init container chowns the runtime material and the
    # output spool to the worker's uid: the emptyDirs arrive owned by root and
    # the worker refuses material it does not own at 0600. It runs as uid 0 with
    # only CHOWN, DAC_OVERRIDE and FOWNER, on a read-only root filesystem, and
    # exits before the worker starts; the worker container itself is non-root.
    ("elitea-worker", "material"): {
        "nonroot": "chowns the runtime material to the worker uid before the worker starts",
    },
}
# The sandbox workspace-run Pods are writable by design (the agent builds the
# user's project in them), but the supervisor creates them at run time from
# services/elitea-worker-rust; the chart renders none, so they need no entry.

KINDS = {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}


def pod_spec(doc):
    kind = doc["kind"]
    if kind == "Pod":
        return doc["spec"]
    if kind == "CronJob":
        return doc["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    return doc["spec"]["template"]["spec"]


def first_party(doc):
    labels = doc.get("metadata", {}).get("labels", {}) or {}
    # Subcharts label themselves with their own chart name.
    chart = labels.get("helm.sh/chart", "")
    return not chart or chart.startswith("elitea-")


failures, used, checked = [], set(), 0
for path in sys.argv[1:]:
    for doc in yaml.safe_load_all(open(path)):
        if not doc or doc.get("kind") not in KINDS or not first_party(doc):
            continue
        name = doc["metadata"]["name"]
        spec = pod_spec(doc)
        pod = spec.get("securityContext") or {}
        for container in (spec.get("initContainers") or []) + (spec.get("containers") or []):
            checked += 1
            sc = container.get("securityContext") or {}
            nonroot = sc.get("runAsNonRoot", pod.get("runAsNonRoot"))
            results = {
                "nonroot": nonroot is True,
                "escalation": sc.get("allowPrivilegeEscalation") is False,
                "capabilities": "ALL" in ((sc.get("capabilities") or {}).get("drop") or []),
                "readonly": sc.get("readOnlyRootFilesystem") is True,
            }
            key = (name, container["name"])
            allowed = ALLOWED.get(key, {})
            for rule, ok in results.items():
                if ok:
                    continue
                if rule in allowed:
                    used.add((key, rule))
                    continue
                failures.append(f"{doc['kind']}/{name} container {container['name']}: {rule} is not enforced")

for key, rules in ALLOWED.items():
    for rule in rules:
        if (key, rule) not in used:
            failures.append(f"ALLOWED lists {key} {rule}, which no rendered container needs any more; remove it")

if checked < 15:
    failures.append(f"only {checked} containers were checked; the render is not what this test expects")
if failures:
    print("\n".join(sorted(set(failures))), file=sys.stderr)
    sys.exit(1)
print(f"render-pod-security: {checked} containers checked, OK")
PY
