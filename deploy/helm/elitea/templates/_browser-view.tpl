{{/*
  The cloud browser view relay (ADR-0049 decision 6, #1597): Main's client
  identity toward the browser profile's supervisors. Off by default; on, it
  renders ELITEA_RUNTIME_BROWSER_VIEW_CONFIG and adds browser-view-client.crt
  and .key to Main's runtime material (elitea.externalSecrets.mainRuntimeMaterialKeys),
  issued from the runtime CA like code-owner-client. A null value is missing:
  Helm 4 keeps `--set key=null` as a key with no value.
*/}}
{{- define "elitea.browserView.validate" -}}
{{- $runtime := .Values.main.runtime | default dict -}}
{{- $view := $runtime.browserView | default dict -}}
{{- if not (kindIs "map" $view) -}}{{- fail "main.runtime.browserView requires an object" -}}{{- end -}}
{{- range $field, $_ := $view -}}
{{- if not (has $field (list "enabled" "mainWorkloadIdentity" "supervisors" "maxFps")) -}}{{- fail (printf "main.runtime.browserView contains an unknown field %s" $field) -}}{{- end -}}
{{- end -}}
{{- if and (hasKey $view "enabled") (not (kindIs "invalid" $view.enabled)) (not (kindIs "bool" $view.enabled)) -}}
{{- fail "main.runtime.browserView.enabled must be boolean" -}}
{{- end -}}
{{- if ne $view.enabled true -}}
{{- if or $view.mainWorkloadIdentity $view.supervisors $view.maxFps -}}
{{- fail "main.runtime.browserView settings require main.runtime.browserView.enabled=true" -}}
{{- end -}}
{{- else -}}
{{- if or (not $runtime.enabled) (ne ($runtime.agentExecutionDispatch | default dict).enabled true) (eq (len ($runtime.sandboxAudiences | default list)) 0) -}}
{{- fail "main.runtime.browserView requires the runtime, agent dispatch and main.runtime.sandboxAudiences" -}}
{{- end -}}
{{- if or (not (kindIs "string" $view.mainWorkloadIdentity)) (not $view.mainWorkloadIdentity) -}}
{{- fail "main.runtime.browserView requires mainWorkloadIdentity" -}}
{{- end -}}
{{- if or (not (kindIs "slice" $view.supervisors)) (eq (len $view.supervisors) 0) (gt (len $view.supervisors) 16) -}}
{{- fail "main.runtime.browserView requires one to sixteen supervisors" -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $supervisor := $view.supervisors -}}
{{- if or (not (kindIs "map" $supervisor)) (ne (len $supervisor) 2) (not $supervisor.audience) (not $supervisor.httpsOrigin) -}}
{{- fail "main.runtime.browserView supervisors require exactly audience and httpsOrigin" -}}
{{- end -}}
{{- if not (has $supervisor.audience $runtime.sandboxAudiences) -}}
{{- fail (printf "main.runtime.browserView supervisor audience %s must be in main.runtime.sandboxAudiences" (toString $supervisor.audience)) -}}
{{- end -}}
{{- if or (hasKey $seen (toString $supervisor.audience)) (not (regexMatch "^https://[^/?#@:[:space:]]+:[0-9]+/?$" (toString $supervisor.httpsOrigin))) -}}
{{- fail "main.runtime.browserView supervisors require unique audiences and fixed https://host:port origins (the supervisor gRPC listener)" -}}
{{- end -}}
{{- $_ := set $seen (toString $supervisor.audience) true -}}
{{- end -}}
{{- if not (kindIs "invalid" $view.maxFps) -}}
{{- if or (not (regexMatch "^[0-9]+$" (toString $view.maxFps))) (gt (int $view.maxFps) 10) -}}
{{- fail "main.runtime.browserView.maxFps is a whole number of at most 10 (0 is 10)" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "elitea.browserView.env" -}}
{{- $runtime := .Values.main.runtime -}}
{{- $view := $runtime.browserView | default dict -}}
{{- if eq $view.enabled true -}}
{{- $dir := $runtime.material.mountPath | toString | trimSuffix "/" -}}
{{- $supervisors := list -}}
{{- range $supervisor := $view.supervisors -}}
{{- $supervisors = append $supervisors (dict "audience" $supervisor.audience "https_origin" $supervisor.httpsOrigin) -}}
{{- end -}}
{{- $config := dict "main_workload_identity" $view.mainWorkloadIdentity "certificate_chain_path" (printf "%s/browser-view-client.crt" $dir) "private_key_path" (printf "%s/browser-view-client.key" $dir) "server_ca_path" (printf "%s/runtime-ca.crt" $dir) "supervisors" $supervisors -}}
{{- if and (not (kindIs "invalid" $view.maxFps)) (gt (int $view.maxFps) 0) -}}{{- $_ := set $config "max_fps" (int $view.maxFps) -}}{{- end }}
ELITEA_RUNTIME_BROWSER_VIEW_CONFIG: {{ $config | toJson | quote }}
{{- end -}}
{{- end -}}
