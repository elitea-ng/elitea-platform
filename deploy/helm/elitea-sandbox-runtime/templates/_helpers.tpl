{{- define "elitea-sandbox-runtime.labels" -}}
app.kubernetes.io/name: elitea-sandbox-runtime
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: installer
{{- end -}}
{{- define "elitea-sandbox-runtime.selectorLabels" -}}
app.kubernetes.io/name: elitea-sandbox-runtime
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
