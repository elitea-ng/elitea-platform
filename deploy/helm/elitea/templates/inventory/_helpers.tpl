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
else the chart-wide tag. The engine is its own repository
(ghcr.io/elitea-ng/elitea-inventory-engine, ADR-0027), so there is no suffix.
*/}}
{{- define "elitea-inventory.engineTag" -}}
{{- .Values.inventory.engine.image.tag | default .Values.image.tag | toString -}}
{{- end }}

{{/*
elitea-inventory.sidecar — whether the pod runs the engine sidecar: only while
the HOST's runner (inventory.env.ELITEA_INVENTORY_RUNNER) is "sidecar" (or its
pre-ADR-0027 alias "legacy"), the host's word for "dial the engine socket".
The sidecar's own runner is inventory.engine.runner (native or fixture); the
host's value never reaches the engine container.
*/}}
{{- define "elitea-inventory.sidecar" -}}
{{- if has (get (.Values.inventory.env | default dict) "ELITEA_INVENTORY_RUNNER" | toString) (list "sidecar" "legacy") -}}true{{- end -}}
{{- end }}

{{/*
elitea-inventory.platformGrpc — "true" when the platform gRPC service is on:
inventory.platformGrpc.clients names at least one certificate identity. Empty
otherwise, and the service is then not configured, exposed or admitted at all.
*/}}
{{- define "elitea-inventory.platformGrpc" -}}
{{- if (.Values.inventory.platformGrpc | default dict).clients -}}true{{- end -}}
{{- end }}

{{/*
elitea-inventory.databaseSecrets — the engine's database URL secret, as JSON:
inventory.secrets.ELITEA_INVENTORY_DATABASE_URL, else postgresql.existingSecret
(the shared define every provider uses). The Deployment's engine container and
the migrate Job both read THIS, so they cannot migrate one database and serve
another.
*/}}
{{- define "elitea-inventory.databaseSecrets" -}}
{{- $all := fromJson (include "elitea.provider.databaseSecret" (dict "ctx" . "provider" "inventory" "urlKey" "ELITEA_INVENTORY_DATABASE_URL")) -}}
{{- $picked := pick $all "ELITEA_INVENTORY_DATABASE_URL" -}}
{{- if not (get $picked "ELITEA_INVENTORY_DATABASE_URL") -}}{{- $picked = dict -}}{{- end -}}
{{- $picked | toJson -}}
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
{{- $hostSidecar := has $runner (list "sidecar" "legacy") -}}

{{/*
  Guard #0: the platform gRPC service is authorised by the verified client
  certificate, so it cannot exist without mutual TLS; its port must not
  collide with the SPI's; and the chart renders its two settings itself.
*/}}
{{- if include "elitea-inventory.platformGrpc" . -}}
{{- if not .Values.inventory.mtls.enabled -}}
{{- fail "inventory.platformGrpc.clients is set, but inventory.mtls.enabled is false: the platform service authorises a caller by its verified client certificate and refuses to start without mutual TLS. Enable mtls, or empty platformGrpc.clients." -}}
{{- end -}}
{{- if eq (.Values.inventory.platformGrpc.port | int) 8080 -}}
{{- fail "inventory.platformGrpc.port is 8080, the SPI's port: the platform service is a listener of its own." -}}
{{- end -}}
{{- if or (hasKey $env "ELITEA_INVENTORY_PLATFORM_CLIENTS") (hasKey $env "ELITEA_INVENTORY_PLATFORM_GRPC_ADDR") -}}
{{- fail "inventory.env sets ELITEA_INVENTORY_PLATFORM_CLIENTS or ELITEA_INVENTORY_PLATFORM_GRPC_ADDR; the chart renders both from inventory.platformGrpc (clients, port). Remove them from env." -}}
{{- end -}}
{{- end -}}

{{/*
  Guard #1: values from before the Python engine was removed (ADR-0027).

  The sidecar is the native image now. A tag with the old "-engine" suffix, or
  the old repository, would be pulled as written and fail at pull time or run
  the wrong thing, and `legacy` as the sidecar's own runner names the Python
  engine, which the native binary refuses. Each is refused here with the
  setting that replaces it.
*/}}
{{- $engineRunner := .Values.inventory.engine.runner | toString -}}
{{- if eq $engineRunner "legacy" -}}
{{- fail "inventory.engine.runner is \"legacy\", which ran the Python engine sidecar. That engine is removed (ADR-0027): set native (the default) to run the native engine, or fixture for its canned graph." -}}
{{- end -}}
{{- if not (has $engineRunner (list "native" "fixture")) -}}
{{- fail (printf "inventory.engine.runner must be native or fixture, got %q" $engineRunner) -}}
{{- end -}}
{{- $engineTag := include "elitea-inventory.engineTag" . -}}
{{- if or (hasSuffix "-engine" $engineTag) (hasSuffix "/elitea-inventory" (.Values.inventory.engine.image.repository | toString)) -}}
{{- fail (printf "inventory.engine.image (%s:%s) names the retired Python engine image. The sidecar is ghcr.io/elitea-ng/elitea-inventory-engine with the chart-wide tag, no \"-engine\" suffix (ADR-0027). Remove the override." .Values.inventory.engine.image.repository $engineTag) -}}
{{- end -}}

{{/*
  Guard #1b: the native engine's database and git allowlist.

  The Rust engine keeps every graph in PostgreSQL (schema inventory_graph) and
  has no other storage, so it REFUSES to start without
  ELITEA_INVENTORY_DATABASE_URL: without this guard that is a sidecar in
  CrashLoopBackOff and a host that never becomes ready. The URL must come from
  the place the migrate Job reads it: inventory.secrets.
  ELITEA_INVENTORY_DATABASE_URL, else postgresql.existingSecret.
  A plain inventory.env value is not enough: the Job never receives it.

  The engine also CLONES repositories itself, and its allowlist is fail-closed:
  unset, every clone is refused, at the point a user asked for an ingestion.
  `*` is a legitimate posture and is accepted; it has to be written down.
*/}}
{{- if and $hostSidecar (eq $engineRunner "native") -}}
{{- $dbSecrets := fromJson (include "elitea-inventory.databaseSecrets" .) -}}
{{- if not (hasKey $dbSecrets "ELITEA_INVENTORY_DATABASE_URL") -}}
{{- fail "inventory.engine.runner is \"native\" but no ELITEA_INVENTORY_DATABASE_URL secret reaches the engine and its migrate Job: neither inventory.secrets.ELITEA_INVENTORY_DATABASE_URL nor postgresql.existingSecret is set (a plain inventory.env value reaches the engine but not the migrate Job). The native engine keeps every graph in the inventory_graph PostgreSQL schema and has no other storage, so it refuses to start without one. Name the secret that holds the database URL, or use inventory.engine.runner fixture." -}}
{{- end -}}
{{- if not (get $env "ELITEA_INVENTORY_GIT_ALLOWLIST") -}}
{{- fail "inventory.engine.runner is \"native\" but inventory.env.ELITEA_INVENTORY_GIT_ALLOWLIST is empty. The native engine clones repositories itself and its allowlist is fail-closed: every clone would be refused, per invocation, at the point a user asked for an ingestion. List the git hosts (comma-separated, e.g. \"github.com,*.github.com\"), or write \"*\" to disable the control explicitly. Use the SAME value as main.env.ELITEA_INVENTORY_GIT_ALLOWLIST: the facade checks it before opening the vault, the engine before cloning." -}}
{{- end -}}
{{- end -}}

{{/*
  Guard #2: the host's runner and the sidecar must agree that there IS a
  sidecar.

  The Deployment renders the second container only for runner=sidecar (or
  its alias legacy). A host
  configured with an engine socket and no sidecar to answer on it comes up,
  passes its TCP probe, and refuses every invocation with an unreachable
  socket — a service that looks healthy and serves nothing.
*/}}
{{- if and (not $hostSidecar) (get $env "ELITEA_INVENTORY_ENGINE_SOCKET") -}}
{{- fail (printf "inventory.env.ELITEA_INVENTORY_ENGINE_SOCKET is set but inventory.env.ELITEA_INVENTORY_RUNNER is %q, so no engine sidecar is rendered and nothing will ever listen on that socket. The host would pass its TCP probe and refuse every invocation. Set the runner to \"sidecar\", or clear the socket." $runner) -}}
{{- end -}}
{{- if and $hostSidecar (not (get $env "ELITEA_INVENTORY_ENGINE_SOCKET")) -}}
{{- fail "inventory.env.ELITEA_INVENTORY_RUNNER is \"sidecar\" but inventory.env.ELITEA_INVENTORY_ENGINE_SOCKET is empty. The host refuses that combination at boot (internal/apps/registry.go), so this install is a CrashLoopBackOff." -}}
{{- end -}}
{{- end }}
