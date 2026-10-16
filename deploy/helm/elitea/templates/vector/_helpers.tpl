{{/*
elitea-vector (ADR-0031). Release-INDEPENDENT names, for the providers'
reason: the server certificate's DNS SANs name the Service, and the client
certificate's one DNS SAN is the identity elitea-main admits.
*/}}
{{- define "elitea-vector.fullname" -}}elitea-vector{{- end }}
{{- define "elitea-vector.qdrantName" -}}elitea-qdrant{{- end }}

{{/*
elitea-vector rolls BEFORE elitea-main (worker unit M9 rollout safety). Every
manifest of the vector stack (its Qdrant, Services, certificates, network
policies, Deployment) is in Argo CD sync-wave -5, ahead of elitea-main and
everything else at the default wave 0, so a new elitea-main never introspects
for an older elitea-vector: Argo applies a wave and waits for it to be healthy
before the next. The whole stack shares the wave, never only the Deployment: a
Deployment ahead of its own certificates would wait for Secrets a later wave
creates, and the sync would never finish. elitea-vector's readiness does not
need elitea-main (only a REFUSAL by elitea-main fails it), so the earlier wave
cannot wait on a later one. The migration Job is a PreSync hook and runs
before every wave. A plain `helm upgrade` has no waves: there elitea-vector
tolerates sources it cannot name (it ignores them), which is what makes either
order safe.
*/}}
{{- define "elitea-vector.syncWave" -}}
argocd.argoproj.io/sync-wave: "-5"
{{- end }}

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

{{/*
elitea-vector.mainClientEnabled — "true" when this chart issues elitea-main
its administrator client certificate (ADR-0031 decision 4): vector.enabled
and vector.mainClient.enabled.
*/}}
{{- define "elitea-vector.mainClientEnabled" -}}
{{- if and .Values.vector.enabled ((.Values.vector.mainClient | default dict).enabled) -}}true{{- end -}}
{{- end }}

{{/* The identity elitea-vector admits elitea-main as: dns:<mainClient.dnsName>. */}}
{{- define "elitea-vector.mainClientIdentity" -}}
{{- printf "dns:%s" (lower (.Values.vector.mainClient.dnsName | toString)) -}}
{{- end }}

{{/*
elitea-vector.adminIdentities — the comma-separated ELITEA_VECTOR_ADMIN_IDENTITIES:
this chart's elitea-main (when mainClient is on) and then vector.adminIdentities,
each once.
*/}}
{{- define "elitea-vector.adminIdentities" -}}
{{- $identities := list -}}
{{- if include "elitea-vector.mainClientEnabled" . -}}
{{- $identities = append $identities (include "elitea-vector.mainClientIdentity" .) -}}
{{- end -}}
{{- range .Values.vector.adminIdentities -}}
{{- $identities = append $identities . -}}
{{- end -}}
{{- join "," ($identities | uniq) -}}
{{- end }}

{{/* elitea-vector's gRPC target as elitea-main dials it: <service>:<port>. */}}
{{- define "elitea-vector.grpcTarget" -}}
{{- printf "%s:%v" .Values.vector.service.name .Values.vector.service.port -}}
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
{{- if include "elitea-vector.mainClientEnabled" . -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$" (.Values.vector.mainClient.dnsName | toString)) -}}
{{- fail (printf "vector.mainClient.dnsName must be one lower-case DNS name, got %q: it is elitea-main's client certificate's only SAN and elitea-vector admits it as dns:<name>." .Values.vector.mainClient.dnsName) -}}
{{- end -}}
{{- if eq (lower (.Values.vector.mainClient.dnsName | toString)) (lower (.Values.vector.mtls.clientDnsName | toString)) -}}
{{- fail (printf "vector.mainClient.dnsName and vector.mtls.clientDnsName are both %q: elitea-main's administrator identity and elitea-vector's own client identity must differ, or elitea-vector would admit itself as an administrator." .Values.vector.mainClient.dnsName) -}}
{{- end -}}
{{- end -}}
{{- end }}
