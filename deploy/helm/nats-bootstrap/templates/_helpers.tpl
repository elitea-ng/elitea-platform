{{- define "nats-bootstrap.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nats-bootstrap.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "nats-bootstrap.labels" -}}
helm.sh/chart: {{ include "nats-bootstrap.name" . }}-{{ .Chart.Version }}
app.kubernetes.io/name: {{ include "nats-bootstrap.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
nats-bootstrap.secretName — the Secret holding one account's bootstrap
certificate: tls.secretNames.<account>, or the one templates/certificate.yaml
writes.
*/}}
{{- define "nats-bootstrap.secretName" -}}
{{- $names := .ctx.Values.tls.secretNames | default dict -}}
{{- get $names .account | default (printf "%s-%s-nats-client-tls" (include "nats-bootstrap.fullname" .ctx) .account) -}}
{{- end }}
