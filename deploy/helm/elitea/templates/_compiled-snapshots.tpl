{{/* Validate the opt-in deployment contract without reserializing manifest bytes. */}}
{{- define "elitea.compiledSnapshots.integer" -}}
{{- $value := .value -}}
{{- $text := "" -}}
{{- if kindIs "string" $value }}
{{- $text = $value }}
{{- else if or (kindIs "float64" $value) (kindIs "int64" $value) (kindIs "int" $value) }}
{{- if ne (float64 $value) (float64 (int64 $value)) }}{{ fail "compiled snapshot quotas require whole integers" }}{{ end }}
{{- $text = printf "%d" (int64 $value) }}
{{- else }}{{ fail "compiled snapshot quotas require integers or canonical integer strings" }}
{{- end }}
{{- if or (not (regexMatch "^[1-9][0-9]{0,12}$" $text)) (gt (int64 $text) (int64 .maximum)) }}{{ fail "compiled snapshot quota exceeds its positive bound" }}{{ end }}
{{- $text -}}
{{- end }}

{{- define "elitea.compiledSnapshots.object" -}}
{{- $c := .config -}}
{{- if not (kindIs "map" $c) }}{{ fail "compiled_snapshot must be an object" }}{{ end }}
{{- if ne (len $c) 3 }}{{ fail "compiled_snapshot requires exactly three fields" }}{{ end }}
{{- range $key := list "profiles_file" "profiles_sha256" "dependency_bundle_sha256" }}
{{- if or (not (hasKey $c $key)) (not (kindIs "string" (get $c $key))) }}{{ fail "compiled_snapshot requires three string fields" }}{{ end }}
{{- end }}
{{- if ne $c.profiles_file .path }}{{ fail "compiled_snapshot profiles_file must use its existing material directory" }}{{ end }}
{{- if or (not (regexMatch "^[a-f0-9]{64}$" $c.profiles_sha256)) (ne $c.profiles_sha256 .pin) }}{{ fail "compiled_snapshot must match the Main manifest pin" }}{{ end }}
{{- if and (ne $c.dependency_bundle_sha256 "") (not (regexMatch "^[a-f0-9]{64}$" $c.dependency_bundle_sha256)) }}{{ fail "compiled_snapshot bundle root must be empty or lowercase SHA-256" }}{{ end }}
{{- end }}

{{- define "elitea.compiledSnapshots.platform" -}}
{{- if not (kindIs "map" .) }}{{ fail "compiled Cargo native_platform must be an object" }}{{ end }}
{{- if or (ne (len .) 3) (ne .os "linux") (not (has .arch (list "arm64" "amd64"))) (ne .abi "gnu") }}{{ fail "compiled Cargo native_platform must select Linux GNU arm64 or amd64" }}{{ end }}
{{- end }}

{{- define "elitea.compiledSnapshots.validate" -}}
{{- $runtime := .Values.main.runtime -}}
{{- $c := $runtime.rustCompiledSnapshots | default dict -}}
{{- if and (hasKey $c "enabled") (not (kindIs "bool" $c.enabled)) }}{{ fail "rustCompiledSnapshots.enabled must be boolean" }}{{ end }}
{{- $enabled := $c.enabled | default false -}}
{{- if not (kindIs "bool" $enabled) }}{{ fail "rustCompiledSnapshots.enabled must be boolean" }}{{ end }}
{{- range $key, $_ := .Values.main.env }}
{{- if or (hasPrefix "ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_" $key) (eq $key "ELITEA_RUST_COMPILED_AGENTSTATE_DSN_FILE") }}{{ fail "compiled snapshot environment must derive from typed chart settings" }}{{ end }}
{{- end }}
{{- if and (not $enabled) ($c.profilesSha256 | default "") }}{{ fail "compiled snapshot pin requires explicit enablement" }}{{ end }}
{{- if $enabled }}
{{- if or (not $runtime.enabled) (not $runtime.agentExecutionDispatch.enabled) (eq (len $runtime.sandboxAudiences) 0) }}{{ fail "compiled snapshots require runtime, agent dispatch, and supervisor audiences" }}{{ end }}
{{- if not (regexMatch "^[a-f0-9]{64}$" (default "" $c.profilesSha256)) }}{{ fail "rustCompiledSnapshots.profilesSha256 must be lowercase SHA-256" }}{{ end }}
{{- if ne (len $c) 8 }}{{ fail "rustCompiledSnapshots requires exactly its eight typed settings" }}{{ end }}
{{- range $field, $max := dict "globalEntries" 100000 "globalBytes" 1099511627776 "tenantEntries" 100000 "tenantBytes" 1099511627776 "publishingTtlSeconds" 300 "readyTtlSeconds" 86400 }}
{{- $_ := include "elitea.compiledSnapshots.integer" (dict "value" (get $c $field) "maximum" $max) }}
{{- end }}
{{- if or (gt (int64 $c.tenantEntries) (int64 $c.globalEntries)) (gt (int64 $c.tenantBytes) (int64 $c.globalBytes)) }}{{ fail "compiled snapshot tenant quotas exceed global quotas" }}{{ end }}
{{- end }}
{{- $workerCount := 0 -}}
{{- $workerProfile := dict -}}
{{- range .Values.worker.runtime.sandboxRuntimes }}
{{- if and .preparation (hasKey .preparation "compiled_snapshot") }}{{ fail "preparation profiles cannot enable compiled snapshots" }}{{ end }}
{{- if hasKey . "compiled_snapshot" }}
{{- if or (not $enabled) (not $.Values.worker.enabled) (ne $.Values.worker.implementation "rust") (ne .language "rust") }}{{ fail "compiled_snapshot requires an enabled Rust execution worker" }}{{ end }}
{{- if or (not (regexMatch "^sha256:[a-f0-9]{64}$" (default "" .image_digest))) (not (regexMatch "^[a-zA-Z0-9_.-]{1,128}$" (default "" .policy_revision))) }}{{ fail "compiled Worker requires an immutable image and exact policy" }}{{ end }}
{{- include "elitea.compiledSnapshots.object" (dict "config" .compiled_snapshot "path" "/run/elitea-runtime/rust-compiled-profiles.json" "pin" $c.profilesSha256) }}
{{- if ne .compiled_snapshot.dependency_bundle_sha256 "" }}
{{- if not .preparation }}{{ fail "compiled Cargo bundle requires its existing preparation profile" }}{{ end }}
{{- include "elitea.compiledSnapshots.platform" .preparation.native_platform }}
{{- end }}
{{- $workerCount = add1 $workerCount }}
{{- $workerProfile = . }}
{{- end }}
{{- end }}
{{- if and $enabled .Values.worker.enabled (ne (int $workerCount) 1) }}{{ fail "compiled snapshots require exactly one Rust execution worker profile" }}{{ end }}
{{- $supervisorCount := 0 -}}
{{- range .Values.sandboxKubernetes.supervisor.profiles }}
{{- if hasKey . "compiled_snapshot" }}
{{- if or (not $enabled) (not $.Values.sandboxKubernetes.supervisor.enabled) (ne (default "" .purpose) "execution") (not (kindIs "slice" .languages)) }}{{ fail "compiled_snapshot requires an enabled Rust execution supervisor" }}{{ end }}
{{- if not (kindIs "bool" .dependencyContentEnabled) }}{{ fail "compiled supervisor must declare dependencyContentEnabled=true" }}{{ end }}
{{- if or (ne (len .languages) 1) (ne (index .languages 0) "rust") (not .dependencyContentEnabled) }}{{ fail "compiled supervisor requires only Rust and dependency content" }}{{ end }}
{{- if or (not (regexMatch "^sha256:[a-f0-9]{64}$" (default "" .image_digest))) (not (regexMatch "^[a-zA-Z0-9_.-]{1,128}$" (default "" .policy_revision))) }}{{ fail "compiled Supervisor requires an immutable image and exact policy" }}{{ end }}
{{- include "elitea.compiledSnapshots.object" (dict "config" .compiled_snapshot "path" "/run/elitea-sandbox/rust-compiled-profiles.json" "pin" $c.profilesSha256) }}
{{- if $workerCount }}
{{- if or (ne .image_digest $workerProfile.image_digest) (ne .policy_revision $workerProfile.policy_revision) (ne .compiled_snapshot.dependency_bundle_sha256 $workerProfile.compiled_snapshot.dependency_bundle_sha256) }}{{ fail "compiled Worker and Supervisor release selections differ" }}{{ end }}
{{- end }}
{{- if ne .compiled_snapshot.dependency_bundle_sha256 "" }}
{{- include "elitea.compiledSnapshots.platform" .native_platform }}
{{- if and $workerCount (ne (toJson .native_platform) (toJson $workerProfile.preparation.native_platform)) }}{{ fail "compiled Cargo Worker and Supervisor platforms differ" }}{{ end }}
{{- end }}
{{- $supervisorCount = add1 $supervisorCount }}
{{- end }}
{{- end }}
{{- if and $enabled .Values.sandboxKubernetes.supervisor.enabled (ne (int $supervisorCount) 1) }}{{ fail "compiled snapshots require exactly one Rust execution supervisor profile" }}{{ end }}
{{- end }}
