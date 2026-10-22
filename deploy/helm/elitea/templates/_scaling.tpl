{{/*
  _scaling.tpl — what every horizontally scaled workload in this chart shares
  (S11, scaling readiness): where an autoscaler's controller and metric come
  from, the replica range a Deployment can be at, the spread and disruption
  budget that range implies, and the grace-period arithmetic.

  ONE RULE runs through all of it: an autoscaler renders only when its
  controller AND its metric source are present. This chart has shipped an
  autoscaler that could not work (the gateway HPA named a metric nothing
  published, and pinned the Deployment at minReplicas with nothing saying
  why), so every source below is either detected, installed by this release,
  or stated by the operator; anything else is refused or falls back to CPU.
*/}}

{{/*
  elitea.scaling.kedaServed — "true" when a ScaledObject in this release has a
  controller to read it:
    * keda.enabled: this release installs KEDA (the pinned kedacore chart);
    * the cluster already serves keda.sh/v1alpha1 (a KEDA installed outside
      this release; `.Capabilities` is the API server's own list);
    * scaling.keda.clusterInstalled: the operator says so, for renders that
      cannot ask the cluster (`helm template`, a CI render, an Argo CD
      instance that does not pass the cluster's API versions).
*/}}
{{- define "elitea.scaling.kedaServed" -}}
{{- if or (dig "enabled" false (.Values.keda | default dict)) (.Capabilities.APIVersions.Has "keda.sh/v1alpha1") (dig "keda" "clusterInstalled" false (.Values.scaling | default dict)) -}}true{{- end -}}
{{- end }}

{{/*
  elitea.scaling.customMetricsServed — "true" when an HPA Pods metric has an
  API to come from (custom.metrics.k8s.io): this release's
  prometheus-adapter, one the cluster already serves, or the operator's word
  (scaling.customMetrics.clusterInstalled).
*/}}
{{- define "elitea.scaling.customMetricsServed" -}}
{{- if or (dig "enabled" false (.Values.prometheusAdapter | default dict)) (.Capabilities.APIVersions.Has "custom.metrics.k8s.io/v1beta1") (.Capabilities.APIVersions.Has "custom.metrics.k8s.io/v1beta2") (dig "customMetrics" "clusterInstalled" false (.Values.scaling | default dict)) -}}true{{- end -}}
{{- end }}

{{/*
  elitea.scaling.prometheusAddress — the Prometheus a KEDA `prometheus`
  trigger queries (scaling.prometheus.serverAddress). Empty: no prometheus
  trigger can render.
*/}}
{{- define "elitea.scaling.prometheusAddress" -}}
{{- dig "prometheus" "serverAddress" "" (.Values.scaling | default dict) | toString | trim -}}
{{- end }}

{{/*
  elitea.scaling.promSelector — the label selector of a KEDA prometheus
  query, braces included. Takes (dict "ctx" $ "matchers" (list "app=\"x\"" ...)).

  The release's namespace scopes the query, so two releases in one cluster
  (or one Prometheus for many clusters) do not add up each other's series. The
  LABEL that carries the namespace depends on the scrape: a Prometheus
  Operator PodMonitor writes `namespace`, many annotation-based scrape configs
  write `kubernetes_namespace`. So the label name is
  scaling.prometheus.namespaceLabel (default `namespace`). Empty leaves the
  namespace out: use it only where Prometheus scrapes this namespace alone.
  scaling.prometheus.extraMatchers adds matchers verbatim (a `cluster` label
  for a Prometheus shared by several clusters, say).

  A query whose label is wrong reads an empty vector. ignoreNullValues makes
  that zero, so each ScaledObject also carries a CPU trigger
  (elitea.scaling.kedaCpuTrigger): the fleet still scales on CPU when the
  metric is absent.
*/}}
{{- define "elitea.scaling.promSelector" -}}
{{- $p := (.ctx.Values.scaling | default dict).prometheus | default dict -}}
{{- $label := (hasKey $p "namespaceLabel") | ternary ($p.namespaceLabel | default "" | toString | trim) "namespace" -}}
{{- $matchers := .matchers | default list -}}
{{- if $label -}}
{{- if not (regexMatch "^[a-zA-Z_][a-zA-Z0-9_]*$" $label) -}}
{{- fail (printf "scaling.prometheus.namespaceLabel must be a Prometheus label name, and it is %q." $label) -}}
{{- end -}}
{{- $matchers = append $matchers (printf "%s=%q" $label .ctx.Release.Namespace) -}}
{{- end -}}
{{- $extra := $p.extraMatchers | default list -}}
{{- if kindIs "string" $extra -}}{{- $extra = list $extra -}}{{- end -}}
{{- range $extra -}}
{{- with (toString . | trim) -}}{{- $matchers = append $matchers . -}}{{- end -}}
{{- end -}}
{{- printf "{%s}" (join "," $matchers) -}}
{{- end }}

{{/*
  elitea.scaling.kedaCpuTrigger — the CPU trigger every ScaledObject in this
  chart carries next to its metric trigger. The HPA KEDA generates takes the
  HIGHER of the two replica counts, so the metric trigger drives the fleet
  while its source publishes, and CPU still scales it when the source is
  absent (a scrape that stopped, a namespace label that does not match, a
  database the scaler cannot reach). Takes the component's autoscaling values.
  Needs metrics-server and CPU requests on the scaled containers, as every
  CPU HPA in this chart does.
*/}}
{{- define "elitea.scaling.kedaCpuTrigger" -}}
- type: cpu
  metricType: Utilization
  metadata:
    value: {{ .targetCPUUtilizationPercentage | default 70 | toString | quote }}
{{- end }}

{{/*
  elitea.scaling.kedaAnnotations — the annotations EVERY ScaledObject and
  TriggerAuthentication in this chart carries. Takes the root context.

  Argo CD: SkipDryRunOnMissingResource. Argo CD dry-runs every object of a
  sync before it applies any; on a first sync with keda.enabled=true the KEDA
  CRDs are in the same sync and not yet applied, so the dry run of a
  ScaledObject fails "no matches for kind". With the option Argo CD skips that
  dry run, applies the CRDs first (it orders CustomResourceDefinitions ahead of
  everything else) and the ScaledObject after them.

  Helm: hook first, release resource after. Helm builds EVERY object of a
  release from the API server's discovery before it creates any, and KEDA's
  chart ships its CRDs as TEMPLATES (templates/crds), not in a crds/
  directory, so `helm install` with keda.enabled=true and a ScaledObject in the
  same release fails "resource mapping not found ... ensure CRDs are installed
  first" (measured, Helm 3.17 and 4.3). So while the cluster does not serve
  keda.sh/v1alpha1 yet and THIS release is what installs it, the object
  renders as a post-install/post-upgrade hook: Helm builds a hook only when it
  runs it, after the release's CRDs exist. It also carries the release's
  ownership metadata (the managed-by label is in every component's labels;
  the two meta.helm.sh annotations are added here), so the NEXT upgrade, which
  sees keda.sh/v1alpha1 served and renders it as a plain resource, adopts the
  existing object instead of failing "exists and cannot be imported".
  deploy/kind proves it on the first install; deploy/helm/tests/
  render-scaling.sh proves both renders.

  Argo CD maps the same hook to PostSync on its first sync (it passes the
  cluster's API versions, so later syncs render the plain resource).
*/}}
{{- define "elitea.scaling.kedaAnnotations" -}}
argocd.argoproj.io/sync-options: SkipDryRunOnMissingResource=true
{{- if and (dig "enabled" false (.Values.keda | default dict)) (not (.Capabilities.APIVersions.Has "keda.sh/v1alpha1")) }}
helm.sh/hook: post-install,post-upgrade
helm.sh/hook-weight: "10"
meta.helm.sh/release-name: {{ .Release.Name }}
meta.helm.sh/release-namespace: {{ .Release.Namespace }}
{{- end }}
{{- end }}

{{/*
  elitea.scaling.requireKeda — refuse a ScaledObject nothing reads. Takes
  (dict "ctx" $ "field" "<values path>" "why" "<what the scaler needs>").
*/}}
{{- define "elitea.scaling.requireKeda" -}}
{{- if not (include "elitea.scaling.kedaServed" .ctx) -}}
{{- fail (printf "%s needs KEDA, and none is available to this release: keda.enabled is false, the cluster does not serve keda.sh/v1alpha1, and scaling.keda.clusterInstalled is false. %s A ScaledObject applied without KEDA is an object no controller reads. Install KEDA once per cluster (https://keda.sh) and, for `helm template` or Argo CD, set scaling.keda.clusterInstalled=true (or pass --api-versions keda.sh/v1alpha1); or set keda.enabled=true to install it with this release (only if the cluster has none: one KEDA per cluster)." .field .why) -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.autoscaled — "true" when an autoscaler owns the component's
  Deployment (its autoscaling.enabled). Takes the component's values.
  While one does, the Deployment renders NO `replicas:` — a value there is
  written back by every `helm upgrade` and every Argo CD sync, snaps the fleet
  to replicaCount, and the autoscaler scales it out again: a burst of
  terminations at each deploy, and an Argo CD application that never stays
  in sync.
*/}}
{{- define "elitea.scaling.autoscaled" -}}
{{- if dig "autoscaling" "enabled" false . -}}true{{- end -}}
{{- end }}

{{/* The fewest replicas the component can run at: the autoscaler's floor, else replicaCount. */}}
{{- define "elitea.scaling.minReplicas" -}}
{{- if include "elitea.scaling.autoscaled" . -}}{{ int .autoscaling.minReplicas }}{{- else -}}{{ int (default 1 .replicaCount) }}{{- end -}}
{{- end }}

{{/* The most replicas the component can run at: the autoscaler's ceiling, else replicaCount. Budgets are checked here. */}}
{{- define "elitea.scaling.maxReplicas" -}}
{{- if include "elitea.scaling.autoscaled" . -}}{{ int .autoscaling.maxReplicas }}{{- else -}}{{ int (default 1 .replicaCount) }}{{- end -}}
{{- end }}

{{/*
  elitea.scaling.validateRange — refuse an autoscaler whose ceiling is below
  its floor, or a floor below 1. Takes (dict "v" <values> "field" "<key>").
*/}}
{{- define "elitea.scaling.validateRange" -}}
{{- if include "elitea.scaling.autoscaled" .v -}}
{{- $a := .v.autoscaling -}}
{{- if lt (int $a.minReplicas) 1 -}}
{{- fail (printf "%s.autoscaling.minReplicas (%v) must be at least 1." .field $a.minReplicas) -}}
{{- end -}}
{{- if lt (int $a.maxReplicas) (int $a.minReplicas) -}}
{{- fail (printf "%s.autoscaling.maxReplicas (%d) is below minReplicas (%d)." .field (int $a.maxReplicas) (int $a.minReplicas)) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.topologySpread — the pod spec's topologySpreadConstraints.
  Takes (dict "ctx" $ "v" <values> "selector" <selector label block>).

  An explicit <component>.topologySpreadConstraints list is rendered verbatim.
  Otherwise, when the component can run more than one replica and
  scaling.topologySpread.enabled is true (the default), two SOFT constraints:
  spread over zones, then over nodes, ScheduleAnyway both. Soft on purpose: a
  single-zone or single-node cluster still schedules every replica, it just
  cannot spread them. matchLabelKeys pod-template-hash counts only the
  current ReplicaSet, so a rollout's old pods do not skew where the new ones
  land. A single-replica component gets nothing: there is nothing to spread.
*/}}
{{- define "elitea.scaling.topologySpread" -}}
{{- $v := .v -}}
{{- $s := (.ctx.Values.scaling | default dict).topologySpread | default dict -}}
{{- if $v.topologySpreadConstraints }}
topologySpreadConstraints:
  {{- toYaml $v.topologySpreadConstraints | nindent 2 }}
{{- else if and (dig "enabled" true $s) (gt (int (include "elitea.scaling.maxReplicas" $v)) 1) }}
topologySpreadConstraints:
  {{- range $key := list "topology.kubernetes.io/zone" "kubernetes.io/hostname" }}
  - maxSkew: 1
    topologyKey: {{ $key }}
    whenUnsatisfiable: ScheduleAnyway
    matchLabelKeys:
      - pod-template-hash
    labelSelector:
      matchLabels:
        {{- $.selector | nindent 8 }}
  {{- end }}
{{- end }}
{{- end }}

{{/*
  elitea.scaling.pdb — a PodDisruptionBudget, rendered only when it can be
  honoured: <component>.podDisruptionBudget.enabled AND the component's floor
  is above one replica (the autoscaler's minReplicas, else replicaCount).
  minAvailable 1 over a single replica blocks every voluntary eviction —
  `kubectl drain`, a node-pool upgrade, the cluster autoscaler's scale-down —
  forever; over two it keeps one serving while the other moves.
  Takes (dict "ctx" $ "v" <values> "name" <object name> "labels" <labels>
  "selector" <selector labels> "annotations" <optional annotation block>).
*/}}
{{- define "elitea.scaling.pdb" -}}
{{- $pdb := .v.podDisruptionBudget | default dict -}}
{{- if and $pdb.enabled (gt (int (include "elitea.scaling.minReplicas" .v)) 1) }}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ .name }}
  labels:
    {{- .labels | nindent 4 }}
  {{- with .annotations }}
  annotations:
    {{- . | nindent 4 }}
  {{- end }}
spec:
  minAvailable: {{ $pdb.minAvailable | default 1 }}
  selector:
    matchLabels:
      {{- .selector | nindent 6 }}
{{- end }}
{{- end }}

{{/*
  elitea.durationSeconds — a Go duration string ("30s", "2m30s", "1h", "500ms")
  as whole seconds, rounded up. Takes (dict "field" "<values path>" "value" v).
  A bare integer is read as seconds. Anything else is refused by name: the
  services parse it with time.ParseDuration and exit on a bad value, so a
  render that guessed would only move the failure to the pod.
*/}}
{{- define "elitea.durationSeconds" -}}
{{- $v := .value | toString | trim -}}
{{- if regexMatch "^[0-9]+$" $v -}}
{{- $v -}}
{{- else if regexMatch "^([0-9]+(\\.[0-9]+)?(h|ms|m|s))+$" $v -}}
{{- $ms := 0.0 -}}
{{- range $part := regexFindAll "[0-9]+(\\.[0-9]+)?(h|ms|m|s)" $v -1 -}}
{{- $n := float64 (regexFind "^[0-9]+(\\.[0-9]+)?" $part) -}}
{{- $unit := trimPrefix (regexFind "^[0-9]+(\\.[0-9]+)?" $part) $part -}}
{{- $mult := dict "h" 3600000.0 "m" 60000.0 "s" 1000.0 "ms" 1.0 -}}
{{- $ms = addf $ms (mulf $n (get $mult $unit)) -}}
{{- end -}}
{{- ceil (divf $ms 1000.0) | int -}}
{{- else -}}
{{- fail (printf "%s must be a Go duration such as \"30s\" or \"2m30s\", and it is %q." .field $v) -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.validateGrace — the grace period has to outlast what runs
  inside it. Kubernetes starts terminationGracePeriodSeconds when it starts the
  preStop hook, so the hook's sleep, the process's own drain and the margin
  for what follows the drain all spend the same clock; past it the kubelet
  sends SIGKILL, mid-drain. Takes (dict "field" "<component>" "grace" n
  "parts" (list (list "<label>" seconds) ...) ).
*/}}
{{- define "elitea.scaling.validateGrace" -}}
{{- $need := 0 -}}
{{- $named := list -}}
{{- range .parts -}}
{{- $need = add $need (int (index . 1)) -}}
{{- $named = append $named (printf "%s %ds" (index . 0) (int (index . 1))) -}}
{{- end -}}
{{- if lt (int .grace) $need -}}
{{- fail (printf "%s.terminationGracePeriodSeconds is %d, but what runs inside it needs %d (%s). The kubelet sends SIGKILL when the grace period ends, whatever the drain is doing: raise terminationGracePeriodSeconds to at least %d, or shorten the drain." .field (int .grace) $need (join " + " $named) $need) -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.preStop — the container's preStop hook as the NATIVE sleep
  action (Kubernetes 1.30+), which needs no shell or sleep binary: every image
  here is distroless. The sleep gives the endpoint controllers, kube-proxy and
  the load balancer time to take the pod out of rotation BEFORE the process
  gets SIGTERM, so no new request lands on a process that has stopped
  accepting. Takes the seconds; 0 renders nothing.
*/}}
{{- define "elitea.scaling.preStop" -}}
{{- if gt (int .) 0 }}
lifecycle:
  preStop:
    sleep:
      seconds: {{ int . }}
{{- end }}
{{- end }}

{{/*
  elitea.scaling.cpuHpa — a CPU HorizontalPodAutoscaler (metrics-server) for a
  stateless component that publishes no better signal, rendered while
  <component>.autoscaling.enabled. Takes (dict "v" <values> "field" "<key>"
  "name" <Deployment name> "labels" <labels>). Without metrics-server the HPA
  reports FailedGetResourceMetric and holds minReplicas: the fixed count it
  replaces, not an outage.
*/}}
{{- define "elitea.scaling.cpuHpa" -}}
{{- if include "elitea.scaling.autoscaled" .v }}
{{- include "elitea.scaling.validateRange" (dict "v" .v "field" .field) }}
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ .name }}
  labels:
    {{- .labels | nindent 4 }}
  annotations:
    elitea.ai/autoscaling-source: cpu
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ .name }}
  minReplicas: {{ int .v.autoscaling.minReplicas }}
  maxReplicas: {{ int .v.autoscaling.maxReplicas }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ int (.v.autoscaling.targetCPUUtilizationPercentage | default 70) }}
{{- end }}
{{- end }}

{{/*
  elitea.scaling.podMonitor — a Prometheus Operator PodMonitor for a
  component's metrics port, rendered only where monitoring.coreos.com/v1 is
  served and <component>.metrics.podMonitor.enabled (the default). The same
  shape as the scheduler's (templates/scheduler/monitoring.yaml). Takes (dict
  "ctx" $ "v" <values> "name" <object name> "labels" <labels> "selector"
  <selector labels> "port" <container port NAME> "path" <path>).
*/}}
{{- define "elitea.scaling.podMonitor" -}}
{{- $m := .v.metrics | default dict -}}
{{- if and (.ctx.Capabilities.APIVersions.Has "monitoring.coreos.com/v1") (dig "podMonitor" "enabled" true $m) }}
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: {{ .name }}
  labels:
    {{- .labels | nindent 4 }}
    {{- with (dig "podMonitor" "labels" dict $m) }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  selector:
    matchLabels:
      {{- .selector | nindent 6 }}
  podMetricsEndpoints:
    - port: {{ .port }}
      path: {{ .path }}
      interval: {{ dig "podMonitor" "interval" "30s" $m }}
{{- end }}
{{- end }}

{{/*
  elitea.scaling.scraped — "true" when something this chart renders makes
  Prometheus scrape the component: the PodMonitor (where the Operator is
  served) or the prometheus.io/* pod annotations. A KEDA prometheus trigger
  over a metric nothing scrapes reads an empty vector — zero — and holds the
  fleet at its floor while the load grows, so the trigger is refused without
  one. <component>.metrics.scrapedExternally states a scrape this chart
  cannot see (your own scrape config).
*/}}
{{- define "elitea.scaling.scraped" -}}
{{- $m := .v.metrics | default dict -}}
{{- if or (and (.ctx.Capabilities.APIVersions.Has "monitoring.coreos.com/v1") (dig "podMonitor" "enabled" true $m)) (dig "scrapeAnnotations" false $m) (dig "scrapedExternally" false $m) -}}true{{- end -}}
{{- end }}

{{/*
  elitea.scaling.scrapeAnnotations — the prometheus.io/* pod annotations for
  an annotation-based scrape, when <component>.metrics.scrapeAnnotations.
  Takes (dict "v" <values> "port" <number> "path" <path>).
*/}}
{{- define "elitea.scaling.scrapeAnnotations" -}}
{{- if dig "scrapeAnnotations" false (.v.metrics | default dict) }}
prometheus.io/scrape: "true"
prometheus.io/port: {{ .port | int64 | quote }}
prometheus.io/path: {{ .path }}
{{- end }}
{{- end }}

{{/*
  elitea.scaling.subappSource — which scaler a sub-application host
  (DeepWiki, Inventory) uses. Takes (dict "ctx" $ "key" "deepwiki"|"inventory"
  "dbSecret" <the host's database secret ref, or empty>).

  There is no queue in front of these hosts: an invocation runs on the replica
  that accepted it, start to end. So the load is the number of invocations
  running, and one replica holds MAX_PARALLEL_WORKERS of them. DeepWiki
  admits a generation against the slots of the WHOLE fleet and refuses it
  (service_busy) when they are all busy, so the running count never goes
  above the fleet's slots and alone could never ask for one more replica:
  both sources add the demand refused in the last 5 minutes.
    keda-prometheus  sum(elitea_subapp_invocations_in_flight{app=...}) +
                     increase(elitea_subapp_slot_refusals_total[5m]) from the
                     host's admin /metrics, divided by MAX_PARALLEL_WORKERS.
                     Needs KEDA, scaling.prometheus.serverAddress and a scrape.
    keda-postgresql  count(*) of the host's invocations rows that are running
                     (finished_at IS NULL) or were refused busy in the last 5
                     minutes, divided by MAX_PARALLEL_WORKERS. Needs KEDA and
                     the host's own database (DeepWiki: invocations;
                     Inventory: inventory_graph.invocations).
    cpu              a CPU HorizontalPodAutoscaler (metrics-server).
  `auto` takes the first that is available, in that order.
*/}}
{{- define "elitea.scaling.subappSource" -}}
{{- $v := index .ctx.Values .key -}}
{{- $a := $v.autoscaling | default dict -}}
{{- $want := $a.source | default "auto" | toString -}}
{{- $keda := include "elitea.scaling.kedaServed" .ctx -}}
{{- $prom := and $keda (include "elitea.scaling.prometheusAddress" .ctx) (include "elitea.scaling.scraped" (dict "ctx" .ctx "v" $v)) -}}
{{- $pg := and $keda .dbSecret -}}
{{- if eq $want "auto" -}}
{{- if $prom -}}keda-prometheus{{- else if $pg -}}keda-postgresql{{- else -}}cpu{{- end -}}
{{- else if eq $want "keda-prometheus" -}}
{{- include "elitea.scaling.requireKeda" (dict "ctx" .ctx "field" (printf "%s.autoscaling.source=keda-prometheus" .key) "why" "The prometheus trigger is read by the KEDA operator.") -}}
{{- if not (include "elitea.scaling.prometheusAddress" .ctx) -}}
{{- fail (printf "%s.autoscaling.source=keda-prometheus needs scaling.prometheus.serverAddress: the Prometheus the KEDA operator queries for elitea_subapp_invocations_in_flight." .key) -}}
{{- end -}}
{{- if not (include "elitea.scaling.scraped" (dict "ctx" .ctx "v" $v)) -}}
{{- fail (printf "%s.autoscaling.source=keda-prometheus, but nothing makes Prometheus scrape the host's admin /metrics: the Prometheus Operator is not served (no PodMonitor), %s.metrics.scrapeAnnotations is false and %s.metrics.scrapedExternally is false. An unscraped gauge reads as zero, and the fleet would sit at its floor under load." .key .key .key) -}}
{{- end -}}
keda-prometheus
{{- else if eq $want "keda-postgresql" -}}
{{- include "elitea.scaling.requireKeda" (dict "ctx" .ctx "field" (printf "%s.autoscaling.source=keda-postgresql" .key) "why" "The postgresql trigger is read by the KEDA operator.") -}}
{{- if not .dbSecret -}}
{{- fail (printf "%s.autoscaling.source=keda-postgresql needs the host's invocation store, and this host has none: no database secret is configured (postgresql.existingSecret or %s.secrets.ELITEA_%s_DATABASE_URL)." .key .key (upper .key)) -}}
{{- end -}}
keda-postgresql
{{- else if eq $want "cpu" -}}
cpu
{{- else -}}
{{- fail (printf "%s.autoscaling.source must be auto, keda-prometheus, keda-postgresql or cpu, and it is %q." .key $want) -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.workerSource — which scaler the agent worker uses. Its load is
  the backlog of its JetStream durable consumer, which only KEDA's
  nats-jetstream scaler reads.
    keda  a ScaledObject on consumer lag (plus CPU). Needs KEDA.
    cpu   a CPU HorizontalPodAutoscaler (metrics-server). The fallback: CPU
          rises later than the backlog, because a turn waits on a model.
  `auto` (the default) takes keda when KEDA is available, else cpu.
*/}}
{{- define "elitea.scaling.workerSource" -}}
{{- $want := (.Values.worker.autoscaling.source | default "auto") | toString -}}
{{- if eq $want "auto" -}}
{{- if include "elitea.scaling.kedaServed" . -}}keda{{- else -}}cpu{{- end -}}
{{- else if eq $want "keda" -}}
{{- include "elitea.scaling.requireKeda" (dict "ctx" . "field" "worker.autoscaling.source=keda" "why" "The worker's consumer-lag trigger is read by the KEDA operator. Use source=auto or cpu for a CPU HorizontalPodAutoscaler instead.") -}}
keda
{{- else if eq $want "cpu" -}}
cpu
{{- else -}}
{{- fail (printf "worker.autoscaling.source must be auto, keda or cpu, and it is %q." $want) -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.gatewaySource — which scaler the LLM gateway uses. Its load is
  concurrent /llm streams (gateway_llm_sse_active_connections, a GAUGE on its
  :8083/metrics); CPU is a poor signal for I/O-bound streaming.
    keda-prometheus  sum of the gauge over the namespace, divided by
                     targetSSEConnectionsPerPod. Needs KEDA,
                     scaling.prometheus.serverAddress and a scrape.
    adapter          an HPA Pods metric through custom.metrics.k8s.io (this
                     release's prometheus-adapter, or one the cluster serves).
    cpu              a CPU HorizontalPodAutoscaler.
  `auto` takes the first that is available, in that order.
*/}}
{{- define "elitea.scaling.gatewaySource" -}}
{{- $v := .Values.llmGateway -}}
{{- $want := $v.autoscaling.source | default "auto" | toString -}}
{{- $keda := include "elitea.scaling.kedaServed" . -}}
{{- $prom := and $keda (include "elitea.scaling.prometheusAddress" .) (include "elitea.scaling.scraped" (dict "ctx" . "v" $v)) -}}
{{- $adapter := include "elitea.scaling.customMetricsServed" . -}}
{{- if eq $want "auto" -}}
{{- if $prom -}}keda-prometheus{{- else if $adapter -}}adapter{{- else -}}cpu{{- end -}}
{{- else if eq $want "keda-prometheus" -}}
{{- include "elitea.scaling.requireKeda" (dict "ctx" . "field" "llmGateway.autoscaling.source=keda-prometheus" "why" "The prometheus trigger is read by the KEDA operator.") -}}
{{- if not (include "elitea.scaling.prometheusAddress" .) -}}
{{- fail "llmGateway.autoscaling.source=keda-prometheus needs scaling.prometheus.serverAddress: the Prometheus the KEDA operator queries for gateway_llm_sse_active_connections." -}}
{{- end -}}
{{- if not (include "elitea.scaling.scraped" (dict "ctx" . "v" $v)) -}}
{{- fail "llmGateway.autoscaling.source=keda-prometheus, but nothing makes Prometheus scrape the gateway's :8083/metrics: the Prometheus Operator is not served (no PodMonitor), llmGateway.metrics.scrapeAnnotations is false and llmGateway.metrics.scrapedExternally is false. An unscraped gauge reads as zero, and the fleet would sit at its floor under load." -}}
{{- end -}}
keda-prometheus
{{- else if eq $want "adapter" -}}
{{- if not $adapter -}}
{{- fail (printf "llmGateway.autoscaling.source=adapter needs the custom metrics API (custom.metrics.k8s.io), and none is available: prometheusAdapter.enabled is false, the cluster does not serve it, and scaling.customMetrics.clusterInstalled is false. Without it the HPA reports FailedGetPodsMetric and pins the gateway at minReplicas with nothing else saying why. Set prometheusAdapter.enabled=true (only if the cluster has no custom metrics adapter: one per cluster), install one, or use source=cpu.") -}}
{{- end -}}
adapter
{{- else if eq $want "cpu" -}}
cpu
{{- else -}}
{{- fail (printf "llmGateway.autoscaling.source must be auto, keda-prometheus, adapter or cpu, and it is %q." $want) -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.validateClusterAddons — KEDA and prometheus-adapter are
  CLUSTER-WIDE: CRDs, and an APIService each (external.metrics.k8s.io for
  KEDA, custom.metrics.k8s.io for the adapter). Two of either in one cluster
  fight over the same cluster-scoped objects, and the loser's scalers read
  nothing. So enable them only on a cluster that has none.

  `lookup` sees the cluster during `helm install` / `helm upgrade` and returns
  nothing under `helm template` and Argo CD. Where it sees one, an object this
  release does not own is refused here, by name; elsewhere NOTES.txt carries
  the warning (Argo CD shows no NOTES, which is why docs/UPGRADING.md and the
  Scaling page say it too).
*/}}
{{- define "elitea.scaling.validateClusterAddons" -}}
{{- $owned := dict -}}
{{- if dig "enabled" false (.Values.keda | default dict) -}}
{{- range $kind, $name := dict "CustomResourceDefinition" "scaledobjects.keda.sh" "APIService" "v1beta1.external.metrics.k8s.io" -}}
{{- $api := ternary "apiextensions.k8s.io/v1" "apiregistration.k8s.io/v1" (eq $kind "CustomResourceDefinition") -}}
{{- $found := lookup $api $kind "" $name -}}
{{- if $found -}}
{{- $ann := $found.metadata.annotations | default dict -}}
{{- if or (ne (get $ann "meta.helm.sh/release-name" | toString) $.Release.Name) (ne (get $ann "meta.helm.sh/release-namespace" | toString) $.Release.Namespace) -}}
{{- fail (printf "keda.enabled=true, but this cluster already has the %s %s, and this release does not own it (release %q in %q does). KEDA is cluster-wide: one per cluster. Set keda.enabled=false and scaling.keda.clusterInstalled=true to use the KEDA already there." $kind $name (get $ann "meta.helm.sh/release-name" | toString | default "outside Helm") (get $ann "meta.helm.sh/release-namespace" | toString | default "-")) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if dig "enabled" false (.Values.prometheusAdapter | default dict) -}}
{{- if not (dig "prometheus" "url" "" .Values.prometheusAdapter) -}}
{{- fail "prometheusAdapter.enabled=true needs prometheusAdapter.prometheus.url: the Prometheus the adapter reads gateway_llm_sse_active_connections from (for example http://prometheus-operated.monitoring.svc, with prometheus.port 9090)." -}}
{{- end -}}
{{- $found := lookup "apiregistration.k8s.io/v1" "APIService" "" "v1beta1.custom.metrics.k8s.io" -}}
{{- if $found -}}
{{- $ann := $found.metadata.annotations | default dict -}}
{{- if or (ne (get $ann "meta.helm.sh/release-name" | toString) $.Release.Name) (ne (get $ann "meta.helm.sh/release-namespace" | toString) $.Release.Namespace) -}}
{{- fail (printf "prometheusAdapter.enabled=true, but this cluster already serves custom.metrics.k8s.io through the APIService v1beta1.custom.metrics.k8s.io, which this release does not own. A custom metrics adapter is cluster-wide: one per cluster. Set prometheusAdapter.enabled=false and scaling.customMetrics.clusterInstalled=true, and add a rule for gateway_llm_sse_active_connections to the adapter already there.") -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
  elitea.scaling.subappAutoscaler — a sub-application host's autoscaler and
  everything around it: the HPA or the ScaledObject (+ TriggerAuthentication),
  the PodDisruptionBudget and the PodMonitor. Takes (dict "ctx" $ "key"
  "deepwiki"|"inventory" "fullname" <name> "labels" <labels> "selector"
  <selector labels> "dbSecret" <{secretName,key} of the HOST's database, or
  empty>).
*/}}
{{- define "elitea.scaling.subappAutoscaler" -}}
{{- $ctx := .ctx -}}
{{- $v := index $ctx.Values .key -}}
{{- $prefix := printf "ELITEA_%s_" (upper .key) -}}
{{- $env := $v.env | default dict -}}
{{- $slots := include "elitea.integer" (dict "field" (printf "%s.env.%sMAX_PARALLEL_WORKERS" .key $prefix) "value" (get $env (printf "%sMAX_PARALLEL_WORKERS" $prefix) | default "1" | toString) "min" 1) -}}
{{- $app := printf "elitea-%s" .key -}}
{{- if include "elitea.scaling.autoscaled" $v }}
{{- $a := $v.autoscaling }}
{{- $source := include "elitea.scaling.subappSource" (dict "ctx" $ctx "key" .key "dbSecret" .dbSecret) | trim }}
{{- if eq $source "cpu" }}
---
# CPU, the fallback: no KEDA trigger source is available (see
# {{ .key }}.autoscaling). An invocation's work is mostly the engine's, which
# shows on CPU, but a replica waiting on a model call looks idle.
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ .fullname }}
  labels:
    {{- .labels | nindent 4 }}
  annotations:
    elitea.ai/autoscaling-source: cpu
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ .fullname }}
  minReplicas: {{ $a.minReplicas }}
  maxReplicas: {{ $a.maxReplicas }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ $a.targetCPUUtilizationPercentage | default 70 }}
  behavior:
    scaleDown:
      stabilizationWindowSeconds: {{ $a.cooldownSeconds | default 600 }}
{{- else }}
{{- if eq $source "keda-postgresql" }}
---
# The KEDA operator's credential for the postgresql trigger: the SAME Secret
# and key the host reads its invocation store from, so the scaler cannot count
# rows in a different database than the fleet writes.
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: {{ .fullname }}-keda-postgresql
  labels:
    {{- .labels | nindent 4 }}
  annotations:
    {{- include "elitea.scaling.kedaAnnotations" $ctx | nindent 4 }}
spec:
  secretTargetRef:
    - parameter: connection
      name: {{ .dbSecret.secretName }}
      key: {{ .dbSecret.key }}
{{- end }}
---
# {{ $app }} scaled by KEDA on the invocations it is RUNNING. There is no queue:
# an invocation runs on the replica that accepted it, start to end, and one
# replica runs {{ $prefix }}MAX_PARALLEL_WORKERS ({{ $slots }}) at once, so the
# target per replica is exactly that.
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: {{ .fullname }}
  labels:
    {{- .labels | nindent 4 }}
  annotations:
    {{- include "elitea.scaling.kedaAnnotations" $ctx | nindent 4 }}
spec:
  scaleTargetRef:
    name: {{ .fullname }}
  minReplicaCount: {{ $a.minReplicas }}
  maxReplicaCount: {{ $a.maxReplicas }}
  pollingInterval: {{ $a.pollingIntervalSeconds | default 30 }}
  # Long: a replica that is scaled in cancels what it cannot finish within
  # its drain, and an invocation runs for minutes.
  cooldownPeriod: {{ $a.cooldownSeconds | default 600 }}
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleDown:
          stabilizationWindowSeconds: {{ $a.cooldownSeconds | default 600 }}
  triggers:
    {{- if eq $source "keda-prometheus" }}
    # elitea_subapp_invocations_in_flight from every replica's admin /metrics,
    # plus the generations refused busy in the last 5 minutes. `or vector(0)`
    # keeps one absent series from making the whole sum empty.
    {{- $sel := include "elitea.scaling.promSelector" (dict "ctx" $ctx "matchers" (list (printf "app=%q" $app))) }}
    - type: prometheus
      metadata:
        serverAddress: {{ include "elitea.scaling.prometheusAddress" $ctx | quote }}
        query: {{ ($a.prometheus | default dict).query | default (printf "(sum(elitea_subapp_invocations_in_flight%s) or vector(0)) + (sum(increase(elitea_subapp_slot_refusals_total%s[5m])) or vector(0))" $sel $sel) | quote }}
        threshold: {{ $slots | quote }}
        ignoreNullValues: "true"
    {{- else }}
    # The unfinished rows (running, and admitted but waiting for a local slot)
    # of the host's shared invocation store, plus the rows refused busy in the
    # last 5 minutes: the table the engine's migrations own. DeepWiki's is
    # unqualified and resolves through the DSN's search_path, as the host's
    # own statements do; Inventory's is in the inventory_graph schema.
    - type: postgresql
      metadata:
        query: {{ printf "SELECT count(*) FROM %s WHERE finished_at IS NULL OR (finished_at > now() - interval '5 minutes' AND result->>'error_category' = 'service_busy')" (.table | default "invocations") | quote }}
        targetQueryValue: {{ $slots | quote }}
      authenticationRef:
        name: {{ .fullname }}-keda-postgresql
    {{- end }}
    # CPU next to the metric: the HPA takes the higher of the two, so the
    # host still scales when its metric source is absent.
    {{- include "elitea.scaling.kedaCpuTrigger" $a | nindent 4 }}
{{- end }}
{{- end }}
{{- with (include "elitea.scaling.pdb" (dict "ctx" $ctx "v" $v "name" .fullname "labels" .labels "selector" .selector)) }}
---
{{- . }}
{{- end }}
{{- with (include "elitea.scaling.podMonitor" (dict "ctx" $ctx "v" $v "name" .fullname "labels" .labels "selector" .selector "port" "admin" "path" "/metrics")) }}
---
# Scrapes the host's admin /metrics (elitea_subapp_invocations_in_flight,
# elitea_subapp_invocation_slots, elitea_subapp_slot_refusals_total).
{{- . }}
{{- end }}
{{- end }}
