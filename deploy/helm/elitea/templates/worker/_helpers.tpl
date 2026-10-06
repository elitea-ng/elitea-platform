{{- define "elitea-worker-python.name" -}}
{{- default "elitea-worker-python" .Values.worker.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Release-INDEPENDENT on purpose. These names are certificate material: the
runtime mTLS server certificates carry DNS SANs for them, the worker dials
control/output/content on `elitea-main`, and platform_origin is pinned to a
certificate whose only SAN is `elitea-platform-edge`. Helm's usual
"<release>-<chart>" would rename them per release and fail every handshake with
an error that reads like a trust problem, nowhere near the rename.

There is exactly one of each per release, so there is nothing for a release
prefix to disambiguate. Override only alongside re-minting the certificate.
*/}}
{{- define "elitea-worker-python.fullname" -}}
{{- default "elitea-worker" .Values.worker.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-worker-python.labels" -}}
helm.sh/chart: {{ printf "%s-%s" "elitea-worker-python" .Chart.Version | replace "+" "_" }}
{{ include "elitea-worker-python.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "elitea-worker-python.selectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-worker-python.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The material file names, in one place. The init container copies exactly these
and deletes anything else in the destination, so a Secret that carries a key
this list does not name is a provisioning mistake that fails loudly rather than
a file the worker silently ignores.
*/}}
{{- define "elitea-worker-python.materialFiles" -}}
runtime-ca.crt agent-worker-client.crt agent-worker-client.key command-signing-keyring.json worker-output-spool-key agent-checkpoint-connection{{ if (.Values.main.runtime.rustCompiledSnapshots | default dict).enabled }} rust-compiled-profiles.json{{ end }}
{{- end }}

{{/*
Which worker image this release runs.

`worker.image.repository` wins when set, and that is the documented way to pin
either implementation — including pinning back to the Python worker without
touching `worker.implementation`. Empty (the default) derives the repository
from `worker.implementation`.

The helper names are NOT renamed to match. `elitea-worker-python.name` feeds
app.kubernetes.io/name, which is in the Deployment's selector and immutable
after install: renaming it turns every upgrade into an unrecoverable "field is
immutable" error. The names are historical, the images are not.
*/}}
{{- define "elitea-worker.imageRepository" -}}
{{- if .Values.worker.image.repository -}}
{{- .Values.worker.image.repository -}}
{{- else if eq .Values.worker.implementation "rust" -}}
ghcr.io/elitea-ng/elitea-worker-rust
{{- else -}}
ghcr.io/elitea-ng/elitea-worker-python
{{- end -}}
{{- end }}

{{/*
True when this release runs the native Rust worker. One place, because four
templates branch on it and a values path repeated four times is four chances
to typo a key that Helm resolves to empty and reads as `false`.
*/}}
{{- define "elitea-worker.isRust" -}}
{{- eq .Values.worker.implementation "rust" -}}
{{- end }}
