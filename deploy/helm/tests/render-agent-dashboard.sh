#!/usr/bin/env bash
# render-agent-dashboard.sh — the agent runs/errors/tool calls/spend Grafana
# dashboard (ADR-0042, #1499) and the stream-generic audit export alerts.
#
# Asserts:
#   - by default no ConfigMap carries the grafana_dashboard label;
#   - main.grafanaDashboard.enabled=true renders exactly one, labelled
#     grafana_dashboard: "1", whose JSON parses, uses a datasource template
#     variable (no hard-coded datasource uid), and whose panels query the
#     expected metrics and ONLY metrics a service exposes (the allowlist below
#     names each one's defining file, so a renamed or invented metric fails);
#   - the audit export lag and backlog alerts group by `stream` and name it in
#     their annotations, with no stream value hard-coded, so a new stream
#     (run-step evidence) alerts on its own series.
#
# Needs helm and python3 with PyYAML.
set -euo pipefail
# Every `helm template` here starts from the pre-S10 feature switches
# (fixtures/features-off.yaml), prepended so any --set a render names wins.
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
_helm_bin="${HELM:-helm}"
helm() {
  if [ "${1:-}" = template ]; then shift; command "$_helm_bin" template -f "$FEATURES_OFF" "$@"
  else command "$_helm_bin" "$@"; fi
}
DIR="$(cd "$(dirname "$0")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

render() {
  helm template elitea "$DIR/helm/elitea" -n elitea --api-versions monitoring.coreos.com/v1 \
    --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
    --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true \
    "$@"
}

render > "$TMP/off.yaml"
render --set main.grafanaDashboard.enabled=true \
  --set-string main.grafanaDashboard.annotations.grafana_folder=Elitea > "$TMP/on.yaml"
# The audit export alerts render only with an exporter configured.
render --set-string main.env.ELITEA_AUDIT_EXPORT_WEBHOOK_URL=https://siem.example.invalid/hook \
  --show-only templates/main/audit-export-monitoring.yaml > "$TMP/alerts.yaml"

python3 - "$TMP/off.yaml" "$TMP/on.yaml" "$TMP/alerts.yaml" <<'PY'
import json, re, sys, yaml

def docs(path):
    return [d for d in yaml.safe_load_all(open(path)) if d]

def dashboards(path):
    return [d for d in docs(path) if d.get("kind") == "ConfigMap"
            and "grafana_dashboard" in ((d.get("metadata") or {}).get("labels") or {})]

fail = []
if dashboards(sys.argv[1]):
    fail.append("default render has a grafana_dashboard ConfigMap; the toggle must default to off")

on = dashboards(sys.argv[2])
if len(on) != 1:
    fail.append(f"enabled: want exactly one grafana_dashboard ConfigMap, got {len(on)}")
else:
    cm = on[0]
    if cm["metadata"]["labels"]["grafana_dashboard"] != "1":
        fail.append('grafana_dashboard label is not "1"')
    if (cm["metadata"].get("annotations") or {}).get("grafana_folder") != "Elitea":
        fail.append("main.grafanaDashboard.annotations did not reach the ConfigMap")
    data = cm.get("data") or {}
    if len(data) != 1:
        fail.append(f"want one dashboard JSON key, got {sorted(data)}")
    dash = json.loads(next(iter(data.values())))
    tvars = {v["name"]: v for v in dash["templating"]["list"]}
    if tvars.get("datasource", {}).get("type") != "datasource":
        fail.append("no `datasource` template variable of type datasource")
    if tvars.get("pgdatasource", {}).get("query") != "grafana-postgresql-datasource":
        fail.append("no `pgdatasource` template variable for the PostgreSQL data source")
    exprs, sqls = [], []
    def walk(panels):
        for p in panels:
            walk(p.get("panels") or [])
            # Per-agent panels read the database (an agent id is unbounded
            # cardinality, so no Prometheus series carries it); every other
            # panel reads Prometheus.
            want = "${pgdatasource}" if (p.get("datasource") or {}).get("type") == "grafana-postgresql-datasource" else "${datasource}"
            ds = p.get("datasource")
            if ds is not None and ds.get("uid") != want:
                fail.append(f"panel {p.get('title')!r}: datasource uid {ds.get('uid')!r} is not {want}")
            for t in p.get("targets") or []:
                if (t.get("datasource") or {}).get("uid") != want:
                    fail.append(f"panel {p.get('title')!r}: target datasource is not {want}")
                if want == "${pgdatasource}":
                    sqls.append(t["rawSql"])
                else:
                    exprs.append(t["expr"])
    walk(dash["panels"])
    # The PostgreSQL panels read only these tables, the ones the docs tell the
    # operator to grant the read-only Grafana role.
    pg_allowed = {"elitea_runtime.run_step_heads", "elitea_runtime.run_steps", "gateway.llm_usage_events"}
    pg_used = set()
    for q in sqls:
        pg_used |= set(re.findall(r"\b(?:FROM|JOIN)\s+([a-z_]+\.[a-z_]+)", q))
        if "$__timeFilter(" not in q or "LIMIT" not in q:
            fail.append("a PostgreSQL panel query is not bounded by $__timeFilter and LIMIT")
    for name in sorted(pg_used - pg_allowed):
        fail.append(f"a PostgreSQL panel reads {name}, outside the documented grant")
    if len(sqls) != 3:
        fail.append(f"want 3 per-agent PostgreSQL panels (runs, tool calls, spend), got {len(sqls)}")
    # Every metric a service exposes that the dashboard may use, and where it
    # is defined. A name outside this list is a metric nobody exports.
    allowed = {
        # services/elitea-main/internal/auditexport/metrics.go
        "elitea_audit_export_delivered_total", "elitea_audit_export_failed_total",
        "elitea_audit_export_dead_lettered_total", "elitea_audit_export_backlog",
        "elitea_audit_export_lag_seconds",
        # services/elitea-llm-gateway/internal/llmproxy/handler.go
        "gateway_llm_sse_active_connections",
        # services/elitea-llm-gateway/internal/llmproxy/modelmap.go
        "gateway_model_map_refused_no_project_total", "gateway_model_map_refused_no_database_total",
        "gateway_model_map_refused_lookup_failed_total",
        # services/elitea-llm-gateway/internal/llmproxy/audio.go
        "gateway_audio_unpriced_total", "gateway_audio_default_priced_total",
        "gateway_audio_non_token_priced_total", "gateway_audio_refused_unpriced_model_total",
        # services/elitea-llm-gateway/internal/llmproxy/realtime.go
        "gateway_realtime_sessions_opened_total", "gateway_realtime_refused_unpriced_model_total",
        "gateway_realtime_refused_capacity_total", "gateway_realtime_turns_billed_total",
        "gateway_realtime_turns_unpriced_total", "gateway_realtime_turns_refused_total",
        "gateway_realtime_sessions_closed_budget_total", "gateway_realtime_frames_dropped_total",
        "gateway_realtime_turn_basis_mismatch_total", "gateway_realtime_turns_unbilled_total",
        "gateway_realtime_sessions_closed_model_total",
        # services/elitea-llm-gateway/internal/failmode/recovery.go
        "gateway_budget_outage_rows", "gateway_budget_recovery_failures_total",
        # services/elitea-llm-gateway/cmd/elitea-llm-gateway/main.go
        "gateway_budget_enforcement_enabled",
        # services/elitea-scheduler/internal/budgetwriteback/metrics.go
        "budget_writeback_dropped_total", "budget_writeback_num_ack_pending",
        "budget_writeback_num_redelivered",
    }
    used = set()
    for e in exprs:
        used |= set(re.findall(r"\b(?:elitea|gateway|budget_writeback)_[a-z0-9_]+\b", e))
    for name in sorted(used - allowed):
        fail.append(f"dashboard queries {name}, which no service exposes")
    expected = {
        "elitea_audit_export_delivered_total",    # runs (run-step records)
        "gateway_llm_sse_active_connections",     # runs (live load)
        "elitea_audit_export_failed_total",       # errors
        "elitea_audit_export_dead_lettered_total",
        "gateway_model_map_refused_lookup_failed_total",
        "gateway_realtime_turns_unpriced_total",  # spend integrity
        "budget_writeback_dropped_total",
        "gateway_budget_outage_rows",
    }
    for name in sorted(expected - used):
        fail.append(f"dashboard no longer queries {name}")
    if not any('source="run_step"' in e for e in exprs):
        fail.append('no panel reads the run-step evidence (source="run_step")')

rules = [d for d in docs(sys.argv[3]) if d.get("kind") == "PrometheusRule"]
if len(rules) != 1:
    fail.append(f"audit export: want one PrometheusRule, got {len(rules)}")
else:
    by = {r["alert"]: r for g in rules[0]["spec"]["groups"] for r in g["rules"]}
    for name in ("EliteaAuditExportLag", "EliteaAuditExportBacklog"):
        r = by.get(name)
        if r is None:
            fail.append(f"{name} is missing"); continue
        if not re.search(r"by \(stream\)", r["expr"]):
            fail.append(f"{name}: expr does not group by stream: {r['expr']}")
        for key in ("summary", "description"):
            if "{{ $labels.stream }}" not in r["annotations"].get(key, ""):
                fail.append(f"{name}: {key} does not name {{{{ $labels.stream }}}}")
        text = r["expr"] + " ".join(r["annotations"].values())
        for hard in ("security_audit", "admin_audit", "run_step", "audit_events", "outbox"):
            if hard in text:
                fail.append(f"{name}: hard-codes {hard!r}; it must work for any stream")
    dl = by.get("EliteaAuditExportDeadLettered")
    if dl is None or "by (sink, source)" not in dl["expr"] or "{{ $labels.source }}" not in dl["annotations"]["description"]:
        fail.append("EliteaAuditExportDeadLettered must group by (sink, source) and name the source")

if fail:
    sys.exit("render-agent-dashboard:\n  " + "\n  ".join(fail))
PY
echo "render-agent-dashboard: passed"
