#!/usr/bin/env bash
# render-scheduler-alerts.sh — the budget write-back alerts FIRE ([human
# decision] 2026-10-16, "Isolate, then drop"; fourth review of #1296).
#
# Renders the elitea chart's scheduler PrometheusRule (with the Prometheus
# Operator API served), extracts its rule groups, and runs `promtool test
# rules` on fixtures/scheduler-alerts.test.yaml. That file proves:
#   - the drop alert fires on the FIRST drop (0 -> 1);
#   - it fires on a series that appears late, already at 1;
#   - it does not fire when nothing is dropped;
#   - the ack-pending alert pages above 80% of MaxAckPending for 5m.
#
# PROMTOOL names the binary (default: promtool on PATH). CI sets
# SCHEDULER_ALERTS_REQUIRE_PROMTOOL=1, so a missing promtool fails the job
# instead of reading as a pass.
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
DIR="$(cd "$(dirname "$0")/../.." && pwd)"
HELM="${HELM:-helm}"
PROMTOOL="${PROMTOOL:-promtool}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A values-file number is a float64 to Helm; a bare toString printed one of a
# million or more as "1e+06" and the positive-integer check refused a valid
# setting. Needs no promtool, so it runs before the skip below. (--set would
# hand the chart an int64 and hide the bug, hence the file.)
printf 'scheduler:\n  metrics:\n    prometheusRule:\n      maxAckPending: 1000000\n' > "$TMP/big.yaml"
if ! "$HELM" template elitea "$DIR/helm/elitea" -n elitea --api-versions monitoring.coreos.com/v1 \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true \
  -f "$TMP/big.yaml" --show-only templates/scheduler/monitoring.yaml | grep -qF '/ 1000000 > 0.8'; then
  echo "render-scheduler-alerts: maxAckPending 1000000 from a values file did not render as an integer" >&2
  exit 1
fi

if ! command -v "$PROMTOOL" >/dev/null 2>&1; then
  if [ "${SCHEDULER_ALERTS_REQUIRE_PROMTOOL:-0}" = "1" ]; then
    echo "render-scheduler-alerts: SCHEDULER_ALERTS_REQUIRE_PROMTOOL=1 and $PROMTOOL is not available" >&2
    exit 1
  fi
  echo "render-scheduler-alerts: skipped (set PROMTOOL to a promtool binary)" >&2
  exit 0
fi

"$HELM" template elitea "$DIR/helm/elitea" -n elitea --api-versions monitoring.coreos.com/v1 \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true \
  --show-only templates/scheduler/monitoring.yaml > "$TMP/render.yaml"

python3 - "$TMP/render.yaml" "$TMP/rules.yaml" <<'PY'
import sys, yaml
rules = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind") == "PrometheusRule"]
if len(rules) != 1:
    sys.exit(f"render-scheduler-alerts: want one PrometheusRule, got {len(rules)}")
yaml.safe_dump({"groups": rules[0]["spec"]["groups"]}, open(sys.argv[2], "w"))
PY
cp "$DIR/helm/tests/fixtures/scheduler-alerts.test.yaml" "$TMP/scheduler-alerts.test.yaml"
"$PROMTOOL" check rules "$TMP/rules.yaml"
"$PROMTOOL" test rules "$TMP/scheduler-alerts.test.yaml"
echo "render-scheduler-alerts: passed"
