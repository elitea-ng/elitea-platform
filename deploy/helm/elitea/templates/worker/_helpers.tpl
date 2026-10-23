{{/*
The pod name label. Its DEFAULT value stays "elitea-worker-python" although
only the Rust worker exists: app.kubernetes.io/name is in the Deployment's
selector and immutable after install (renaming it turns every upgrade into an
unrecoverable "field is immutable" error), and the NATS NetworkPolicy
admits the worker pods by this exact name (nats.networkPolicy.clients). The value is historical, the image is not.
*/}}
{{- define "elitea-worker.name" -}}
{{- default "elitea-worker-python" .Values.worker.nameOverride | trunc 63 | trimSuffix "-" }}
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
{{- define "elitea-worker.fullname" -}}
{{- default "elitea-worker" .Values.worker.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-worker.labels" -}}
helm.sh/chart: {{ printf "%s-%s" "elitea-worker-python" .Chart.Version | replace "+" "_" }}
{{ include "elitea-worker.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "elitea-worker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-worker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The material file names, in one place. The init container copies exactly these
and deletes anything else in the destination, so a Secret that carries a key
this list does not name is a provisioning mistake that fails loudly rather than
a file the worker silently ignores.
*/}}
{{- define "elitea-worker.materialFiles" -}}
runtime-ca.crt agent-worker-client.crt agent-worker-client.key command-signing-keyring.json worker-output-spool-key agent-checkpoint-connection{{ if (.Values.main.runtime.rustCompiledSnapshots | default dict).enabled }} rust-compiled-profiles.json{{ end }}
{{- end }}

{{/*
The worker image repository: the Rust worker, the only one. `worker.image.repository`
wins when set, which is how an image is pinned by digest or pulled from a mirror.
*/}}
{{- define "elitea-worker.imageRepository" -}}
{{- .Values.worker.image.repository | default "ghcr.io/elitea-ng/elitea-worker-rust" -}}
{{- end }}
