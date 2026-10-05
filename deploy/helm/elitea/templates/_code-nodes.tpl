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
{{- $owner := $runtime.codeOwnerRecovery | default dict -}}
{{- $workspace := $runtime.codeWorkspace | default dict -}}
{{- $platform := $runtime.codePlatform | default dict -}}
{{- $debug := $runtime.codeDebugArtifacts | default dict -}}
{{- $fields := dict "codeOwnerRecovery" (list "enabled" "mainWorkloadIdentity" "supervisors") "codeWorkspace" (list "enabled" "repositoryCapabilities" "egressAllowlist" "policy") "codePlatform" (list "enabled" "brokerPolicies") "codeDebugArtifacts" (list "enabled") -}}
{{- range $name, $settings := dict "codeOwnerRecovery" $owner "codeWorkspace" $workspace "codePlatform" $platform "codeDebugArtifacts" $debug -}}
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
{{- if and (not $owner.enabled) (or $owner.mainWorkloadIdentity $owner.supervisors) -}}
{{- fail "Code owner settings require codeOwnerRecovery.enabled=true" -}}
{{- end -}}
{{- if and (not $workspace.enabled) (or $workspace.repositoryCapabilities $workspace.egressAllowlist $workspace.policy) -}}
{{- fail "Code workspace settings require codeWorkspace.enabled=true" -}}
{{- end -}}
{{- if and (not $platform.enabled) $platform.brokerPolicies -}}
{{- fail "Code broker policies require codePlatform.enabled=true" -}}
{{- end -}}
{{- if or $owner.enabled $workspace.enabled $platform.enabled $debug.enabled -}}
{{- if or (not $runtime.enabled) (not ($runtime.agentExecutionDispatch | default dict).enabled) (not $owner.enabled) (eq (len ($runtime.sandboxAudiences | default list)) 0) -}}
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
{{- if or (not (has $supervisor.audience $runtime.sandboxAudiences)) (hasKey $seen $supervisor.audience) (not (regexMatch "^https://[^/?#@[:space:]]+/?$" $supervisor.httpsOrigin)) -}}
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

{{- define "elitea.codeNodes.env" -}}
{{- $runtime := .Values.main.runtime -}}
{{- $dir := $runtime.material.mountPath | trimSuffix "/" -}}
{{- $owner := $runtime.codeOwnerRecovery | default dict -}}
{{- if $owner.enabled -}}
{{- $supervisors := list -}}
{{- range $supervisor := $owner.supervisors -}}
{{- $supervisors = append $supervisors (dict "audience" $supervisor.audience "https_origin" $supervisor.httpsOrigin) -}}
{{- end }}
ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED: "true"
ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG: {{ dict "main_workload_identity" $owner.mainWorkloadIdentity "certificate_chain_path" (printf "%s/code-owner-client.crt" $dir) "private_key_path" (printf "%s/code-owner-client.key" $dir) "server_ca_path" (printf "%s/runtime-ca.crt" $dir) "supervisors" $supervisors | toJson | quote }}
{{- end -}}
{{- $workspace := $runtime.codeWorkspace | default dict -}}
{{- if $workspace.enabled }}
ELITEA_RUNTIME_CODE_WORKSPACE_ENABLED: "true"
ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG: {{ dict "revision" 1 "repository_capabilities" $workspace.repositoryCapabilities "egress_allowlist" $workspace.egressAllowlist "policy" $workspace.policy | toJson | quote }}
{{- end -}}
{{- $platform := $runtime.codePlatform | default dict -}}
{{- if $platform.enabled }}
ELITEA_RUNTIME_CODE_PLATFORM_ENABLED: "true"
ELITEA_RUNTIME_CODE_PLATFORM_CONFIG: {{ dict "revision" 1 "content_keys_file" (printf "%s/code-platform-content-keys.json" $dir) "broker_policies" $platform.brokerPolicies | toJson | quote }}
{{- end -}}
{{- if ($runtime.codeDebugArtifacts | default dict).enabled }}
ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED: "true"
ELITEA_CODE_DEBUG_AGENTSTATE_DSN_FILE: {{ printf "%s/agent-checkpoint-connection" $dir | quote }}
{{- end -}}
{{- end -}}
