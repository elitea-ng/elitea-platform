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
#   defaults    every component autoscales by default: an HPA at its default
#               range for main, web, elitea-vector, the gateway, DeepWiki,
#               Inventory, the worker and the OTel Collector (CPU where no
#               better source is available)
#   off         <component>.autoscaling.enabled=false: no HPA, and the
#               Deployment carries replicas: (web, the gateway, the OTel
#               Collector, the worker, egressProxy); egressProxy's floor of two
#               (autoscaling.minReplicas, else replicaCount)
#   scheduler   the active/active pair: a PDB and a spread at two, none at one,
#               three refused
#   sources     each autoscaler with and without its source: the worker's
#               KEDA trigger and its CPU fallback (source auto / keda / cpu);
#               DeepWiki's and Inventory's keda-prometheus / keda-postgresql /
#               cpu; the gateway's keda-prometheus / adapter / cpu
#   stores      DeepWiki and Inventory above one replica only with their
#               invocation store (the database secret); refused without it
#   keda        keda.enabled on/off; the first-install hook and the adopted
#               plain resource; SkipDryRunOnMissingResource on EVERY
#               ScaledObject and TriggerAuthentication; a CPU trigger after
#               the main trigger of EVERY ScaledObject
#   cleanup     the pre-delete KEDA cleanup hook with keda.enabled, and none
#               beside an external KEDA
#   queries     the generated Prometheus and PostgreSQL queries;
#               scaling.prometheus.namespaceLabel (custom, empty, invalid) and
#               scaling.prometheus.extraMatchers
#   adapter     prometheusAdapter on/off; its rules name exactly the gateway
#               metric and nothing else (no default rules)
#   replicas    no Deployment carries `replicas:` while an autoscaler owns it
#   spread/pdb  a topology spread on every Deployment that can run more than
#               one replica; a PDB on every Deployment whose floor is above
#               one, and on none whose floor is one
#   grace       terminationGracePeriodSeconds >= preStop + drain + margin, for
#               elitea-main, the gateway, DeepWiki, Inventory, elitea-vector,
#               the OTel Collector and the worker; one second short is refused
#   budget      the connection-budget refusal lists every component; the
#               defaults fit the default server
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
# with its database, Inventory with its database — so every component the
# budget counts is here.
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
# under test is never the connection budget's by accident. The budget section
# below sets its own server.
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
# below are in scope. The expression is truthy for a pass. A render that
# failed has no documents, so its checks fail too.
check() {
  local name="$1" what="$2" expr="$3"
  if [ ! -s "$TMP/$name.yaml" ]; then fail "$what (render '$name' is empty)"; return; fi
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
def container(deploy, name):
    return [c for c in pod(deploy)['containers'] if c['name'] == name][0]
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
def secret_env(deploy, name, key):
    # The Secret a container env var reads, or None.
    for e in container(deploy, name).get('env', []):
        if e['name'] == key:
            return ((e.get('valueFrom') or {}).get('secretKeyRef') or {}).get('name')
    return None
def job_pod(name):
    return one('Job', name)['spec']['template']['spec']
def job_env(name):
    return {e['name']: {k: v for k, v in e.items() if k != 'name'} for e in job_pod(name)['containers'][0].get('env', [])}
def ann(obj):
    return (obj.get('metadata') or {}).get('annotations') or {}
def spread_keys(deploy):
    return sorted((c['topologyKey'], c['whenUnsatisfiable']) for c in pod(deploy).get('topologySpreadConstraints') or [])
def triggers(name):
    return [t['type'] for t in one('ScaledObject', name)['spec']['triggers']]
def trigger(name, i):
    return one('ScaledObject', name)['spec']['triggers'][i]
def prom_queries():
    return [t['metadata']['query'] for o in objs('ScaledObject') for t in o['spec']['triggers'] if t['type'] == 'prometheus']
def hpa_range(name):
    h = one('HorizontalPodAutoscaler', name)
    return (h['spec']['minReplicas'], h['spec']['maxReplicas']) if h else None
def scaler(name):
    # The HPA or ScaledObject that owns a Deployment's replica count, or None.
    for h in objs('HorizontalPodAutoscaler'):
        if h['spec']['scaleTargetRef']['name'] == name:
            return (h['spec']['minReplicas'], h['spec']['maxReplicas'])
    for o in objs('ScaledObject'):
        if o['spec']['scaleTargetRef']['name'] == name:
            return (o['spec'].get('minReplicaCount', 1), o['spec']['maxReplicaCount'])
    return None
def deployments():
    return [d['metadata']['name'] for d in objs('Deployment')]
def owned():
    return [n for n in deployments() if scaler(n)]
def floor(name):
    s = scaler(name)
    return s[0] if s else one('Deployment', name)['spec'].get('replicas', 1)
def ceiling(name):
    s = scaler(name)
    return s[1] if s else one('Deployment', name)['spec'].get('replicas', 1)
sys.exit(0 if eval('(' + sys.argv[2].strip() + ')') else 1)
PY
  then ok "$what"; else fail "$what"; fi
}

ZONE_NODE="[('kubernetes.io/hostname', 'ScheduleAnyway'), ('topology.kubernetes.io/zone', 'ScheduleAnyway')]"
KEDA_API=(--api-versions keda.sh/v1alpha1)
MON_API=(--api-versions monitoring.coreos.com/v1)
PROM=(--set-string scaling.prometheus.serverAddress=http://prometheus-operated.monitoring.svc:9090)
EGRESS=(-f "$DIR/helm/tests/fixtures/egress-proxy-values.yaml")

# ── the shipped defaults ─────────────────────────────────────────────────────
#
# Every component autoscales by default. With no KEDA, no adapter and no
# Prometheus, each autoscaler falls back to CPU (the worker, the gateway,
# DeepWiki, Inventory) or is a CPU HPA already (main, web, elitea-vector, the
# OTel Collector). The scheduler runs a fixed active/active pair.
echo "== defaults"
expect_render "the operator's values" default
check default "no ScaledObject, TriggerAuthentication or KEDA without asking" \
  "not objs('ScaledObject') and not objs('TriggerAuthentication') and not has('Deployment', 'keda-operator')"
# Each HPA: name -> (min, max, source annotation or None for the ones that
# predate the annotation).
check default "an HPA for every component, at its default range" "
{n: hpa_range(n) for n in ('elitea-main','elitea-web','elitea-vector','elitea-llm-gateway','elitea-deepwiki','elitea-inventory','elitea-worker','elitea-otel-collector')} == {
 'elitea-main': (2, 8), 'elitea-web': (2, 6), 'elitea-vector': (2, 6), 'elitea-llm-gateway': (2, 4),
 'elitea-deepwiki': (1, 3), 'elitea-inventory': (1, 3), 'elitea-worker': (1, 10), 'elitea-otel-collector': (1, 3)}"
check default "the fallback HPAs say their source is cpu" \
  "all(ann(one('HorizontalPodAutoscaler', n)).get('elitea.ai/autoscaling-source') == 'cpu' for n in ('elitea-llm-gateway','elitea-deepwiki','elitea-inventory','elitea-worker','elitea-otel-collector'))"
check default "every HPA scales the Deployment of its own name" \
  "all(h['spec']['scaleTargetRef'] == {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'name': h['metadata']['name']} and has('Deployment', h['metadata']['name']) for h in objs('HorizontalPodAutoscaler'))"
check default "no Deployment carries replicas: while an autoscaler owns it" \
  "all('replicas' not in one('Deployment', n)['spec'] for n in owned())"
check default "the scheduler: two replicas, RollingUpdate" \
  "one('Deployment','elitea-scheduler')['spec']['replicas'] == 2 and one('Deployment','elitea-scheduler')['spec']['strategy']['type'] == 'RollingUpdate'"
check default "every Deployment that can run more than one replica has a topology spread" \
  "all(spread_keys(n) for n in deployments() if ceiling(n) > 1)"
check default "the chart's own components spread over zones then nodes, softly" \
  "all(spread_keys(n) == $ZONE_NODE for n in ('elitea-main','elitea-web','elitea-vector','elitea-llm-gateway','elitea-deepwiki','elitea-inventory','elitea-worker','elitea-otel-collector','elitea-scheduler'))"
check default "every Deployment whose floor is above one has a PDB" \
  "all(has('PodDisruptionBudget', n) for n in deployments() if floor(n) > 1)"
check default "no PDB over a floor of one (it would block every drain)" \
  "not [n for n in deployments() if floor(n) == 1 and has('PodDisruptionBudget', n)]"
check default "the PDBs: main, web, vector, the gateway and the scheduler; none for a floor of one" \
  "all(has('PodDisruptionBudget', n) for n in ('elitea-main','elitea-web','elitea-vector','elitea-llm-gateway','elitea-scheduler')) and not any(has('PodDisruptionBudget', n) for n in ('elitea-deepwiki','elitea-inventory','elitea-worker','elitea-otel-collector'))"
check default "elitea-main: native preStop sleep 10, shutdown 30s, grace 50" \
  "container('elitea-main','elitea-main')['lifecycle']['preStop'] == {'sleep': {'seconds': 10}} and pod('elitea-main')['terminationGracePeriodSeconds'] >= 10 + 30 + 10 and env('elitea-main','elitea-main','ELITEA_SHUTDOWN_TIMEOUT') == '30s'"
check default "the gateway: native preStop sleep 10, grace 190" \
  "container('elitea-llm-gateway','elitea-llm-gateway')['lifecycle']['preStop'] == {'sleep': {'seconds': 10}} and pod('elitea-llm-gateway')['terminationGracePeriodSeconds'] == 190"
check default "DeepWiki and Inventory: preStop sleep 5, drain 3600, grace 3630 >= 5 + 10 + 3600 + 10" "
all(container(n, n)['lifecycle']['preStop'] == {'sleep': {'seconds': 5}} and
    env(n, n, 'ELITEA_%s_DRAIN_TIMEOUT_SECONDS' % n.split('-')[1].upper()) == '3600' and
    pod(n)['terminationGracePeriodSeconds'] == 3630
    for n in ('elitea-deepwiki', 'elitea-inventory'))"
# The kubelet sends SIGTERM to both containers at once. The engine sidecar
# sleeps through the host's preStop and gRPC stop (5 + 15) so a call the host
# admits in that window does not find the engine gone.
check default "DeepWiki and Inventory engines: preStop sleep preStop + 15 = 20" "
all(container(n, 'engine')['lifecycle']['preStop'] == {'sleep': {'seconds': 20}}
    for n in ('elitea-deepwiki', 'elitea-inventory'))"
check default "elitea-vector: preStop sleep 5, grace >= 5 + gRPC drain 20" \
  "container('elitea-vector','elitea-vector')['lifecycle']['preStop'] == {'sleep': {'seconds': 5}} and pod('elitea-vector')['terminationGracePeriodSeconds'] >= 25"
check default "the OTel Collector: preStop sleep 5, grace 30 >= 5 + batch flush 10" \
  "pod('elitea-otel-collector')['containers'][0]['lifecycle']['preStop'] == {'sleep': {'seconds': 5}} and pod('elitea-otel-collector')['terminationGracePeriodSeconds'] == 30"
check default "the worker: grace >= shutdown 30s + 15" "pod('elitea-worker')['terminationGracePeriodSeconds'] >= 45"
check default "LLM_BUDGET_EXPECTED_REPLICAS derived: 4, the gateway's ceiling" \
  "env('elitea-llm-gateway','elitea-llm-gateway','LLM_BUDGET_EXPECTED_REPLICAS') == '4'"
check default "Inventory runs more than one replica: its host reads the invocation store" \
  "hpa_range('elitea-inventory')[1] > 1 and secret_env('elitea-inventory','elitea-inventory','ELITEA_INVENTORY_DATABASE_URL')"

# ── each autoscaler OFF: no HPA, and the Deployment carries replicas: ─────
#
# The other direction of the defaults: with <component>.autoscaling.enabled
# false, no HPA renders and the Deployment states its fixed count.
echo "== autoscaling off"
for pair in "web:elitea-web" "llmGateway:elitea-llm-gateway" "otelCollector:elitea-otel-collector" "worker:elitea-worker"; do
  key="${pair%%:*}" name="${pair##*:}"
  expect_render "$key.autoscaling.enabled=false" "off-$key" --set "$key.autoscaling.enabled=false"
  check "off-$key" "$key: no HPA, no ScaledObject, and the Deployment carries replicas:" \
    "not has('HorizontalPodAutoscaler','$name') and not has('ScaledObject','$name') and 'replicas' in one('Deployment','$name')['spec']"
done
check off-otelCollector "one OTel Collector: no spread and no PDB" \
  "spread_keys('elitea-otel-collector') == [] and not has('PodDisruptionBudget','elitea-otel-collector')"
expect_render "egressProxy on (its own floor of two)" egress "${EGRESS[@]}"
check egress "egressProxy: a CPU HPA 2-6, no replicas:, a PDB and a spread" \
  "hpa_range('elitea-egress-proxy') == (2, 6) and ann(one('HorizontalPodAutoscaler','elitea-egress-proxy')).get('elitea.ai/autoscaling-source') == 'cpu' and 'replicas' not in one('Deployment','elitea-egress-proxy')['spec'] and has('PodDisruptionBudget','elitea-egress-proxy') and spread_keys('elitea-egress-proxy')"
expect_render "egressProxy.autoscaling.enabled=false" egress-off "${EGRESS[@]}" --set egressProxy.autoscaling.enabled=false
check egress-off "egressProxy off the HPA: no HPA, replicas: 2" \
  "not has('HorizontalPodAutoscaler','elitea-egress-proxy') and one('Deployment','elitea-egress-proxy')['spec']['replicas'] == 2"
expect_refusal "egressProxy with an autoscaler floor of one" "egressProxy.autoscaling.minReplicas must be at least 2" \
  "${EGRESS[@]}" --set egressProxy.autoscaling.minReplicas=1
expect_refusal "egressProxy with one fixed replica" "egressProxy.replicaCount must be at least 2" \
  "${EGRESS[@]}" --set egressProxy.autoscaling.enabled=false --set egressProxy.replicaCount=1

# ── the scheduler: an active/active pair ──────────────────────────────────
echo "== scheduler"
check default "two schedulers: a PDB and a spread" \
  "has('PodDisruptionBudget','elitea-scheduler') and spread_keys('elitea-scheduler') == $ZONE_NODE"
expect_render "one scheduler" sched-one --set scheduler.replicaCount=1
check sched-one "one scheduler: no PDB, no spread, replicas: 1" \
  "not has('PodDisruptionBudget','elitea-scheduler') and spread_keys('elitea-scheduler') == [] and one('Deployment','elitea-scheduler')['spec']['replicas'] == 1"
expect_refusal "three schedulers" "scheduler.replicaCount is 3" --set scheduler.replicaCount=3

# ── KEDA: the worker's trigger, with and without a controller ──────────────
#
# worker.autoscaling.source: auto takes KEDA when it is served, else a CPU
# HPA (the defaults above). keda without KEDA is refused.
echo "== keda"
expect_refusal "worker.autoscaling.source=keda with no KEDA anywhere" "needs KEDA, and none is available" \
  --set worker.autoscaling.source=keda
expect_render "worker.autoscaling.source=cpu beside KEDA" worker-cpu --set worker.autoscaling.source=cpu "${KEDA_API[@]}"
expect_refusal "a CPU HPA for the worker with minReplicas 0 (scale to zero is KEDA's alone)" "worker.autoscaling.minReplicas (0) must be at least 1" \
  --set worker.autoscaling.minReplicas=0
check worker-cpu "a CPU HPA, no ScaledObject" \
  "ann(one('HorizontalPodAutoscaler','elitea-worker')).get('elitea.ai/autoscaling-source') == 'cpu' and not has('ScaledObject','elitea-worker')"
expect_render "keda.enabled: this release installs KEDA" keda-bundled --set keda.enabled=true
check keda-bundled "the bundled KEDA operator renders" "has('Deployment', 'keda-operator')"
check keda-bundled "first install: the ScaledObject is a post-install hook carrying the release's ownership" \
  "ann(one('ScaledObject','elitea-worker')).get('helm.sh/hook') == 'post-install,post-upgrade' and ann(one('ScaledObject','elitea-worker')).get('meta.helm.sh/release-name') == 'elitea' and one('ScaledObject','elitea-worker')['metadata']['labels'].get('app.kubernetes.io/managed-by') == 'Helm'"
expect_render "keda.enabled on a cluster already serving keda.sh (the upgrade after it)" keda-adopted \
  --set keda.enabled=true "${KEDA_API[@]}"
check keda-adopted "the next upgrade renders a plain resource, which Helm adopts" \
  "'helm.sh/hook' not in ann(one('ScaledObject','elitea-worker'))"
expect_render "a KEDA the cluster already runs (Capabilities)" keda-cluster "${KEDA_API[@]}"
check keda-cluster "no second KEDA, and no hook" \
  "not has('Deployment','keda-operator') and 'helm.sh/hook' not in ann(one('ScaledObject','elitea-worker'))"
expect_render "scaling.keda.clusterInstalled (a render that cannot ask)" keda-flag --set scaling.keda.clusterInstalled=true
check keda-flag "the worker scales on its JetStream consumer then CPU, with no replicas:, no HPA of ours and a PDB only above one" \
  "triggers('elitea-worker') == ['nats-jetstream', 'cpu'] and 'replicas' not in one('Deployment','elitea-worker')['spec'] and not has('HorizontalPodAutoscaler','elitea-worker') and not has('PodDisruptionBudget','elitea-worker')"
expect_render "the worker with a floor of two" keda-floor2 \
  --set scaling.keda.clusterInstalled=true --set worker.autoscaling.minReplicas=2
check keda-floor2 "a floor of two gets the worker a PDB and the spread" \
  "has('PodDisruptionBudget','elitea-worker') and spread_keys('elitea-worker') == $ZONE_NODE"

# ── the KEDA uninstall hook ─────────────────────────────────────────────────
#
# With keda.enabled the release deletes KEDA's operator together with the
# ScaledObjects. A pre-delete hook deletes this release's KEDA objects first,
# while the operator can still run their finalizers. An external KEDA outlives
# the release, so then there is no hook.
echo "== keda cleanup"
check keda-bundled "the hook: a ServiceAccount, a Role, a RoleBinding and a Job, all pre-delete" "
all(has(k, 'elitea-keda-cleanup') and ann(one(k, 'elitea-keda-cleanup')).get('helm.sh/hook') == 'pre-delete'
    for k in ('ServiceAccount', 'Role', 'RoleBinding', 'Job'))"
check keda-bundled "the Role: list, deletecollection and patch on keda.sh scaledobjects and triggerauthentications, nothing else" "
one('Role', 'elitea-keda-cleanup')['rules'] == [{'apiGroups': ['keda.sh'], 'resources': ['scaledobjects', 'triggerauthentications'], 'verbs': ['list', 'deletecollection', 'patch']}]"
check keda-bundled "the RoleBinding gives the Role to the hook's ServiceAccount" "
one('RoleBinding', 'elitea-keda-cleanup')['roleRef']['name'] == 'elitea-keda-cleanup' and
one('RoleBinding', 'elitea-keda-cleanup')['subjects'] == [{'kind': 'ServiceAccount', 'name': 'elitea-keda-cleanup', 'namespace': 'elitea'}]"
check keda-bundled "the Job runs /elitea-keda-cleanup from the elitea-main image as the hook's ServiceAccount" "
job_pod('elitea-keda-cleanup')['serviceAccountName'] == 'elitea-keda-cleanup' and
job_pod('elitea-keda-cleanup')['containers'][0]['command'] == ['/elitea-keda-cleanup'] and
job_pod('elitea-keda-cleanup')['containers'][0]['image'].rsplit(':', 1)[0] == container('elitea-main','elitea-main')['image'].rsplit(':', 1)[0]"
check keda-bundled "the Job's env: the namespace from the downward API, this release's selector, the timeout" "
job_env('elitea-keda-cleanup') == {
 'ELITEA_KEDA_CLEANUP_NAMESPACE': {'valueFrom': {'fieldRef': {'fieldPath': 'metadata.namespace'}}},
 'ELITEA_KEDA_CLEANUP_SELECTOR': {'value': 'app.kubernetes.io/instance=elitea'},
 'ELITEA_KEDA_CLEANUP_TIMEOUT': {'value': '120s'}}"
check keda-bundled "the selector finds every KEDA object of this release" \
  "all(o['metadata']['labels'].get('app.kubernetes.io/instance') == 'elitea' for o in objs('ScaledObject') + objs('TriggerAuthentication'))"
expect_render "kedaCleanup.timeoutSeconds 300" keda-timeout --set keda.enabled=true --set kedaCleanup.timeoutSeconds=300
check keda-timeout "the timeout reaches the Job" "job_env('elitea-keda-cleanup')['ELITEA_KEDA_CLEANUP_TIMEOUT'] == {'value': '300s'}"
expect_refusal "a cleanup timeout below 10 seconds" "kedaCleanup.timeoutSeconds must be at least 10" \
  --set keda.enabled=true --set kedaCleanup.timeoutSeconds=5
check default "no hook without keda.enabled" "not [d for d in D if 'keda-cleanup' in d['metadata']['name']]"
check keda-flag "no hook beside an external KEDA (scaling.keda.clusterInstalled)" "not [d for d in D if 'keda-cleanup' in d['metadata']['name']]"
check keda-cluster "no hook beside an external KEDA (Capabilities)" "not [d for d in D if 'keda-cleanup' in d['metadata']['name']]"

# ── DeepWiki: keda-prometheus / keda-postgresql / cpu ──────────────────────
echo "== deepwiki"
DW_PG_QUERY="SELECT count(*) FROM invocations WHERE finished_at IS NULL OR (finished_at > now() - interval '5 minutes' AND result->>'error_category' = 'service_busy')"
DW_PROM_QUERY='(sum(elitea_subapp_invocations_in_flight{app="elitea-deepwiki",namespace="elitea"}) or vector(0)) + (sum(increase(elitea_subapp_slot_refusals_total{app="elitea-deepwiki",namespace="elitea"}[5m])) or vector(0))'
check default "auto without KEDA: a CPU HPA, no ScaledObject, no replicas:" \
  "one('HorizontalPodAutoscaler','elitea-deepwiki')['spec']['metrics'][0]['resource']['name'] == 'cpu' and not has('ScaledObject','elitea-deepwiki') and 'replicas' not in one('Deployment','elitea-deepwiki')['spec']"
expect_render "auto with KEDA, no Prometheus: the invocation store" dw-pg --set scaling.keda.clusterInstalled=true
check dw-pg "a postgresql trigger over running and refused-busy invocations, then CPU; its TriggerAuthentication on the host's DB Secret" \
  "triggers('elitea-deepwiki') == ['postgresql', 'cpu'] and trigger('elitea-deepwiki', 0)['metadata']['query'] == \"$DW_PG_QUERY\" and one('TriggerAuthentication','elitea-deepwiki-keda-postgresql')['spec']['secretTargetRef'][0]['parameter'] == 'connection' and not has('HorizontalPodAutoscaler','elitea-deepwiki')"
expect_render "auto with KEDA, Prometheus and the Operator: in-flight invocations" dw-prom \
  --set scaling.keda.clusterInstalled=true "${PROM[@]}" "${MON_API[@]}"
check dw-prom "a prometheus trigger on in-flight invocations plus refused slots, threshold MAX_PARALLEL_WORKERS, and the PodMonitor that feeds it" \
  "triggers('elitea-deepwiki') == ['prometheus', 'cpu'] and trigger('elitea-deepwiki', 0)['metadata']['query'] == '$DW_PROM_QUERY' and trigger('elitea-deepwiki', 0)['metadata']['threshold'] == env('elitea-deepwiki','elitea-deepwiki','ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS') and has('PodMonitor','elitea-deepwiki')"
expect_refusal "keda-prometheus without a Prometheus address" "needs scaling.prometheus.serverAddress" \
  --set scaling.keda.clusterInstalled=true --set deepwiki.autoscaling.source=keda-prometheus
expect_refusal "keda-prometheus with nothing scraping the host" "nothing makes Prometheus scrape" \
  --set scaling.keda.clusterInstalled=true --set deepwiki.autoscaling.source=keda-prometheus "${PROM[@]}"
expect_render "keda-prometheus with scrape annotations instead of a PodMonitor" dw-annot \
  --set scaling.keda.clusterInstalled=true --set deepwiki.autoscaling.source=keda-prometheus "${PROM[@]}" \
  --set deepwiki.metrics.scrapeAnnotations=true
check dw-annot "the pods carry the scrape annotations for the admin port" \
  "one('Deployment','elitea-deepwiki')['spec']['template']['metadata']['annotations'].get('prometheus.io/scrape') == 'true'"
expect_refusal "keda-postgresql without KEDA" "needs KEDA, and none is available" \
  --set deepwiki.autoscaling.source=keda-postgresql
expect_refusal "an unknown source" "must be auto, keda-prometheus, keda-postgresql or cpu" \
  --set deepwiki.autoscaling.source=queue
# More than one replica shares its invocations through the database. The
# fixture runner reads no database, so it isolates this guard.
NO_DB=(--set postgresql.existingSecret= --set deepwiki.engine.runner=fixture --set inventory.engine.runner=fixture)
expect_refusal "DeepWiki above one replica without its invocation store" "deepwiki can run 3 replicas (autoscaling.maxReplicas), but its host has no invocation store" \
  "${NO_DB[@]}" --set inventory.autoscaling.enabled=false
expect_render "DeepWiki at one replica without a database" dw-one "${NO_DB[@]}" \
  --set deepwiki.autoscaling.enabled=false --set inventory.autoscaling.enabled=false

# ── Inventory: more than one replica on its invocation store ───────────────
echo "== inventory"
INV_PG_QUERY="SELECT count(*) FROM inventory_graph.invocations WHERE finished_at IS NULL OR (finished_at > now() - interval '5 minutes' AND result->>'error_category' = 'service_busy')"
check default "Inventory autoscales 1-3 with its database (a CPU HPA without KEDA)" \
  "hpa_range('elitea-inventory') == (1, 3) and 'replicas' not in one('Deployment','elitea-inventory')['spec']"
expect_render "Inventory, two fixed replicas with the database" inv-two \
  --set inventory.autoscaling.enabled=false --set inventory.replicaCount=2
check inv-two "two replicas, a PDB and the spread" \
  "one('Deployment','elitea-inventory')['spec']['replicas'] == 2 and has('PodDisruptionBudget','elitea-inventory') and spread_keys('elitea-inventory') == $ZONE_NODE"
expect_refusal "Inventory above one replica without its invocation store" "inventory can run 3 replicas (autoscaling.maxReplicas), but its host has no invocation store" \
  "${NO_DB[@]}" --set deepwiki.autoscaling.enabled=false
expect_refusal "two fixed Inventory replicas without the store" "inventory can run 2 replicas (replicaCount)" \
  "${NO_DB[@]}" --set deepwiki.autoscaling.enabled=false --set inventory.autoscaling.enabled=false --set inventory.replicaCount=2
expect_render "Inventory at one replica without a database" inv-one "${NO_DB[@]}" \
  --set deepwiki.autoscaling.enabled=false --set inventory.autoscaling.enabled=false
check inv-one "no database: no HPA, no PDB, and the host gets no database URL" \
  "not has('HorizontalPodAutoscaler','elitea-inventory') and not has('PodDisruptionBudget','elitea-inventory') and not secret_env('elitea-inventory','elitea-inventory','ELITEA_INVENTORY_DATABASE_URL')"
expect_render "Inventory on keda-postgresql" inv-pg --set scaling.keda.clusterInstalled=true --set inventory.autoscaling.source=keda-postgresql
check inv-pg "a postgresql trigger on inventory_graph.invocations (running and refused busy), then CPU; its TriggerAuthentication" \
  "triggers('elitea-inventory') == ['postgresql', 'cpu'] and trigger('elitea-inventory', 0)['metadata']['query'] == \"$INV_PG_QUERY\" and has('TriggerAuthentication','elitea-inventory-keda-postgresql')"
expect_refusal "Inventory on keda-postgresql without its database" "inventory.autoscaling.source=keda-postgresql needs the host's invocation store" \
  "${NO_DB[@]}" --set deepwiki.autoscaling.enabled=false --set inventory.autoscaling.maxReplicas=1 \
  --set scaling.keda.clusterInstalled=true --set inventory.autoscaling.source=keda-postgresql

# ── the gateway: keda-prometheus / adapter / cpu ───────────────────────────
echo "== gateway"
check default "auto with nothing: a CPU HPA 2-4, no replicas:, a PDB (floor two), the spread" \
  "one('HorizontalPodAutoscaler','elitea-llm-gateway')['spec']['metrics'][0]['resource']['name'] == 'cpu' and 'replicas' not in one('Deployment','elitea-llm-gateway')['spec'] and has('PodDisruptionBudget','elitea-llm-gateway') and spread_keys('elitea-llm-gateway') == $ZONE_NODE"
expect_render "a gateway floor of one" gw-floor1 --set llmGateway.autoscaling.minReplicas=1
check gw-floor1 "a floor of one: no PDB, the spread stays (the ceiling is four)" \
  "not has('PodDisruptionBudget','elitea-llm-gateway') and spread_keys('elitea-llm-gateway') == $ZONE_NODE"
expect_refusal "adapter with no custom metrics API" "needs the custom metrics API" \
  --set llmGateway.autoscaling.source=adapter
expect_refusal "the bundled adapter without a Prometheus to read" "needs prometheusAdapter.prometheus.url" \
  --set prometheusAdapter.enabled=true
ADAPTER=(--set prometheusAdapter.enabled=true --set-string prometheusAdapter.prometheus.url=http://prometheus-operated.monitoring.svc)
expect_render "auto with the bundled adapter: a Pods metric" gw-adapter "${ADAPTER[@]}"
check gw-adapter "the HPA reads gateway_llm_sse_active_connections per pod" \
  "one('HorizontalPodAutoscaler','elitea-llm-gateway')['spec']['metrics'][0]['type'] == 'Pods' and one('HorizontalPodAutoscaler','elitea-llm-gateway')['spec']['metrics'][0]['pods']['metric']['name'] == 'gateway_llm_sse_active_connections'"
check gw-adapter "the adapter renders, with exactly the gateway's metric and no default rules" "
has('Deployment', 'elitea-prometheus-adapter') and has('APIService', 'v1beta1.custom.metrics.k8s.io') and
[r['name']['as'] for r in yaml.safe_load(one('ConfigMap', 'elitea-prometheus-adapter')['data']['config.yaml'])['rules']] == ['gateway_llm_sse_active_connections'] and
list(yaml.safe_load(one('ConfigMap', 'elitea-prometheus-adapter')['data']['config.yaml'])) == ['rules']"
check default "the adapter off: no prometheus-adapter Deployment or APIService" \
  "not [d for d in D if 'prometheus-adapter' in d['metadata']['name']]"
expect_render "auto with KEDA, Prometheus and the Operator: SSE streams" gw-keda \
  --set scaling.keda.clusterInstalled=true "${PROM[@]}" "${MON_API[@]}"
check gw-keda "a prometheus trigger on the namespace's gateway_llm_sse_active_connections then CPU, the PodMonitor, no HPA of ours" \
  "triggers('elitea-llm-gateway') == ['prometheus', 'cpu'] and trigger('elitea-llm-gateway', 0)['metadata']['query'] == 'sum(gateway_llm_sse_active_connections{namespace=\"elitea\"})' and has('PodMonitor','elitea-llm-gateway') and not has('HorizontalPodAutoscaler','elitea-llm-gateway')"
expect_refusal "keda-prometheus without KEDA" "needs KEDA, and none is available" \
  --set llmGateway.autoscaling.source=keda-prometheus "${PROM[@]}" "${MON_API[@]}"
expect_render "replicaCount 3, no autoscaler" gw-three --set llmGateway.autoscaling.enabled=false --set llmGateway.replicaCount=3
check gw-three "LLM_BUDGET_EXPECTED_REPLICAS is 3, replicas: 3, and a PDB" \
  "env('elitea-llm-gateway','elitea-llm-gateway','LLM_BUDGET_EXPECTED_REPLICAS') == '3' and one('Deployment','elitea-llm-gateway')['spec']['replicas'] == 3 and has('PodDisruptionBudget','elitea-llm-gateway')"
expect_render "one gateway, no autoscaler" gw-one --set llmGateway.autoscaling.enabled=false --set llmGateway.replicaCount=1
check gw-one "one gateway: LLM_BUDGET_EXPECTED_REPLICAS 1, no PDB, no spread" \
  "env('elitea-llm-gateway','elitea-llm-gateway','LLM_BUDGET_EXPECTED_REPLICAS') == '1' and not has('PodDisruptionBudget','elitea-llm-gateway') and spread_keys('elitea-llm-gateway') == []"
expect_refusal "an explicit LLM_BUDGET_EXPECTED_REPLICAS below the fixed fleet" "LLM_BUDGET_EXPECTED_REPLICAS is 2, but this gateway can run 3 replicas (replicaCount)" \
  --set llmGateway.autoscaling.enabled=false --set llmGateway.replicaCount=3 --set-string llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS=2
expect_refusal "an explicit LLM_BUDGET_EXPECTED_REPLICAS below the autoscaler's ceiling" "LLM_BUDGET_EXPECTED_REPLICAS is 3, but this gateway can run 4 replicas (autoscaling.maxReplicas)" \
  --set-string llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS=3
expect_render "an explicit value above the fleet is the operator's to make" gw-above \
  --set-string llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS=5
expect_refusal "the removed preStop.command" "llmGateway.preStop.command was removed" \
  --set 'llmGateway.preStop.command[0]=sleep'

# ── Prometheus queries: the namespace label and extra matchers ─────────────
#
# The namespace label depends on the scrape: `namespace` from a PodMonitor,
# `kubernetes_namespace` from many annotation scrapes. A wrong label reads an
# empty vector, so the label is a value, and empty omits the matcher.
echo "== prometheus queries"
PQ=(--set scaling.keda.clusterInstalled=true "${PROM[@]}" "${MON_API[@]}")
expect_render "namespaceLabel kubernetes_namespace" pq-label "${PQ[@]}" --set scaling.prometheus.namespaceLabel=kubernetes_namespace
check pq-label "every generated query matches kubernetes_namespace, never namespace" \
  "trigger('elitea-llm-gateway', 0)['metadata']['query'] == 'sum(gateway_llm_sse_active_connections{kubernetes_namespace=\"elitea\"})' and all('kubernetes_namespace=\"elitea\"' in q and ',namespace=' not in q and '{namespace=' not in q for q in prom_queries())"
expect_render "namespaceLabel empty" pq-none "${PQ[@]}" --set-string scaling.prometheus.namespaceLabel=
check pq-none "no generated query names the namespace" \
  "prom_queries() and all('elitea\"' not in q.replace('elitea-', '') for q in prom_queries()) and trigger('elitea-deepwiki', 0)['metadata']['query'].startswith('(sum(elitea_subapp_invocations_in_flight{app=\"elitea-deepwiki\"})')"
expect_refusal "a namespaceLabel that is not a label name" "scaling.prometheus.namespaceLabel must be a Prometheus label name" \
  "${PQ[@]}" --set scaling.prometheus.namespaceLabel=kube-ns
expect_render "extraMatchers" pq-extra "${PQ[@]}" \
  --set 'scaling.prometheus.extraMatchers[0]=cluster="prod-eu"' --set 'scaling.prometheus.extraMatchers[1]=env="x"'
check pq-extra "each extra matcher is appended to every generated query, verbatim" \
  "trigger('elitea-llm-gateway', 0)['metadata']['query'] == 'sum(gateway_llm_sse_active_connections{namespace=\"elitea\",cluster=\"prod-eu\",env=\"x\"})' and len(prom_queries()) == 3 and all(q.count('namespace=\"elitea\",cluster=\"prod-eu\",env=\"x\"}') == 2 for q in prom_queries() if 'elitea_subapp' in q)"
check dw-prom "the default queries match only the release namespace" \
  "prom_queries() and all(q.count('namespace=\"elitea\"') >= 1 and 'cluster=' not in q for q in prom_queries())"

# ── every ScaledObject: SkipDryRunOnMissingResource, and a CPU trigger ────
echo "== argo cd"
expect_render "every autoscaler on KEDA at once" all-keda \
  --set keda.enabled=true "${PROM[@]}" "${MON_API[@]}" \
  --set deepwiki.autoscaling.source=keda-postgresql --set inventory.autoscaling.source=keda-postgresql
check all-keda "four ScaledObjects and two TriggerAuthentications, each with SkipDryRunOnMissingResource=true" "
sorted(o['metadata']['name'] for o in objs('ScaledObject')) == ['elitea-deepwiki', 'elitea-inventory', 'elitea-llm-gateway', 'elitea-worker'] and
sorted(o['metadata']['name'] for o in objs('TriggerAuthentication')) == ['elitea-deepwiki-keda-postgresql', 'elitea-inventory-keda-postgresql'] and
all(ann(o).get('argocd.argoproj.io/sync-options') == 'SkipDryRunOnMissingResource=true' for o in objs('ScaledObject') + objs('TriggerAuthentication'))"
# The CPU trigger after the main trigger: the HPA takes the higher count, so
# the fleet scales on CPU when the metric source is absent.
for so in all-keda dw-prom gw-keda keda-flag inv-pg; do
  check "$so" "$so: every ScaledObject ends with a CPU Utilization trigger after its main trigger" "
objs('ScaledObject') and all(len(o['spec']['triggers']) == 2 and o['spec']['triggers'][0]['type'] != 'cpu' and
    o['spec']['triggers'][1]['type'] == 'cpu' and o['spec']['triggers'][1].get('metricType') == 'Utilization' and
    o['spec']['triggers'][1]['metadata']['value'] == '70'
    for o in objs('ScaledObject'))"
done
check all-keda "no ScaledObject and HPA of ours scale the same Deployment" \
  "not ({o['spec']['scaleTargetRef']['name'] for o in objs('ScaledObject')} & {h['spec']['scaleTargetRef']['name'] for h in objs('HorizontalPodAutoscaler')})"
check all-keda "no autoscaled Deployment carries replicas:" \
  "all('replicas' not in one('Deployment', n)['spec'] for n in ('elitea-main','elitea-web','elitea-vector','elitea-worker','elitea-deepwiki','elitea-inventory','elitea-llm-gateway','elitea-otel-collector'))"

# ── spread off ─────────────────────────────────────────────────────────────
expect_render "scaling.topologySpread.enabled=false" nospread --set scaling.topologySpread.enabled=false
check nospread "no default spread anywhere" \
  "all(not pod(d['metadata']['name']).get('topologySpreadConstraints') for d in objs('Deployment') if d['metadata']['name'].startswith('elitea-'))"

# ── grace periods ──────────────────────────────────────────────────────────
#
# Each refusal is one second short of what runs inside the grace; the render
# beside it is exactly enough.
echo "== grace"
expect_refusal "elitea-main's grace below preStop + shutdown + 10" "main.terminationGracePeriodSeconds is 40, but what runs inside it needs 50" \
  --set main.terminationGracePeriodSeconds=40
expect_refusal "the gateway's grace below its drain" "llmGateway.terminationGracePeriodSeconds is 100" \
  --set llmGateway.terminationGracePeriodSeconds=100
expect_refusal "DeepWiki's grace below preStop 5 + 10 + drain 3600 + 10" "deepwiki.terminationGracePeriodSeconds is 3624, but what runs inside it needs 3625" \
  --set deepwiki.terminationGracePeriodSeconds=3624
expect_refusal "Inventory's grace below preStop 5 + 10 + drain 3600 + 10" "inventory.terminationGracePeriodSeconds is 3624, but what runs inside it needs 3625" \
  --set inventory.terminationGracePeriodSeconds=3624
expect_refusal "elitea-vector's grace below preStop 5 + gRPC drain 20" "vector.terminationGracePeriodSeconds is 24, but what runs inside it needs 25" \
  --set vector.terminationGracePeriodSeconds=24
expect_refusal "the OTel Collector's grace below preStop 5 + batch flush 10" "otelCollector.terminationGracePeriodSeconds is 14, but what runs inside it needs 15" \
  --set otelCollector.terminationGracePeriodSeconds=14
expect_refusal "the worker's grace below shutdown + 15" "worker.terminationGracePeriodSeconds is 20" \
  --set worker.terminationGracePeriodSeconds=20
expect_render "each grace exactly at its sum" grace-exact \
  --set deepwiki.terminationGracePeriodSeconds=3625 --set inventory.terminationGracePeriodSeconds=3625 \
  --set vector.terminationGracePeriodSeconds=25 --set otelCollector.terminationGracePeriodSeconds=15
expect_render "a shorter preStop with a grace to match" grace-prestop \
  --set vector.preStop.sleepSeconds=0 --set vector.terminationGracePeriodSeconds=20 \
  --set otelCollector.preStop.sleepSeconds=0 --set otelCollector.terminationGracePeriodSeconds=10
check grace-prestop "preStop 0: no preStop hook" \
  "'lifecycle' not in container('elitea-vector','elitea-vector') and 'lifecycle' not in pod('elitea-otel-collector')['containers'][0]"
expect_render "a longer drain with a grace to match" grace-ok \
  --set-string main.env.ELITEA_SHUTDOWN_TIMEOUT=1m --set main.terminationGracePeriodSeconds=80

# ── the connection budget lists every component ────────────────────────────
echo "== budget"
expect_refusal "a server too small for the release" "only 5 are available" --set postgresql.maxConnections=30
for part in "elitea-main " "llm-gateway " "scheduler " "deepwiki host " "deepwiki engine " "inventory host " "inventory engine " "worker AgentState "; do
  if grep -qF -- "$part" "$TMP/refused.err"; then ok "the budget names $part"; else fail "the budget refusal does not name '$part'"; fi
done
# The shipped defaults at every ceiling fit the default server of 600.
expect_render "the defaults inside the default server (600)" budget-default --set postgresql.maxConnections=600

if [ "$fails" -ne 0 ]; then
  echo "render-scaling: $fails check(s) failed" >&2
  exit 1
fi
echo "render-scaling: every check passed"
