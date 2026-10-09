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
elitea-deepwiki.callbackBaseUrl — the origin the provider calls back to: an
explicit main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL, else platform-edge when
deepwiki.callbackViaPlatformEdge is on, else empty (which validateDeepWiki
refuses for an enabled facade).
*/}}
{{- define "elitea-deepwiki.callbackBaseUrl" -}}
{{- $explicit := get (.Values.main.env | default dict) "ELITEA_DEEPWIKI_CALLBACK_BASE_URL" | default "" | toString -}}
{{- if $explicit -}}
{{- $explicit -}}
{{- else if .Values.deepwiki.callbackViaPlatformEdge -}}
{{- .Values.worker.runtime.platformOrigin -}}
{{- end -}}
{{- end -}}

{{/* Where both containers find the runtime CA for the callback hop. */}}
{{- define "elitea-deepwiki.runtimeCaPath" -}}/run/elitea-runtime-ca{{- end -}}

{{- define "elitea-deepwiki.validateGuards" -}}
{{- $env := .Values.deepwiki.env | default dict -}}

{{/*
  Guard #0: the callback hop through platform-edge (ADR-0027). The edge is a
  worker component, so without the worker there is nothing to dial; and an
  explicit callback URL that is not the edge would leave the runtime CA
  mounted for a hop that never uses it.
*/}}
{{- if .Values.deepwiki.callbackViaPlatformEdge -}}
{{- if not (and .Values.worker.enabled .Values.worker.platformEdge.enabled) -}}
{{- fail "deepwiki.callbackViaPlatformEdge is on, but platform-edge is not rendered: it needs worker.enabled and worker.platformEdge.enabled. Turn those on, or leave the callback hop on main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL (cleartext in-cluster)." -}}
{{- end -}}
{{- $explicit := get (.Values.main.env | default dict) "ELITEA_DEEPWIKI_CALLBACK_BASE_URL" | default "" | toString -}}
{{- if and $explicit (ne $explicit (.Values.worker.runtime.platformOrigin | toString)) -}}
{{- fail (printf "deepwiki.callbackViaPlatformEdge is on, so the callback hop is %s, but main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL says %q. Leave it unset (the chart fills it in) or turn the flag off." .Values.worker.runtime.platformOrigin $explicit) -}}
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

