{{/*
elitea-sandbox-runtime.fullname — the prefix of the DaemonSet and ConfigMap
names: fullnameOverride, else the release name. The elitea chart, which
carries this chart as its `sandboxRuntime` dependency, sets the override so
its objects are not named after the platform release.
*/}}
{{- define "elitea-sandbox-runtime.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 52 | trimSuffix "-" -}}
{{- end -}}
{{/*
elitea-sandbox-runtime.namespace — where the DaemonSet and ConfigMap go:
`namespace`, else the release's. The installer runs privileged, so this
namespace needs the privileged Pod Security level; as a dependency of the
elitea chart that is a namespace of its own, not the platform's.
*/}}
{{- define "elitea-sandbox-runtime.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end -}}
{{- define "elitea-sandbox-runtime.labels" -}}
app.kubernetes.io/name: elitea-sandbox-runtime
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: installer
{{- end -}}
{{- define "elitea-sandbox-runtime.selectorLabels" -}}
app.kubernetes.io/name: elitea-sandbox-runtime
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
