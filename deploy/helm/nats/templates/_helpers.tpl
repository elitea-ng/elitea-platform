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
operator's when security.ca.create is false.
*/}}
{{- define "elitea-nats.issuerRef" -}}
{{- if .Values.security.ca.create -}}
name: {{ .Values.security.ca.name }}
kind: Issuer
group: cert-manager.io
{{- else -}}
name: {{ required "security.ca.create is false, so security.issuerRef.name must name the issuer of a dedicated NATS CA" .Values.security.issuerRef.name }}
kind: {{ .Values.security.issuerRef.kind | default "ClusterIssuer" }}
group: cert-manager.io
{{- end -}}
{{- end }}
