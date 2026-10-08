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
{{- include "elitea.netpol.peer" (dict "ctx" . "labels" (printf "%s\napp.kubernetes.io/component: worker" (include "elitea-worker-python.selectorLabels" .))) -}}
{{- end }}

{{- define "elitea.netpol.edgePeer" -}}
{{- include "elitea.netpol.peer" (dict "ctx" . "labels" (printf "%s\napp.kubernetes.io/component: platform-edge" (include "elitea-worker-python.selectorLabels" .))) -}}
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
