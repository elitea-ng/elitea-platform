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

{{/* The worker Deployment's pods. The component label is what separates them from the platform edge, which shares name and instance. */}}
{{- define "elitea.netpol.workerPeer" -}}
{{- include "elitea.netpol.peer" (dict "ctx" . "labels" (printf "%s\napp.kubernetes.io/component: worker" (include "elitea-worker.selectorLabels" .))) -}}
{{- end }}

{{- define "elitea.netpol.edgePeer" -}}
{{- include "elitea.netpol.peer" (dict "ctx" . "labels" (printf "%s\napp.kubernetes.io/component: platform-edge" (include "elitea-worker.selectorLabels" .))) -}}
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
  networkpolicy.yaml), which is the same policy for both: the SPI port admits
  elitea-main (and the probe peers), and, when the platform gRPC service is on,
  its port admits elitea-main alone. One helper, so the two providers cannot
  drift apart.

  Takes a dict:
    ctx            the chart context
    name           the policy's object name (<fullname>-netpol)
    labels         the provider's labels block
    selectorLabels the provider's pod selector block (the Deployment's)
    title          the provider's name in the comment (DeepWiki, Inventory)
    key            the provider's values key (deepwiki, inventory)
    grpcPort       the platform gRPC port (<key>.platformGrpc.port)
    platformGrpc   non-empty when the platform gRPC service is on
                   (elitea-<key>.platformGrpc)
*/}}
{{- define "elitea.netpol.providerIngress" -}}
{{- $np := .ctx.Values.networkPolicies }}
# Ingress to the {{ .title }} provider pods. elitea-main is the only door
# (ADR-0022 decision 5): the provider trusts a client certificate from the
# internal CA, and this policy keeps every other pod from even attempting the
# handshake.
#
#   8080 (http)         the SPI, over mutual TLS: elitea-main's facade.
{{- if .platformGrpc }}
#   {{ .grpcPort }} (platform-grpc)  the platform service
#                       ({{ .key }}.platformGrpc): elitea-main, and only the
#                       identities in platformGrpc.clients may call it.
{{- end }}
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
        - port: 8080
          protocol: TCP
      from:
        {{- include "elitea.netpol.mainPeer" .ctx | nindent 8 }}
        {{- with $np.probeFrom }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    {{- if .platformGrpc }}
    - ports:
        - port: {{ .grpcPort }}
          protocol: TCP
      from:
        {{- include "elitea.netpol.mainPeer" .ctx | nindent 8 }}
    {{- end }}
{{- end }}
