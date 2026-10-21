{{- define "elitea-deepwiki.name" -}}
{{- default "elitea-deepwiki" .Values.deepwiki.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Release-INDEPENDENT, for the same reason elitea-llm-gateway's is: these names
are certificate material. The server certificate's DNS SANs name this Service,
and elitea-main verifies exactly that SAN. Helm's usual "<release>-<chart>"
would rename it per release and fail every handshake with an error that reads
like a trust problem, nowhere near the rename.
*/}}
{{- define "elitea-deepwiki.fullname" -}}
{{- default "elitea-deepwiki" .Values.deepwiki.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
These three are the SHARED provider shape (templates/_provider.tpl). They stay
as named aliases rather than being replaced at every call site, so that
"elitea-deepwiki.labels" keeps meaning what it meant and a provider that ever
needs to diverge does it in one place — here — instead of in six files.

The name and fullname helpers above deliberately do NOT delegate: they are
certificate material, and a provider must be able to state its own naming rule
without editing a shared file.
*/}}
{{- define "elitea-deepwiki.labels" -}}
{{- include "elitea.provider.labels" (dict "ctx" . "provider" "deepwiki") -}}
{{- end }}

{{- define "elitea-deepwiki.selectorLabels" -}}
{{- include "elitea.provider.selectorLabels" (dict "ctx" . "provider" "deepwiki") -}}
{{- end }}

{{- define "elitea-deepwiki.serviceAccountName" -}}
{{- include "elitea.provider.serviceAccountName" (dict "ctx" . "provider" "deepwiki") -}}
{{- end }}

{{/*
elitea-deepwiki.validateGuards — the settings that are silently wrong rather
than loudly missing.

Each is checked at `helm template` time and not only at container start,
because the container's own refusal is a CrashLoopBackOff an operator has to
go read logs for, and this is a message in the terminal that ran the command.
*/}}
{{/*
elitea-deepwiki.modelCalls — "true" when this release runs the engine sidecar
(deepwiki.enabled, ELITEA_DEEPWIKI_RUNNER native) with the native engine
(deepwiki.engine.runner native; the fixture runner calls no model), the
container that makes DeepWiki's model calls. Those go over elitea.llm.v1.ModelService on
elitea-main's platform listener (#1359 U2) — the listener serves no HTTP, and
the engine refuses a cleartext model origin.
*/}}
{{- define "elitea-deepwiki.modelCalls" -}}
{{- if .Values.deepwiki.enabled -}}
{{- if and (eq (include "elitea-deepwiki.sidecar" .) "native") (eq (.Values.deepwiki.engine.runner | toString) "native") -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/*
elitea-deepwiki.callbackMaterial — "true" whenever DeepWiki runs: the HOST
container uploads and reads wiki artifacts over ArtifactObjectService on
elitea-main's platform listener (#1359 U5) and the engine sidecar makes its
model calls and artifact-folder reads there, so the pod always mounts the
runtime CA and the callback client certificate.
*/}}
{{- define "elitea-deepwiki.callbackMaterial" -}}
{{- if .Values.deepwiki.enabled -}}true{{- end -}}
{{- end -}}

{{/* Where both containers find the runtime CA for the callback hop. */}}
{{- define "elitea-deepwiki.runtimeCaPath" -}}/run/elitea-runtime-ca{{- end -}}

{{/* Where the callback hop's client certificate (tls.crt / tls.key of
     deepwiki.callbackClient.secretName) is mounted. */}}
{{- define "elitea-deepwiki.callbackClientPath" -}}/run/elitea-callback-client{{- end -}}

{{/*
elitea-deepwiki.platformGrpc — "true" when the platform gRPC service is on:
deepwiki.platformGrpc.clients names at least one certificate identity. Empty
otherwise, and the service is then not configured, exposed or admitted at all.
*/}}
{{- define "elitea-deepwiki.platformGrpc" -}}
{{- if (.Values.deepwiki.platformGrpc | default dict).clients -}}true{{- end -}}
{{- end -}}

{{/*
elitea-deepwiki.platformGrpcTarget — the host:port elitea-main dials for the
platform gRPC service (#1243): the Service name (which the server certificate's
SANs name, so the client verifies exactly that) and the platform port. Empty
unless the component is rendered with the service on; templates/main/
configmap.yaml derives ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET from it.
*/}}
{{- define "elitea-deepwiki.platformGrpcTarget" -}}
{{- if and .Values.deepwiki.enabled (include "elitea-deepwiki.platformGrpc" .) -}}
{{- printf "%s:%d" .Values.deepwiki.service.name (.Values.deepwiki.platformGrpc.port | int) -}}
{{- end -}}
{{- end -}}

{{/*
elitea-deepwiki.platformClientMaterialIsChartIssued — "true" when the client
material elitea-main presents on the DeepWiki hops is the facade client
certificate THIS chart issues (deepwiki.mtls, common name elitea-main) and main,
rendered here, mounts that very Secret. Empty for operator-supplied material:
main mounts another Secret or a volume, mtls is off, or main is not rendered
in this release.
*/}}
{{- define "elitea-deepwiki.platformClientMaterialIsChartIssued" -}}
{{- $main := .Values.main | default dict -}}
{{- $material := get ($main.fileConfig | default dict) "deepwikiClientMaterial" | default dict -}}
{{- $secret := $material.secretName | default "" | toString -}}
{{- $volume := $material.volume | default dict -}}
{{- if and $main.enabled .Values.deepwiki.mtls.enabled (not $volume) (or (eq $secret "") (eq $secret (.Values.deepwiki.mtls.clientSecretName | toString))) -}}
true
{{- end -}}
{{- end -}}

{{/*
elitea-deepwiki.platformClientName — the identity (certificate common name or DNS
SAN) deepwiki.platformGrpc.clients must name for elitea-main's calls to be
admitted: elitea-main when the chart issues the material, otherwise
main.platformClientName (default elitea-main).
*/}}
{{- define "elitea-deepwiki.platformClientName" -}}
{{- if include "elitea-deepwiki.platformClientMaterialIsChartIssued" . -}}
elitea-main
{{- else -}}
{{ .Values.main.platformClientName | default "elitea-main" }}
{{- end -}}
{{- end -}}

{{- define "elitea-deepwiki.validateGuards" -}}
{{- /*
  The provider's one listener is gRPC over mutual TLS (#1359): the SPI and the
  platform operations, every caller authorised by its verified client
  certificate. So mtls is mandatory, the allowlist cannot be empty, and the
  HTTPS SPI port is gone.
*/ -}}
{{- if hasKey (.Values.deepwiki.service | default dict) "port" -}}
{{- fail "deepwiki.service.port was removed with the HTTPS SPI (#1359): the provider serves gRPC only, on deepwiki.platformGrpc.port. Remove service.port (docs/UPGRADING.md)." -}}
{{- end -}}
{{- if not .Values.deepwiki.mtls.enabled -}}
{{- fail "deepwiki.mtls.enabled is false, but the provider serves gRPC over mutual TLS only (#1359) and refuses to start without its server certificate and client CA. Enable mtls." -}}
{{- end -}}
{{- if not (include "elitea-deepwiki.platformGrpc" .) -}}
{{- fail "deepwiki.platformGrpc.clients is empty, so the provider's one listener admits nobody and elitea-main's facade is refused on every call (#1359). The default is [elitea-main]." -}}
{{- end -}}
{{- $env := .Values.deepwiki.env | default dict -}}

{{/*
  Guard: the platform gRPC service is authorised by the verified client
  certificate, so it cannot exist without mutual TLS; and its port must not
  collide with the SPI's.
*/}}
{{- if include "elitea-deepwiki.platformGrpc" . -}}
{{- if not .Values.deepwiki.mtls.enabled -}}
{{- fail "deepwiki.platformGrpc.clients is set, but deepwiki.mtls.enabled is false: the platform service authorises a caller by its verified client certificate and refuses to start without mutual TLS. Enable mtls, or empty platformGrpc.clients." -}}
{{- end -}}
{{- /*
  The allowlist must name the identity elitea-main presents on this hop. When
  this chart issues the facade client certificate itself (deepwiki.mtls, with
  main mounting that Secret), the identity is the Certificate's common name,
  elitea-main, and nothing else can be right. With OPERATOR-SUPPLIED material
  (main mounts another Secret, or a volume, or main is not rendered here) the
  chart cannot read the certificate, so the operator states the name in
  main.platformClientName (default elitea-main) and the allowlist is checked
  against that instead.
*/ -}}
{{- $expectedClient := include "elitea-deepwiki.platformClientName" . -}}
{{- $statedClient := .Values.main.platformClientName | default "elitea-main" | toString -}}
{{- if and (include "elitea-deepwiki.platformClientMaterialIsChartIssued" .) (ne $statedClient "elitea-main") -}}
{{- fail (printf "main.platformClientName is %q, but the chart issues the DeepWiki facade client certificate itself (deepwiki.mtls) with the common name elitea-main, which is what elitea-main presents on this hop. Leave main.platformClientName at elitea-main, or point main.fileConfig.deepwikiClientMaterial at your own material." $statedClient) -}}
{{- end -}}
{{- if not (has $expectedClient .Values.deepwiki.platformGrpc.clients) -}}
{{- if include "elitea-deepwiki.platformClientMaterialIsChartIssued" . -}}
{{- fail (printf "deepwiki.platformGrpc.clients is %v, which does not name elitea-main. The facade client certificate this chart issues (deepwiki.mtls) has the common name elitea-main, and it is the certificate elitea-main presents on this hop too, so an allowlist without it refuses every call elitea-main makes and the service deletes nothing. Add elitea-main." .Values.deepwiki.platformGrpc.clients) -}}
{{- else -}}
{{- fail (printf "deepwiki.platformGrpc.clients is %v, which does not name %q (main.platformClientName), the identity of the operator-supplied client material elitea-main presents on this hop. An allowlist without it refuses every call elitea-main makes and the service deletes nothing. Add it, or set main.platformClientName to the name the material carries." .Values.deepwiki.platformGrpc.clients $expectedClient) -}}
{{- end -}}
{{- end -}}
{{- if or (hasKey $env "ELITEA_DEEPWIKI_PLATFORM_CLIENTS") (hasKey $env "ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR") -}}
{{- fail "deepwiki.env sets ELITEA_DEEPWIKI_PLATFORM_CLIENTS or ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR; the chart renders both from deepwiki.platformGrpc (clients, port). Remove them from env." -}}
{{- end -}}
{{- end -}}

{{/*
  Guard #0: the callback hop. It is always gRPC on elitea-main's platform
  listener now (#1359 U5): the host's artifact uploads and reads over
  ArtifactObjectService, the engine's model calls and artifact-folder reads
  over ModelService and ArtifactObjectService. The two switches that once
  chose the hop are refused whatever their value: callbackViaPlatformEdge
  (ADR-0027, gone with platform-edge) and callbackViaPlatformListener (the
  only path now, so nothing is left to choose).
*/}}
{{- if .Values.deepwiki.callbackViaPlatformEdge -}}
{{- fail "deepwiki.callbackViaPlatformEdge was removed with platform-edge (#1359). The callback hop is always gRPC on elitea-main's platform listener now: remove the key and mint the callback client certificate into deepwiki.callbackClient.secretName (docs/UPGRADING.md)." -}}
{{- end -}}
{{- if hasKey .Values.deepwiki "callbackViaPlatformListener" -}}
{{- fail "deepwiki.callbackViaPlatformListener was removed (#1359 U5): DeepWiki's artifact hop is always gRPC (elitea.runtime.v1.ArtifactObjectService) on elitea-main's platform listener, with the runtime CA and deepwiki.callbackClient, so there is nothing left to choose. Remove the key, and remove main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL too (docs/UPGRADING.md)." -}}
{{- end -}}
{{- if .Values.deepwiki.enabled -}}
{{- if not .Values.main.runtime.enabled -}}
{{- fail "deepwiki.enabled needs main.runtime.enabled: the provider host uploads and reads wiki artifacts over elitea.runtime.v1.ArtifactObjectService, and the engine makes its model calls over elitea.llm.v1.ModelService, both on elitea-main's platform gRPC listener (#1359 U2/U5), which only the runtime opens. Turn the runtime on (main.runtime.enabled)." -}}
{{- end -}}
{{- if not .Values.deepwiki.callbackClient.secretName -}}
{{- fail "deepwiki.enabled needs deepwiki.callbackClient.secretName: the provider host and engine reach elitea-main's platform listener (#1359 U2/U5), which requires a client certificate the runtime CA signed, admitted as dns:elitea-subapp-callback. Create a Secret with tls.crt and tls.key (deploy/scripts/gen-runtime-certs.sh mints subapp-callback-client.crt/.key) and name it there." -}}
{{- end -}}
{{- end -}}
{{/*
  Guard #0a: the platform-listener settings. Guard #0 already requires the
  listener (main.runtime) and the callback client certificate for any
  DeepWiki install, sidecar or not. The chart owns the host's artifact-hop
  and the engine's model-hop settings, so deepwiki.env may not repeat them: a
  name in both is a duplicate env entry whose winner is decided by order.
*/}}
{{- range $name := list "ELITEA_DEEPWIKI_MODEL_SERVICE_URL" "ELITEA_DEEPWIKI_ARTIFACT_SERVICE_URL" "ELITEA_DEEPWIKI_CALLBACK_CA_FILE" "ELITEA_DEEPWIKI_CALLBACK_CERT_FILE" "ELITEA_DEEPWIKI_CALLBACK_KEY_FILE" -}}
{{- if hasKey $env $name -}}
{{- fail (printf "deepwiki.env sets %s, which the chart renders itself: the host's artifact calls and the engine's model and artifact calls go to elitea-main's platform listener (worker.runtime.platformOrigin) with the runtime CA and deepwiki.callbackClient. Remove it from deepwiki.env." $name) -}}
{{- end -}}
{{- end -}}
{{/*
  Guard #1: the git-host allowlist.

  It is FAIL-CLOSED on both sides — an unset value refuses every clone — so an
  unset allowlist is not a security hole. It is a service that cannot do the
  one thing it exists for, and it fails per invocation, at the point a user
  asked for a wiki. The chart makes the operator state a posture instead.

  `*` is a legitimate posture and is accepted. It has to be written down,
  which is the difference between a decision and an omission.
*/}}
{{- if not (get $env "ELITEA_DEEPWIKI_GIT_ALLOWLIST") -}}
{{- fail "deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST is empty, and the allowlist is fail-closed: every clone would be refused, per invocation, at the point a user asked for a wiki. List the git hosts this deployment may clone from, comma-separated (e.g. \"github.com,*.github.com\"), or write \"*\" to disable the control explicitly. The SAME value is read by elitea-main's facade, which checks it before opening the vault; setting one and not the other is what this chart prevents by rendering both from here." -}}
{{- end -}}

{{/*
  Guard #2: the runner.

  The Python engine is gone (ADR-0026): the sidecar is always the native
  image, running its native or its fixture runner. A value from before that
  change — `legacy` on either switch, or the Python image's own keys — would
  otherwise be read as something else or ignored, so each is refused with
  the setting that replaces it.
*/}}
{{- $runner := get $env "ELITEA_DEEPWIKI_RUNNER" | toString -}}
{{- if eq $runner "legacy" -}}
{{- fail "deepwiki.env.ELITEA_DEEPWIKI_RUNNER is \"legacy\", which named the Python engine sidecar. That engine is removed and the sidecar is now the native image (ADR-0026). Set it to \"native\" (the default) to run the sidecar, and choose its runner with deepwiki.engine.runner (native or fixture). See docs/UPGRADING.md." -}}
{{- end -}}
{{- $engine := .Values.deepwiki.engine | default dict -}}
{{- if or (hasKey $engine "image") (hasKey $engine "resources") -}}
{{- fail "deepwiki.engine.image and deepwiki.engine.resources configured the Python engine sidecar, which is removed (ADR-0026). The sidecar is the native image: set deepwiki.engine.native.image and deepwiki.engine.native.resources instead, and remove the old keys. See docs/UPGRADING.md." -}}
{{- end -}}
{{- $sidecar := include "elitea-deepwiki.sidecar" . -}}

{{/*
  Guard #3: the native engine's database.

  The Rust engine stages and publishes every index in the deepwiki PostgreSQL
  database (ADR-0026 decision 5) and has no other index storage, so it
  REFUSES to start without ELITEA_DEEPWIKI_DATABASE_URL. Without this guard
  that refusal is a sidecar in CrashLoopBackOff and a host that never becomes
  ready. The URL must come from the place the migrate Job reads it:
  deepwiki.secrets.ELITEA_DEEPWIKI_DATABASE_URL, else postgresql.existingSecret.
  A plain deepwiki.env value is not enough: the Job never receives it, so the
  pre-install migration would fail after a render that passed.
*/}}
{{- if and (eq $sidecar "native") (eq (.Values.deepwiki.engine.runner | toString) "native") -}}
{{- $secrets := fromJson (include "elitea.provider.databaseSecret" (dict "ctx" . "provider" "deepwiki" "urlKey" "ELITEA_DEEPWIKI_DATABASE_URL")) -}}
{{- if not (hasKey $secrets "ELITEA_DEEPWIKI_DATABASE_URL") -}}
{{- fail "deepwiki.engine.runner is \"native\" but no ELITEA_DEEPWIKI_DATABASE_URL secret reaches the engine and its migrate Job: neither deepwiki.secrets.ELITEA_DEEPWIKI_DATABASE_URL nor postgresql.existingSecret is set (a plain deepwiki.env value reaches the engine but not the migrate Job). The native engine stages and publishes every index in the deepwiki PostgreSQL database and has no other storage, so it refuses to start without one, and the pod would never become ready. Name the secret that holds the database URL, or use runner fixture." -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea-deepwiki.sidecar — whether the pod runs the engine sidecar:

  none    deepwiki.env.ELITEA_DEEPWIKI_RUNNER is anything but native
          (unavailable: the host refuses every tool; fixture: the host's own
          canned results). No sidecar is rendered.
  native  ELITEA_DEEPWIKI_RUNNER is native: the host dials the Rust engine
          (elitea-deepwiki-engine-native, ADR-0026) over the shared socket.
          The sidecar's own runner is deepwiki.engine.runner: native (the
          engine) or fixture (its canned results, no database needed).
*/}}
{{- define "elitea-deepwiki.sidecar" -}}
{{- $runner := get (.Values.deepwiki.env | default dict) "ELITEA_DEEPWIKI_RUNNER" | toString -}}
{{- $engineRunner := .Values.deepwiki.engine.runner | toString -}}
{{- if eq $engineRunner "legacy" -}}
{{- fail "deepwiki.engine.runner is \"legacy\", which ran the Python engine sidecar. That engine is removed (ADR-0026): set native (the default) to run the native engine, or fixture for its canned results. See docs/UPGRADING.md." -}}
{{- end -}}
{{- if not (has $engineRunner (list "native" "fixture")) -}}
{{- fail (printf "deepwiki.engine.runner must be native or fixture, got %q" $engineRunner) -}}
{{- end -}}
{{- if eq $runner "native" -}}
native
{{- else -}}
none
{{- end -}}
{{- end }}

{{/*
elitea-deepwiki.nativeTag — the native engine's image tag: the value set,
else the chart-wide tag. It is its own repository, so no suffix.
*/}}
{{- define "elitea-deepwiki.nativeTag" -}}
{{- .Values.deepwiki.engine.native.image.tag | default .Values.image.tag | toString -}}
{{- end }}

{{/*
elitea-deepwiki.nativeWorkerMemoryBytes — ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES
for the native engine, DERIVED from the container's memory limit so the two
cannot disagree: 85 % of it, the share the engine itself takes of its cgroup
`memory.max` when the variable is unset (config.rs CGROUP_MEMORY_PERCENT).
The rest is for the serving parent (the in-process ask, deep_research and
resolve_wiki, the pools) and the page cache, which share the container
limit with the worker. The worker child's RLIMIT_AS counts address space,
which is never below resident memory, so a runaway generation fails inside
the engine as MemoryError / out_of_memory (a result the caller can read)
before the kubelet OOM-kills the container (a sidecar restart and every
in-flight invocation lost). Only Gi and Mi quantities are accepted, and the
cap must be at least 1 GiB (the engine refuses a smaller one), so the limit
must be at least 1205Mi.
*/}}
{{- define "elitea-deepwiki.nativeWorkerMemoryBytes" -}}
{{- $limit := (((.Values.deepwiki.engine.native.resources | default dict).limits | default dict).memory | default "") | toString -}}
{{- $bytes := 0 -}}
{{- if regexMatch "^[0-9]+Gi$" $limit -}}
{{- $bytes = mul (trimSuffix "Gi" $limit | atoi) 1073741824 -}}
{{- else if regexMatch "^[0-9]+Mi$" $limit -}}
{{- $bytes = mul (trimSuffix "Mi" $limit | atoi) 1048576 -}}
{{- else -}}
{{- fail (printf "deepwiki.engine.native.resources.limits.memory must be a whole number of Gi or Mi (it sets the generation worker's address-space cap, ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES), got %q" $limit) -}}
{{- end -}}
{{- $cap := mul (div (int64 $bytes) 100) 85 -}}
{{- if lt (int64 $cap) 1073741824 -}}
{{- fail (printf "deepwiki.engine.native.resources.limits.memory is %s, so the worker's address-space cap (85%% of it) is below 1 GiB, which the engine refuses because a generation worker cannot start its thread pools in less. Set at least 1205Mi" $limit) -}}
{{- end -}}
{{- $cap -}}
{{- end }}

