{{- define "elitea-inventory.name" -}}
{{- default "elitea-inventory" .Values.inventory.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Release-INDEPENDENT, for the reason elitea-deepwiki's and elitea-llm-gateway's
are: these names are certificate material. The server certificate's DNS SANs
name this Service, and elitea-main verifies exactly that SAN. Helm's usual
"<release>-<chart>" would rename it per release and fail every handshake with an
error that reads like a trust problem, nowhere near the rename.
*/}}
{{- define "elitea-inventory.fullname" -}}
{{- default "elitea-inventory" .Values.inventory.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The SHARED provider shape (templates/_provider.tpl), as named aliases. The name
and fullname helpers above deliberately do NOT delegate: they are certificate
material, and a provider must be able to state its own naming rule without
editing a shared file.
*/}}
{{- define "elitea-inventory.labels" -}}
{{- include "elitea.provider.labels" (dict "ctx" . "provider" "inventory") -}}
{{- end }}

{{- define "elitea-inventory.selectorLabels" -}}
{{- include "elitea.provider.selectorLabels" (dict "ctx" . "provider" "inventory") -}}
{{- end }}

{{- define "elitea-inventory.serviceAccountName" -}}
{{- include "elitea.provider.serviceAccountName" (dict "ctx" . "provider" "inventory") -}}
{{- end }}

{{/*
elitea-inventory.engineTag — the engine sidecar's image tag: the value set,
else the chart-wide tag with "-engine" appended, which is how the `-engine`
variant is published.
*/}}
{{- define "elitea-inventory.engineTag" -}}
{{- if .Values.inventory.engine.image.tag -}}
{{- .Values.inventory.engine.image.tag -}}
{{- else -}}
{{- printf "%s-engine" (.Values.image.tag | toString) -}}
{{- end -}}
{{- end }}

{{/*
elitea-inventory.validateGuards — the settings that are silently wrong rather
than loudly missing.

Checked at `helm template` time and not only at container start, because the
container's own refusal is a CrashLoopBackOff an operator has to go read logs
for, and this is a message in the terminal that ran the command.
*/}}
{{- define "elitea-inventory.validateGuards" -}}
{{- $env := .Values.inventory.env | default dict -}}
{{- $runner := get $env "ELITEA_INVENTORY_RUNNER" | toString -}}

{{/*
  Guard #1: the engine sidecar's image.

  The published elitea-inventory image carries the engine SOURCE and the
  FIXTURE runner, but not the knowledge-graph engine's 126-package closure. So
  `runner: legacy` on a plain tag is a sidecar that cannot import its engine and
  fails every tool at INVOCATION time — after a user asked for an ingestion —
  rather than at start.

  DeepWiki's guard is the same rule. Both refuse the wrong combination and
  permit the right one: a guard that refused both would be indistinguishable
  from a broken template.
*/}}
{{- $engineTag := include "elitea-inventory.engineTag" . -}}
{{- if and (eq $runner "legacy") (not (contains "-engine" $engineTag)) -}}
{{- fail (printf "inventory.env.ELITEA_INVENTORY_RUNNER is \"legacy\" but the engine sidecar's image tag (%s) does not end in \"-engine\". The plain elitea-inventory image carries the engine SOURCE and its FIXTURE runner, not the knowledge-graph engine's closure, so the sidecar cannot import the engine and every tool fails at invocation time rather than at start. Set inventory.engine.image.tag to the -engine variant (the default derives it from the chart-wide tag), set inventory.engine.runner to \"fixture\" for a stack that only needs deterministic results, or set the host's runner to \"unavailable\"." $engineTag) -}}
{{- end -}}

{{/*
  Guard #2: the host's runner and the sidecar must agree that there IS a
  sidecar.

  The Deployment renders the second container only for runner=legacy. A host
  configured with an engine socket and no sidecar to answer on it comes up,
  passes its TCP probe, and refuses every invocation with an unreachable
  socket — a service that looks healthy and serves nothing.
*/}}
{{- if and (ne $runner "legacy") (get $env "ELITEA_INVENTORY_ENGINE_SOCKET") -}}
{{- fail (printf "inventory.env.ELITEA_INVENTORY_ENGINE_SOCKET is set but inventory.env.ELITEA_INVENTORY_RUNNER is %q, so no engine sidecar is rendered and nothing will ever listen on that socket. The host would pass its TCP probe and refuse every invocation. Set the runner to \"legacy\", or clear the socket." $runner) -}}
{{- end -}}
{{- if and (eq $runner "legacy") (not (get $env "ELITEA_INVENTORY_ENGINE_SOCKET")) -}}
{{- fail "inventory.env.ELITEA_INVENTORY_RUNNER is \"legacy\" but inventory.env.ELITEA_INVENTORY_ENGINE_SOCKET is empty. The host refuses that combination at boot (internal/apps/registry.go), so this install is a CrashLoopBackOff." -}}
{{- end -}}
{{- end }}
