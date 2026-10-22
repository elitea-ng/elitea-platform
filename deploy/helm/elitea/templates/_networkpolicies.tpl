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
