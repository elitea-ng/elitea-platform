#!/usr/bin/env bash
#
# The shared provider shape (templates/_provider.tpl) holds together.
#
# WHAT THESE DEFINES EXIST FOR. A provider service is rendered by several
# templates that have to agree with each other, and every disagreement below is
# silent — the manifests apply, the pods start, and the wrong thing happens:
#
#   * The migration Job and the Deployment must read the SAME database. A Job
#     pointed at a different one migrates a database the service never uses,
#     reports success, and leaves the real schema untouched — the service then
#     answers every read with a missing-relation error.
#   * The mTLS volume and its mount are a PAIR. A mount with no volume is a pod
#     that will not schedule, which is loud. A volume with no mount is a service
#     serving plain HTTP while believing it requires a client certificate, which
#     is not.
#   * The migration Job must carry BOTH ordering dialects. Argo ignores
#     helm.sh/hook-weight when its own annotation is present, so a Job with only
#     Helm's is unordered under Argo.
#   * The Service's selector must match the Deployment's pod labels, or the
#     Service has no endpoints and every call to the provider fails at connect.
#
# These were all true before the refactor too. What changed is that one define
# now decides each of them, so this suite is what stops the define from being
# edited into disagreeing with itself.
set -euo pipefail

CHART="deploy/helm/elitea"
BASE=(-f "$CHART/values-standalone.yaml"
      --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
      --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
      --set deepwiki.enabled=true
      --set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com)

failures=0
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }
pass() { printf '  ok: %s\n' "$1"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# deepwiki.enabled is FALSE in the shipped defaults and every template here sits
# inside that condition. Without turning it on, every assertion below passes
# against manifests that never rendered.
helm template shape "$CHART" "${BASE[@]}" > "$work/on.yaml"

python3 - "$work/on.yaml" <<'PY'
import sys
import yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]

def find(kind, name_contains):
    for d in docs:
        if d.get("kind") == kind and name_contains in d.get("metadata", {}).get("name", ""):
            return d
    return None

deployment = find("Deployment", "deepwiki")
job = find("Job", "deepwiki")
service = find("Service", "deepwiki")

failures = []
def check(ok, message):
    if ok:
        print(f"  ok: {message}")
    else:
        failures.append(message)
        print(f"FAIL: {message}", file=sys.stderr)

# A floor first. If the render produced no provider objects at all, every
# check below would pass by never finding anything to disagree about.
check(deployment is not None and job is not None and service is not None,
      "the provider's Deployment, migration Job and Service all rendered")
if failures:
    raise SystemExit(1)

def secret_env(pod_spec):
    out = {}
    for container in pod_spec["containers"]:
        for env in container.get("env") or []:
            ref = (env.get("valueFrom") or {}).get("secretKeyRef")
            if ref:
                out[env["name"]] = (ref["name"], ref["key"])
    return out

dep_secrets = secret_env(deployment["spec"]["template"]["spec"])
job_secrets = secret_env(job["spec"]["template"]["spec"])

check("ELITEA_DEEPWIKI_DATABASE_URL" in dep_secrets,
      "the Deployment reads its database URL from a Secret")
check(dep_secrets.get("ELITEA_DEEPWIKI_DATABASE_URL")
      == job_secrets.get("ELITEA_DEEPWIKI_DATABASE_URL"),
      "the migration Job and the Deployment read the SAME database secret "
      f"(Job: {job_secrets.get('ELITEA_DEEPWIKI_DATABASE_URL')}, "
      f"Deployment: {dep_secrets.get('ELITEA_DEEPWIKI_DATABASE_URL')})")

pod = deployment["spec"]["template"]["spec"]
volumes = {v["name"] for v in pod.get("volumes") or []}
mounts = {m["name"] for c in pod["containers"] for m in (c.get("volumeMounts") or [])}
check(("mtls" in volumes) == ("mtls" in mounts),
      "the mTLS volume and its mount are present or absent together "
      f"(volume={'mtls' in volumes}, mount={'mtls' in mounts})")

annotations = job["metadata"].get("annotations") or {}
check("helm.sh/hook" in annotations and "argocd.argoproj.io/hook" in annotations,
      "the migration Job carries both the Helm and the Argo ordering dialects")

selector = service["spec"]["selector"]
pod_labels = deployment["spec"]["template"]["metadata"]["labels"]
missing = {k: v for k, v in selector.items() if pod_labels.get(k) != v}
# WHAT THIS CHECK CANNOT SEE, stated because it looks stronger than it is:
# both sides derive from the same selectorLabels define, so a mutation INSIDE
# that define changes them together and this check still passes. What it does
# catch is a CALL SITE that stops using the define — which is how a selector and
# a pod label actually drift in practice, and which was verified by mutation.
check(not missing,
      f"the Service selector matches the Deployment's pod labels (mismatched: {missing})")

if failures:
    print(f"\n{len(failures)} check(s) failed.", file=sys.stderr)
    raise SystemExit(1)
PY

if [ "$failures" -ne 0 ]; then
  exit 1
fi
printf '\nAll provider-shape checks passed.\n'
