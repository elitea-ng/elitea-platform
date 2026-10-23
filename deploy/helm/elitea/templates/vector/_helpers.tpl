{{/*
elitea-vector (ADR-0031). Release-INDEPENDENT names, for the providers'
reason: the server certificate's DNS SANs name the Service, and the client
certificate's one DNS SAN is the identity elitea-main admits.
*/}}
{{- define "elitea-vector.fullname" -}}elitea-vector{{- end }}
{{/*
The bundled Qdrant (the upstream qdrant subchart) under the FIXED names its
values pin (qdrant.fullnameOverride / qdrant.nameOverride): the subchart
cannot read these helpers, so the two sides agree by validateGuards refusing
any other value.
*/}}
{{- define "elitea-vector.qdrantName" -}}elitea-vectordb{{- end }}
{{- define "elitea-vector.qdrantAppName" -}}elitea-qdrant{{- end }}

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

{{/* The subchart's pod selector (qdrant.selectorLabels), as the NetworkPolicy selects it. */}}
{{- define "elitea-vector.qdrantSelectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-vector.qdrantAppName" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Labels for what THIS chart renders for the subchart (its Certificate, NetworkPolicy). */}}
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

{{/*
elitea-vector's gRPC target as elitea-main dials it: <headless service>:<port>
(vector.service.grpcName), so its round_robin reaches every replica. The
certificate names vector.service.name, which the main ConfigMap sets as
ELITEA_VECTOR_GRPC_SERVER_NAME.
*/}}
{{- define "elitea-vector.grpcTarget" -}}
{{- printf "%s:%v" (.Values.vector.service.grpcName | default .Values.vector.service.name) .Values.vector.service.port -}}
{{- end }}

{{/*
elitea-vector's origin as the DeepWiki and Inventory engines dial it
(ELITEA_<APP>_VECTOR_URL): https://<service>:<port>. The host is the Service
name, which the server certificate's first DNS SAN names, so the engines need
no ELITEA_<APP>_VECTOR_SERVER_NAME.
*/}}
{{- define "elitea-vector.origin" -}}
{{- printf "https://%s:%v" .Values.vector.service.name .Values.vector.service.port -}}
{{- end }}

{{/*
elitea-vector.qdrantTls — "true" when this release runs Qdrant itself (the
qdrant subchart, qdrant.enabled). Its REST, gRPC and peer listeners are then
always TLS (ADR-0031 decision 8) with the certificate
templates/vector/qdrant-certificate.yaml issues, and elitea-vector dials it
over https. An external Qdrant's scheme is its URL's.
*/}}
{{- define "elitea-vector.qdrantTls" -}}
{{- if (.Values.qdrant | default dict).enabled -}}true{{- end -}}
{{- end }}

{{/*
elitea-vector.qdrantTlsSecret — the Secret the bundled Qdrant mounts its node
certificate from: the secretName of the `qdrant-tls` entry in
qdrant.additionalVolumes. ONE source, so the Certificate this chart renders
and the volume the subchart mounts cannot name different Secrets. Empty when
there is no such volume (validateGuards refuses that).
*/}}
{{- define "elitea-vector.qdrantTlsSecret" -}}
{{- range (.Values.qdrant.additionalVolumes | default list) -}}
{{- if and (eq (.name | toString) "qdrant-tls") .secret -}}{{- .secret.secretName -}}{{- end -}}
{{- end -}}
{{- end }}

{{/*
Qdrant's gRPC URL, as elitea-vector dials it. The bundled host is the name
the Qdrant certificate's first SAN carries, so tonic verifies exactly it.
*/}}
{{- define "elitea-vector.qdrantUrl" -}}
{{- if include "elitea-vector.qdrantTls" . -}}
{{- printf "https://%s.%s.svc:6334" (include "elitea-vector.qdrantName" .) .Release.Namespace -}}
{{- else -}}
{{- .Values.vector.qdrant.external.url -}}
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
{{- fail "vector.qdrant.apiKeySecret.name is empty. elitea-vector is the only holder of the Qdrant API key, and the bundled Qdrant reads the same Secret; without a key Qdrant serves every client on the network. Create a Secret with the key and name it here." -}}
{{- end -}}
{{- /* The pre-S10 keys: the bundled Qdrant is the qdrant subchart now. */ -}}
{{- if hasKey .Values.vector.qdrant "mode" -}}
{{- fail (printf "vector.qdrant.mode (%v) is gone: the bundled Qdrant is the upstream qdrant chart, switched by qdrant.enabled. mode internal → leave qdrant.enabled true (the default) and delete vector.qdrant.mode; mode external → qdrant.enabled=false with vector.qdrant.external.url. docs/UPGRADING.md has the steps, including moving the data off the old elitea-qdrant StatefulSet." .Values.vector.qdrant.mode) -}}
{{- end -}}
{{- if hasKey .Values.vector.qdrant "internal" -}}
{{- fail "vector.qdrant.internal is gone: the bundled Qdrant is the upstream qdrant chart and takes its settings under the top-level qdrant key (replicas → qdrant.replicaCount, storage.size → qdrant.persistence.size, storage.storageClassName → qdrant.persistence.storageClassName, resources/affinity/nodeSelector/tolerations → the same names under qdrant; image and tls are fixed by this chart). docs/UPGRADING.md maps each key." -}}
{{- end -}}
{{- if include "elitea-vector.qdrantTls" . -}}
{{- $q := .Values.qdrant -}}
{{- if ne ($q.fullnameOverride | toString) (include "elitea-vector.qdrantName" .) -}}
{{- fail (printf "qdrant.fullnameOverride must be %q, got %q: elitea-vector dials that Service, the node certificate names it, and the start command (qdrant.args) joins peers under it." (include "elitea-vector.qdrantName" .) ($q.fullnameOverride | toString)) -}}
{{- end -}}
{{- if ne ($q.nameOverride | toString) (include "elitea-vector.qdrantAppName" .) -}}
{{- fail (printf "qdrant.nameOverride must be %q, got %q: the Qdrant NetworkPolicy selects the pods by that label." (include "elitea-vector.qdrantAppName" .) ($q.nameOverride | toString)) -}}
{{- end -}}
{{- if or $q.apiKey $q.readOnlyApiKey -}}
{{- fail "qdrant.apiKey / qdrant.readOnlyApiKey are not supported: the upstream chart reads a referenced Secret with lookup, which helm template and Argo CD cannot (the key would render empty). The key reaches Qdrant from vector.qdrant.apiKeySecret through qdrant.env QDRANT__SERVICE__API_KEY." -}}
{{- end -}}
{{- $key := dict -}}
{{- range ($q.env | default list) -}}
{{- if eq (.name | toString) "QDRANT__SERVICE__API_KEY" -}}{{- $key = ((.valueFrom | default dict).secretKeyRef | default dict) -}}{{- end -}}
{{- end -}}
{{- if or (ne ($key.name | toString) (.Values.vector.qdrant.apiKeySecret.name | toString)) (ne ($key.key | toString) (.Values.vector.qdrant.apiKeySecret.key | toString)) -}}
{{- fail (printf "qdrant.env must set QDRANT__SERVICE__API_KEY from secretKeyRef %s/%s (vector.qdrant.apiKeySecret), got %v: otherwise Qdrant and elitea-vector hold different keys, or Qdrant none. A values file that replaces qdrant.env replaces the whole list; repeat that entry." .Values.vector.qdrant.apiKeySecret.name .Values.vector.qdrant.apiKeySecret.key $key) -}}
{{- end -}}
{{- if not (($q.config.service | default dict).enable_tls) -}}
{{- fail "qdrant.config.service.enable_tls must be true: elitea-vector dials the bundled Qdrant over https and the chart has no plaintext path." -}}
{{- end -}}
{{- if not (include "elitea-vector.qdrantTlsSecret" .) -}}
{{- fail "qdrant.additionalVolumes has no `qdrant-tls` secret volume: the bundled Qdrant serves TLS from the certificate mounted there, and templates/vector/qdrant-certificate.yaml issues into its secretName. A values file that replaces qdrant.additionalVolumes replaces the whole list; repeat that entry." -}}
{{- end -}}
{{- $nodes := int $q.replicaCount -}}
{{- if gt $nodes 1 -}}
{{- if not (($q.config.cluster | default dict).enabled) -}}
{{- fail (printf "qdrant.replicaCount is %d but qdrant.config.cluster.enabled is false: the nodes would run as %d unrelated stores behind one Service. Use values-qdrant-ha.yaml." $nodes $nodes) -}}
{{- end -}}
{{- if not ((($q.config.cluster | default dict).p2p | default dict).enable_tls) -}}
{{- fail "qdrant.config.cluster.p2p.enable_tls must be true with more than one node: peer (Raft and shard transfer) traffic is internal traffic, and the chart has no plaintext path." -}}
{{- end -}}
{{- end -}}
{{- if gt (int .Values.vector.collection.replicationFactor) $nodes -}}
{{- fail (printf "vector.collection.replicationFactor is %d but qdrant.replicaCount is %d: Qdrant cannot place more replicas than nodes, so every collection would be created under-replicated." (int .Values.vector.collection.replicationFactor) $nodes) -}}
{{- end -}}
{{- else if not .Values.vector.qdrant.external.url -}}
{{- fail "qdrant.enabled is false, so vector.qdrant.external.url (the gRPC URL of the Qdrant you run, port 6334 by default) must be set." -}}
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
