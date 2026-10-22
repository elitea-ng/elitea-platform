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
elitea-inventory.modelCalls — "true" when this release runs the engine sidecar
(inventory.enabled, host runner sidecar) with the native engine
(inventory.engine.runner native; the fixture runner calls no model), the
container that makes
Inventory's model calls. Those go over elitea.llm.v1.ModelService on
elitea-main's platform listener (#1359 U2) — the listener serves no HTTP, and
the engine refuses a cleartext model origin — so the sidecar always gets the
runtime CA, the callback client certificate (inventory.callbackClient) and
ELITEA_INVENTORY_MODEL_SERVICE_URL.
*/}}
{{- define "elitea-inventory.modelCalls" -}}
{{- if .Values.inventory.enabled -}}
{{- if and (include "elitea-inventory.sidecar" .) (eq (.Values.inventory.engine.runner | toString) "native") -}}true{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea-inventory.callbackMaterial — "true" whenever Inventory runs: the HOST
container uploads and reads graph artifacts over ArtifactObjectService on
elitea-main's platform listener (#1359 U5), so the pod always mounts the
runtime CA and the callback client certificate, sidecar or not.
*/}}
{{- define "elitea-inventory.callbackMaterial" -}}
{{- if .Values.inventory.enabled -}}true{{- end -}}
{{- end }}

{{/* Where the host and the engine find the runtime CA for the platform hop. */}}
{{- define "elitea-inventory.runtimeCaPath" -}}/run/elitea-runtime-ca{{- end -}}

{{/* Where the engine's callback client certificate (tls.crt / tls.key of
     inventory.callbackClient.secretName) is mounted. */}}
{{- define "elitea-inventory.callbackClientPath" -}}/run/elitea-callback-client{{- end -}}

{{/*
elitea-inventory.platformGrpc — "true" when the platform gRPC service is on:
inventory.platformGrpc.clients names at least one certificate identity. Empty
otherwise, and the service is then not configured, exposed or admitted at all.
*/}}
{{- define "elitea-inventory.platformGrpc" -}}
{{- if (.Values.inventory.platformGrpc | default dict).clients -}}true{{- end -}}
{{- end }}

{{/*
elitea-inventory.platformGrpcTarget — the host:port elitea-main dials for the
platform gRPC service (#1244): the Service name (which the server certificate's
SANs name, so the client verifies exactly that) and the platform port. Empty
unless the component is rendered with the service on; templates/main/
configmap.yaml derives ELITEA_INVENTORY_PLATFORM_GRPC_TARGET from it.
*/}}
{{- define "elitea-inventory.platformGrpcTarget" -}}
{{- if and .Values.inventory.enabled (include "elitea-inventory.platformGrpc" .) -}}
{{- printf "%s:%d" .Values.inventory.service.name (.Values.inventory.platformGrpc.port | int) -}}
{{- end -}}
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
{{/*
elitea-inventory.platformClientMaterialIsChartIssued — "true" when the client
material elitea-main presents on the Inventory hops is the facade client
certificate THIS chart issues (inventory.mtls, common name elitea-main) and main,
rendered here, mounts that very Secret. Empty for operator-supplied material:
main mounts another Secret or a volume, mtls is off, or main is not rendered
in this release.
*/}}
{{- define "elitea-inventory.platformClientMaterialIsChartIssued" -}}
{{- $main := .Values.main | default dict -}}
{{- $material := get ($main.fileConfig | default dict) "inventoryClientMaterial" | default dict -}}
{{- $secret := $material.secretName | default "" | toString -}}
{{- $volume := $material.volume | default dict -}}
{{- if and $main.enabled .Values.inventory.mtls.enabled (not $volume) (or (eq $secret "") (eq $secret (.Values.inventory.mtls.clientSecretName | toString))) -}}
true
{{- end -}}
{{- end -}}

{{/*
elitea-inventory.platformClientName — the identity (certificate common name or DNS
SAN) inventory.platformGrpc.clients must name for elitea-main's calls to be
admitted: elitea-main when the chart issues the material, otherwise
main.platformClientName (default elitea-main).
*/}}
{{- define "elitea-inventory.platformClientName" -}}
{{- if include "elitea-inventory.platformClientMaterialIsChartIssued" . -}}
elitea-main
{{- else -}}
{{ .Values.main.platformClientName | default "elitea-main" }}
{{- end -}}
{{- end -}}

{{- define "elitea-inventory.validateGuards" -}}
{{- /*
  The provider's one listener is gRPC over mutual TLS (#1359): the SPI and the
  platform operations, every caller authorised by its verified client
  certificate. So mtls is mandatory, the allowlist cannot be empty, and the
  HTTPS SPI port is gone.
*/ -}}
{{- if hasKey (.Values.inventory.service | default dict) "port" -}}
{{- fail "inventory.service.port was removed with the HTTPS SPI (#1359): the provider serves gRPC only, on inventory.platformGrpc.port. Remove service.port (docs/UPGRADING.md)." -}}
{{- end -}}
{{- if not .Values.inventory.mtls.enabled -}}
{{- fail "inventory.mtls.enabled is false, but the provider serves gRPC over mutual TLS only (#1359) and refuses to start without its server certificate and client CA. Enable mtls." -}}
{{- end -}}
{{- /*
  The native engine keeps every graph's entity vectors in elitea-vector (ADR-0031; PostgreSQL holds
  none) and REFUSES to start without ELITEA_INVENTORY_VECTOR_URL. The chart sets it
  from this release's elitea-vector, so a native runner needs vector.enabled,
  or inventory.env.ELITEA_INVENTORY_VECTOR_URL naming one deployed elsewhere (whose
  certificate the runtime CA signs and whose client CA trusts the callback
  identity). Without this guard the refusal is a sidecar in CrashLoopBackOff.
*/ -}}
{{- if and (include "elitea-inventory.modelCalls" .) (not .Values.vector.enabled) (not (get (.Values.inventory.env | default dict) "ELITEA_INVENTORY_VECTOR_URL")) -}}
{{- fail "inventory.engine.runner is \"native\" but vector.enabled is false and inventory.env.ELITEA_INVENTORY_VECTOR_URL is unset. The native engine keeps every graph's entity vectors in elitea-vector (ADR-0031) and refuses to start without it; PostgreSQL holds no vector. Enable vector (this chart's elitea-vector and Qdrant), name an external elitea-vector in inventory.env.ELITEA_INVENTORY_VECTOR_URL, or use runner fixture." -}}
{{- end -}}
{{- if not (include "elitea-inventory.platformGrpc" .) -}}
{{- fail "inventory.platformGrpc.clients is empty, so the provider's one listener admits nobody and elitea-main's facade is refused on every call (#1359). The default is [elitea-main]." -}}
{{- end -}}
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
{{- /*
  The allowlist must name the identity elitea-main presents on this hop. When
  this chart issues the facade client certificate itself (inventory.mtls, with
  main mounting that Secret), the identity is the Certificate's common name,
  elitea-main, and nothing else can be right. With OPERATOR-SUPPLIED material
  (main mounts another Secret, or a volume, or main is not rendered here) the
  chart cannot read the certificate, so the operator states the name in
  main.platformClientName (default elitea-main) and the allowlist is checked
  against that instead.
*/ -}}
{{- $expectedClient := include "elitea-inventory.platformClientName" . -}}
{{- $statedClient := .Values.main.platformClientName | default "elitea-main" | toString -}}
{{- if and (include "elitea-inventory.platformClientMaterialIsChartIssued" .) (ne $statedClient "elitea-main") -}}
{{- fail (printf "main.platformClientName is %q, but the chart issues the Inventory facade client certificate itself (inventory.mtls) with the common name elitea-main, which is what elitea-main presents on this hop. Leave main.platformClientName at elitea-main, or point main.fileConfig.inventoryClientMaterial at your own material." $statedClient) -}}
{{- end -}}
{{- if not (has $expectedClient .Values.inventory.platformGrpc.clients) -}}
{{- if include "elitea-inventory.platformClientMaterialIsChartIssued" . -}}
{{- fail (printf "inventory.platformGrpc.clients is %v, which does not name elitea-main. The facade client certificate this chart issues (inventory.mtls) has the common name elitea-main, and it is the certificate elitea-main presents on this hop too, so an allowlist without it refuses every call elitea-main makes and the service deletes nothing. Add elitea-main." .Values.inventory.platformGrpc.clients) -}}
{{- else -}}
{{- fail (printf "inventory.platformGrpc.clients is %v, which does not name %q (main.platformClientName), the identity of the operator-supplied client material elitea-main presents on this hop. An allowlist without it refuses every call elitea-main makes and the service deletes nothing. Add it, or set main.platformClientName to the name the material carries." .Values.inventory.platformGrpc.clients $expectedClient) -}}
{{- end -}}
{{- end -}}
{{- if or (hasKey $env "ELITEA_INVENTORY_PLATFORM_CLIENTS") (hasKey $env "ELITEA_INVENTORY_PLATFORM_GRPC_ADDR") -}}
{{- fail "inventory.env sets ELITEA_INVENTORY_PLATFORM_CLIENTS or ELITEA_INVENTORY_PLATFORM_GRPC_ADDR; the chart renders both from inventory.platformGrpc (clients, port). Remove them from env." -}}
{{- end -}}
{{- if hasKey $env "ELITEA_INVENTORY_ADMIN_ADDR" -}}
{{- fail "inventory.env sets ELITEA_INVENTORY_ADMIN_ADDR; the chart renders it from inventory.admin.port, which the probes and the NetworkPolicy also use. Remove it from env." -}}
{{- end -}}
{{- if eq ((.Values.inventory.admin | default dict).port | int) (.Values.inventory.platformGrpc.port | int) -}}
{{- fail "inventory.admin.port equals inventory.platformGrpc.port; the plain-HTTP admin port and the mutual-TLS gRPC listener cannot share a port." -}}
{{- end -}}
{{- end -}}

{{/*
  Guard #0b: the engine's model path (#1359 U2). The engine sidecar makes its
  model calls over ModelService on elitea-main's platform listener; Guard #0c
  below requires that listener (main.runtime) and the runtime-CA client
  certificate for every Inventory install, sidecar or not, because the host
  needs them too. inventory.engine.callbackCa (a private CA for an
  https callback origin) is retired: the model origin is now always the
  platform listener, whose CA is the runtime CA the chart mounts itself. The
  chart owns the model-hop settings, so inventory.env may not repeat them.
*/}}
{{- if hasKey (.Values.inventory.engine | default dict) "callbackCa" -}}
{{- fail "inventory.engine.callbackCa was removed (#1359 U2): the engine's model calls go over ModelService on elitea-main's platform listener, and the chart mounts the runtime CA (worker.materialSecretName, key runtime-ca.crt) and inventory.callbackClient for that hop itself. Remove inventory.engine.callbackCa and set inventory.callbackClient.secretName (docs/UPGRADING.md)." -}}
{{- end -}}
{{- range $name := list "ELITEA_INVENTORY_MODEL_SERVICE_URL" "ELITEA_INVENTORY_ARTIFACT_SERVICE_URL" "ELITEA_INVENTORY_CALLBACK_CA_FILE" "ELITEA_INVENTORY_CALLBACK_CERT_FILE" "ELITEA_INVENTORY_CALLBACK_KEY_FILE" -}}
{{- if hasKey $env $name -}}
{{- fail (printf "inventory.env sets %s, which the chart renders itself: the host's artifact calls and the engine's model and artifact calls go to elitea-main's platform listener (worker.runtime.platformOrigin) with the runtime CA and inventory.callbackClient. Remove it from inventory.env." $name) -}}
{{- end -}}
{{- end -}}
{{/*
  Guard #0c: the host's artifact hop (#1359 U5). The host uploads and reads
  graph artifacts over ArtifactObjectService on the platform listener whatever
  runner it uses, so every Inventory install needs the runtime and the
  callback client certificate.
*/}}
{{- if .Values.inventory.enabled -}}
{{- if not .Values.main.runtime.enabled -}}
{{- fail "inventory.enabled needs main.runtime.enabled: the provider host reads and writes graph artifacts over elitea.runtime.v1.ArtifactObjectService on elitea-main's platform gRPC listener (#1359 U5), which only the runtime opens, so every import and export would fail. Turn the runtime on (main.runtime.enabled)." -}}
{{- end -}}
{{- if not (.Values.inventory.callbackClient | default dict).secretName -}}
{{- fail "inventory.enabled needs inventory.callbackClient.secretName: the provider host reaches elitea-main's platform listener for its artifacts (ArtifactObjectService, #1359 U5), and the listener requires a client certificate the runtime CA signed, admitted as dns:elitea-subapp-callback. Create a Secret with tls.crt and tls.key (deploy/scripts/gen-runtime-certs.sh mints subapp-callback-client.crt/.key; DeepWiki's callbackClient Secret can be reused) and name it there." -}}
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
