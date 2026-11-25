#!/usr/bin/env bash
# render-break-glass.sh — the chart never creates or holds a sealed
# break-glass credential (ADR-0059 decision 5, "Path 2"; story R2.6a, #2520).
#
# A sealed credential is issued by an operator, in the workload, with
# `elitea-main break-glass issue`, and printed once. It must never be in a
# Kubernetes Secret, a ConfigMap, a Helm release or GitOps history, and the
# chart must never run the command for anybody. This renders the chart in the
# shapes the other render tests use and fails on:
#
#   * a Job or CronJob (or any Pod template) whose command or arguments run
#     `break-glass` or name a sealed credential;
#   * a Secret or ConfigMap whose name or keys name break-glass or a sealed
#     credential;
#   * anything, including base64-decoded Secret data, that has the shape of a
#     credential (`bg_<24 hex>.<64 hex>`).
#
# Usage: deploy/helm/tests/render-break-glass.sh
# Needs: helm, python3 with PyYAML. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
FEATURES_OFF="$REPO/deploy/helm/tests/fixtures/features-off.yaml"
command -v helm >/dev/null 2>&1 || { echo "render-break-glass.sh needs helm on PATH" >&2; exit 2; }

work="$(mktemp -d)"
trap 'rm -rf "${work:?}"' EXIT

standalone=(-f "$FEATURES_OFF" -f "$CHART/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=)

helm template bg "$CHART" "${standalone[@]}" > "$work/standalone.yaml"
helm template bg "$CHART" "${standalone[@]}" --set worker.enabled=true > "$work/worker.yaml"

cat > "$work/check.py" <<'PY'
import base64, re, sys, yaml

CREDENTIAL = re.compile(r'bg_[0-9a-fA-F]{24}\.[0-9a-fA-F]{64}')
NAMES = re.compile(r'break[-_.]?glass|sealed[-_.]?credential', re.IGNORECASE)
failures = []

def strings(value):
    if isinstance(value, dict):
        for k, v in value.items():
            yield str(k)
            yield from strings(v)
    elif isinstance(value, list):
        for v in value:
            yield from strings(v)
    elif value is not None:
        yield str(value)

def pod_specs(doc):
    spec = doc.get('spec') or {}
    if doc.get('kind') == 'Pod':
        yield spec
    template = spec.get('template') or (spec.get('jobTemplate') or {}).get('spec', {}).get('template')
    if template:
        yield template.get('spec') or {}

rendered = 0
for path in sys.argv[1:]:
    for doc in yaml.safe_load_all(open(path)):
        if not doc:
            continue
        rendered += 1
        kind, name = doc.get('kind', ''), (doc.get('metadata') or {}).get('name', '')
        where = f'{path.rsplit("/", 1)[-1]}: {kind}/{name}'
        for text in strings(doc):
            if CREDENTIAL.search(text):
                failures.append(f'{where} holds a sealed credential')
        if kind in ('Secret', 'ConfigMap'):
            keys = list((doc.get('data') or {}).keys()) + list((doc.get('stringData') or {}).keys())
            if NAMES.search(name) or any(NAMES.search(k) for k in keys):
                failures.append(f'{where} is a {kind} for break-glass')
            for key, value in (doc.get('data') or {}).items():
                if kind == 'Secret':
                    try:
                        decoded = base64.b64decode(str(value)).decode('utf-8', 'replace')
                    except Exception:
                        continue
                    if CREDENTIAL.search(decoded):
                        failures.append(f'{where} key {key} holds a sealed credential')
        for spec in pod_specs(doc):
            for container in (spec.get('initContainers') or []) + (spec.get('containers') or []):
                argv = ' '.join(str(x) for x in (container.get('command') or []) + (container.get('args') or []))
                if NAMES.search(argv) or re.search(r'\bbreak-glass\b', argv):
                    failures.append(f'{where} container {container.get("name")} runs break-glass: {argv}')
        if kind in ('Job', 'CronJob') and NAMES.search(name):
            failures.append(f'{where} is a Job for break-glass')

if rendered < 20:
    failures.append(f'premise: only {rendered} documents rendered')
if failures:
    print('\n'.join('FAIL: ' + f for f in failures), file=sys.stderr)
    sys.exit(1)
print(f'ok: {rendered} rendered documents hold no sealed credential, no break-glass Job and no break-glass Secret')
PY
python3 "$work/check.py" "$work/standalone.yaml" "$work/worker.yaml"

# The renderer must catch what it looks for: a Secret with a credential in
# it, and a Job that runs the command, both fail.
cat > "$work/negative.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata: {name: emergency}
stringData:
  credential: bg_0123456789abcdef01234567.0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
---
apiVersion: batch/v1
kind: Job
metadata: {name: issue}
spec:
  template:
    spec:
      containers:
        - name: main
          command: [elitea-main, break-glass, issue, --user, "1"]
YAML
for n in $(seq 1 20); do printf -- '---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: filler-%s}\n' "$n" >> "$work/negative.yaml"; done
if python3 "$work/check.py" "$work/negative.yaml" 2>/dev/null
then
  echo "FAIL: the negative fixture was not caught" >&2
  exit 1
fi
echo "ok: the checks catch a credential in a Secret and a Job that runs break-glass"
