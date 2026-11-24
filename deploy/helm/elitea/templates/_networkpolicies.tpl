{{/*
  Peers for the chart's NetworkPolicies (templates/main/networkpolicy.yaml,
  templates/worker/*networkpolicy.yaml, templates/sandbox/supervisor-networkpolicy.yaml).

  Every in-chart peer is a namespaceSelector on the release namespace TOGETHER
  with a podSelector built from the SAME selector helper the workload's own
  Deployment uses, so a rename moves the policy with the pods. A podSelector
  alone would match same-labelled pods in every namespace.

  elitea.netpol.peer takes {ctx, labels}, where labels is a block of
  `key: value` lines.
*/}}
{{- define "elitea.netpol.peer" -}}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ .ctx.Release.Namespace }}
  podSelector:
    matchLabels:
      {{- .labels | nindent 6 }}
{{- end }}

{{- define "elitea.netpol.mainPeer" -}}
{{- include "elitea.netpol.peer" (dict "ctx" . "labels" (include "elitea-main.selectorLabels" .)) -}}
{{- end }}

{{/* The worker Deployment's pods, by name, instance and component (the component label is part of the immutable selector; it once separated them from the retired platform edge). */}}
{{- define "elitea.netpol.workerPeer" -}}
{{- include "elitea.netpol.peer" (dict "ctx" . "labels" (printf "%s\napp.kubernetes.io/component: worker" (include "elitea-worker.selectorLabels" .))) -}}
{{- end }}

{{- define "elitea.netpol.providerPeer" -}}
{{- include "elitea.netpol.peer" (dict "ctx" .ctx "labels" (include "elitea.provider.selectorLabels" .)) -}}
{{- end }}

{{/* Cluster DNS, the same peer shape as the LLM gateway's policy. */}}
{{- define "elitea.netpol.dnsEgress" -}}
- to:
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          k8s-app: kube-dns
  ports:
    - protocol: UDP
      port: 53
    - protocol: TCP
      port: 53
{{- end }}

{{/*
  ── Egress building blocks (#2951, #2952) ────────────────────────────────────
  Shared by the worker's and the LLM gateway's egress policies, so the two
  cannot disagree about what "public" means or where a dependency lives.
*/}}

{{/* Cluster DNS for the egress policies: networkPolicies.dnsEgress replaces
     the k8s-app=kube-dns rule (NodeLocal DNSCache, OpenShift's resolver). */}}
{{- define "elitea.netpol.clusterDnsEgress" -}}
{{- with .Values.networkPolicies.dnsEgress -}}
{{- toYaml . -}}
{{- else -}}
{{- include "elitea.netpol.dnsEgress" . -}}
{{- end -}}
{{- end }}

{{/* The cluster's pod and Service ranges, as a JSON list:
     networkPolicies.clusterCIDRs, else the egress proxy's ranges
     (egressProxy.networkPolicy, else sandboxKubernetes.workspaceRuns.egress).
     One cluster, one answer. */}}
{{- define "elitea.netpol.clusterCidrs" -}}
{{- $own := .Values.networkPolicies.clusterCIDRs | default list -}}
{{- if $own -}}
{{- $own | toJson -}}
{{- else -}}
{{- include "elitea-egress-proxy.clusterCidrs" . -}}
{{- end -}}
{{- end }}

{{/* Address ranges a public-internet rule never admits: RFC 1918, loopback,
     link-local (the cloud metadata endpoints, 169.254.169.254 and AWS's
     fd00:ec2::254 in unique-local), carrier-grade NAT, "this network", IETF
     protocol assignments, benchmarking, multicast, reserved, the Azure
     platform endpoint, IPv6 unique-local, link-local, loopback and multicast. */}}
{{- define "elitea.netpol.deniedV4" -}}
{{- list "10.0.0.0/8" "172.16.0.0/12" "192.168.0.0/16" "127.0.0.0/8" "169.254.0.0/16" "100.64.0.0/10" "0.0.0.0/8" "192.0.0.0/24" "198.18.0.0/15" "224.0.0.0/4" "240.0.0.0/4" "168.63.129.16/32" | toJson -}}
{{- end }}
{{- define "elitea.netpol.deniedV6" -}}
{{- list "fc00::/7" "fe80::/10" "::1/128" "ff00::/8" | toJson -}}
{{- end }}

{{/*
  elitea.netpol.publicEgress — one egress rule: 0.0.0.0/0 (and ::/0 with
  networkPolicies.ipv6) minus the denied ranges and the cluster's own ranges,
  on the given ports. NetworkPolicy has no "deny", so the carve-out is the
  ipBlock `except` list. Takes {ctx, ports}.
*/}}
{{- define "elitea.netpol.publicEgress" -}}
{{- $v4 := include "elitea.netpol.deniedV4" .ctx | fromJsonArray -}}
{{- $v6 := include "elitea.netpol.deniedV6" .ctx | fromJsonArray -}}
{{- range (include "elitea.netpol.clusterCidrs" .ctx | fromJsonArray) -}}
{{- if contains ":" (toString .) }}{{ $v6 = append $v6 (toString .) }}{{ else }}{{ $v4 = append $v4 (toString .) }}{{ end -}}
{{- end -}}
- to:
    - ipBlock:
        cidr: 0.0.0.0/0
        except:
          {{- range uniq $v4 }}
          - {{ . | quote }}
          {{- end }}
    {{- if .ctx.Values.networkPolicies.ipv6 }}
    - ipBlock:
        cidr: ::/0
        except:
          {{- range uniq $v6 }}
          - {{ . | quote }}
          {{- end }}
    {{- end }}
  ports:
    {{- toYaml .ports | nindent 4 }}
{{- end }}

{{/* NATS: the bundled server's pods on 4222. A NATS this chart does not run
     (nats.enabled false) is still an in-cluster Service (natsClient.service in
     natsClient.namespace): networkPolicies.egressPeers.nats names its pods,
     else every pod of natsClient's namespace on natsClient.port. */}}
{{- define "elitea.netpol.natsEgress" -}}
{{- $p := .Values.networkPolicies.egressPeers.nats -}}
{{- if and (not .Values.nats.enabled) (not $p.to) }}
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.natsClient.namespace | default .Release.Namespace }}
  ports:
    - protocol: TCP
      port: {{ .Values.natsClient.port }}
{{- end }}
{{- if .Values.nats.enabled }}
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ include "elitea-nats.namespace" . }}
      podSelector:
        matchLabels:
          {{- include "elitea-nats.serverSelectorLabels" . | nindent 10 }}
  ports:
    - protocol: TCP
      port: 4222
{{- end }}
{{- with $p.to }}
- to:
    {{- toYaml . | nindent 4 }}
  ports:
    {{- toYaml $p.ports | nindent 4 }}
{{- end }}
{{- end }}

{{/* PostgreSQL: networkPolicies.egressPeers.postgresql (the chart cannot know a
     managed database's address), and the bundled PgBouncer when it runs. */}}
{{- define "elitea.netpol.postgresqlEgress" -}}
{{- $p := .Values.networkPolicies.egressPeers.postgresql -}}
{{- with $p.to }}
- to:
    {{- toYaml . | nindent 4 }}
  ports:
    {{- toYaml $p.ports | nindent 4 }}
{{- end }}
{{- if .Values.pgbouncer.enabled }}
- to:
    {{- include "elitea.netpol.peer" (dict "ctx" . "labels" (include "elitea-pgbouncer.selectorLabels" .)) | nindent 4 }}
  ports:
    - protocol: TCP
      port: {{ .Values.pgbouncer.service.port }}
{{- end }}
{{- end }}

{{/* The bundled OTel collector's OTLP ports, while it runs. */}}
{{- define "elitea.netpol.otelEgress" -}}
{{- if .Values.otelCollector.enabled }}
- to:
    {{- include "elitea.netpol.peer" (dict "ctx" . "labels" (include "otel-collector.selectorLabels" .)) | nindent 4 }}
  ports:
    - protocol: TCP
      port: {{ .Values.otelCollector.service.grpcPort }}
    - protocol: TCP
      port: {{ .Values.otelCollector.service.httpPort }}
{{- end }}
{{- end }}

{{/*
  elitea.netpol.namesPeer — ONE in-namespace peer for several of the chart's
  workload families, by app.kubernetes.io/name (In) and instance. Takes
  {ctx, components}; each component is admitted only while it runs:
    main        elitea-main's pods
    mainJobs    elitea-main's migration Jobs (<name>-migrate, <name>-agentstate-migrate)
    scheduler, llmGateway, worker (the Deployment and its workload-session
    Jobs), deepwiki, inventory (each with its migration Job), vector
  Renders nothing when no listed component runs.
*/}}
{{- define "elitea.netpol.namesPeer" -}}
{{- $v := .ctx.Values -}}
{{- $names := list -}}
{{- range .components -}}
{{- if and (eq . "main") $v.main.enabled -}}{{- $names = append $names (include "elitea-main.name" $.ctx) -}}{{- end -}}
{{- if and (eq . "mainJobs") $v.main.enabled -}}{{- $names = concat $names (list (printf "%s-migrate" (include "elitea-main.name" $.ctx)) (printf "%s-agentstate-migrate" (include "elitea-main.name" $.ctx))) -}}{{- end -}}
{{- if and (eq . "scheduler") $v.scheduler.enabled -}}{{- $names = append $names (include "elitea-scheduler.name" $.ctx) -}}{{- end -}}
{{- if and (eq . "llmGateway") $v.llmGateway.enabled -}}{{- $names = append $names (include "elitea-llm-gateway.name" $.ctx) -}}{{- end -}}
{{- if and (eq . "worker") $v.worker.enabled -}}{{- $names = append $names (include "elitea-worker.name" $.ctx) -}}{{- end -}}
{{- if and (eq . "deepwiki") $v.deepwiki.enabled -}}{{- $names = append $names (include "elitea.provider.name" (dict "ctx" $.ctx "provider" "deepwiki")) -}}{{- end -}}
{{- if and (eq . "inventory") $v.inventory.enabled -}}{{- $names = append $names (include "elitea.provider.name" (dict "ctx" $.ctx "provider" "inventory")) -}}{{- end -}}
{{- if and (eq . "vector") $v.vector.enabled -}}{{- $names = append $names "elitea-vector" -}}{{- end -}}
{{- end -}}
{{- with $names }}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ $.ctx.Release.Namespace }}
  podSelector:
    matchLabels:
      app.kubernetes.io/instance: {{ $.ctx.Release.Name }}
    matchExpressions:
      - key: app.kubernetes.io/name
        operator: In
        values:
          {{- range . }}
          - {{ . | quote }}
          {{- end }}
{{- end }}
{{- end }}

{{/* "true" when the worker's egress policy renders, and when its public rule does. */}}
{{- define "elitea.netpol.workerEgress" -}}
{{- if and .Values.networkPolicies.enabled .Values.worker.enabled .Values.worker.networkPolicy.egressEnabled -}}true{{- end -}}
{{- end }}
{{- define "elitea.netpol.workerPublicEgress" -}}
{{- $w := .Values.worker.networkPolicy -}}
{{- if and (include "elitea.netpol.workerEgress" .) (eq (toString $w.mode) "public") $w.allowPublicEgress -}}true{{- end -}}
{{- end }}
{{/* "true" when the LLM gateway's egress policy renders, and when its public rule does. */}}
{{- define "elitea.netpol.gatewayEgress" -}}
{{- if and .Values.networkPolicies.enabled .Values.llmGateway.enabled .Values.llmGateway.networkPolicy.enabled -}}true{{- end -}}
{{- end }}
{{- define "elitea.netpol.gatewayPublicEgress" -}}
{{- if and (include "elitea.netpol.gatewayEgress" .) .Values.llmGateway.networkPolicy.allowPublicEgress -}}true{{- end -}}
{{- end }}

{{/*
  elitea.netpol.permitEgress — the worker's private permit
  (main.env.ELITEA_WORKER_EGRESS_PRIVATE_PERMIT, #2950) as egress rules, so a
  destination the operator permits in the chart stays reachable: a CIDR block
  admits that block on every port (the permit pins none), an IP literal its
  /32 or /128, on its port when the entry names one. Link-local entries are
  skipped: the worker never admits them. A host name cannot become an ipBlock;
  those need worker.networkPolicy.privateEgress (guards.yaml refuses a host
  name entry with no privateEgress or extraEgress at all).
  Output: a JSON list of {cidr, port} (port "" = any).
*/}}
{{- define "elitea.netpol.permitEntries" -}}
{{- $out := list -}}
{{- $raw := get (.Values.main.env | default dict) "ELITEA_WORKER_EGRESS_PRIVATE_PERMIT" | default "" | toString -}}
{{- range $e := splitList "," $raw -}}
{{- $e = trim $e -}}
{{- if $e -}}
{{- $cidr := "" -}}{{- $port := "" -}}
{{- if regexMatch "^([0-9]{1,3}\\.){3}[0-9]{1,3}/[0-9]{1,2}$|^[0-9A-Fa-f:]+/[0-9]{1,3}$" $e -}}
{{- $cidr = $e -}}
{{- else if regexMatch "^([0-9]{1,3}\\.){3}[0-9]{1,3}(:[0-9]+)?$" $e -}}
{{- $parts := splitList ":" $e -}}
{{- $cidr = printf "%s/32" (first $parts) -}}
{{- if gt (len $parts) 1 }}{{ $port = last $parts }}{{ end -}}
{{- else if regexMatch "^\\[[0-9A-Fa-f:]+\\](:[0-9]+)?$" $e -}}
{{- $cidr = printf "%s/128" (regexReplaceAll "^\\[([0-9A-Fa-f:]+)\\].*$" $e "${1}") -}}
{{- $port = regexReplaceAll "^\\[[0-9A-Fa-f:]+\\]:?" $e "" -}}
{{- else if regexMatch "^([0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4}$" $e -}}
{{- $cidr = printf "%s/128" $e -}}
{{- end -}}
{{- if $cidr -}}
{{- $out = append $out (dict "cidr" $cidr "port" $port) -}}
{{- else -}}
{{- $out = append $out (dict "host" $e) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end }}

{{/*
  elitea.netpol.privateCidrRefusal — why a block cannot be a private egress rule
  of the worker, or "" when it can. Shared by worker.networkPolicy.privateEgress
  and the CIDR/IP entries of ELITEA_WORKER_EGRESS_PRIVATE_PERMIT, so the two
  ways of naming an on-premises host obey one rule: never the whole address
  space or a block wide enough to be most of it (IPv4 shorter than /8, IPv6
  shorter than /16), and never a block that overlaps loopback or link-local
  (the cloud metadata endpoints). Input: the CIDR string.
*/}}
{{- define "elitea.netpol.privateCidrRefusal" -}}
{{- $c := toString . -}}
{{- if regexMatch "^([0-9]{1,3}\\.){3}[0-9]{1,3}/[0-9]{1,2}$" $c -}}
{{- $parts := splitList "/" $c -}}
{{- $prefix := atoi (last $parts) -}}
{{- $o := splitList "." (first $parts) -}}
{{- if or (lt $prefix 8) (gt $prefix 32) -}}
whole-space
{{- else -}}
{{- $a := atoi (index $o 0) -}}{{- $b := atoi (index $o 1) -}}
{{- /* Overlap with 169.254.0.0/16: the second octet under the block's mask. */ -}}
{{- $step := 1 -}}
{{- if lt $prefix 16 }}{{ $step = index (list 256 128 64 32 16 8 4 2) (sub $prefix 8) }}{{ end -}}
{{- if eq $a 127 -}}
loopback
{{- else if and (eq $a 169) (eq (div $b $step) (div 254 $step)) -}}
link-local
{{- end -}}
{{- end -}}
{{- else if regexMatch "^[0-9A-Fa-f:]+/[0-9]{1,3}$" $c -}}
{{- $l := lower $c -}}
{{- $prefix := atoi (last (splitList "/" $l)) -}}
{{- if or (lt $prefix 16) (gt $prefix 128) -}}
whole-space
{{- else if or (hasPrefix "fe8" $l) (hasPrefix "fe9" $l) (hasPrefix "fea" $l) (hasPrefix "feb" $l) (hasPrefix "::1/" $l) -}}
link-local
{{- end -}}
{{- end -}}
{{- end }}

{{/* The sandbox supervisor Deployment's pods (no name label; templates/sandbox/supervisor.yaml). */}}
{{- define "elitea-sandbox-supervisor.selectorLabels" -}}
app.kubernetes.io/component: sandbox-supervisor
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
  elitea.netpol.providerIngress — the ingress NetworkPolicy of a sub-application
  provider (templates/deepwiki/networkpolicy.yaml, templates/inventory/
  networkpolicy.yaml), which is the same policy for both: the one gRPC port
  admits elitea-main (and the probe peers). One helper, so the two providers
  cannot drift apart.

  Takes a dict:
    ctx            the chart context
    name           the policy's object name (<fullname>-netpol)
    labels         the provider's labels block
    selectorLabels the provider's pod selector block (the Deployment's)
    title          the provider's name in the comment (DeepWiki, Inventory)
    key            the provider's values key (deepwiki, inventory)
    grpcPort       the provider's gRPC port (<key>.platformGrpc.port)
    adminPort      the provider's admin port (<key>.admin.port)
*/}}
{{- define "elitea.netpol.providerIngress" -}}
{{- $np := .ctx.Values.networkPolicies }}
# Ingress to the {{ .title }} provider pods. elitea-main is the only door
# (ADR-0022 decision 5): the provider trusts a client certificate from the
# internal CA, and this policy keeps every other pod from even attempting the
# handshake.
#
#   {{ .grpcPort }} (platform-grpc)  the provider SPI and the platform service,
#                       gRPC over mutual TLS (#1359): elitea-main, and only
#                       the identities in {{ .key }}.platformGrpc.clients may
#                       call it.
#   {{ .adminPort }} (admin)          /healthz, /readyz and /metrics, plain HTTP:
#                       networkPolicies.probeFrom and the Prometheus in
#                       networkPolicies.metricsFrom (the in-flight gauge a KEDA
#                       prometheus trigger reads).
#
# The engine sidecar has no port: it answers on a Unix socket in the pod.
# Kubelet probes are not subject to the policy on the CNIs this chart is tested
# with; networkPolicies.probeFrom is for those that apply it to them. Egress is
# not restricted here.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ .name }}
  namespace: {{ .ctx.Release.Namespace }}
  labels:
    {{- .labels | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- .selectorLabels | nindent 6 }}
  policyTypes:
    - Ingress
  ingress:
    - ports:
        - port: {{ .grpcPort }}
          protocol: TCP
      from:
        {{- include "elitea.netpol.mainPeer" .ctx | nindent 8 }}
        {{- with $np.probeFrom }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    {{- $adminFrom := concat ($np.probeFrom | default list) ($np.metricsFrom | default list) }}
    {{- with $adminFrom }}
    - ports:
        - port: {{ $.adminPort }}
          protocol: TCP
      from:
        {{- toYaml . | nindent 8 }}
    {{- end }}
{{- end }}

{{- /*
The cloud executor's `workspace` supervisor profile JSON (ADR-0034), from
sandboxKubernetes.workspaceRuns. Paths name files the material Secret
carries; sandbox/workspace-profile.yaml validates the values first.
*/ -}}
{{- define "elitea-sandbox.workspaceProfile" -}}
{{- $s := .Values.sandboxKubernetes -}}
{{- $w := $s.workspaceRuns -}}
{{- $p := $w.profile -}}
{{- $dir := "/run/elitea-sandbox" -}}
{{- $backend := dict "kind" "kubernetes" "namespace" $s.executionNamespace "runtime_class" $s.runtimeClass.name "node_selector" $w.nodeSelector -}}
{{- with $w.tolerations }}{{ $_ := set $backend "tolerations" . }}{{ end -}}
{{- dict
  "revision" 1 "purpose" "workspace"
  "listen_address" (printf "0.0.0.0:%v" $p.port)
  "owner" $p.owner "audience" $p.audience
  "ca_path" (printf "%s/%s" $dir $p.material.ca)
  "certificate_path" (printf "%s/%s" $dir $p.material.certificate)
  "private_key_path" (printf "%s/%s" $dir $p.material.privateKey)
  "verification_keyring_path" (printf "%s/%s" $dir $p.material.keyring)
  "image" $p.image
  "concurrency" (int64 $p.concurrency)
  "max_workspace_bytes" (int64 $p.maxWorkspaceBytes)
  "max_cpu_millis" (int64 $p.maxCpuMillis)
  "max_memory_bytes" (int64 $p.maxMemoryBytes)
  "max_wall_seconds" (int64 $p.maxWallSeconds)
  "backend" $backend | toJson -}}
{{- end -}}

{{- /*
The cloud browser's `browser` supervisor profile JSON (ADR-0049), from
sandboxKubernetes.browserSessions. Paths name files the material Secret
carries; sandbox/browser-profile.yaml validates the values first.
*/ -}}
{{- define "elitea-sandbox.browserProfile" -}}
{{- $s := .Values.sandboxKubernetes -}}
{{- $b := $s.browserSessions -}}
{{- $p := $b.profile -}}
{{- $dir := "/run/elitea-sandbox" -}}
{{- $backend := dict "kind" "kubernetes" "namespace" $s.executionNamespace "runtime_class" $s.runtimeClass.name "node_selector" $b.nodeSelector -}}
{{- with $b.tolerations }}{{ $_ := set $backend "tolerations" . }}{{ end -}}
{{- $profile := dict
  "revision" 1 "purpose" "browser"
  "listen_address" (printf "0.0.0.0:%v" $p.port)
  "owner" $p.owner "audience" $p.audience
  "ca_path" (printf "%s/%s" $dir $p.material.ca)
  "certificate_path" (printf "%s/%s" $dir $p.material.certificate)
  "private_key_path" (printf "%s/%s" $dir $p.material.privateKey)
  "verification_keyring_path" (printf "%s/%s" $dir $p.material.keyring)
  "image" $p.image
  "concurrency" (int64 $p.concurrency)
  "sessions_per_run" (int64 $p.sessionsPerRun)
  "max_pages" (int64 $p.maxPages)
  "max_wall_seconds" (int64 $p.maxWallSeconds)
  "engine_sandbox" (eq (toString $p.engineSandbox) "true")
  "full" (dict "max_cpu_millis" (int64 $p.full.maxCpuMillis) "max_memory_bytes" (int64 $p.full.maxMemoryBytes))
  "backend" $backend -}}
{{- with $p.lite }}{{ $_ := set $profile "lite" (dict "max_cpu_millis" (int64 .maxCpuMillis) "max_memory_bytes" (int64 .maxMemoryBytes)) }}{{ $_ := set $profile "lite_image" (toString $p.liteImage) }}{{ end -}}
{{- $profile | toJson -}}
{{- end -}}
