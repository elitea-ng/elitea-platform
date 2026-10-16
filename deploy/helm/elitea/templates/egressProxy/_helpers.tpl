{{/*
elitea-egress-proxy (ADR-0034 decision 9, "Host allowlist: the egress proxy").
The egress boundary for cloud-executor sandboxes whose project restricts the
network to a host allowlist. A release-independent name: the sandbox reaches
the proxy as http://run:<token>@elitea-egress-proxy.<namespace>.svc:<port>.
*/}}
{{- define "elitea-egress-proxy.fullname" -}}elitea-egress-proxy{{- end }}

{{- define "elitea-egress-proxy.selectorLabels" -}}
app.kubernetes.io/name: elitea-egress-proxy
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "elitea-egress-proxy.labels" -}}
{{ include "elitea-egress-proxy.selectorLabels" . }}
app.kubernetes.io/component: egress-proxy
app.kubernetes.io/part-of: elitea
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-egress-proxy.serviceAccountName" -}}
{{- default (include "elitea-egress-proxy.fullname" .) .Values.egressProxy.serviceAccount.name }}
{{- end }}

{{/* The Secret that holds the run-token public keys. */}}
{{- define "elitea-egress-proxy.keysSecretName" -}}
{{- default "elitea-egress-proxy-keys" .Values.egressProxy.publicKeys.existingSecret }}
{{- end }}

{{/*
The proxy as the sandbox reaches it, without credentials: elitea-main serves
it in the runtime context and the worker adds run:<token>@.
*/}}
{{- define "elitea-egress-proxy.url" -}}
http://{{ include "elitea-egress-proxy.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.egressProxy.service.port }}
{{- end }}

{{/* The audit target: elitea-main's control listener, host:port. */}}
{{- define "elitea-egress-proxy.auditTarget" -}}
{{- if .Values.egressProxy.audit.target -}}
{{- .Values.egressProxy.audit.target -}}
{{- else -}}
{{- printf "%s:%s" (include "elitea-main.fullname" .) (include "elitea-main.runtimePort" .Values.main.runtime.listeners.controlAddress) -}}
{{- end -}}
{{- end }}

{{- define "elitea-egress-proxy.auditServerName" -}}
{{- default (include "elitea-egress-proxy.auditTarget" . | splitList ":" | first) .Values.egressProxy.audit.serverName -}}
{{- end }}

{{/* "true" when the proxy reports to elitea-main's audit service. */}}
{{- define "elitea-egress-proxy.auditEnabled" -}}
{{- if and .Values.egressProxy.enabled .Values.egressProxy.audit.enabled -}}true{{- end -}}
{{- end }}

{{/* The namespace of the sandbox pods the proxy serves. */}}
{{- define "elitea-egress-proxy.clientNamespace" -}}
{{- default .Values.sandboxKubernetes.executionNamespace .Values.egressProxy.networkPolicy.clientNamespace }}
{{- end }}

{{/*
The address ranges the proxy never reaches: the built-in deny list and the
cluster's pod and service ranges. The NetworkPolicy excepts them from the
proxy's egress, and the proxy refuses them itself (ELITEA_EGRESS_PROXY_DENY_CIDRS),
so a CNI without NetworkPolicy still leaves one wall.
*/}}
{{- define "elitea-egress-proxy.denyCidrs" -}}
{{- $n := .Values.egressProxy.networkPolicy -}}
{{- uniq (concat ($n.denyCidrs | default list) ($n.podCidrs | default list) ($n.serviceCidrs | default list)) | join "," -}}
{{- end }}

{{/*
Every refusal is the install the operator asked for, minus one thing the
proxy cannot run safely without. Rendered from deployment.yaml.
*/}}
{{- define "elitea-egress-proxy.validateGuards" -}}
{{- $e := .Values.egressProxy -}}
{{- $n := $e.networkPolicy -}}
{{- if and (not $e.publicKeys.existingSecret) (not $e.publicKeys.keys) -}}
{{- fail "egressProxy.enabled needs the run-token public keys: set egressProxy.publicKeys.existingSecret or egressProxy.publicKeys.keys" -}}
{{- end -}}
{{- if not $e.signingKey.existingSecret -}}
{{- fail "egressProxy.enabled needs elitea-main's run-token signing key: set egressProxy.signingKey.existingSecret (services/elitea-egress-proxy/README.md, \"Creating the key pair\")" -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9._-]+$" ($e.signingKey.key | toString)) -}}
{{- fail "egressProxy.signingKey.key must be a Secret data key" -}}
{{- end -}}
{{- if and $e.publicKeys.existingSecret (eq $e.signingKey.existingSecret $e.publicKeys.existingSecret) -}}
{{- fail "egressProxy.signingKey.existingSecret must not be egressProxy.publicKeys.existingSecret: the proxy would mount the private signing key" -}}
{{- end -}}
{{- if and $e.publicKeys.existingSecret $e.publicKeys.keys -}}
{{- fail "egressProxy.publicKeys: set existingSecret or keys, not both" -}}
{{- end -}}
{{- range $name, $pem := ($e.publicKeys.keys | default dict) -}}
{{- if not (regexMatch "^[A-Za-z0-9._-]+$" $name) -}}
{{- fail (printf "egressProxy.publicKeys.keys: %q is not a valid Secret key name" $name) -}}
{{- end -}}
{{- if not (contains "-----BEGIN PUBLIC KEY-----" (toString $pem)) -}}
{{- fail (printf "egressProxy.publicKeys.keys.%s must be a PEM PUBLIC KEY block; never put the private signing key here" $name) -}}
{{- end -}}
{{- end -}}
{{- if $n.enabled -}}
{{- if not (include "elitea-egress-proxy.clientNamespace" .) -}}
{{- fail "egressProxy.networkPolicy needs the sandbox namespace: set egressProxy.networkPolicy.clientNamespace or sandboxKubernetes.executionNamespace" -}}
{{- end -}}
{{- if not $n.clientPodLabels -}}
{{- fail "egressProxy.networkPolicy.clientPodLabels must name the workspace-run sandbox pods" -}}
{{- end -}}
{{- if or (not $n.podCidrs) (not $n.serviceCidrs) -}}
{{- fail "egressProxy.networkPolicy needs the cluster's podCidrs and serviceCidrs, so the proxy cannot reach in-cluster services" -}}
{{- end -}}
{{- if or (not $n.dnsNamespaceLabels) (not $n.dnsPodLabels) -}}
{{- fail "egressProxy.networkPolicy needs explicit DNS namespace and Pod labels" -}}
{{- end -}}
{{- end -}}
{{- range concat ($n.denyCidrs | default list) ($n.podCidrs | default list) ($n.serviceCidrs | default list) -}}
{{- if not (regexMatch "^[a-fA-F0-9.:]+/[0-9]+$" .) -}}
{{- fail (printf "egressProxy.networkPolicy: %q is not an explicit CIDR block" .) -}}
{{- end -}}
{{- end -}}
{{- if lt (int $e.replicaCount) 2 -}}
{{- fail "egressProxy.replicaCount must be at least 2: one replica makes every rollout an egress outage" -}}
{{- end -}}
{{- $grace := add (int $e.drainDelaySeconds) (int $e.shutdownTimeoutSeconds) 5 (ternary 10 0 (eq (include "elitea-egress-proxy.auditEnabled" .) "true")) -}}
{{- if lt (int $e.terminationGracePeriodSeconds) (int $grace) -}}
{{- fail (printf "egressProxy.terminationGracePeriodSeconds must be at least drainDelaySeconds + shutdownTimeoutSeconds + 5, plus 10 for the last audit reports with egressProxy.audit.enabled (%d)" $grace) -}}
{{- end -}}
{{- if $e.audit.enabled -}}
{{- if not .Values.main.runtime.enabled -}}
{{- fail "egressProxy.audit.enabled needs main.runtime.enabled: elitea-main serves EgressAuditService on its runtime control listener (or set egressProxy.audit.enabled=false)" -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$" ($e.audit.clientDnsName | toString)) -}}
{{- fail "egressProxy.audit.clientDnsName must be one lower-case DNS name: it is the proxy's client certificate's only SAN" -}}
{{- end -}}
{{- if and .Values.vector.enabled (eq (lower ($e.audit.clientDnsName | toString)) (lower (.Values.vector.mtls.clientDnsName | toString))) -}}
{{- fail "egressProxy.audit.clientDnsName must differ from vector.mtls.clientDnsName: each workload's identity is admitted to its own service only" -}}
{{- end -}}
{{- end -}}
{{- end }}
