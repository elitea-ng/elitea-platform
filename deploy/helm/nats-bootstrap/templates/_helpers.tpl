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

{{/*
nats-bootstrap.int renders one numeric value as a quoted integer, or FAILS the
render. Called as (list "path.in.values" .Values.path.in.values).

`int64` alone turns anything it cannot parse ("40k", "1GiB", a typo) into 0,
and natscli reads 0 as "server default": MaxAckPending back at 1000, MaxBytes
and MaxMsgs unbounded. A YAML integer of 7+ digits arrives as a float64
(1.073741824e+09), and `quote` alone renders it in scientific notation, which
natscli refuses. So accept an integer string, an int, or a WHOLE-number float,
and refuse everything else, naming the value.
*/}}
{{- define "nats-bootstrap.int" -}}
{{- $name := index . 0 -}}
{{- $v := index . 1 -}}
{{- $s := toString $v -}}
{{- if kindIs "float64" $v -}}
{{- $s = printf "%.0f" $v -}}
{{- if ne (float64 $s) $v -}}
{{- fail (printf "nats-bootstrap: %s must be a whole number, and it is %v" $name $v) -}}
{{- end -}}
{{- end -}}
{{- if not (regexMatch "^-?[0-9]+$" $s) -}}
{{- fail (printf "nats-bootstrap: %s must be an integer (no unit suffix), and it is %q" $name $s) -}}
{{- end -}}
{{- $s | quote -}}
{{- end -}}
