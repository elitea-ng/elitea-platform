#!/usr/bin/env bash
# render-helm3-parity.sh — the elitea chart renders the SAME objects with
# Helm 3 as with Helm 4.
#
# CI installs, lints and packages with Helm 4 (scripts/ci/check-helm-version.mjs
# holds the pin). Argo CD renders a chart with the Helm 3 it bundles, so a
# template that only Helm 4 accepts, or that Helm 3 renders differently,
# breaks every Argo CD install with CI green. This renders each shipped values
# shape with both and compares the parsed objects (Helm 4 prints a few blank
# lines Helm 3 does not; those are not a difference). It also renders the
# packaged chart with Helm 3, which is what an OCI source hands Argo CD.
#
#   HELM3=/path/to/helm3 HELM4=/path/to/helm4 render-helm3-parity.sh
#
# Needs python3 + PyYAML. No network.
set -euo pipefail

cd "$(dirname "$0")/../../.."
: "${HELM3:?HELM3 must name a Helm 3 binary}"
: "${HELM4:?HELM4 must name a Helm 4 binary}"
"$HELM3" version --short | grep -q '^v3\.' || { echo "HELM3 is not Helm 3: $("$HELM3" version --short)" >&2; exit 1; }
"$HELM4" version --short | grep -q '^v4\.' || { echo "HELM4 is not Helm 4: $("$HELM4" version --short)" >&2; exit 1; }

CHART=deploy/helm/elitea
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

POSTURE=(--set-string 'llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1'
         --set-string 'llmGateway.egressPosture=public-unrestricted')
# name | helm args (one shape per line; word-split on purpose)
shapes=(
  "default|-f deploy/helm/tests/fixtures/operator-values.yaml"
  "qdrant-ha|-f deploy/helm/tests/fixtures/operator-values.yaml -f $CHART/values-qdrant-ha.yaml"
  "qdrant-external|-f deploy/helm/tests/fixtures/operator-values.yaml --set qdrant.enabled=false --set-string vector.qdrant.external.url=https://qdrant.example.invalid:6334"
  "staging|-f $CHART/values-staging.yaml ${POSTURE[*]}"
  "standalone|-f $CHART/values-standalone.yaml ${POSTURE[*]} --set main.autoscaling.maxReplicas=3"
  "auth-minimal|-f $CHART/values-auth-minimal.yaml ${POSTURE[*]}"
  "gke|-f $CHART/values-gke.yaml"
  "nats-ha|-f $CHART/values-staging.yaml -f $CHART/values-nats-ha.yaml ${POSTURE[*]}"
  "nats-off|-f $CHART/values-staging.yaml ${POSTURE[*]} --set nats.enabled=false --set natsBootstrap.enabled=false --set-string natsClient.service=my-nats"
  "sandbox-runtime|-f $CHART/values-staging.yaml ${POSTURE[*]} --set sandboxRuntime.enabled=true --set sandboxRuntime.mode=containerd"
)

"$HELM4" package "$CHART" -d "$work/pkg" >/dev/null 2>&1
tgz=("$work"/pkg/elitea-*.tgz)

fails=0
for shape in "${shapes[@]}"; do
  name="${shape%%|*}"
  # shellcheck disable=SC2206 # the args are word-split on purpose
  args=(${shape#*|})
  for src in dir pkg; do
    chart="$CHART"; [ "$src" = pkg ] && chart="${tgz[0]}"
    if ! "$HELM3" template ci "$chart" -n elitea "${args[@]}" > "$work/$name-$src-3.yaml" 2> "$work/err3"; then
      echo "FAIL: $name ($src): Helm 3 refuses to render"; tail -3 "$work/err3"; fails=$((fails + 1)); continue
    fi
    if ! "$HELM4" template ci "$chart" -n elitea "${args[@]}" > "$work/$name-$src-4.yaml" 2> "$work/err4"; then
      echo "FAIL: $name ($src): Helm 4 refuses to render"; tail -3 "$work/err4"; fails=$((fails + 1)); continue
    fi
    if python3 - "$work/$name-$src-3.yaml" "$work/$name-$src-4.yaml" <<'PY'
import sys, yaml
a, b = ([d for d in yaml.safe_load_all(open(p)) if d] for p in sys.argv[1:3])
if a == b:
    sys.exit(0)
ka = {(d.get("kind"), d.get("metadata", {}).get("name")) for d in a}
kb = {(d.get("kind"), d.get("metadata", {}).get("name")) for d in b}
print(f"  only Helm 3: {sorted(ka - kb)}  only Helm 4: {sorted(kb - ka)}")
for x, y in zip(a, b):
    if x != y:
        print(f"  first differing object: {x.get('kind')}/{x.get('metadata', {}).get('name')}")
        break
sys.exit(1)
PY
    then echo "ok:   $name ($src): Helm 3 and Helm 4 render the same objects"
    else echo "FAIL: $name ($src): Helm 3 and Helm 4 render different objects"; fails=$((fails + 1))
    fi
  done
done

if [ "$fails" -ne 0 ]; then echo "$fails render(s) differ or fail"; exit 1; fi
echo "render-helm3-parity: Helm $("$HELM3" version --short) and $("$HELM4" version --short) render the same chart"
