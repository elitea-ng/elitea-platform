{{- define "elitea-scheduler.name" -}}
{{- default "elitea-scheduler" .Values.scheduler.nameOverride | trunc 63 | trimSuffix "-" }}
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
{{- define "elitea-scheduler.fullname" -}}
{{- default "elitea-scheduler" .Values.scheduler.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-scheduler.labels" -}}
helm.sh/chart: {{ include "elitea-scheduler.name" . }}-{{ .Chart.Version }}
{{ include "elitea-scheduler.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "elitea-scheduler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-scheduler.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "elitea-scheduler.serviceAccountName" -}}
{{- if .Values.scheduler.serviceAccount.create }}
{{- default (include "elitea-scheduler.fullname" .) .Values.scheduler.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.scheduler.serviceAccount.name }}
{{- end }}
{{- end }}
