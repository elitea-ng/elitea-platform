{{/*
Release-wide settings that several components must agree on (#1460):
global.publicUrl, global.trustedProxyCidrs, global.otelEndpoint and
certManager.issuers. values.yaml (`global`, `certManager`) lists every value
each one fills.
*/}}

{{/*
elitea.sharedIssuer — the issuer of one CA ("internal" or "runtime") from
certManager.issuers, as YAML {name, kind}. A kind left empty is Issuer when
the chart creates the issuers (certManager.issuers.create) and ClusterIssuer
when it does not, which is the shape every mTLS block defaulted to before.
Pass (dict "ctx" $ "ca" "internal"|"runtime").
*/}}
{{- define "elitea.sharedIssuer" -}}
{{- $issuers := dig "issuers" dict (.ctx.Values.certManager | default dict) -}}
{{- $create := $issuers.create | default false -}}
{{- $i := get $issuers .ca | default dict -}}
{{- $kind := $i.kind | default (ternary "Issuer" "ClusterIssuer" $create) | toString -}}
{{- if not (has $kind (list "Issuer" "ClusterIssuer")) -}}
{{- fail (printf "certManager.issuers.%s.kind is %q; it must be Issuer or ClusterIssuer (or empty)." .ca $kind) -}}
{{- end -}}
{{- if and $create (eq $kind "ClusterIssuer") -}}
{{- fail (printf "certManager.issuers.create is true, and certManager.issuers.%s.kind is ClusterIssuer. The chart creates NAMESPACED Issuers in this release's namespace; a ClusterIssuer of that name would be one nothing creates, and every Certificate that names it would stay not Ready. Leave the kind empty (Issuer), or set create: false and bring the ClusterIssuer yourself." .ca) -}}
{{- end -}}
name: {{ required (printf "certManager.issuers.%s.name names the %s CA issuer" .ca .ca) $i.name }}
kind: {{ $kind }}
{{- end }}

{{/*
elitea.issuerRef — the issuerRef body of one mTLS Certificate: the block's own
issuerRef where it names an issuer, else certManager.issuers.<ca>. A block
issuerRef with a name and no kind takes the shared kind.
Pass (dict "ctx" $ "ref" <block>.issuerRef "ca" "internal"|"runtime").
*/}}
{{- define "elitea.issuerRef" -}}
{{- $ref := .ref | default dict -}}
{{- $shared := include "elitea.sharedIssuer" (dict "ctx" .ctx "ca" .ca) | fromYaml -}}
name: {{ $ref.name | default $shared.name }}
kind: {{ $ref.kind | default $shared.kind }}
{{- end }}

{{/*
elitea.otelEndpoint — the OTLP/HTTP base URL a component exports to when its
own env names none: global.otelEndpoint, else the bundled collector while
otelCollector.enabled, else "" (no collector).
*/}}
{{- define "elitea.otelEndpoint" -}}
{{- $g := .Values.global | default dict -}}
{{- if $g.otelEndpoint -}}
{{- $g.otelEndpoint -}}
{{- else if (.Values.otelCollector | default dict).enabled -}}
{{- printf "http://%s:%v" (include "otel-collector.fullname" .) .Values.otelCollector.service.httpPort -}}
{{- end -}}
{{- end }}

{{/*
elitea.otelEnv — the worker container's OTel variables, as env list items.
The worker has no free env map, so it takes the derived value only.
*/}}
{{- define "elitea.otelEnv" -}}
{{- with include "elitea.otelEndpoint" . }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ . | quote }}
{{- else }}
- name: OTEL_SDK_DISABLED
  value: "true"
{{- end }}
{{- end }}

{{/*
elitea.applyGlobals — writes the values derived from `global` into .Values,
where each target is empty, so every reader (the ConfigMaps, the guards, the
required-values list) sees one derived state. Each write happens only while
the target is empty, so it is idempotent and an operator's own value wins.
templates/guards/required/values.yaml calls it before anything else reads
the values (it is the first template Helm renders).
*/}}
{{- define "elitea.applyGlobals" -}}
{{- $v := .Values -}}
{{- $g := $v.global | default dict -}}
{{- if not (kindIs "map" $v.main.env) -}}{{- $_ := set $v.main "env" dict -}}{{- end -}}
{{- $mainEnv := $v.main.env -}}
{{- $auth := $v.main.fileConfig.authConfig | default dict -}}
{{- $doc := $auth.document -}}
{{- $hasDoc := and (kindIs "map" $doc) (gt (len $doc) 0) -}}

{{- /* ── global.publicUrl ── */ -}}
{{- $url := trimSuffix "/" ($g.publicUrl | default "" | toString) -}}
{{- if $url -}}
{{- if not (regexMatch "^https?://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]+)?$" $url) -}}
{{- fail (printf "global.publicUrl is %q. It must be the origin browsers open: a scheme and a host, an optional port, no path, e.g. https://elitea.example.com." $g.publicUrl) -}}
{{- end -}}
{{- $hostPort := regexReplaceAll "^https?://" $url "" -}}
{{- $host := regexReplaceAll ":[0-9]+$" $hostPort "" -}}
{{- if not (get $mainEnv "DEPLOYMENT_URL") -}}{{- $_ := set $mainEnv "DEPLOYMENT_URL" $url -}}{{- end -}}
{{- if and (get $mainEnv "OIDC_ISSUER_URL") (not (get $mainEnv "OIDC_REDIRECT_URI")) -}}
{{- /* internal/api/v2/auth/util.go, OIDCCallbackPath */ -}}
{{- $_ := set $mainEnv "OIDC_REDIRECT_URI" (printf "%s/auth/oidc/callback" $url) -}}
{{- end -}}
{{- if and $hasDoc (not $doc.public_origin) -}}{{- $_ := set $doc "public_origin" $url -}}{{- end -}}
{{- if not (kindIs "map" $v.llmGateway.env) -}}{{- $_ := set $v.llmGateway "env" dict -}}{{- end -}}
{{- if not (get $v.llmGateway.env "GATEWAY_SELF_LLM_ORIGINS") -}}
{{- /* The gateway rejects a credential whose api_base is one of these or
       shares a path-segment prefix with one (internal/account,
       isSelfReferential), so /llm covers /llm/v1 too. */ -}}
{{- $_ := set $v.llmGateway.env "GATEWAY_SELF_LLM_ORIGINS" (printf "%s/llm,http://%s:%v/llm" $url (include "elitea-main.fullname" .) $v.main.service.port) -}}
{{- end -}}
{{- if not (kindIs "map" $v.worker.runtime) -}}{{- $_ := set $v.worker "runtime" dict -}}{{- end -}}
{{- if not $v.worker.runtime.publicBaseUrl -}}{{- $_ := set $v.worker.runtime "publicBaseUrl" $url -}}{{- end -}}
{{- if not $v.worker.runtimeSession.expectedIdentity -}}
{{- /* The SPIFFE ID deploy/scripts/mint-secrets.sh puts in the worker
       certificate for the same URL and namespace. */ -}}
{{- $_ := set $v.worker.runtimeSession "expectedIdentity" (printf "spiffe://%s/ns/%s/sa/agent-worker" $host .Release.Namespace) -}}
{{- end -}}
{{- $ing := $v.main.ingress -}}
{{- if not $ing.hosts -}}{{- $_ := set $ing "hosts" (list $host) -}}{{- end -}}
{{- range $tls := ($ing.tls | default list) -}}
{{- if and (kindIs "map" $tls) (not $tls.hosts) -}}{{- $_ := set $tls "hosts" (list $host) -}}{{- end -}}
{{- end -}}
{{- end -}}

{{- /* ── global.trustedProxyCidrs ── */ -}}
{{- $cidrs := $g.trustedProxyCidrs | default list -}}
{{- if $cidrs -}}
{{- if not (kindIs "slice" $cidrs) -}}
{{- fail "global.trustedProxyCidrs must be a list of CIDRs, e.g. [10.0.0.0/8, 35.191.0.0/16]." -}}
{{- end -}}
{{- if not (get $mainEnv "ELITEA_TRUSTED_PROXY_CIDRS") -}}{{- $_ := set $mainEnv "ELITEA_TRUSTED_PROXY_CIDRS" (join "," $cidrs) -}}{{- end -}}
{{- if and $hasDoc (not $doc.trusted_proxy_cidrs) -}}{{- $_ := set $doc "trusted_proxy_cidrs" $cidrs -}}{{- end -}}
{{- end -}}

{{- /* ── global.imageGenerationModel (#1969) ──
       One value feeds elitea-main's ELITEA_IMAGE_GENERATION_MODEL and the
       worker's image_generation_model. An explicit main.env key or
       worker.runtime.imageGenerationModel wins, so no key is rendered twice. */ -}}
{{- if $g.imageGenerationModel -}}
{{- if not (get $mainEnv "ELITEA_IMAGE_GENERATION_MODEL") -}}{{- $_ := set $mainEnv "ELITEA_IMAGE_GENERATION_MODEL" $g.imageGenerationModel -}}{{- end -}}
{{- if not (kindIs "map" $v.worker.runtime) -}}{{- $_ := set $v.worker "runtime" dict -}}{{- end -}}
{{- if not $v.worker.runtime.imageGenerationModel -}}{{- $_ := set $v.worker.runtime "imageGenerationModel" $g.imageGenerationModel -}}{{- end -}}
{{- end -}}

{{- /* ── OpenTelemetry ── */ -}}
{{- $endpoint := include "elitea.otelEndpoint" . -}}
{{- range $c := list "main" "scheduler" "llmGateway" "deepwiki" "inventory" "vector" -}}
{{- $comp := get $v $c -}}
{{- if not (kindIs "map" $comp.env) -}}{{- $_ := set $comp "env" dict -}}{{- end -}}
{{- $env := $comp.env -}}
{{- if not (or (get $env "OTEL_EXPORTER_OTLP_ENDPOINT") (get $env "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") (get $env "OTEL_SDK_DISABLED")) -}}
{{- if $endpoint -}}
{{- $_ := set $env "OTEL_EXPORTER_OTLP_ENDPOINT" $endpoint -}}
{{- else if ne $c "llmGateway" -}}
{{- /* The gateway does not read OTEL_SDK_DISABLED: an empty endpoint already
       turns its export off (internal/config, OTLPEndpoint). */ -}}
{{- $_ := set $env "OTEL_SDK_DISABLED" "true" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea.placeholders — every string value under .value that still holds an
unfilled `<...>` placeholder (values-gke.yaml's <PROJECT_ID>, <pod CIDR>,
<client id>, ...), one "path: value" line each. A placeholder is `<`, a
letter, then letters, digits, spaces, `_`, `.` or `-`, then `>`. So "a < b",
"<<", "<!-- -->", HTML tags with attributes (`<a href=...>`) and comparisons
do not match. A plain tag name such as "<br>" does: no value of this chart
holds HTML.
*/}}
{{- define "elitea.placeholders" -}}
{{- $out := list -}}
{{- $path := .path -}}
{{- if kindIs "map" .value -}}
{{- range $k := keys .value | sortAlpha -}}
{{- $sub := include "elitea.placeholders" (dict "path" (ternary $k (printf "%s.%s" $path $k) (eq $path "")) "value" (get $.value $k)) -}}
{{- if $sub -}}{{- $out = append $out $sub -}}{{- end -}}
{{- end -}}
{{- else if kindIs "slice" .value -}}
{{- range $i, $item := .value -}}
{{- $sub := include "elitea.placeholders" (dict "path" (printf "%s[%d]" $path $i) "value" $item) -}}
{{- if $sub -}}{{- $out = append $out $sub -}}{{- end -}}
{{- end -}}
{{- else if kindIs "string" .value -}}
{{- if regexMatch "<[A-Za-z][A-Za-z0-9 _.-]*>" .value -}}
{{- $out = append $out (printf "%s: %s" $path (.value | trim | quote)) -}}
{{- end -}}
{{- end -}}
{{- join "\n" $out -}}
{{- end }}

{{/*
elitea.validatePlaceholders — refuses a release whose values still hold a
`<...>` placeholder. An example file copied with one left in renders and
installs cleanly; the value then reaches a pod (a CIDR list, a bucket name, a
Workload Identity annotation) and fails far from its cause.
*/}}
{{- define "elitea.validatePlaceholders" -}}
{{- $found := include "elitea.placeholders" (dict "path" "" "value" .Values) -}}
{{- if $found -}}
{{- fail (printf "these values still hold a <...> placeholder from an example values file. Replace each one with your own value:\n  - %s" (join "\n  - " (splitList "\n" $found))) -}}
{{- end -}}
{{- end }}
