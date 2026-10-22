{{/*
The bootstrap Job's names. Release-INDEPENDENT, like every other component
here: the pod label nats-bootstrap is what nats.networkPolicy.clients admits
to 4222, and elitea-nats-bootstrap is the name the separate nats-bootstrap
chart used (installed as release `elitea-nats-bootstrap`), so the certificate
Secrets keep their names across the move into this chart.
*/}}
{{- define "nats-bootstrap.name" -}}
nats-bootstrap
{{- end }}

{{- define "nats-bootstrap.fullname" -}}
elitea-nats-bootstrap
{{- end }}

{{- define "nats-bootstrap.labels" -}}
helm.sh/chart: {{ include "nats-bootstrap.name" . }}-{{ .Chart.Version }}
app.kubernetes.io/name: {{ include "nats-bootstrap.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
nats-bootstrap.natsUrl — natsBootstrap.natsUrl, or the URL every component
dials (natsClient), so the Job and the services cannot name different servers.
*/}}
{{- define "nats-bootstrap.natsUrl" -}}
{{- .Values.natsBootstrap.natsUrl | default (include "elitea.natsUrl" .) -}}
{{- end }}

{{/*
nats-bootstrap.issuerRef — the issuer of the bootstrap identities:
natsBootstrap.tls.certificate.issuerRef, or the one natsClient's
certificates come from.
*/}}
{{- define "nats-bootstrap.issuerRef" -}}
{{- $own := .Values.natsBootstrap.tls.certificate.issuerRef | default dict -}}
{{- $client := .Values.natsClient.tls.issuerRef | default dict -}}
name: {{ $own.name | default $client.name | required "natsBootstrap.tls.certificate.issuerRef.name (or natsClient.tls.issuerRef.name) names the NATS CA issuer" }}
kind: {{ $own.kind | default $client.kind | default "Issuer" }}
group: cert-manager.io
{{- end }}

{{- define "nats-bootstrap.trustDomain" -}}
{{- .Values.natsBootstrap.tls.certificate.trustDomain | default .Values.natsClient.tls.trustDomain -}}
{{- end }}

{{/*
nats-bootstrap.secretName — the Secret holding one account's bootstrap
certificate: natsBootstrap.tls.secretNames.<account>, or the one
certificate.yaml writes.
*/}}
{{- define "nats-bootstrap.secretName" -}}
{{- $names := .ctx.Values.natsBootstrap.tls.secretNames | default dict -}}
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
{{- fail (printf "natsBootstrap.%s must be a whole number, and it is %v" $name $v) -}}
{{- end -}}
{{- end -}}
{{- if not (regexMatch "^-?[0-9]+$" $s) -}}
{{- fail (printf "natsBootstrap.%s must be an integer (no unit suffix), and it is %q" $name $s) -}}
{{- end -}}
{{- $s | quote -}}
{{- end -}}
