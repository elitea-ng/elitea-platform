#!/usr/bin/env bash
# render-safety-alerts.sh — the runtime safety alerts render where they can
# fire, and FIRE (ADR-0041 decisions 1 and 2, #1498).
#
# Renders templates/main/safety-monitoring.yaml and checks:
#   - with the Prometheus Operator API and the default listener, one
#     PodMonitor and one PrometheusRule;
#   - with an audit exporter configured, the audit export's PodMonitor
#     scrapes the port, so this file adds no second PodMonitor (two would
#     double every sum()) but keeps the rule;
#   - with the listener off (ELITEA_AUDIT_EXPORT_METRICS_ADDR empty) or no
#     Operator API, nothing (an alert over a metric nothing scrapes could
#     never fire);
#   - no rule groups by a project or an agent label.
# Then, with promtool, `promtool test rules` on
# fixtures/safety-alerts.test.yaml proves:
#   - the trip alert fires on the FIRST trip (0 -> 1) and on a series that
#     appears late, already at 1;
#   - a platform suspension is critical, the scheduled self-test's is not
#     alerted on;
#   - an agent suspension alerts only after activeFor.
#
# PROMTOOL names the binary (default: promtool on PATH). CI sets
# SAFETY_ALERTS_REQUIRE_PROMTOOL=1, so a missing promtool fails the job
# instead of reading as a pass.
set -euo pipefail
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
DIR="$(cd "$(dirname "$0")/../.." && pwd)"
HELM_BIN="${HELM:-helm}"
PROMTOOL="${PROMTOOL:-promtool}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

render() {
  "$HELM_BIN" template elitea "$DIR/helm/elitea" -n elitea -f "$FEATURES_OFF" \
    --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
    --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true \
    "$@" --show-only templates/main/safety-monitoring.yaml 2>/dev/null || true
}

kinds() { { grep -E '^kind: ' || true; } | sort | tr '\n' ' '; }

fail() { echo "render-safety-alerts: $*" >&2; exit 1; }

got="$(render --api-versions monitoring.coreos.com/v1 | kinds)"
[ "$got" = "kind: PodMonitor kind: PrometheusRule " ] || fail "default render: want a PodMonitor and a PrometheusRule, got '$got'"

got="$(render --api-versions monitoring.coreos.com/v1 \
  --set-string main.env.ELITEA_AUDIT_EXPORT_WEBHOOK_URL=https://siem.example.invalid/events | kinds)"
[ "$got" = "kind: PrometheusRule " ] || fail "with the audit exporter: want the rule and no second PodMonitor, got '$got'"

got="$(render --api-versions monitoring.coreos.com/v1 --set-string main.env.ELITEA_AUDIT_EXPORT_METRICS_ADDR= | kinds)"
[ -z "$got" ] || fail "with the listener off: want nothing, got '$got'"

got="$(render | kinds)"
[ -z "$got" ] || fail "without the Operator API: want nothing, got '$got'"

got="$(render --api-versions monitoring.coreos.com/v1 --set main.safetyAlerts.prometheusRule.enabled=false | kinds)"
[ "$got" = "kind: PodMonitor " ] || fail "with the rule off: want the PodMonitor only, got '$got'"

render --api-versions monitoring.coreos.com/v1 > "$TMP/render.yaml"
if grep -nE 'project|application_id|agent_id' "$TMP/render.yaml" | grep -E 'by \(|\{.*(project|application)' >/dev/null; then
  fail "a rule groups or filters by a project or an agent label"
fi

if ! command -v "$PROMTOOL" >/dev/null 2>&1; then
  if [ "${SAFETY_ALERTS_REQUIRE_PROMTOOL:-0}" = "1" ]; then
    fail "SAFETY_ALERTS_REQUIRE_PROMTOOL=1 and $PROMTOOL is not available"
  fi
  echo "render-safety-alerts: render checks passed; promtool tests skipped (set PROMTOOL to a promtool binary)" >&2
  exit 0
fi

python3 - "$TMP/render.yaml" "$TMP/rules.yaml" <<'PY'
import sys, yaml
rules = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind") == "PrometheusRule"]
if len(rules) != 1:
    sys.exit(f"render-safety-alerts: want one PrometheusRule, got {len(rules)}")
yaml.safe_dump({"groups": rules[0]["spec"]["groups"]}, open(sys.argv[2], "w"))
PY
cp "$DIR/helm/tests/fixtures/safety-alerts.test.yaml" "$TMP/safety-alerts.test.yaml"
"$PROMTOOL" check rules "$TMP/rules.yaml"
"$PROMTOOL" test rules "$TMP/safety-alerts.test.yaml"
echo "render-safety-alerts: passed"
