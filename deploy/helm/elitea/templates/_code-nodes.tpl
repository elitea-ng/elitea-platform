{{/* Code consumer settings come only from the typed operator block. */}}
{{- define "elitea.codeNodes.integer" -}}
{{- $value := .value -}}
{{- if not (or (kindIs "float64" $value) (kindIs "int64" $value) (kindIs "int" $value)) -}}
{{- fail (printf "Code policy %s requires a whole JSON number" .field) -}}
{{- end -}}
{{- if ne (float64 $value) (float64 (int64 $value)) -}}
{{- fail (printf "Code policy %s requires a whole JSON number" .field) -}}
{{- end -}}
{{- $text := printf "%d" (int64 $value) -}}
{{- if or (not (regexMatch "^[1-9][0-9]{0,12}$" $text)) (gt (int64 $value) (int64 .maximum)) -}}
{{- fail (printf "Code policy %s exceeds its positive bound" .field) -}}
{{- end -}}
{{- $text -}}
{{- end -}}

{{- define "elitea.codeNodes.validate" -}}
{{- $runtime := .Values.main.runtime | default dict -}}
{{- $raw := dict "codeOwnerRecovery" ($runtime.codeOwnerRecovery | default dict) "codeWorkspace" ($runtime.codeWorkspace | default dict) "codePlatform" ($runtime.codePlatform | default dict) "codeDebugArtifacts" ($runtime.codeDebugArtifacts | default dict) -}}
{{- include "elitea.removedRecoverySettings" . -}}
{{- $fields := dict "codeOwnerRecovery" (list "mainWorkloadIdentity" "supervisors") "codeWorkspace" (list "enabled" "repositoryCapabilities" "egressAllowlist" "policy") "codePlatform" (list "enabled" "brokerPolicies") "codeDebugArtifacts" (list "enabled") -}}
{{- range $name, $settings := $raw -}}
{{- if not (kindIs "map" $settings) -}}{{- fail (printf "Code settings %s require an object" $name) -}}{{- end -}}
{{- range $field, $_ := $settings -}}
{{- if not (has $field (get $fields $name)) -}}{{- fail (printf "Code settings %s contain an unknown field" $name) -}}{{- end -}}
{{- end -}}
{{- if and (hasKey $settings "enabled") (not (kindIs "bool" $settings.enabled)) -}}
{{- fail (printf "main.runtime.%s.enabled must be boolean" $name) -}}
{{- end -}}
{{- end -}}
{{- range $name, $_ := .Values.main.env -}}
{{- if or (hasPrefix "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_" $name) (hasPrefix "ELITEA_RUNTIME_CODE_WORKSPACE_" $name) (hasPrefix "ELITEA_RUNTIME_CODE_PLATFORM_" $name) (eq $name "ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED") (eq $name "ELITEA_CODE_DEBUG_AGENTSTATE_DSN_FILE") -}}
{{- fail "Code consumer environment must derive from typed chart settings" -}}
{{- end -}}
{{- end -}}
{{- $rawWorkspace := get $raw "codeWorkspace" -}}
{{- $rawPlatform := get $raw "codePlatform" -}}
{{- if and (eq (toString $rawWorkspace.enabled) "false") (or $rawWorkspace.repositoryCapabilities $rawWorkspace.egressAllowlist $rawWorkspace.policy) -}}
{{- fail "Code workspace settings require codeWorkspace.enabled=true" -}}
{{- end -}}
{{- if and (eq (toString $rawPlatform.enabled) "false") $rawPlatform.brokerPolicies -}}
{{- fail "Code broker policies require codePlatform.enabled=true" -}}
{{- end -}}
{{- /* From here on, the effective settings: canonical where empty. */ -}}
{{- $code := include "elitea.code.settings" . | fromJson -}}
{{- $owner := $code.owner -}}
{{- $workspace := $code.workspace -}}
{{- $platform := $code.platform -}}
{{- $debug := $code.debug -}}
{{- $audiences := $code.audiences -}}
{{- if or $owner.enabled $workspace.enabled $platform.enabled $debug.enabled -}}
{{- if or (not $runtime.enabled) (not ($runtime.agentExecutionDispatch | default dict).enabled) (not $owner.enabled) (eq (len $audiences) 0) -}}
{{- fail "Code consumers require runtime, agent dispatch, Code owner recovery, and exact sandbox audiences" -}}
{{- end -}}
{{- if or (not $owner.mainWorkloadIdentity) (not (kindIs "slice" $owner.supervisors)) (eq (len $owner.supervisors) 0) (gt (len $owner.supervisors) 16) -}}
{{- fail "Code owner requires its Main workload identity and one to sixteen supervisors" -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $supervisor := $owner.supervisors -}}
{{- if or (not (kindIs "map" $supervisor)) (ne (len $supervisor) 2) (not $supervisor.audience) (not $supervisor.httpsOrigin) -}}
{{- fail "Code owner supervisors require exactly audience and httpsOrigin" -}}
{{- end -}}
{{- if or (not (has $supervisor.audience $audiences)) (hasKey $seen $supervisor.audience) (not (regexMatch "^https://[^/?#@[:space:]]+/?$" $supervisor.httpsOrigin)) -}}
{{- fail "Code owner supervisors require unique configured audiences and fixed HTTPS origins" -}}
{{- end -}}
{{- $_ := set $seen $supervisor.audience true -}}
{{- end -}}
{{- end -}}
{{- if $workspace.enabled -}}
{{- if or (not (kindIs "slice" $workspace.repositoryCapabilities)) (ne (len $workspace.repositoryCapabilities) 1) (ne (index $workspace.repositoryCapabilities 0) "github") -}}
{{- fail "Code workspace currently requires the github repository capability" -}}
{{- end -}}
{{- if or (not (kindIs "slice" $workspace.egressAllowlist)) (eq (len $workspace.egressAllowlist) 0) (gt (len $workspace.egressAllowlist) 16) (ne (len ($workspace.egressAllowlist | uniq)) (len $workspace.egressAllowlist)) -}}
{{- fail "Code workspace requires one to sixteen distinct host:port egress entries" -}}
{{- end -}}
{{- range $rule := $workspace.egressAllowlist -}}
{{- if or (not (kindIs "string" $rule)) (not (regexMatch "^([a-z0-9.-]+|\\[[0-9a-f:]+\\]):[1-9][0-9]{0,4}$" $rule)) -}}
{{- fail "Code workspace egress requires exact canonical host:port entries without wildcards or CIDRs" -}}
{{- end -}}
{{- if gt (int (last (splitList ":" $rule))) 65535 -}}
{{- fail "Code workspace egress port exceeds 65535" -}}
{{- end -}}
{{- end -}}
{{- if or (not (kindIs "map" $workspace.policy)) (ne (len $workspace.policy) 9) -}}
{{- fail "Code workspace policy requires revision one and its eight bounded limits" -}}
{{- end -}}
{{- $_ := include "elitea.codeNodes.integer" (dict "field" "revision" "value" $workspace.policy.revision "maximum" 1) -}}
{{- range $field, $maximum := dict "max_files" 4096 "max_file_bytes" 4194304 "max_total_bytes" 134217728 "max_manifest_bytes" 4194304 "max_path_bytes" 1024 "max_depth" 64 "max_projections" 128 "max_acquisition_seconds" 3600 -}}
{{- $_ := include "elitea.codeNodes.integer" (dict "field" $field "value" (get $workspace.policy $field) "maximum" $maximum) -}}
{{- end -}}
{{- if or (lt (int64 $workspace.policy.max_total_bytes) (int64 $workspace.policy.max_file_bytes)) (lt (int64 $workspace.policy.max_manifest_bytes) 1024) -}}
{{- fail "Code workspace total bytes must cover one file; manifest bytes must be at least 1024" -}}
{{- end -}}
{{- end -}}
{{- if $platform.enabled -}}
{{- if or (not (kindIs "slice" $platform.brokerPolicies)) (eq (len $platform.brokerPolicies) 0) (gt (len $platform.brokerPolicies) 16) -}}
{{- fail "Code platform requires one to sixteen broker policies" -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $policy := $platform.brokerPolicies -}}
{{- if or (not (kindIs "map" $policy)) (ne (len $policy) 3) -}}
{{- fail "Code broker policies require exactly revision, max_calls, and max_total_bytes" -}}
{{- end -}}
{{- $_ := include "elitea.codeNodes.integer" (dict "field" "revision" "value" $policy.revision "maximum" 1) -}}
{{- range $field, $maximum := dict "max_calls" 4096 "max_total_bytes" 67108864 -}}
{{- $_ := include "elitea.codeNodes.integer" (dict "field" $field "value" (get $policy $field) "maximum" $maximum) -}}
{{- end -}}
{{- $identity := toJson $policy -}}
{{- if hasKey $seen $identity -}}{{- fail "Code broker policies must be distinct" -}}{{- end -}}
{{- $_ := set $seen $identity true -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
  The Rust worker always runs its node attempt journal, so every Code node is
  an original-Code visit that elitea-main admits before any sandbox dispatch,
  and Main admits it only with original-Code owner recovery, signing only for
  its owner supervisors' audiences. Without it each Code node fails at
  admission with the generic runtime failure, and every pod still reports
  Ready. So every execution audience (and a platform_client's) needs an owner
  supervisor. A preparation audience is NOT an owner supervisor: Main only
  signs its grants, so it must be in the sandbox audiences.
*/}}
{{- define "elitea.codeNodes.validateWorker" -}}
{{- $runtimes := include "elitea.sandbox.runtimes" . | fromJsonArray -}}
{{- if and .Values.worker.enabled (gt (len $runtimes) 0) -}}
{{- $code := include "elitea.code.settings" . | fromJson -}}
{{- $owner := $code.owner -}}
{{- if not $owner.enabled -}}
{{- fail "worker.runtime.sandboxRuntimes (the Code sandbox) needs the agent runtime (main.runtime.enabled and main.runtime.agentExecutionDispatch.enabled): the worker's node journal is always on, and Code nodes would be refused without original-Code owner recovery, which elitea-main runs only with Agent dispatch, before any sandbox dispatch." -}}
{{- end -}}
{{- $owned := list -}}
{{- range $supervisor := $owner.supervisors -}}{{- $owned = append $owned $supervisor.audience -}}{{- end -}}
{{- range $profile := $runtimes -}}
{{- $execution := list $profile.audience -}}
{{- if $profile.platform_client -}}{{- $execution = append $execution $profile.platform_client.audience -}}{{- end -}}
{{- range $audience := $execution -}}
{{- if not (has $audience $owned) -}}
{{- fail (printf "worker.runtime.sandboxRuntimes has no main.runtime.codeOwnerRecovery supervisor for audience %q: the node journal is always on, so Code nodes on it would be refused without original-Code owner recovery for that supervisor. Add the supervisor to codeOwnerRecovery.supervisors." (toString $audience)) -}}
{{- end -}}
{{- end -}}
{{- if $profile.preparation -}}
{{- if not (has $profile.preparation.audience $code.audiences) -}}
{{- fail (printf "worker.runtime.sandboxRuntimes preparation audience %q is not in main.runtime.sandboxAudiences: elitea-main would not sign its dependency preparation grants. Preparation audiences need no owner supervisor, only the audience." (toString $profile.preparation.audience)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "elitea.codeNodes.env" -}}
{{- $runtime := .Values.main.runtime -}}
{{- $dir := $runtime.material.mountPath | trimSuffix "/" -}}
{{- $code := include "elitea.code.settings" . | fromJson -}}
{{- $owner := $code.owner -}}
{{- if $owner.enabled -}}
{{- $supervisors := list -}}
{{- range $supervisor := $owner.supervisors -}}
{{- $supervisors = append $supervisors (dict "audience" $supervisor.audience "https_origin" $supervisor.httpsOrigin) -}}
{{- end }}
ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG: {{ dict "main_workload_identity" $owner.mainWorkloadIdentity "certificate_chain_path" (printf "%s/code-owner-client.crt" $dir) "private_key_path" (printf "%s/code-owner-client.key" $dir) "server_ca_path" (printf "%s/runtime-ca.crt" $dir) "supervisors" $supervisors | toJson | quote }}
{{- end -}}
{{- $workspace := $code.workspace -}}
{{- if $workspace.enabled }}
ELITEA_RUNTIME_CODE_WORKSPACE_ENABLED: "true"
ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG: {{ dict "revision" 1 "repository_capabilities" $workspace.repositoryCapabilities "egress_allowlist" $workspace.egressAllowlist "policy" $workspace.policy | toJson | quote }}
{{- end -}}
{{- $platform := $code.platform -}}
{{- if $platform.enabled }}
ELITEA_RUNTIME_CODE_PLATFORM_ENABLED: "true"
ELITEA_RUNTIME_CODE_PLATFORM_CONFIG: {{ dict "revision" 1 "content_keys_file" (printf "%s/code-platform-content-keys.json" $dir) "broker_policies" $platform.brokerPolicies | toJson | quote }}
{{- end -}}
{{- if $code.debug.enabled }}
ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED: "true"
ELITEA_CODE_DEBUG_AGENTSTATE_DSN_FILE: {{ printf "%s/agent-checkpoint-connection" $dir | quote }}
{{- end -}}
{{- end -}}
