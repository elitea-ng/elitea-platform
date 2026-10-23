{{/*
The upstream subchart's names, computed the way it computes them (nats 1.3.9
templates/_helpers.tpl). The server certificate's SANs must name the Service
and the headless Service exactly, or every client handshake fails with an
error that reads like a trust problem.
*/}}
{{- define "elitea-nats.fullname" -}}
{{- $sub := .Values.nats | default dict -}}
{{- if $sub.fullnameOverride -}}
{{- $sub.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "nats" $sub.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "elitea-nats.name" -}}
{{- default "nats" (.Values.nats | default dict).nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "elitea-nats.namespace" -}}
{{- default .Release.Namespace (.Values.nats | default dict).namespaceOverride -}}
{{- end }}

{{- define "elitea-nats.serviceName" -}}
{{- (.Values.nats.service | default dict).name | default (include "elitea-nats.fullname" .) -}}
{{- end }}

{{- define "elitea-nats.headlessServiceName" -}}
{{- (.Values.nats.headlessService | default dict).name | default (printf "%s-headless" (include "elitea-nats.fullname" .)) -}}
{{- end }}

{{/* The NATS pods, as the upstream chart labels them. */}}
{{- define "elitea-nats.serverSelectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-nats.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: nats
{{- end }}

{{- define "elitea-nats.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: elitea-nats
{{- end }}

{{/*
The issuer that signs NATS certificates: the chart's own CA Issuer, or the
operator's when nats.security.ca.create is false.
*/}}
{{- define "elitea-nats.routeIssuerRef" -}}
{{- if .Values.nats.security.ca.create -}}
name: {{ .Values.nats.security.routeCA.name }}
kind: Issuer
group: cert-manager.io
{{- else -}}
name: {{ required "nats.security.ca.create is false and the cluster is enabled, so nats.security.routeIssuerRef.name must name the issuer of a dedicated NATS ROUTE CA" .Values.nats.security.routeIssuerRef.name }}
kind: {{ .Values.nats.security.routeIssuerRef.kind | default "ClusterIssuer" }}
group: cert-manager.io
{{- end -}}
{{- end }}

{{- define "elitea-nats.issuerRef" -}}
{{- if .Values.nats.security.ca.create -}}
name: {{ .Values.nats.security.ca.name }}
kind: Issuer
group: cert-manager.io
{{- else -}}
name: {{ required "nats.security.ca.create is false, so nats.security.issuerRef.name must name the issuer of a dedicated NATS CA" .Values.nats.security.issuerRef.name }}
kind: {{ .Values.nats.security.issuerRef.kind | default "ClusterIssuer" }}
group: cert-manager.io
{{- end -}}
{{- end }}

{{/*
Argo CD sync wave of everything the NATS server needs (#1076 material here,
and the upstream subchart's resources through their `merge` values), and of
the bootstrap Job (natsBootstrap). Argo CD applies a wave and waits for it to
be healthy before the next, so the server, its certificates and the JetStream
assets all exist before the workloads (wave 0) start — they bind the assets
and exit at boot without them. Earlier than elitea-vector's -5, which does not
depend on NATS. A plain `helm install` has no waves: there the bootstrap is a
post-install hook and the workloads restart until it has run.
*/}}
{{- define "elitea-nats.syncWave" -}}
argocd.argoproj.io/sync-wave: "-10"
{{- end }}
