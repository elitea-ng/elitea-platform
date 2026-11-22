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
nats-bootstrap.issuerLabels — the labels of the issuer's ServiceAccount, Role
and RoleBinding (issuer.yaml). No pod carries them: the issuer is the bootstrap
Job's init container (job.yaml).
*/}}
{{- define "nats-bootstrap.issuerLabels" -}}
helm.sh/chart: {{ include "nats-bootstrap.name" . }}-{{ .Chart.Version }}
app.kubernetes.io/name: nats-bootstrap-issuer
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
nats-bootstrap.certificateName — one account's bootstrap Certificate. Called
as (dict "account" <account> "ctx" $).
*/}}
{{- define "nats-bootstrap.certificateName" -}}
{{- printf "%s-%s-nats-client" (include "nats-bootstrap.fullname" .ctx) .account -}}
{{- end }}

{{/*
nats-bootstrap.certificate — the account's bootstrap identity (#1076): a client
certificate from the NATS CA whose URI SAN the server maps to the one user in
that account that may create streams and consumers. Called as
(dict "account" <account> "ctx" $).

The chart does not apply it. It is rendered as JSON into the bootstrap
ConfigMap, and the Job's init container creates it, owned by the Job.
files/nats-bootstrap-issue.sh says why: Argo CD failed the sync on a hook
Certificate that was briefly Ready=False while cert-manager issued it. It
lives as long as the Job, and the certificate itself one hour
(natsBootstrap.tls.certificate.duration).
*/}}
{{- define "nats-bootstrap.certificate" -}}
{{- $ := .ctx -}}
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ include "nats-bootstrap.certificateName" . }}
  # Not nats-bootstrap.labels: app.kubernetes.io/instance is the label Argo
  # CD's label tracking reads, and an object it tracks but did not render is
  # one it reports OutOfSync and may prune in the middle of the run. Neither
  # Helm nor Argo CD manages it; the Job does.
  labels:
    app.kubernetes.io/name: nats-bootstrap
    app.kubernetes.io/component: bootstrap-identity
    app.kubernetes.io/managed-by: {{ include "nats-bootstrap.fullname" $ }}
spec:
  secretName: {{ include "nats-bootstrap.secretName" . }}
  duration: {{ $.Values.natsBootstrap.tls.certificate.duration }}
  renewBefore: {{ $.Values.natsBootstrap.tls.certificate.renewBefore }}
  isCA: false
  usages:
    - client auth
  commonName: elitea-nats-bootstrap-{{ .account }}
  uris:
    - spiffe://{{ include "nats-bootstrap.trustDomain" $ }}/nats/elitea-nats-bootstrap-{{ .account }}
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    {{- include "nats-bootstrap.issuerRef" $ | nindent 4 }}
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
certificate: natsBootstrap.tls.secretNames.<account>, or the one the Job's
own Certificate (nats-bootstrap.certificate) writes.
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
