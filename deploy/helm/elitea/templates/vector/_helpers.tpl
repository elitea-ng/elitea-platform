{{/*
elitea-vector (ADR-0031). Release-INDEPENDENT names, for the providers'
reason: the server certificate's DNS SANs name the Service, and the client
certificate's one DNS SAN is the identity elitea-main admits.
*/}}
{{- define "elitea-vector.fullname" -}}elitea-vector{{- end }}
{{- define "elitea-vector.qdrantName" -}}elitea-qdrant{{- end }}

{{- define "elitea-vector.selectorLabels" -}}
app.kubernetes.io/name: elitea-vector
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "elitea-vector.labels" -}}
{{ include "elitea-vector.selectorLabels" . }}
app.kubernetes.io/component: vector
app.kubernetes.io/part-of: elitea
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-vector.qdrantSelectorLabels" -}}
app.kubernetes.io/name: elitea-qdrant
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "elitea-vector.qdrantLabels" -}}
{{ include "elitea-vector.qdrantSelectorLabels" . }}
app.kubernetes.io/component: vector-store
app.kubernetes.io/part-of: elitea
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-vector.serviceAccountName" -}}
{{- if .Values.vector.serviceAccount.create -}}
{{- default (include "elitea-vector.fullname" .) .Values.vector.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.vector.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/* The identity elitea-main admits on its control listener. */}}
{{- define "elitea-vector.clientIdentity" -}}
{{- printf "dns:%s" (lower .Values.vector.mtls.clientDnsName) -}}
{{- end }}

{{/* Qdrant's gRPC URL, as elitea-vector dials it. */}}
{{- define "elitea-vector.qdrantUrl" -}}
{{- if eq (.Values.vector.qdrant.mode | toString) "external" -}}
{{- .Values.vector.qdrant.external.url -}}
{{- else -}}
{{- printf "http://%s.%s.svc:6334" (include "elitea-vector.qdrantName" .) .Release.Namespace -}}
{{- end -}}
{{- end }}

{{/* elitea-main's control listener. */}}
{{- define "elitea-vector.introspectionUrl" -}}
{{- if .Values.vector.introspection.url -}}
{{- .Values.vector.introspection.url -}}
{{- else -}}
{{- printf "https://%s:%s" (include "elitea-main.fullname" .) (include "elitea-main.runtimePort" .Values.main.runtime.listeners.controlAddress) -}}
{{- end -}}
{{- end }}

{{/*
elitea-vector.validateGuards — each refused combination installs cleanly and
then refuses every request, which looks like a healthy service.
*/}}
{{- define "elitea-vector.validateGuards" -}}
{{- if not .Values.main.runtime.enabled -}}
{{- fail "vector.enabled needs main.runtime.enabled: elitea-vector verifies every token through elitea-main's TokenIntrospectionService on the runtime control listener, which exists only with the runtime plane. Without it every request is refused (fail closed). Turn the runtime on, or leave vector off." -}}
{{- end -}}
{{- if not .Values.vector.qdrant.apiKeySecret.name -}}
{{- fail "vector.qdrant.apiKeySecret.name is empty. elitea-vector is the only holder of the Qdrant API key, and the internal StatefulSet reads the same Secret; without a key Qdrant serves every client on the network. Create a Secret with the key and name it here." -}}
{{- end -}}
{{- $mode := .Values.vector.qdrant.mode | toString -}}
{{- if not (has $mode (list "internal" "external")) -}}
{{- fail (printf "vector.qdrant.mode must be internal or external, got %q" $mode) -}}
{{- end -}}
{{- if and (eq $mode "external") (not .Values.vector.qdrant.external.url) -}}
{{- fail "vector.qdrant.mode is external, so vector.qdrant.external.url (the Qdrant gRPC URL, port 6334 by default) must be set." -}}
{{- end -}}
{{- if and (eq $mode "internal") (gt (int .Values.vector.collection.replicationFactor) (int .Values.vector.qdrant.internal.replicas)) -}}
{{- fail (printf "vector.collection.replicationFactor is %d but vector.qdrant.internal.replicas is %d: Qdrant cannot place more replicas than nodes, so every collection would be created under-replicated." (int .Values.vector.collection.replicationFactor) (int .Values.vector.qdrant.internal.replicas)) -}}
{{- end -}}
{{- if lt (int .Values.vector.autoscaling.minReplicas) 2 -}}
{{- fail "vector.autoscaling.minReplicas must be at least 2 (ADR-0031 decision 8): elitea-vector is on the path of every vector read and write." -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$" (.Values.vector.mtls.clientDnsName | toString)) -}}
{{- fail (printf "vector.mtls.clientDnsName must be one lower-case DNS name, got %q: it is the client certificate's only SAN and elitea-main admits it as dns:<name>." .Values.vector.mtls.clientDnsName) -}}
{{- end -}}
{{- range .Values.vector.adminIdentities -}}
{{- if not (or (hasPrefix "dns:" .) (hasPrefix "spiffe://" .)) -}}
{{- fail (printf "vector.adminIdentities entry %q is not a canonical identity: write dns:<lower-case DNS SAN> or a spiffe:// URI." .) -}}
{{- end -}}
{{- end -}}
{{- end }}
