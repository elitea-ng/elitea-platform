{{- define "otel-collector.name" -}}
{{- default "otel-collector" .Values.otelCollector.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Release-INDEPENDENT on purpose. These names are certificate material: the
runtime mTLS server certificates carry DNS SANs for them, the worker dials
control/output/content/platform on `elitea-main` (platform_origin is
`https://elitea-main:9446`). Helm's usual
"<release>-<chart>" would rename them per release and fail every handshake with
an error that reads like a trust problem, nowhere near the rename.

There is exactly one of each per release, so there is nothing for a release
prefix to disambiguate. Override only alongside re-minting the certificate.
*/}}
{{- define "otel-collector.fullname" -}}
{{- default "elitea-otel-collector" .Values.otelCollector.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "otel-collector.labels" -}}
helm.sh/chart: {{ include "otel-collector.name" . }}-{{ .Chart.Version }}
{{ include "otel-collector.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "otel-collector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "otel-collector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "otel-collector.serviceAccountName" -}}
{{- if .Values.otelCollector.serviceAccount.create }}
{{- default (include "otel-collector.fullname" .) .Values.otelCollector.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.otelCollector.serviceAccount.name }}
{{- end }}
{{- end }}
