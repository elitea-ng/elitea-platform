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
elitea-deepwiki.validateGuards — the two settings that are silently wrong
rather than loudly missing.

Both are checked at `helm template` time and not only at container start,
because the container's own refusal is a CrashLoopBackOff an operator has to
go read logs for, and this is a message in the terminal that ran the command.
*/}}
{{- define "elitea-deepwiki.validateGuards" -}}
{{- $env := .Values.deepwiki.env | default dict -}}

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

  The published image carries the engine SOURCE but not its dependency
  closure, so the default runner REFUSES every tool. That is correct for the
  default image and wrong for an operator who set `runner: legacy` expecting
  work to happen — they need the engine image, and nothing else in this chart
  would tell them.
*/}}
{{- $runner := get $env "ELITEA_DEEPWIKI_RUNNER" | toString -}}
{{- $sidecar := include "elitea-deepwiki.sidecar" . -}}
{{- $engineTag := include "elitea-deepwiki.engineTag" . -}}
{{- if and (eq $sidecar "python") (eq $runner "legacy") (not (contains "-engine" $engineTag)) -}}
{{- fail (printf "deepwiki.env.ELITEA_DEEPWIKI_RUNNER is \"legacy\" but the engine sidecar's image tag (%s) does not end in \"-engine\". The plain elitea-deepwiki image carries the engine SOURCE and not its ~92-package closure (torch, transformers, faiss-cpu, tree-sitter), so the sidecar cannot import it and every tool fails at invocation time rather than at start. Set deepwiki.engine.image.tag to the -engine variant (the default derives it from the chart-wide tag), or set the runner to unavailable." $engineTag) -}}
{{- end -}}

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
{{- if eq $sidecar "native" -}}
{{- $secrets := fromJson (include "elitea.provider.databaseSecret" (dict "ctx" . "provider" "deepwiki" "urlKey" "ELITEA_DEEPWIKI_DATABASE_URL")) -}}
{{- if not (hasKey $secrets "ELITEA_DEEPWIKI_DATABASE_URL") -}}
{{- fail "deepwiki.engine.runner is \"native\" but no ELITEA_DEEPWIKI_DATABASE_URL secret reaches the engine and its migrate Job: neither deepwiki.secrets.ELITEA_DEEPWIKI_DATABASE_URL nor postgresql.existingSecret is set (a plain deepwiki.env value reaches the engine but not the migrate Job). The native engine stages and publishes every index in the deepwiki PostgreSQL database and has no other storage, so it refuses to start without one, and the pod would never become ready. Name the secret that holds the database URL, or use runner legacy or fixture." -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea-deepwiki.sidecar — which engine sidecar the pod runs, from the two
runner settings:

  none    deepwiki.env.ELITEA_DEEPWIKI_RUNNER is anything but legacy or native
          (unavailable: the host refuses every tool; fixture: the host's own
          canned results). No sidecar is rendered.
  native  deepwiki.engine.runner is native: the Rust engine
          (elitea-deepwiki-engine-native, ADR-0026). The host's runner is
          rendered as native whatever env says, so the one switch cannot
          leave the host dialling a Python engine that is not there.
  python  otherwise: the Python -engine image, its runner the
          deepwiki.engine.runner value (legacy or fixture).

A host runner of native with a Python sidecar is refused: it names the
native engine and would talk to the Python one.
*/}}
{{- define "elitea-deepwiki.sidecar" -}}
{{- $runner := get (.Values.deepwiki.env | default dict) "ELITEA_DEEPWIKI_RUNNER" | toString -}}
{{- $engineRunner := .Values.deepwiki.engine.runner | toString -}}
{{- if not (has $engineRunner (list "legacy" "fixture" "native")) -}}
{{- fail (printf "deepwiki.engine.runner must be legacy, fixture or native, got %q" $engineRunner) -}}
{{- end -}}
{{- if not (has $runner (list "legacy" "native")) -}}
none
{{- else if eq $engineRunner "native" -}}
native
{{- else if eq $runner "native" -}}
{{- fail (printf "deepwiki.env.ELITEA_DEEPWIKI_RUNNER is \"native\" but deepwiki.engine.runner is %q, which runs the PYTHON sidecar. Set deepwiki.engine.runner to native: that one switch selects the native image and renders the host's runner." $engineRunner) -}}
{{- else -}}
python
{{- end -}}
{{- end }}

{{/*
elitea-deepwiki.hostRunner — the host's ELITEA_DEEPWIKI_RUNNER: native when
the native sidecar runs, else the env value as written.
*/}}
{{- define "elitea-deepwiki.hostRunner" -}}
{{- if eq (include "elitea-deepwiki.sidecar" .) "native" -}}
native
{{- else -}}
{{- get (.Values.deepwiki.env | default dict) "ELITEA_DEEPWIKI_RUNNER" | toString -}}
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

{{/*
elitea-deepwiki.engineTag — the engine sidecar's image tag: the value set,
else the chart-wide tag with "-engine" appended, which is how the
`-engine` variant is published.
*/}}
{{- define "elitea-deepwiki.engineTag" -}}
{{- if .Values.deepwiki.engine.image.tag -}}
{{- .Values.deepwiki.engine.image.tag -}}
{{- else -}}
{{- printf "%s-engine" (.Values.image.tag | toString) -}}
{{- end -}}
{{- end }}
