{{/*
  Durability defaults (PLAN §8.3): the Code runtime is ON wherever the agent
  runtime runs, with the canonical shape of the standalone compose stack
  (deploy/scripts/code-material.py holds the same table). Values only carry
  deployment MATERIAL; an empty setting means the canonical one, and missing
  material fails the render with the value's name.

  elitea.agentRuntime.on   main.runtime.enabled + agentExecutionDispatch
  elitea.sandbox.on        the agent runtime + worker.enabled
  elitea.sandbox.kubernetes  the chart's own sandbox objects render
  elitea.code.settings     Main's effective Code consumer settings (JSON)
  elitea.sandbox.runtimes  the worker's effective sandbox_runtimes (JSON list)
  elitea.sandbox.profileConfigs  the rendered supervisor configs (JSON map)
*/}}

{{- define "elitea.agentRuntime.on" -}}
{{- $r := .Values.main.runtime | default dict -}}
{{- if and $r.enabled ($r.agentExecutionDispatch | default dict).enabled -}}true{{- end -}}
{{- end -}}

{{- define "elitea.sandbox.on" -}}
{{- if and .Values.worker.enabled (include "elitea.agentRuntime.on" .) -}}true{{- end -}}
{{- end -}}

{{- define "elitea.sandbox.kubernetes" -}}
{{- if and (include "elitea.sandbox.on" .) .Values.sandboxKubernetes.enabled -}}true{{- end -}}
{{- end -}}

{{- define "elitea.sandbox.supervisorOn" -}}
{{- if and (include "elitea.sandbox.kubernetes" .) .Values.sandboxKubernetes.supervisor.enabled -}}true{{- end -}}
{{- end -}}

{{/* The canonical Main-side table (CONTRACT, compose). */}}
{{- define "elitea.code.canonical" -}}
{{- toJson (dict
  "identity" "dns:elitea-code-owner-main"
  "audiences" (list "dns:elitea-sandbox-deno" "dns:elitea-sandbox-rust" "dns:elitea-sandbox-deno-native" "dns:elitea-sandbox-preparation" "dns:elitea-sandbox-javascript-preparation" "dns:elitea-sandbox-typescript-preparation" "dns:elitea-sandbox-rust-preparation")
  "supervisors" (list
    (dict "audience" "dns:elitea-sandbox-deno" "httpsOrigin" "https://elitea-sandbox-deno:9446")
    (dict "audience" "dns:elitea-sandbox-deno-native" "httpsOrigin" "https://elitea-sandbox-deno-native:9450")
    (dict "audience" "dns:elitea-sandbox-rust" "httpsOrigin" "https://elitea-sandbox-rust:9447"))
  "repositoryCapabilities" (list "github")
  "egressAllowlist" (list "api.github.com:443")
  "policy" (dict "revision" 1 "max_files" 1024 "max_file_bytes" 1048576 "max_total_bytes" 16777216 "max_manifest_bytes" 524288 "max_path_bytes" 255 "max_depth" 16 "max_projections" 64 "max_acquisition_seconds" 300)
  "brokerPolicies" (list (dict "revision" 1 "max_calls" 32 "max_total_bytes" 1048576))
) -}}
{{- end -}}

{{/*
  Main's effective Code consumers. Original-Code owner recovery, the Code
  workspace, the Code platform broker and Code debug artifacts are always on
  with the agent runtime (PLAN §8.7: no gates, none has an enable bit). An
  empty setting takes the canonical value. Explicit settings without the agent runtime are refused:
  they would configure nothing.
*/}}
{{- define "elitea.code.settings" -}}
{{- $runtime := .Values.main.runtime | default dict -}}
{{- $on := include "elitea.agentRuntime.on" . -}}
{{- $canonical := include "elitea.code.canonical" . | fromJson -}}
{{- $owner := $runtime.codeOwnerRecovery | default dict -}}
{{- $workspace := $runtime.codeWorkspace | default dict -}}
{{- $platform := $runtime.codePlatform | default dict -}}
{{- $debug := $runtime.codeDebugArtifacts | default dict -}}
{{- if and (not $on) (or $owner.mainWorkloadIdentity $owner.supervisors $workspace.repositoryCapabilities $workspace.egressAllowlist $workspace.policy $platform.brokerPolicies) -}}
{{- fail "main.runtime Code consumer settings (codeOwnerRecovery, codeWorkspace, codePlatform) apply only where the agent runtime runs: set main.runtime.enabled and main.runtime.agentExecutionDispatch.enabled, or clear them." -}}
{{- end -}}
{{- $result := dict -}}
{{- $o := dict "enabled" (eq (toString $on) "true") -}}
{{- if $o.enabled -}}
{{- $_ := set $o "mainWorkloadIdentity" ($owner.mainWorkloadIdentity | default $canonical.identity) -}}
{{- $_ := set $o "supervisors" (ternary $owner.supervisors $canonical.supervisors (not (empty $owner.supervisors))) -}}
{{- end -}}
{{- $w := dict "enabled" (eq (toString $on) "true") -}}
{{- if $w.enabled -}}
{{- $_ := set $w "repositoryCapabilities" (ternary $workspace.repositoryCapabilities $canonical.repositoryCapabilities (not (empty $workspace.repositoryCapabilities))) -}}
{{- $_ := set $w "egressAllowlist" (ternary $workspace.egressAllowlist $canonical.egressAllowlist (not (empty $workspace.egressAllowlist))) -}}
{{- $_ := set $w "policy" (ternary $workspace.policy $canonical.policy (not (empty $workspace.policy))) -}}
{{- end -}}
{{- $p := dict "enabled" (eq (toString $on) "true") -}}
{{- if $p.enabled -}}
{{- $_ := set $p "brokerPolicies" (ternary $platform.brokerPolicies $canonical.brokerPolicies (not (empty $platform.brokerPolicies))) -}}
{{- end -}}
{{- $d := dict "enabled" (eq (toString $on) "true") -}}
{{- $audiences := $runtime.sandboxAudiences | default list -}}
{{- if and (empty $audiences) (or $o.enabled $w.enabled $p.enabled $d.enabled) -}}
{{- $audiences = $canonical.audiences -}}
{{- end -}}
{{- toJson (dict "owner" $o "workspace" $w "platform" $p "debug" $d "audiences" $audiences) -}}
{{- end -}}

{{- define "elitea.sandbox.digest" -}}
{{- if not (regexMatch "^[^[:space:]@]+@sha256:[a-f0-9]{64}$" (toString .value)) -}}
{{- fail (printf "%s is required: the Code sandbox is on wherever the agent runtime runs (main.runtime.enabled, main.runtime.agentExecutionDispatch.enabled and worker.enabled). Set it to %s by registry digest (registry/name@sha256:<64 hex>)." .path .what) -}}
{{- end -}}
{{- end -}}

{{/* runnerImages.<runner>, checked; prints the image reference. */}}
{{- define "elitea.sandbox.runnerImage" -}}
{{- $images := .ctx.Values.sandboxKubernetes.runnerImages | default dict -}}
{{- if not (hasKey $images .runner) -}}
{{- fail (printf "sandboxKubernetes profile runner %q is not a key of sandboxKubernetes.runnerImages (deno, rust)" .runner) -}}
{{- end -}}
{{- include "elitea.sandbox.digest" (dict "path" (printf "sandboxKubernetes.runnerImages.%s" .runner) "value" (get $images .runner) "what" (printf "the %s Code runner image" .runner)) -}}
{{- get $images .runner -}}
{{- end -}}

{{- define "elitea.sandbox.nativePlatform" -}}
{{- $arch := .Values.sandboxKubernetes.nativePlatformArch | toString -}}
{{- if not (has $arch (list "amd64" "arm64")) -}}
{{- fail (printf "sandboxKubernetes.nativePlatformArch must be amd64 or arm64, not %q" $arch) -}}
{{- end -}}
{{- toJson (dict "os" "linux" "arch" $arch "abi" "gnu") -}}
{{- end -}}

{{/* The chart-rendered profiles (entries with `name`), validated, as a JSON map name -> entry. */}}
{{- define "elitea.sandbox.renderedProfiles" -}}
{{- $result := dict -}}
{{- range $entry := .Values.sandboxKubernetes.supervisor.profiles -}}
{{- if not (kindIs "map" $entry) -}}{{- fail "sandboxKubernetes.supervisor.profiles entries must be objects" -}}{{- end -}}
{{- if and (hasKey $entry "name") (hasKey $entry "file") -}}{{- fail "a sandboxKubernetes.supervisor.profiles entry has either `name` (rendered by the chart) or `file` (read from the material Secret), not both" -}}{{- end -}}
{{- if hasKey $entry "name" -}}
{{- $name := toString $entry.name -}}
{{- if not (regexMatch "^[a-z]([a-z0-9-]{0,46}[a-z0-9])?$" $name) -}}{{- fail (printf "sandboxKubernetes profile name %q must be a DNS label of at most 48 characters" $name) -}}{{- end -}}
{{- if hasKey $result $name -}}{{- fail (printf "sandboxKubernetes profile name %q is not unique" $name) -}}{{- end -}}
{{- if hasKey $entry "compiled_snapshot" -}}{{- fail (printf "sandboxKubernetes profile %q: compiled_snapshot is supported on `file` profiles only (rustCompiledSnapshots)" $name) -}}{{- end -}}
{{- range $key := list "port" "serviceName" "purpose" "languages" "policyRevision" "runner" "timeoutSeconds" "memoryMiB" -}}
{{- if not (hasKey $entry $key) -}}{{- fail (printf "sandboxKubernetes profile %q needs %s" $name $key) -}}{{- end -}}
{{- end -}}
{{- range $key, $_ := $entry -}}
{{- if not (has $key (list "name" "port" "serviceName" "purpose" "languages" "policyRevision" "runner" "timeoutSeconds" "memoryMiB" "nativeWorkspaceMiB" "codePlatformProfile")) -}}{{- fail (printf "sandboxKubernetes profile %q has the unknown field %s" $name $key) -}}{{- end -}}
{{- end -}}
{{- if not (has (toString $entry.purpose) (list "execution" "preparation")) -}}{{- fail (printf "sandboxKubernetes profile %q purpose must be execution or preparation" $name) -}}{{- end -}}
{{- if or (not (kindIs "slice" $entry.languages)) (empty $entry.languages) -}}{{- fail (printf "sandboxKubernetes profile %q needs a non-empty languages list" $name) -}}{{- end -}}
{{- if not (regexMatch "^[a-zA-Z0-9_.-]{1,128}$" (toString $entry.policyRevision)) -}}{{- fail (printf "sandboxKubernetes profile %q policyRevision must be 1-128 of [a-zA-Z0-9_.-]" $name) -}}{{- end -}}
{{- range $key := list "timeoutSeconds" "memoryMiB" -}}
{{- if or (not (regexMatch "^[1-9][0-9]{0,5}$" (toString (get $entry $key)))) -}}{{- fail (printf "sandboxKubernetes profile %q %s must be a positive integer" $name $key) -}}{{- end -}}
{{- end -}}
{{- $native := int64 (default 0 $entry.nativeWorkspaceMiB) -}}
{{- if and (gt $native 0) (or (lt $native 512) (gt $native 4096) (lt (int64 $entry.memoryMiB) (add $native 128))) -}}{{- fail (printf "sandboxKubernetes profile %q nativeWorkspaceMiB must be 0 or 512-4096, with memoryMiB at least nativeWorkspaceMiB + 128" $name) -}}{{- end -}}
{{- if and $entry.codePlatformProfile (ne (toString $entry.purpose) "execution") -}}{{- fail (printf "sandboxKubernetes profile %q: codePlatformProfile is for execution profiles only" $name) -}}{{- end -}}
{{- $_ := set $result $name $entry -}}
{{- end -}}
{{- end -}}
{{- toJson $result -}}
{{- end -}}

{{/*
  The supervisor configuration of every rendered profile (process.rs Config),
  as a JSON map name -> config. Certificates and database files come from the
  material Secret, prepared into /run/elitea-sandbox.
*/}}
{{- define "elitea.sandbox.profileConfigs" -}}
{{- $s := .Values.sandboxKubernetes -}}
{{- $base := "/run/elitea-sandbox" -}}
{{- $code := include "elitea.code.settings" . | fromJson -}}
{{- $identity := ($code.owner.mainWorkloadIdentity | default (include "elitea.code.canonical" . | fromJson).identity) -}}
{{- $staging := dict -}}
{{- range $s.supervisor.contentStaging -}}{{- $_ := set $staging (toString .name) true -}}{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$" (toString $s.cluster)) -}}{{- fail "sandboxKubernetes.cluster must be a DNS-like name (the cluster's durable receipt identity)" -}}{{- end -}}
{{- if not (regexMatch "^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$" (toString $s.runtimeClass)) -}}{{- fail "sandboxKubernetes.runtimeClass must name a RuntimeClass (a DNS label)" -}}{{- end -}}
{{- if or (empty $s.nodeSelector) (gt (len $s.nodeSelector) 16) -}}{{- fail "sandboxKubernetes.nodeSelector must select the sandbox nodes explicitly (one to sixteen labels)" -}}{{- end -}}
{{- $result := dict -}}
{{- range $name, $p := include "elitea.sandbox.renderedProfiles" . | fromJson -}}
{{- if not (hasKey $staging $name) -}}{{- fail (printf "sandboxKubernetes profile %q needs a sandboxKubernetes.supervisor.contentStaging entry named %q" $name $name) -}}{{- end -}}
{{- $execution := eq (toString $p.purpose) "execution" -}}
{{- $image := include "elitea.sandbox.runnerImage" (dict "ctx" $ "runner" (toString $p.runner)) -}}
{{- $namespace := $s.executionNamespace -}}
{{- if not $execution -}}
{{- if not $s.preparation.enabled -}}{{- fail (printf "sandboxKubernetes profile %q is a preparation profile and needs sandboxKubernetes.preparation.enabled=true (its Pods run in the preparation namespace)" $name) -}}{{- end -}}
{{- $namespace = $s.preparation.namespace -}}
{{- end -}}
{{- $svc := toString $p.serviceName -}}
{{- $config := dict
  "revision" 1
  "purpose" (toString $p.purpose)
  "listen_address" (printf "0.0.0.0:%d" (int $p.port))
  "owner" (printf "%s-%s" $.Release.Name $svc)
  "audience" (printf "dns:%s" $svc)
  "ca_path" (printf "%s/runtime-ca.crt" $base)
  "certificate_path" (printf "%s/%s.crt" $base $svc)
  "private_key_path" (printf "%s/%s.key" $base $svc)
  "verification_keyring_path" (printf "%s/command-signing-keyring.json" $base)
  "database_url_path" (printf "%s/database-url" $base)
  "database_ca_path" (printf "%s/database-ca.crt" $base)
  "database_connections" (ternary 4 2 $execution)
  "image_digest" (last (splitList "@" $image))
  "policy_revision" (toString $p.policyRevision)
  "languages" $p.languages
  "concurrency" (ternary 4 1 $execution)
  "memory_bytes" (mul (int64 $p.memoryMiB) 1048576)
  "cpu_limit" 1.0
  "timeout_seconds" (int64 $p.timeoutSeconds)
  "dependency_content" (dict
    "origin" $.Values.worker.runtime.contentOrigin
    "ca_path" (printf "%s/runtime-ca.crt" $base)
    "certificate_path" (printf "%s/%s-content-client.crt" $base $svc)
    "private_key_path" (printf "%s/%s-content-client.key" $base $svc)
    "staging_root" (printf "/run/elitea-sandbox-content/%s/private" $name)
    "capacity" 1
    "timeout_seconds" 30)
  "backend" (dict
    "kind" "kubernetes"
    "cluster" (toString $s.cluster)
    "namespace" $namespace
    "image" $image
    "runtime_class" (toString $s.runtimeClass)
    "node_selector" $s.nodeSelector)
-}}
{{- if gt (int64 (default 0 $p.nativeWorkspaceMiB)) 0 -}}
{{- $_ := set $config "native_platform" (include "elitea.sandbox.nativePlatform" $ | fromJson) -}}
{{- $_ := set $config "native_workspace_bytes" (mul (int64 $p.nativeWorkspaceMiB) 1048576) -}}
{{- end -}}
{{- if $execution -}}
{{- $_ := set $config "code_owner_requester" $identity -}}
{{- if $p.codePlatformProfile -}}{{- $_ := set $config "code_platform_profile" true -}}{{- end -}}
{{- end -}}
{{- $_ := set $result $name $config -}}
{{- end -}}
{{- toJson $result -}}
{{- end -}}

{{/*
  The worker's effective sandbox_runtimes: the operator's explicit list, or,
  where the Code sandbox is on, the four canonical languages derived from the
  chart's own supervisor profiles (CONTRACT "Worker sandbox_runtimes").
*/}}
{{- define "elitea.sandbox.runtimes" -}}
{{- $explicit := .Values.worker.runtime.sandboxRuntimes | default list -}}
{{- if not (empty $explicit) -}}
{{- toJson $explicit -}}
{{- else if include "elitea.sandbox.on" . -}}
{{- if not (include "elitea.sandbox.supervisorOn" .) -}}
{{- fail "the Code sandbox is on wherever the agent runtime runs: with sandboxKubernetes.enabled or sandboxKubernetes.supervisor.enabled false, set worker.runtime.sandboxRuntimes to the supervisors you deploy yourself." -}}
{{- end -}}
{{- $profiles := include "elitea.sandbox.renderedProfiles" . | fromJson -}}
{{- $code := include "elitea.code.settings" . | fromJson -}}
{{- $platform := $code.platform -}}
{{- $result := list -}}
{{- range $row := list (list "python" "deno" "prep" true) (list "javascript" "denonative" "jsprep" true) (list "typescript" "denonative" "tsprep" true) (list "rust" "rust" "rsprep" false) -}}
{{- $language := index $row 0 -}}
{{- range $name := list (index $row 1) (index $row 2) -}}
{{- if not (hasKey $profiles $name) -}}
{{- fail (printf "worker.runtime.sandboxRuntimes is empty, so the chart derives %s from the supervisor profile named %q, which sandboxKubernetes.supervisor.profiles does not render. Restore it, or set worker.runtime.sandboxRuntimes explicitly." $language $name) -}}
{{- end -}}
{{- end -}}
{{- $e := get $profiles (index $row 1) -}}
{{- $target := printf "%s:%d" (toString $e.serviceName) (int $e.port) -}}
{{- $audience := printf "dns:%s" (toString $e.serviceName) -}}
{{- $digest := last (splitList "@" (include "elitea.sandbox.runnerImage" (dict "ctx" $ "runner" (toString $e.runner)))) -}}
{{- $entry := dict "language" $language "target" $target "audience" $audience "image_digest" $digest "policy_revision" (toString $e.policyRevision) "timeout_seconds" (int64 $e.timeoutSeconds) -}}
{{- if and (index $row 3) $e.codePlatformProfile -}}
{{- $policy := first $platform.brokerPolicies -}}
{{- $_ := set $entry "platform_client" (dict "target" $target "audience" $audience "image_digest" $digest "policy_revision" (toString $e.policyRevision) "timeout_seconds" (int64 $e.timeoutSeconds) "max_calls" (int64 $policy.max_calls) "max_total_bytes" (int64 $policy.max_total_bytes)) -}}
{{- end -}}
{{- $prep := get $profiles (index $row 2) -}}
{{- $preparation := dict "target" (printf "%s:%d" (toString $prep.serviceName) (int $prep.port)) "audience" (printf "dns:%s" (toString $prep.serviceName)) "image_digest" (last (splitList "@" (include "elitea.sandbox.runnerImage" (dict "ctx" $ "runner" (toString $prep.runner))))) "policy_revision" (toString $prep.policyRevision) "timeout_seconds" (int64 $prep.timeoutSeconds) -}}
{{- if gt (int64 (default 0 $prep.nativeWorkspaceMiB)) 0 -}}
{{- $_ := set $preparation "native_platform" (include "elitea.sandbox.nativePlatform" $ | fromJson) -}}
{{- end -}}
{{- $_ := set $entry "preparation" $preparation -}}
{{- $result = append $result $entry -}}
{{- end -}}
{{- toJson $result -}}
{{- else -}}
{{- toJson list -}}
{{- end -}}
{{- end -}}

{{/*
  Recovery is not a setting. The worker refuses agent_node_recovery and
  agent_model_checkpoint_recovery in runtime.json, and elitea-main refuses
  the ELITEA_RUNTIME_CODE_*_ENABLED variables; the values that wrote them are
  gone, and a values file that still names one is told so.
*/}}
{{- define "elitea.removedRecoverySettings" -}}
{{- $mainRuntime := .Values.main.runtime | default dict -}}
{{- if hasKey ($mainRuntime.codeOwnerRecovery | default dict) "enabled" -}}
{{- fail "main.runtime.codeOwnerRecovery.enabled was removed: Code owner recovery is always on with Agent dispatch. Delete the value from your values file; mainWorkloadIdentity, supervisors and sandboxAudiences stay configurable." -}}
{{- end -}}
{{- range $key := list "codeWorkspace" "codePlatform" "codeDebugArtifacts" -}}
{{- if hasKey (get $mainRuntime $key | default dict) "enabled" -}}
{{- fail (printf "main.runtime.%s.enabled was removed: the Code consumers are always on with Agent dispatch. Delete the value from your values file; their other settings stay configurable." $key) -}}
{{- end -}}
{{- end -}}
{{- $r := (.Values.worker | default dict).runtime | default dict -}}
{{- if hasKey $r "agentNodeRecovery" -}}
{{- fail "worker.runtime.agentNodeRecovery was removed: node recovery is always on. Delete the value from your values file." -}}
{{- end -}}
{{- if hasKey $r "agentModelCheckpointRecovery" -}}
{{- fail "worker.runtime.agentModelCheckpointRecovery was removed: model-checkpoint recovery is always on. Delete the value from your values file." -}}
{{- end -}}
{{- end -}}

{{/* The rendered supervisor profiles' ConfigMap, release-prefixed. */}}
{{- define "elitea-sandbox.profilesConfigMap" -}}
{{- printf "%s-sandbox-profiles" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
