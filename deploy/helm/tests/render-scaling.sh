#!/usr/bin/env bash
# render-scaling.sh — scale-out readiness (S11), on rendered output.
#
# ONE RULE runs through deploy/helm/elitea/templates/_scaling.tpl: an
# autoscaler renders only when its controller AND its metric source are
# present. This chart has already shipped an autoscaler that could not scale
# (the gateway HPA named a metric nothing published and pinned the fleet at
# minReplicas, with nothing anywhere saying so), so every check below renders
# BOTH directions: a guard that only refuses is indistinguishable from a
# broken template, and one that only renders is not a guard.
#
#   sources     each autoscaler with and without its source: the worker's
#               KEDA trigger; DeepWiki's keda-prometheus / keda-postgresql /
#               cpu; Inventory's single-replica rule; the gateway's
#               keda-prometheus / adapter / cpu
#   keda        keda.enabled on/off; the first-install hook and the adopted
#               plain resource; SkipDryRunOnMissingResource on EVERY
#               ScaledObject and TriggerAuthentication
#   adapter     prometheusAdapter on/off; its rules name exactly the gateway
#               metric and nothing else (no default rules)
#   replicas    no Deployment carries `replicas:` while an autoscaler owns it
#   spread/pdb  soft zone + node spread on every multi-replica workload; a PDB
#               only where the floor is above one replica
#   grace       terminationGracePeriodSeconds >= preStop + drain + margin, for
#               elitea-main, the gateway, DeepWiki and the worker
#   budget      the connection-budget refusal lists every component
#   gateway     LLM_BUDGET_EXPECTED_REPLICAS derived from the replica ceiling,
#               an explicit lower value refused
#
# Usage: deploy/helm/tests/render-scaling.sh   (after build-deps.sh, as CI)
# Needs: helm, python3 + PyYAML. No cluster, no network.
set -euo pipefail

DIR="$(cd "$(dirname "$0")/../.." && pwd)"   # deploy/
CHART="$DIR/helm/elitea"
# The operator's values and nothing else: the shipped defaults (S10) with
# every feature on — main, vector, web, gateway, scheduler, worker, DeepWiki
# with its database, Inventory — so every component the budget counts is here.
OPERATOR="$DIR/helm/tests/fixtures/operator-values.yaml"
HELM="${HELM:-helm}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }
ok() { echo "ok:   $*"; }

# render <name> helm-args... -> $TMP/<name>.yaml (and .err); status of helm.
# To files, never `helm ... | grep -q`: under pipefail grep -q closing the pipe
# early can SIGPIPE helm and fail a render that printed the line.
#
# A server sized for the fleet (postgresql.maxConnections 2000), so a guard
# under test is never the connection budget's by accident: the worker alone at
# its default ceiling of 10 replicas holds 320 AgentState connections. The
# budget section below sets its own, smaller server.
render() {
  local name="$1"; shift
  "$HELM" template elitea "$CHART" -n elitea -f "$OPERATOR" --set postgresql.maxConnections=2000 "$@" \
    > "$TMP/$name.yaml" 2> "$TMP/$name.err"
}

# expect_render <what> <name> helm-args...
expect_render() {
  local what="$1" name="$2"; shift 2
  if render "$name" "$@"; then ok "renders: $what"; else fail "refused: $what — $(tail -c 600 "$TMP/$name.err")"; fi
}

# expect_refusal <what> <fragment> helm-args...
expect_refusal() {
  local what="$1" fragment="$2"; shift 2
  if render refused "$@"; then fail "rendered, expected a refusal: $what"; return; fi
  if grep -qF -- "$fragment" "$TMP/refused.err"; then ok "refused: $what"
  else fail "refused for the wrong reason: $what — $(tail -c 600 "$TMP/refused.err")"; fi
}

# check <name> <what> <python expression over D (the documents)>; the helpers
# below are in scope. The expression is truthy for a pass.
check() {
  local name="$1" what="$2" expr="$3"
  if python3 - "$TMP/$name.yaml" "$expr" <<'PY'
import sys, yaml
D = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
def objs(kind, name=None):
    return [d for d in D if d.get('kind') == kind and (name is None or d['metadata']['name'] == name)]
def one(kind, name):
    found = objs(kind, name)
    return found[0] if found else None
def has(kind, name):
    return one(kind, name) is not None
def pod(name):
    return one('Deployment', name)['spec']['template']['spec']
def env(deploy, container, key):
    for c in pod(deploy)['containers']:
        if c['name'] == container:
            for e in c.get('env', []):
                if e['name'] == key:
                    return e.get('value')
    for cm in objs('ConfigMap'):
        if key in (cm.get('data') or {}) and cm['metadata']['name'].startswith(deploy):
            return cm['data'][key]
    return None
def ann(obj):
    return (obj.get('metadata') or {}).get('annotations') or {}
def spread_keys(deploy):
    return sorted((c['topologyKey'], c['whenUnsatisfiable']) for c in pod(deploy).get('topologySpreadConstraints') or [])
def triggers(name):
    return [t['type'] for t in one('ScaledObject', name)['spec']['triggers']]
sys.exit(0 if eval('(' + sys.argv[2].strip() + ')') else 1)
PY
  then ok "$what"; else fail "$what"; fi
}

ZONE_NODE="[('kubernetes.io/hostname', 'ScheduleAnyway'), ('topology.kubernetes.io/zone', 'ScheduleAnyway')]"
KEDA_API=(--api-versions keda.sh/v1alpha1)
MON_API=(--api-versions monitoring.coreos.com/v1)
PROM=(--set-string scaling.prometheus.serverAddress=http://prometheus-operated.monitoring.svc:9090)

# ── the shipped defaults ─────────────────────────────────────────────────────
echo "== defaults"
expect_render "the operator's values" default
check default "no ScaledObject, TriggerAuthentication or KEDA without asking" \
  "not objs('ScaledObject') and not objs('TriggerAuthentication') and not has('Deployment', 'keda-operator')"
check default "elitea-main and elitea-vector carry no replicas: under their HPAs" \
  "'replicas' not in one('Deployment','elitea-main')['spec'] and 'replicas' not in one('Deployment','elitea-vector')['spec']"
check default "the single-replica components keep their replicas:" \
  "one('Deployment','elitea-llm-gateway')['spec']['replicas'] == 1 and one('Deployment','elitea-deepwiki')['spec']['replicas'] == 1"
check default "elitea-main spreads over zones then nodes, softly" "spread_keys('elitea-main') == $ZONE_NODE"
check default "elitea-vector and elitea-web spread too" \
  "spread_keys('elitea-vector') == $ZONE_NODE and spread_keys('elitea-web') == $ZONE_NODE"
check default "a single-replica component gets no spread (nothing to spread)" \
  "spread_keys('elitea-llm-gateway') == [] and spread_keys('elitea-worker') == []"
check default "a PDB on every component whose floor is above one replica" \
  "all(has('PodDisruptionBudget', n) for n in ('elitea-main','elitea-vector','elitea-web'))"
check default "no PDB over a single replica (it would block every drain)" \
  "not has('PodDisruptionBudget','elitea-llm-gateway') and not has('PodDisruptionBudget','elitea-worker') and not has('PodDisruptionBudget','elitea-deepwiki')"
check default "elitea-main: native preStop sleep 10, shutdown 30s, grace 50" \
  "pod('elitea-main')['containers'][0]['lifecycle']['preStop'] == {'sleep': {'seconds': 10}} and pod('elitea-main')['terminationGracePeriodSeconds'] >= 10 + 30 + 10 and env('elitea-main','elitea-main','ELITEA_SHUTDOWN_TIMEOUT') == '30s'"
check default "the gateway: native preStop sleep 10, grace 190" \
  "[c for c in pod('elitea-llm-gateway')['containers'] if c['name']=='elitea-llm-gateway'][0]['lifecycle']['preStop'] == {'sleep': {'seconds': 10}} and pod('elitea-llm-gateway')['terminationGracePeriodSeconds'] == 190"
check default "DeepWiki and Inventory: grace >= 10 + drain 30 + 10" \
  "pod('elitea-deepwiki')['terminationGracePeriodSeconds'] >= 50 and pod('elitea-inventory')['terminationGracePeriodSeconds'] >= 50"
check default "the worker: grace >= shutdown 30s + 15" "pod('elitea-worker')['terminationGracePeriodSeconds'] >= 45"
check default "LLM_BUDGET_EXPECTED_REPLICAS derived: 1 for one replica" \
  "env('elitea-llm-gateway','elitea-llm-gateway','LLM_BUDGET_EXPECTED_REPLICAS') == '1'"

# ── KEDA: the worker's trigger, with and without a controller ──────────────
echo "== keda"
expect_refusal "a worker ScaledObject with no KEDA anywhere" "needs KEDA, and none is available" \
  --set worker.autoscaling.enabled=true
expect_render "keda.enabled: this release installs KEDA" keda-bundled \
  --set keda.enabled=true --set worker.autoscaling.enabled=true
check keda-bundled "the bundled KEDA operator renders" "has('Deployment', 'keda-operator')"
check keda-bundled "first install: the ScaledObject is a post-install hook carrying the release's ownership" \
  "ann(one('ScaledObject','elitea-worker')).get('helm.sh/hook') == 'post-install,post-upgrade' and ann(one('ScaledObject','elitea-worker')).get('meta.helm.sh/release-name') == 'elitea' and one('ScaledObject','elitea-worker')['metadata']['labels'].get('app.kubernetes.io/managed-by') == 'Helm'"
expect_render "keda.enabled on a cluster already serving keda.sh (the upgrade after it)" keda-adopted \
  --set keda.enabled=true --set worker.autoscaling.enabled=true "${KEDA_API[@]}"
check keda-adopted "the next upgrade renders a plain resource, which Helm adopts" \
  "'helm.sh/hook' not in ann(one('ScaledObject','elitea-worker'))"
expect_render "a KEDA the cluster already runs (Capabilities)" keda-cluster \
  --set worker.autoscaling.enabled=true "${KEDA_API[@]}"
check keda-cluster "no second KEDA, and no hook" \
  "not has('Deployment','keda-operator') and 'helm.sh/hook' not in ann(one('ScaledObject','elitea-worker'))"
expect_render "scaling.keda.clusterInstalled (a render that cannot ask)" keda-flag \
  --set worker.autoscaling.enabled=true --set scaling.keda.clusterInstalled=true
check keda-flag "the worker scales on its JetStream consumer, with no replicas: and a PDB only above one" \
  "triggers('elitea-worker') == ['nats-jetstream'] and 'replicas' not in one('Deployment','elitea-worker')['spec'] and not has('PodDisruptionBudget','elitea-worker')"
expect_render "the worker with a floor of two" keda-floor2 \
  --set worker.autoscaling.enabled=true --set scaling.keda.clusterInstalled=true --set worker.autoscaling.minReplicas=2
check keda-floor2 "a floor of two gets the worker a PDB and the spread" \
  "has('PodDisruptionBudget','elitea-worker') and spread_keys('elitea-worker') == $ZONE_NODE"

# ── DeepWiki: keda-prometheus / keda-postgresql / cpu ──────────────────────
echo "== deepwiki"
DW=(--set deepwiki.autoscaling.enabled=true --set deepwiki.autoscaling.maxReplicas=3)
expect_render "auto without KEDA: CPU" dw-cpu "${DW[@]}"
check dw-cpu "a CPU HPA, no ScaledObject, no replicas:" \
  "one('HorizontalPodAutoscaler','elitea-deepwiki')['spec']['metrics'][0]['resource']['name'] == 'cpu' and not objs('ScaledObject') and 'replicas' not in one('Deployment','elitea-deepwiki')['spec']"
expect_render "auto with KEDA, no Prometheus: the invocation store" dw-pg "${DW[@]}" --set scaling.keda.clusterInstalled=true
check dw-pg "a postgresql trigger over running invocations, its TriggerAuthentication on the host's DB Secret" \
  "triggers('elitea-deepwiki') == ['postgresql'] and 'finished_at IS NULL' in one('ScaledObject','elitea-deepwiki')['spec']['triggers'][0]['metadata']['query'] and one('TriggerAuthentication','elitea-deepwiki-keda-postgresql')['spec']['secretTargetRef'][0]['parameter'] == 'connection' and not has('HorizontalPodAutoscaler','elitea-deepwiki')"
expect_render "auto with KEDA, Prometheus and the Operator: in-flight invocations" dw-prom \
  "${DW[@]}" --set scaling.keda.clusterInstalled=true "${PROM[@]}" "${MON_API[@]}"
check dw-prom "a prometheus trigger on elitea_subapp_invocations_in_flight, threshold MAX_PARALLEL_WORKERS, and the PodMonitor that feeds it" \
  "triggers('elitea-deepwiki') == ['prometheus'] and 'elitea_subapp_invocations_in_flight{app=\"elitea-deepwiki\"' in one('ScaledObject','elitea-deepwiki')['spec']['triggers'][0]['metadata']['query'] and one('ScaledObject','elitea-deepwiki')['spec']['triggers'][0]['metadata']['threshold'] == env('elitea-deepwiki','elitea-deepwiki','ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS') and has('PodMonitor','elitea-deepwiki')"
expect_refusal "keda-prometheus without a Prometheus address" "needs scaling.prometheus.serverAddress" \
  "${DW[@]}" --set scaling.keda.clusterInstalled=true --set deepwiki.autoscaling.source=keda-prometheus
expect_refusal "keda-prometheus with nothing scraping the host" "nothing makes Prometheus scrape" \
  "${DW[@]}" --set scaling.keda.clusterInstalled=true --set deepwiki.autoscaling.source=keda-prometheus "${PROM[@]}"
expect_render "keda-prometheus with scrape annotations instead of a PodMonitor" dw-annot \
  "${DW[@]}" --set scaling.keda.clusterInstalled=true --set deepwiki.autoscaling.source=keda-prometheus "${PROM[@]}" \
  --set deepwiki.metrics.scrapeAnnotations=true
check dw-annot "the pods carry the scrape annotations for the admin port" \
  "one('Deployment','elitea-deepwiki')['spec']['template']['metadata']['annotations'].get('prometheus.io/scrape') == 'true'"
expect_refusal "keda-postgresql without KEDA" "needs KEDA, and none is available" \
  "${DW[@]}" --set deepwiki.autoscaling.source=keda-postgresql
expect_refusal "an unknown source" "must be auto, keda-prometheus, keda-postgresql or cpu" \
  "${DW[@]}" --set deepwiki.autoscaling.source=queue

# ── Inventory: one replica until its invocations are durable ───────────────
echo "== inventory"
expect_refusal "Inventory above one replica" "inventory" \
  --set inventory.autoscaling.enabled=true --set inventory.autoscaling.maxReplicas=2
expect_refusal "Inventory on the invocation store it does not have" "keeps its invocations in memory" \
  --set inventory.autoscaling.enabled=true --set inventory.autoscaling.source=keda-postgresql --set scaling.keda.clusterInstalled=true

# ── the gateway: keda-prometheus / adapter / cpu ───────────────────────────
echo "== gateway"
GW=(--set llmGateway.autoscaling.enabled=true --set llmGateway.autoscaling.maxReplicas=4)
expect_render "auto with nothing: CPU" gw-cpu "${GW[@]}"
check gw-cpu "a CPU HPA, no replicas:, the PDB only from a floor of two" \
  "one('HorizontalPodAutoscaler','elitea-llm-gateway')['spec']['metrics'][0]['resource']['name'] == 'cpu' and 'replicas' not in one('Deployment','elitea-llm-gateway')['spec'] and not has('PodDisruptionBudget','elitea-llm-gateway')"
check gw-cpu "LLM_BUDGET_EXPECTED_REPLICAS follows the ceiling (4)" \
  "env('elitea-llm-gateway','elitea-llm-gateway','LLM_BUDGET_EXPECTED_REPLICAS') == '4'"
check gw-cpu "a gateway that can run four replicas spreads" "spread_keys('elitea-llm-gateway') == $ZONE_NODE"
expect_refusal "adapter with no custom metrics API" "needs the custom metrics API" \
  "${GW[@]}" --set llmGateway.autoscaling.source=adapter
expect_refusal "the bundled adapter without a Prometheus to read" "needs prometheusAdapter.prometheus.url" \
  "${GW[@]}" --set prometheusAdapter.enabled=true
ADAPTER=(--set prometheusAdapter.enabled=true --set-string prometheusAdapter.prometheus.url=http://prometheus-operated.monitoring.svc)
expect_render "auto with the bundled adapter: a Pods metric" gw-adapter "${GW[@]}" "${ADAPTER[@]}"
check gw-adapter "the HPA reads gateway_llm_sse_active_connections per pod" \
  "one('HorizontalPodAutoscaler','elitea-llm-gateway')['spec']['metrics'][0]['type'] == 'Pods' and one('HorizontalPodAutoscaler','elitea-llm-gateway')['spec']['metrics'][0]['pods']['metric']['name'] == 'gateway_llm_sse_active_connections'"
check gw-adapter "the adapter renders, with exactly the gateway's metric and no default rules" "
has('Deployment', 'elitea-prometheus-adapter') and has('APIService', 'v1beta1.custom.metrics.k8s.io') and
[r['name']['as'] for r in yaml.safe_load(one('ConfigMap', 'elitea-prometheus-adapter')['data']['config.yaml'])['rules']] == ['gateway_llm_sse_active_connections'] and
list(yaml.safe_load(one('ConfigMap', 'elitea-prometheus-adapter')['data']['config.yaml'])) == ['rules']"
expect_render "the adapter off: no adapter objects" gw-noadapter "${GW[@]}"
check gw-noadapter "no prometheus-adapter Deployment or APIService" \
  "not [d for d in D if 'prometheus-adapter' in d['metadata']['name']]"
expect_render "auto with KEDA, Prometheus and the Operator: SSE streams" gw-keda \
  "${GW[@]}" --set scaling.keda.clusterInstalled=true "${PROM[@]}" "${MON_API[@]}"
check gw-keda "a prometheus trigger on the namespace's gateway_llm_sse_active_connections, the PodMonitor, no HPA of ours" \
  "triggers('elitea-llm-gateway') == ['prometheus'] and 'gateway_llm_sse_active_connections' in one('ScaledObject','elitea-llm-gateway')['spec']['triggers'][0]['metadata']['query'] and has('PodMonitor','elitea-llm-gateway') and not has('HorizontalPodAutoscaler','elitea-llm-gateway')"
expect_refusal "keda-prometheus without KEDA" "needs KEDA, and none is available" \
  "${GW[@]}" --set llmGateway.autoscaling.source=keda-prometheus "${PROM[@]}" "${MON_API[@]}"
expect_render "replicaCount 3, no autoscaler" gw-three --set llmGateway.replicaCount=3
check gw-three "LLM_BUDGET_EXPECTED_REPLICAS is 3, and a PDB now" \
  "env('elitea-llm-gateway','elitea-llm-gateway','LLM_BUDGET_EXPECTED_REPLICAS') == '3' and has('PodDisruptionBudget','elitea-llm-gateway')"
expect_refusal "an explicit LLM_BUDGET_EXPECTED_REPLICAS below the fleet" "LLM_BUDGET_EXPECTED_REPLICAS is 2, but this gateway can run 3" \
  --set llmGateway.replicaCount=3 --set-string llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS=2
expect_render "an explicit value above the fleet is the operator's to make" gw-above \
  --set llmGateway.replicaCount=3 --set-string llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS=5
expect_refusal "the removed preStop.command" "llmGateway.preStop.command was removed" \
  --set 'llmGateway.preStop.command[0]=sleep'

# ── every ScaledObject and TriggerAuthentication: SkipDryRunOnMissingResource
echo "== argo cd"
expect_render "every autoscaler on KEDA at once" all-keda \
  --set keda.enabled=true --set worker.autoscaling.enabled=true "${DW[@]}" "${GW[@]}" "${PROM[@]}" "${MON_API[@]}" \
  --set deepwiki.autoscaling.source=keda-postgresql
check all-keda "four KEDA objects, each with SkipDryRunOnMissingResource=true" "
len(objs('ScaledObject') + objs('TriggerAuthentication')) == 4 and
all(ann(o).get('argocd.argoproj.io/sync-options') == 'SkipDryRunOnMissingResource=true' for o in objs('ScaledObject') + objs('TriggerAuthentication'))"
check all-keda "no autoscaled Deployment carries replicas:" \
  "all('replicas' not in one('Deployment', n)['spec'] for n in ('elitea-main','elitea-vector','elitea-worker','elitea-deepwiki','elitea-llm-gateway'))"

# ── spread off ─────────────────────────────────────────────────────────────
expect_render "scaling.topologySpread.enabled=false" nospread --set scaling.topologySpread.enabled=false
check nospread "no default spread anywhere" \
  "all(not pod(d['metadata']['name']).get('topologySpreadConstraints') for d in objs('Deployment') if d['metadata']['name'].startswith('elitea-'))"

# ── grace periods ──────────────────────────────────────────────────────────
echo "== grace"
expect_refusal "elitea-main's grace below preStop + shutdown + 10" "main.terminationGracePeriodSeconds is 40, but what runs inside it needs 50" \
  --set main.terminationGracePeriodSeconds=40
expect_refusal "the gateway's grace below its drain" "llmGateway.terminationGracePeriodSeconds is 100" \
  --set llmGateway.terminationGracePeriodSeconds=100
expect_refusal "DeepWiki's grace below 10 + drain + 10" "deepwiki.terminationGracePeriodSeconds is 40" \
  --set deepwiki.terminationGracePeriodSeconds=40
expect_refusal "the worker's grace below shutdown + 15" "worker.terminationGracePeriodSeconds is 20" \
  --set worker.terminationGracePeriodSeconds=20
expect_render "a longer drain with a grace to match" grace-ok \
  --set-string main.env.ELITEA_SHUTDOWN_TIMEOUT=1m --set main.terminationGracePeriodSeconds=80

# ── the connection budget lists every component ────────────────────────────
echo "== budget"
expect_refusal "a server too small for the release" "only 5 are available" --set postgresql.maxConnections=30
for part in "elitea-main " "llm-gateway " "scheduler " "deepwiki host " "deepwiki engine " "inventory engine " "worker AgentState "; do
  if grep -qF -- "$part" "$TMP/refused.err"; then ok "the budget names $part"; else fail "the budget refusal does not name '$part'"; fi
done

if [ "$fails" -ne 0 ]; then
  echo "render-scaling: $fails check(s) failed" >&2
  exit 1
fi
echo "render-scaling: every check passed"
