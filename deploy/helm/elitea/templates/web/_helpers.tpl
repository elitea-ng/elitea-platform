{{- define "elitea-web.name" -}}
{{- default "elitea-web" .Values.web.nameOverride | trunc 63 | trimSuffix "-" }}
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
{{- define "elitea-web.fullname" -}}
{{- default "elitea-web" .Values.web.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-web.labels" -}}
helm.sh/chart: {{ include "elitea-web.name" . }}-{{ .Chart.Version }}
{{ include "elitea-web.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "elitea-web.selectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-web.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "elitea-web.serviceAccountName" -}}
{{- if .Values.web.serviceAccount.create }}
{{- default (include "elitea-web.fullname" .) .Values.web.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.web.serviceAccount.name }}
{{- end }}
{{- end }}
