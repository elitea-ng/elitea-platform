{{- define "elitea-main.name" -}}
{{- default "elitea-main" .Values.main.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Release-INDEPENDENT on purpose. These names are certificate material: the
runtime mTLS server certificates carry DNS SANs for them, the worker dials
control/output/content on `elitea-main`, and platform_origin is pinned to a
certificate whose only SAN is `elitea-platform-edge`. Helm's usual
"<release>-<chart>" would rename them per release and fail every handshake with
an error that reads like a trust problem, nowhere near the rename.

There is exactly one of each per release, so there is nothing for a release
prefix to disambiguate. Override only alongside re-minting the certificate.
*/}}
{{- define "elitea-main.fullname" -}}
{{- default "elitea-main" .Values.main.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-main.labels" -}}
helm.sh/chart: {{ include "elitea-main.name" . }}-{{ .Chart.Version }}
{{ include "elitea-main.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "elitea-main.selectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-main.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "elitea-main.serviceAccountName" -}}
{{- if .Values.main.serviceAccount.create }}
{{- default (include "elitea-main.fullname" .) .Values.main.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.main.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
elitea-main.authenticationComposed — does this install authenticate at all?

"true" when the chart composes ANY credential plane, empty otherwise. Two
planes qualify, and neither is more real than the other:

  * fileConfig.authConfig — the Form authentication document. It is what
    cmd/elitea-main turns into a FormGraph.
  * env.OIDC_ISSUER_URL — the OIDC browser-session plane. A SAML or typed
    identity provider composes through the SAME session plane, so an install
    that federates SAML sets this too.

This helper exists because the chart used to spell "authenticated" as
"fileConfig.authConfig.enabled", which silently meant "Form only".
*/}}
{{- define "elitea-main.authenticationComposed" -}}
{{- $env := .Values.main.env | default dict -}}
{{- if or .Values.main.fileConfig.authConfig.enabled (get $env "OIDC_ISSUER_URL" | toString) -}}
true
{{- end -}}
{{- end -}}

{{/*
elitea-main.configurationsEnabled — the EFFECTIVE ELITEA_CONFIGURATIONS_ENABLED.

An operator's stated value always wins. values.yaml ships the key EMPTY, which
means "follow this deployment": the plane comes on when the install has both
prerequisites the chart can see — authentication, and the public project id
the binary requires — and stays off otherwise.

Why not ship a literal "true": the default values file has no authentication
and no public project id, so a literal would turn `helm template` with no
overrides into a failure and, past that, a CrashLoopBackOff. Why not keep the
literal "false": that is the gap. An install that HAS single sign-on and HAS
named its public project was still shipped a dark configuration plane, and had
to discover the flag to get credential and model-catalogue routes at all.

The rendered ConfigMap and every check below read this one helper, so the
manifest and the guards cannot disagree about what the plane is doing.
*/}}
{{- define "elitea-main.configurationsEnabled" -}}
{{- $env := .Values.main.env | default dict -}}
{{- $stated := get $env "ELITEA_CONFIGURATIONS_ENABLED" | toString -}}
{{- if $stated -}}
{{- $stated -}}
{{- else if and (eq (include "elitea-main.authenticationComposed" .) "true") (include "elitea.aiProjectId" .) -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/*
elitea-main.validateCapabilities — issue #382.

The composition root gates whole capabilities on environment variables, and it
REFUSES TO START when a gated capability does not get its prerequisites. Before
this chart set the flags at all, every one of those refusals was unreachable,
because no Kubernetes install ever turned a capability on. Now that the chart
can turn them on, each refusal becomes a CrashLoopBackOff — a pod that starts,
fails, restarts, and reports the cause only in a log line.

This helper moves each of those refusals to `helm template` time. The operator
reads the reason on the terminal that ran the command, before any object
reaches the cluster.

Each check below names the exact Go source that would otherwise refuse at boot.
Keep them in step: a check that drifts from its source is worse than no check,
because it passes a manifest the binary then rejects.
*/}}
{{- define "elitea-main.validateCapabilities" -}}
{{- $env := .Values.main.env | default dict -}}
{{- $runtime := .Values.main.runtime | default dict -}}

{{/*
  cmd/elitea-main/main.go: "ELITEA_CONFIGURATIONS_ENABLED requires an
  authenticated deployment".

  This check used to demand fileConfig.authConfig — the Form authentication
  DOCUMENT — because the composition root tested the FormGraph. That was never
  the requirement, and it made an install with real corporate single sign-on
  unable to turn the plane on at all: OIDC builds no FormGraph, so the whole
  credential and model-catalogue surface was unreachable and every LLM setup
  step fell back to deploy/scripts/seed-llm-api.py or raw SQL.

  cmd/elitea-main now asks productionAuthenticationComposed, which wants a
  reader of the caller's credential plus a principal validator. The OIDC
  session plane carries both, and the SAML/typed-provider plane composes with
  it. So this check asks for SOME authentication: fileConfig.authConfig (the
  Form document) or env.OIDC_ISSUER_URL (the single-sign-on plane).

  A deployment with NEITHER is still refused, in the chart and in the binary.
  The composition root hands the zero AuthConfig to that shape, and mounting
  credential write routes behind no credential is the failure this stops.
*/}}
{{- if eq (include "elitea-main.configurationsEnabled" .) "true" -}}
{{- if ne (include "elitea-main.authenticationComposed" .) "true" -}}
{{- fail "env.ELITEA_CONFIGURATIONS_ENABLED=\"true\" needs an authenticated deployment. Set fileConfig.authConfig.enabled=true and point fileConfig.authConfig.configMapName at an auth configuration ConfigMap, OR set env.OIDC_ISSUER_URL for a single-sign-on install. Without either, cmd/elitea-main refuses to start with \"ELITEA_CONFIGURATIONS_ENABLED requires an authenticated deployment\"." -}}
{{- end -}}
{{/*
  cmd/elitea-main/configurations_config.go: "ELITEA_AI_PROJECT_ID is required".
  The variable is read only on the enabled path, so this pair looks from the
  outside like a pod that starts, exits and restarts. State it here instead.
*/}}
{{- if not (include "elitea.aiProjectId" .) -}}
{{- fail "env.ELITEA_CONFIGURATIONS_ENABLED=\"true\" needs a public project id (platform.aiProjectId, or env.ELITEA_AI_PROJECT_ID). Name the project whose configurations are PUBLIC and merge into every other project's option lookups; it must name a project that EXISTS. cmd/elitea-main/configurations_config.go refuses to start with \"ELITEA_AI_PROJECT_ID is required\"." -}}
{{- end -}}
{{- end -}}

{{/*
  Same refusal, same source, for the project-information route.
*/}}
{{- if eq (get $env "ELITEA_PROJECT_INFO_ENABLED" | toString) "true" -}}
{{- if not .Values.main.fileConfig.authConfig.enabled -}}
{{- fail "env.ELITEA_PROJECT_INFO_ENABLED=\"true\" needs production authentication. Set fileConfig.authConfig.enabled=true and point fileConfig.authConfig.configMapName at an auth configuration ConfigMap." -}}
{{- end -}}
{{- end -}}

{{/*
  Same refusal again, for two more capabilities that need production
  authentication.
  Issues #394 and #395 landed the contract work, so neither capability is dark
  any more. values-standalone.yaml turns both on, together with
  fileConfig.authConfig.enabled. values.yaml keeps both "false", because the
  default install builds no production authentication.
  Keep this refusal. It is what makes an operator's edit fail while the chart
  renders, instead of at pod start.
*/}}
{{- range $name := list "ELITEA_INDEX_TYPES_ENABLED" "ELITEA_APPLICATION_SKILLS_ENABLED" -}}
{{- if eq (get $env $name | toString) "true" -}}
{{- if not $.Values.main.fileConfig.authConfig.enabled -}}
{{- fail (printf "env.%s=\"true\" needs production authentication. Set fileConfig.authConfig.enabled=true and point fileConfig.authConfig.configMapName at an auth configuration ConfigMap." $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
  ELITEA_CONFIGURATIONS_MUTATION_ENABLED is read inside the Configurations
  block: cmd/elitea-main/configurations_config.go refuses the mutation flag
  when the capability itself is off.
*/}}
{{- if eq (get $env "ELITEA_CONFIGURATIONS_MUTATION_ENABLED" | toString) "true" -}}
{{- if ne (include "elitea-main.configurationsEnabled" .) "true" -}}
{{- fail "env.ELITEA_CONFIGURATIONS_MUTATION_ENABLED=\"true\" needs env.ELITEA_CONFIGURATIONS_ENABLED=\"true\". cmd/elitea-main/configurations_config.go rejects the mutation flag on its own." -}}
{{- end -}}
{{/*
  cmd/elitea-main/runtime_composition_validation.go: "current Configurations
  mutation requires the production runtime". The write routes dispatch a
  validation command, so they need the runtime plane to dispatch it on.
*/}}
{{- if not $runtime.enabled -}}
{{- fail "env.ELITEA_CONFIGURATIONS_MUTATION_ENABLED=\"true\" needs runtime.enabled=true. cmd/elitea-main refuses the pair with \"current Configurations mutation requires the production runtime\", because the write routes dispatch a configuration-validation command." -}}
{{- end -}}
{{- end -}}

{{/*
  The runtime block is rendered by elitea-main.runtimeEnv, and the env map is
  rendered beside it into the same ConfigMap. A name in both produces a
  duplicate key, which Kubernetes rejects on apply — long after `helm template`
  looked clean. Own the whole prefix here instead.
*/}}
{{- range $name, $value := $env -}}
{{- if hasPrefix "ELITEA_RUNTIME_" $name -}}
{{- fail (printf "env.%s must not be set. The chart owns every ELITEA_RUNTIME_* name through the runtime: block, and a name in both maps makes a duplicate ConfigMap key. Move the setting to runtime: in values.yaml." $name) -}}
{{- end -}}
{{- end -}}

{{- include "elitea-main.validateLLMGateway" . -}}
{{- include "elitea-main.validateDeepWiki" . -}}
{{- include "elitea-main.validateInventory" . -}}
{{- include "elitea-main.validateSelfLLMOrigins" . -}}
{{- include "elitea-main.validateRuntime" . -}}
{{- include "elitea-main.validateConnectionBudget" . -}}
{{- include "elitea-main.validateAuthMaterial" . -}}
{{- end }}

{{/*
elitea-main.authConfigMapName — the ConfigMap that carries the authentication
configuration document.

Two shapes, and exactly one of them (issue #444):

  * fileConfig.authConfig.document — the document is a chart value, so this
    chart renders the ConfigMap and KNOWS the five material paths. It checks
    them while it renders.
  * fileConfig.authConfig.configMapName — the document stays outside the
    chart. The chart cannot read it, so cmd/elitea-auth-material is the only
    gate.
*/}}
{{- define "elitea-main.authConfigMapName" -}}
{{- $auth := .Values.main.fileConfig.authConfig -}}
{{- if $auth.document -}}
{{- printf "%s-auth-config" (include "elitea-main.fullname" .) -}}
{{- else -}}
{{- required "fileConfig.authConfig.enabled=true needs fileConfig.authConfig.document or fileConfig.authConfig.configMapName" $auth.configMapName -}}
{{- end -}}
{{- end }}

{{/*
elitea-main.authMaterialSourcePath — where the init container reads the raw
Kubernetes Secret that carries the authentication material (issue #444).

DERIVED from fileConfig.authConfig.material.mountPath rather than configured,
so the two can never disagree. The suffix also guarantees the two paths are
siblings: a directory and that same directory plus a suffix cannot contain each
other, so the raw Secret mount can never shadow, or be shadowed by, the
installed material.

The elitea-main container never mounts this path. Only the init container reads
the Secret; the service reads the copies.
*/}}
{{- define "elitea-main.authMaterialSourcePath" -}}
{{- printf "%s-source" (.Values.main.fileConfig.authConfig.material.mountPath | toString | trimSuffix "/") -}}
{{- end }}

{{/*
elitea-main.authMaterialPaths — the five file paths, one per line.

internal/authcomposition/material.go opens exactly these five, and
Config.MaterialFiles is the Go list of the same five. Keep the two in step: a
sixth file added there must appear here, or the chart mounts a directory that
misses it.

An absent key produces an empty line. validateAuthMaterial below refuses that,
so every other caller of this helper receives five usable paths.
*/}}
{{- define "elitea-main.authMaterialPaths" -}}
{{- $document := .Values.main.fileConfig.authConfig.document | default dict -}}
{{- $redis := get $document "redis" | default dict -}}
{{- $credentials := get $document "credentials" | default dict -}}
{{- $form := get (get $document "provider" | default dict) "form" | default dict -}}
{{- $paths := list
  (get $redis "password_file" | toString)
  (get $redis "ca_file" | toString)
  (get $redis "attempt_key_file" | toString)
  (get $credentials "pat_signing_key_file" | toString)
  (get $form "users_json_file" | toString) -}}
{{- join "\n" $paths -}}
{{- end }}

{{/*
elitea-main.validateAuthMaterial — the authentication material (issue #444).

internal/authcomposition/material.go reads five files through
internal/security/securefile. Their paths come from the operator's
authentication-configuration document, NOT from a chart value. So the chart
rendered no volume and no mount for them, and a Kubernetes install of the
runtime plane could not start from the chart alone.

The answer has two halves, and this helper is the first one:

  1. RENDER TIME, here. With the document as a chart value the chart reads the
     five paths, and it refuses a path that the mounted directory cannot serve.
     The operator reads the reason on the terminal.
  2. POD START, in cmd/elitea-auth-material. It reads the SAME document the
     service reads, it derives the same five paths, and it refuses a
     disagreement with the mounted directory. That half is the only one
     available while the document stays in an external ConfigMap.

This helper does NOT validate the whole document.
internal/authcomposition/config.go owns that schema, and a second copy of it
here would drift from it.
*/}}
{{- define "elitea-main.validateAuthMaterial" -}}
{{- $auth := .Values.main.fileConfig.authConfig | default dict -}}
{{- $material := $auth.material | default dict -}}
{{- $runtime := .Values.main.runtime | default dict -}}

{{- if not $auth.enabled -}}
{{/*
  Authentication OFF. cmd/elitea-main composes no FormGraph, so it opens none
  of the five files, and a setting here is silently dead rather than active.
  That is the failure class this chart exists to end, so say it now.
*/}}
{{- if $auth.document -}}
{{- fail "fileConfig.authConfig.document is set but fileConfig.authConfig.enabled is false, so cmd/elitea-main reads no authentication configuration and the document does nothing. Set fileConfig.authConfig.enabled=true, or clear fileConfig.authConfig.document." -}}
{{- end -}}
{{- if $material.secretName -}}
{{- fail "fileConfig.authConfig.material.secretName is set but fileConfig.authConfig.enabled is false. Set fileConfig.authConfig.enabled=true, or clear fileConfig.authConfig.material.secretName." -}}
{{- end -}}
{{- if $material.volume -}}
{{- fail "fileConfig.authConfig.material.volume is set but fileConfig.authConfig.enabled is false. Set fileConfig.authConfig.enabled=true, or clear fileConfig.authConfig.material.volume." -}}
{{- end -}}
{{- else -}}

{{/* One document, from one place. */}}
{{- if and $auth.document $auth.configMapName -}}
{{- fail "set fileConfig.authConfig.document OR fileConfig.authConfig.configMapName, not both. The first makes this chart render the ConfigMap and check the five material paths while it renders. The second points at a ConfigMap you provision, and this chart cannot read its contents." -}}
{{- end -}}
{{- if not (or $auth.document $auth.configMapName) -}}
{{- fail "fileConfig.authConfig.enabled=true needs fileConfig.authConfig.document, the authentication configuration itself. Read internal/authcomposition/config.go for the schema, and deploy/runtime/auth.form.yml for an example. Use fileConfig.authConfig.configMapName instead to keep the document in a ConfigMap that you provision." -}}
{{- end -}}

{{/* The directory that carries the five files. */}}
{{- $mountPath := $material.mountPath | toString | trimSuffix "/" -}}
{{- if not $mountPath -}}
{{- fail "fileConfig.authConfig.enabled=true needs fileConfig.authConfig.material.mountPath. internal/authcomposition/material.go reads five files from that directory: the Auth Redis password, the Auth Redis CA, the browser-attempt key, the PAT signing key and the Form users JSON." -}}
{{- end -}}
{{- if not (hasPrefix "/" $mountPath) -}}
{{- fail (printf "fileConfig.authConfig.material.mountPath must be absolute. internal/security/securefile refuses a relative path. Got %q." ($material.mountPath | toString)) -}}
{{- end -}}
{{/*
  Two directories, never one. Each material install container removes anything
  in its own directory that its own Secret does not carry. One shared directory
  would make the two delete each other's files on every pod start.
*/}}
{{- if $runtime.enabled -}}
{{- $runtimeMount := (($runtime.material | default dict).mountPath | toString | trimSuffix "/") -}}
{{- if eq $mountPath $runtimeMount -}}
{{- fail (printf "fileConfig.authConfig.material.mountPath and runtime.material.mountPath are both %q, and they must differ. Each material install container removes anything in its directory that its own Secret does not carry, so one shared directory makes the two delete each other's files. Give the authentication material its own directory, and put a copy of any shared file, such as the Redis CA, in both Secrets." $mountPath) -}}
{{- end -}}
{{- end -}}
{{- if eq $mountPath ($auth.mountPath | toString | trimSuffix "/") -}}
{{- fail (printf "fileConfig.authConfig.material.mountPath and fileConfig.authConfig.mountPath are both %q. The first is a directory of secret material. The second is the configuration file itself." $mountPath) -}}
{{- end -}}

{{/*
  Two sources, and exactly one of them. Same shape as runtime.material, and
  the same reason: issue #404 found that securefile refuses a Secret volume
  itself, so the supported source needs an init container that copies it.
*/}}
{{- if and $material.secretName $material.volume -}}
{{- fail "set fileConfig.authConfig.material.secretName OR fileConfig.authConfig.material.volume, not both. The first makes the chart copy a Kubernetes Secret into an emptyDir; the second mounts a volume that you supply. Two sources for one mount path cannot both apply." -}}
{{- end -}}
{{- if not (or $material.secretName $material.volume) -}}
{{- fail "fileConfig.authConfig.enabled=true needs fileConfig.authConfig.material.secretName, a Kubernetes Secret that carries the five authentication files. Its keys are the last component of each of the five paths in the authentication configuration. Use fileConfig.authConfig.material.volume instead only when another mechanism already writes those files as real, owner-owned files." -}}
{{- end -}}
{{- if $material.secretName -}}
{{/*
  The init container has to READ the Secret, and the kubelet owns each key as
  root. So the mode must carry the read bit for other users, or the read bit
  for the group together with a pod fsGroup that puts this pod in that group.
  Neither one, and the init container stops on a permission error.
*/}}
{{- $mode := $material.secretDefaultMode | int -}}
{{- $otherRead := div (mod $mode 8) 4 -}}
{{- $groupRead := div (mod (div $mode 8) 8) 4 -}}
{{- $fsGroup := get (.Values.main.podSecurityContext | default dict) "fsGroup" -}}
{{- if and (eq $otherRead 0) (or (eq $groupRead 0) (not $fsGroup)) -}}
{{- fail (printf "fileConfig.authConfig.material.secretDefaultMode %v does not let this pod read the Secret. The kubelet owns each key as root, so the mode needs the read bit for other users (0444), or the read bit for the group (0440) together with podSecurityContext.fsGroup. The init container would stop on a permission error." $material.secretDefaultMode) -}}
{{- end -}}
{{- if not $material.sizeLimit -}}
{{- fail "fileConfig.authConfig.material.secretName needs fileConfig.authConfig.material.sizeLimit, the bound on the memory-backed emptyDir that holds the installed material." -}}
{{- end -}}
{{- end -}}

{{/*
  The five paths. They are reachable only with the document in this values
  file. An external ConfigMap keeps them out of reach, and
  cmd/elitea-auth-material checks them at pod start instead.
*/}}
{{- if $auth.document -}}
{{- $names := list -}}
{{- range $path := splitList "\n" (include "elitea-main.authMaterialPaths" .) -}}
{{- if not $path -}}
{{- fail "fileConfig.authConfig.document must name all five material files: redis.password_file, redis.ca_file, redis.attempt_key_file, credentials.pat_signing_key_file and provider.form.users_json_file. internal/authcomposition/config.go refuses a document without them, and cmd/elitea-main then exits at boot." -}}
{{- end -}}
{{- if ne (dir $path | trimSuffix "/") $mountPath -}}
{{- fail (printf "the authentication configuration reads %s, which is outside fileConfig.authConfig.material.mountPath %s. One volume mount serves one directory, so the chart would render cleanly and the pod would then fail to open that file. Move the path into %s, or set the mount path to %s." $path $mountPath $mountPath (dir $path | trimSuffix "/")) -}}
{{- end -}}
{{- $name := base $path -}}
{{/*
  Every file has to arrive as a Secret KEY, and a Kubernetes Secret key is a
  bounded name. A name that no Secret can carry renders cleanly and then leaves
  the pod without that file.
*/}}
{{- if or (not (regexMatch "^[-._a-zA-Z0-9]+$" $name)) (hasPrefix ".." $name) -}}
{{- fail (printf "the authentication configuration reads %s, and %q cannot be a Kubernetes Secret key. A key holds only letters, digits, '-', '_' and '.', and it cannot start with two dots." $path $name) -}}
{{- end -}}
{{- if has $name $names -}}
{{- fail (printf "the authentication configuration names %q twice. internal/authcomposition/config.go refuses two material references that resolve to one file, and one Secret key cannot serve two purposes." $name) -}}
{{- end -}}
{{- $names = append $names $name -}}
{{- end -}}
{{- end -}}

{{- end -}}
{{- end }}

{{/*
elitea-main.validateLLMGateway — issue #463.

The /llm proxy is all-or-nothing, and the failure it produces when half
configured is a CrashLoopBackOff rather than a disabled feature.

internal/llmproxy/proxy.go builds an mTLS transport whenever Config.Transport is
nil, and the only seam for a plaintext transport is a struct field with no
environment binding, so it is test-only. With LLM_GATEWAY_URL set and any of the
three material paths empty, tls.LoadX509KeyPair or os.ReadFile fails,
llmproxy.New returns an error, and cmd/elitea-main returns
"compose llm gateway proxy" as a fatal boot error.

So this refuses the half-configured values file at `helm template` time.

The reverse direction is NOT an error. A deployment may legitimately run without
the /llm path. That state is no longer silent: with no URL the router answers
503 llm_gateway_not_configured, which names this variable.
*/}}
{{- define "elitea-main.validateLLMGateway" -}}
{{- $env := .Values.main.env | default dict -}}
{{- $clientMaterial := .Values.main.fileConfig.llmGatewayClientMaterial | default dict -}}
{{- $mountPath := $clientMaterial.mountPath | default "" | toString -}}
{{- $url := get $env "LLM_GATEWAY_URL" | toString -}}
{{- $material := dict
  "LLM_GATEWAY_CLIENT_CERT" (get $env "LLM_GATEWAY_CLIENT_CERT" | toString)
  "LLM_GATEWAY_CLIENT_KEY" (get $env "LLM_GATEWAY_CLIENT_KEY" | toString)
  "LLM_GATEWAY_CA_FILE" (get $env "LLM_GATEWAY_CA_FILE" | toString) -}}
{{- if $url -}}
{{- if not (or (hasPrefix "https://" $url) (hasPrefix "http://" $url)) -}}
{{- fail (printf "env.LLM_GATEWAY_URL must be an absolute URL with a scheme and a host. internal/llmproxy/proxy.go refuses anything else: \"target url %q missing scheme or host\"." $url) -}}
{{- end -}}
{{- range $name, $value := $material -}}
{{- if not $value -}}
{{- fail (printf "env.LLM_GATEWAY_URL is set, so env.%s must be set too. internal/llmproxy/proxy.go always builds an mTLS transport and reads this value as a FILE PATH; an empty path makes cmd/elitea-main exit at boot with \"compose llm gateway proxy\". Set all three of LLM_GATEWAY_CLIENT_CERT, LLM_GATEWAY_CLIENT_KEY and LLM_GATEWAY_CA_FILE, and mount the material at those paths." $name) -}}
{{- end -}}
{{- if not (hasPrefix "/" $value) -}}
{{- fail (printf "env.%s must be an absolute file path, not certificate text. Got %q. internal/llmproxy/proxy.go passes it to tls.LoadX509KeyPair / os.ReadFile." $name $value) -}}
{{- end -}}
{{- if and $clientMaterial.enabled (not (hasPrefix $mountPath $value)) -}}
{{- fail (printf "env.%s is %q, which fileConfig.llmGatewayClientMaterial does not serve: its mountPath is %q. A path outside the mounted directory is a file that does not exist in the container, and the pod exits at boot with \"compose llm gateway proxy\"." $name $value $mountPath) -}}
{{- end -}}
{{- end -}}
{{- if not $clientMaterial.enabled -}}
{{- fail "env.LLM_GATEWAY_URL is set, so fileConfig.llmGatewayClientMaterial.enabled must be true. Issue #463 moved the three material variables from secretKeyRef entries to file paths but mounted nothing at them, so the paths named files that no volume served and the pod exited at boot. Set fileConfig.llmGatewayClientMaterial.secretName (a cert-manager Certificate issued INTO THIS namespace gives tls.crt / tls.key / ca.crt), or fileConfig.llmGatewayClientMaterial.volume for material that already exists as files." -}}
{{- end -}}
{{- if and $clientMaterial.secretName $clientMaterial.volume -}}
{{- fail "fileConfig.llmGatewayClientMaterial.secretName and .volume are mutually exclusive: one Deployment volume cannot have two sources. Use secretName for a Kubernetes Secret, or volume for a CSI secret driver." -}}
{{- end -}}
{{- else -}}
{{- range $name, $value := $material -}}
{{- if $value -}}
{{- fail (printf "env.%s is set but env.LLM_GATEWAY_URL is empty, so no proxy is composed and the material is never read. Set env.LLM_GATEWAY_URL, or clear env.%s." $name $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea-main.validateDeepWiki — the same all-or-nothing shape as the /llm hop
above, for the same reason (ADR-0022 P2/P3).

internal/api/v2/deepwiki/config.go's Validate() refuses an enabled facade that
is missing the base URL, any of the three certificate paths, or the callback
origin, and cmd/elitea-main turns that into a fatal boot error. So a
half-configured values file is a CrashLoopBackOff, not a disabled feature, and
this refuses it while the operator is still looking at their terminal.

The reverse direction is checked too, and it is the one that would otherwise be
silent: material configured with the flag off is a mounted Secret nothing
reads, which looks configured and does nothing.
*/}}
{{- define "elitea-main.validateDeepWiki" -}}
{{- $env := .Values.main.env | default dict -}}
{{- $clientMaterial := .Values.main.fileConfig.deepwikiClientMaterial | default dict -}}
{{- $mountPath := $clientMaterial.mountPath | default "" | toString -}}
{{- $enabled := get $env "ELITEA_DEEPWIKI_ENABLED" | toString -}}
{{- $on := has (lower $enabled) (list "1" "true" "yes" "on") -}}
{{- $material := dict
  "ELITEA_DEEPWIKI_CLIENT_CERT_FILE" (get $env "ELITEA_DEEPWIKI_CLIENT_CERT_FILE" | toString)
  "ELITEA_DEEPWIKI_CLIENT_KEY_FILE" (get $env "ELITEA_DEEPWIKI_CLIENT_KEY_FILE" | toString)
  "ELITEA_DEEPWIKI_CA_FILE" (get $env "ELITEA_DEEPWIKI_CA_FILE" | toString) -}}
{{- if and $enabled (not $on) (not (has (lower $enabled) (list "0" "false" "no" "off"))) -}}
{{- fail (printf "env.ELITEA_DEEPWIKI_ENABLED is %q, which is neither true nor false. internal/api/v2/deepwiki/config.go refuses an unrecognised spelling rather than reading it as off, so a typo here is a boot failure and never a quietly disabled feature." $enabled) -}}
{{- end -}}
{{- if $on -}}
{{- $url := get $env "ELITEA_DEEPWIKI_BASE_URL" | toString -}}
{{- if not (hasPrefix "https://" $url) -}}
{{- fail (printf "env.ELITEA_DEEPWIKI_BASE_URL must be an https URL, and it is %q. The provider refuses non-mTLS traffic, so a plain-http origin is a facade that fails on every call; NewProxy catches it at startup." $url) -}}
{{- end -}}
{{- if not (get $env "ELITEA_DEEPWIKI_CALLBACK_BASE_URL") -}}
{{- fail "env.ELITEA_DEEPWIKI_ENABLED is on, so env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL must name the origin the PROVIDER calls back to for artifacts and models. Without it a generation runs to completion and then cannot hand back what it produced — the failure arrives at the end of the most expensive operation the facade offers. Set it to this deployment's own in-cluster origin, e.g. http://elitea-main:8080." -}}
{{- end -}}
{{- if not (get $env "ELITEA_DEEPWIKI_GIT_ALLOWLIST") -}}
{{- fail "env.ELITEA_DEEPWIKI_ENABLED is on, so env.ELITEA_DEEPWIKI_GIT_ALLOWLIST must name the git hosts this deployment may clone from. It is fail-closed: empty refuses every repository, at the point a user asks for a wiki. It must hold the SAME value as deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST — this side checks it before opening the vault, that side before building a clone URL, and two that disagree mean an invocation that starts and then fails." -}}
{{- end -}}
{{- range $name, $value := $material -}}
{{- if not $value -}}
{{- fail (printf "env.ELITEA_DEEPWIKI_ENABLED is on, so env.%s must be set too. The provider terminates mTLS with CERT_REQUIRED, so all three of the client certificate, its key and the CA bundle are mandatory; Config.Validate() names the missing one and cmd/elitea-main exits at boot." $name) -}}
{{- end -}}
{{- if not (hasPrefix "/" $value) -}}
{{- fail (printf "env.%s must be an absolute file path, not certificate text. Got %q — it is passed to tls.LoadX509KeyPair / os.ReadFile." $name $value) -}}
{{- end -}}
{{- if and $clientMaterial.enabled (not (hasPrefix $mountPath $value)) -}}
{{- fail (printf "env.%s is %q, which fileConfig.deepwikiClientMaterial does not serve: its mountPath is %q. A path outside the mounted directory is a file that does not exist in the container." $name $value $mountPath) -}}
{{- end -}}
{{- end -}}
{{- if not $clientMaterial.enabled -}}
{{- fail "env.ELITEA_DEEPWIKI_ENABLED is on, so fileConfig.deepwikiClientMaterial.enabled must be true — otherwise the three paths above name files no volume serves. Set fileConfig.deepwikiClientMaterial.secretName to the Secret the deepwiki chart's facade-client Certificate issues (deepwiki.mtls.clientSecretName, default elitea-main-deepwiki-client-tls), which must exist in THIS namespace." -}}
{{- end -}}
{{- if and $clientMaterial.secretName $clientMaterial.volume -}}
{{- fail "fileConfig.deepwikiClientMaterial.secretName and .volume are mutually exclusive: one Deployment volume cannot have two sources." -}}
{{- end -}}
{{- else -}}
{{- if $clientMaterial.enabled -}}
{{- fail "fileConfig.deepwikiClientMaterial.enabled is true but env.ELITEA_DEEPWIKI_ENABLED is off, so the material is mounted and never read. That is the state that looks configured and does nothing. Turn the facade on, or disable the material." -}}
{{- end -}}
{{- range $name, $value := $material -}}
{{- if $value -}}
{{- fail (printf "env.%s is set but env.ELITEA_DEEPWIKI_ENABLED is off, so no facade is composed and the material is never read. Turn it on, or clear env.%s." $name $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea-main.validateInventory — the same all-or-nothing shape as the DeepWiki
facade above (ADR-0023 H4c).

internal/providerhost/facade's ConfigFromEnv refuses an enabled facade that is
missing the base URL, any of the three certificate paths or the identity
secret, and cmd/elitea-main turns that into a fatal boot error. So a
half-configured values file is a CrashLoopBackOff, not a disabled feature.

TWO THINGS ARE DELIBERATELY NOT REQUIRED HERE, and both are differences from
DeepWiki rather than omissions:

  * ELITEA_INVENTORY_CALLBACK_BASE_URL. An empty one DISABLES source expansion
    and mounts the facade anyway — the eight tools that only read a graph still
    work, and the three that name a source get the provider's own refusal.
    cmd/elitea-main logs which of the two it built. Requiring it here would
    refuse an install the code supports.
  * ELITEA_INVENTORY_GIT_ALLOWLIST. It is fail-closed on the facade side alone;
    the provider has no allowlist to keep in step with, because Inventory has
    no request-supplied clone URL. An unset one refuses every source, at the
    point a user asks for an ingestion — a warning below rather than a refusal,
    so a graph-read-only deployment is still expressible.

The reverse direction is checked, and it is the one that would otherwise be
silent: material configured with the flag off is a mounted Secret nothing
reads, which looks configured and does nothing.
*/}}
{{- define "elitea-main.validateInventory" -}}
{{- $env := .Values.main.env | default dict -}}
{{- $clientMaterial := .Values.main.fileConfig.inventoryClientMaterial | default dict -}}
{{- $mountPath := $clientMaterial.mountPath | default "" | toString -}}
{{- $enabled := get $env "ELITEA_INVENTORY_ENABLED" | toString -}}
{{- $on := has (lower $enabled) (list "1" "true" "yes" "on") -}}
{{- $material := dict
  "ELITEA_INVENTORY_CLIENT_CERT_FILE" (get $env "ELITEA_INVENTORY_CLIENT_CERT_FILE" | toString)
  "ELITEA_INVENTORY_CLIENT_KEY_FILE" (get $env "ELITEA_INVENTORY_CLIENT_KEY_FILE" | toString)
  "ELITEA_INVENTORY_CA_FILE" (get $env "ELITEA_INVENTORY_CA_FILE" | toString) -}}
{{- if and $enabled (not $on) (not (has (lower $enabled) (list "0" "false" "no" "off"))) -}}
{{- fail (printf "env.ELITEA_INVENTORY_ENABLED is %q, which is neither true nor false. facade.ConfigFromEnv refuses an unrecognised spelling rather than reading it as off, so a typo here is a boot failure and never a quietly disabled feature." $enabled) -}}
{{- end -}}
{{- if $on -}}
{{- $url := get $env "ELITEA_INVENTORY_BASE_URL" | toString -}}
{{- if not (hasPrefix "https://" $url) -}}
{{- fail (printf "env.ELITEA_INVENTORY_BASE_URL must be an https URL, and it is %q. The provider refuses non-mTLS traffic, so a plain-http origin is a facade that fails on every call; proxy.New catches it at startup." $url) -}}
{{- end -}}
{{- if not (get $env "ELITEA_INVENTORY_IDENTITY_SECRET") -}}
{{- if not (hasKey (.Values.main.secrets | default dict) "ELITEA_INVENTORY_IDENTITY_SECRET") -}}
{{- fail "env.ELITEA_INVENTORY_ENABLED is on, so ELITEA_INVENTORY_IDENTITY_SECRET must be supplied — as a secrets: entry (the default) or in env. It signs the identity headers the provider verifies; without it ConfigFromEnv refuses at boot. It must hold the SAME value as inventory.secrets.ELITEA_INVENTORY_IDENTITY_SECRET." -}}
{{- end -}}
{{- end -}}
{{- range $name, $value := $material -}}
{{- if not $value -}}
{{- fail (printf "env.ELITEA_INVENTORY_ENABLED is on, so env.%s must be set too. The provider terminates mTLS with CERT_REQUIRED, so all three of the client certificate, its key and the CA bundle are mandatory; ConfigFromEnv names every missing one at once and cmd/elitea-main exits at boot." $name) -}}
{{- end -}}
{{- if not (hasPrefix "/" $value) -}}
{{- fail (printf "env.%s must be an absolute file path, not certificate text. Got %q — it is passed to tls.LoadX509KeyPair / os.ReadFile." $name $value) -}}
{{- end -}}
{{- if and $clientMaterial.enabled (not (hasPrefix $mountPath $value)) -}}
{{- fail (printf "env.%s is %q, which fileConfig.inventoryClientMaterial does not serve: its mountPath is %q. A path outside the mounted directory is a file that does not exist in the container." $name $value $mountPath) -}}
{{- end -}}
{{- end -}}
{{- if not $clientMaterial.enabled -}}
{{- fail "env.ELITEA_INVENTORY_ENABLED is on, so fileConfig.inventoryClientMaterial.enabled must be true — otherwise the three paths above name files no volume serves. Set fileConfig.inventoryClientMaterial.secretName to the Secret the inventory chart's facade-client Certificate issues (inventory.mtls.clientSecretName, default elitea-main-inventory-client-tls), which must exist in THIS namespace." -}}
{{- end -}}
{{- if and $clientMaterial.secretName $clientMaterial.volume -}}
{{- fail "fileConfig.inventoryClientMaterial.secretName and .volume are mutually exclusive: one Deployment volume cannot have two sources." -}}
{{- end -}}
{{- else -}}
{{- if $clientMaterial.enabled -}}
{{- fail "fileConfig.inventoryClientMaterial.enabled is true but env.ELITEA_INVENTORY_ENABLED is off, so the material is mounted and never read. That is the state that looks configured and does nothing. Turn the facade on, or disable the material." -}}
{{- end -}}
{{- range $name, $value := $material -}}
{{- if $value -}}
{{- fail (printf "env.%s is set but env.ELITEA_INVENTORY_ENABLED is off, so no facade is composed and the material is never read. Turn it on, or clear env.%s." $name $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea-main.validateSelfLLMOrigins — issue #467, guard #1 at upsert time.

internal/api/v2/configurations/selfref.go builds the self-origin list from
ELITEA_SELF_LLM_ORIGINS plus DEPLOYMENT_URL with "/llm" appended. When the list
is empty, validateNotSelfReferential returns nil for EVERY credential, so a
credential whose api_base points back at this platform's own /llm origin is
saved. The gateway's request-time check is the backstop, and issue #467 ships
that one empty too.

The chart shipped both names empty, so the guard was inert in every Kubernetes
install. It is NOT given a default value: this chart cannot know the
deployment's public origin, and a guessed origin would guard a name nobody uses
while leaving the real one open, which reads as armed and is not.

DEPLOYMENT_URL alone satisfies this. That is the zero-extra-configuration path
selfref.go already promises for a single-domain deployment.

Checked only when the Configurations plane is on, because that plane owns the
credential write routes this guard runs on. With the plane off there is no
upsert to guard.
*/}}
{{- define "elitea-main.validateSelfLLMOrigins" -}}
{{- $env := .Values.main.env | default dict -}}
{{- if eq (include "elitea-main.configurationsEnabled" .) "true" -}}
{{- if not (or (get $env "ELITEA_SELF_LLM_ORIGINS") (get $env "DEPLOYMENT_URL")) -}}
{{- fail "env.ELITEA_CONFIGURATIONS_ENABLED=\"true\" needs env.DEPLOYMENT_URL or env.ELITEA_SELF_LLM_ORIGINS. With both empty, internal/api/v2/configurations/selfref.go builds an empty self-origin list and the SELF_REFERENTIAL_CREDENTIAL guard (spec §2.6 guard #1) admits every credential, including one whose api_base points back at this platform's own /llm origin. Set env.DEPLOYMENT_URL to this deployment's public base URL (the guard then appends \"/llm\" itself), or list the origins explicitly in env.ELITEA_SELF_LLM_ORIGINS." -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea-main.validateRuntime — the all-or-nothing runtime plane (issue #382).

internal/runtimecomposition/config.go builds the runtime configuration with a
`required()` helper. Every name it asks for must be present and non-empty, or
ConfigFromEnv returns an error and the process exits. It also REJECTS settings
that belong to a dispatch plane which is not explicitly enabled. So a values
file that turns the runtime on and leaves one field blank produces a pod that
cannot boot.

This helper refuses that values file at render time instead.
*/}}
{{/*
elitea-main.validateConnectionBudget — the ceiling nothing enforced.

elitea-main opens the six runtime-plane pools per replica when the runtime
plane is on (36 connections, and validateDependencies forbids them sharing
capacity) PLUS its primary pool. Multiplied by the HPA's ceiling that is ~400
against a stock max_connections of 100, and the chart set no pool size at all.

TWO CHECKS, and the first matters more than the arithmetic.

`ELITEA_DATABASE_MAX_CONNS` unset does not mean "small". cmd/elitea-main's
databasePoolConfig returns EARLY when it is unset, so pgx applies its own
default of max(4, numCPU) — a number that depends on the node the pod lands on
and which this chart cannot know. A budget computed around an unknowable term
is a budget that reports a number it cannot stand behind, so the guard refuses
to compute one instead.

The count is deliberately for elitea-main alone. The scheduler, gateway and
worker hold one pool each and do not autoscale; they are covered by
`reservedConnections`, along with the Jobs and anything else sharing the
server. Modelling every component here would drift the moment one changes,
and the term that actually moves is the one this counts.

The replica count is the HPA's maxReplicas when autoscaling is on, because the
question is what this release may consume WITHOUT anybody doing anything —
`replicaCount` would check a state the autoscaler is free to leave.
*/}}
{{- define "elitea-main.validateConnectionBudget" -}}
{{- $main := .Values.main -}}
{{- $pg := .Values.postgresql | default dict -}}
{{- $env := $main.env | default dict -}}
{{- $runtimeOn := and $main.runtime $main.runtime.enabled -}}

{{- $primary := get $env "ELITEA_DATABASE_MAX_CONNS" | toString -}}
{{- if and $runtimeOn (not $primary) -}}
{{- fail "main.runtime.enabled is true but main.env.ELITEA_DATABASE_MAX_CONNS is unset, so this release's connection use cannot be computed. cmd/elitea-main/database_pool.go returns early when it is unset and pgx then applies max(4, numCPU) — a value that depends on the node the pod lands on. With the runtime plane on, each replica ALSO opens 36 isolated runtime-plane connections that may not share capacity, so at autoscaling.maxReplicas the total is the number that exhausts the server. Set it explicitly (1..64)." -}}
{{- end -}}

{{- if $primary -}}
{{- $perReplica := int $primary -}}
{{- if $runtimeOn -}}
{{/* The compiled phase-one limits, overridable per pool. Kept in the same
     order as PhaseOneDatabasePoolLimits so a reader can diff the two. */}}
{{- $perReplica = add $perReplica
      (int (default 10 (get $env "ELITEA_RUNTIME_DB_ADMISSION_MAX_CONNS")))
      (int (default 8  (get $env "ELITEA_RUNTIME_DB_CONTROL_MAX_CONNS")))
      (int (default 8  (get $env "ELITEA_RUNTIME_DB_OUTPUT_MAX_CONNS")))
      (int (default 4  (get $env "ELITEA_RUNTIME_DB_REPLAY_MAX_CONNS")))
      (int (default 2  (get $env "ELITEA_RUNTIME_DB_TERMINAL_MAX_CONNS")))
      (int (default 4  (get $env "ELITEA_RUNTIME_DB_CONTENT_MAX_CONNS"))) -}}
{{- end -}}

{{- $replicas := int (default 1 $main.replicaCount) -}}
{{- if and $main.autoscaling $main.autoscaling.enabled -}}
{{- $replicas = int $main.autoscaling.maxReplicas -}}
{{- end -}}

{{- $total := mul $perReplica $replicas -}}
{{- $available := sub (int (default 100 $pg.maxConnections)) (int (default 25 $pg.reservedConnections)) -}}
{{- if gt $total $available -}}
{{- fail (printf "this release can open %d PostgreSQL connections (%d per elitea-main replica x %d replicas) but only %d are available (postgresql.maxConnections %d minus reservedConnections %d). Exhaustion does not surface on the replica that caused it: the next component to connect — usually a migration Job or the scheduler — fails with 'sorry, too many clients already'. Lower main.env.ELITEA_DATABASE_MAX_CONNS or the ELITEA_RUNTIME_DB_*_MAX_CONNS pools, lower autoscaling.maxReplicas, or raise postgresql.maxConnections to match the server you actually run." $total $perReplica $replicas $available (int (default 100 $pg.maxConnections)) (int (default 25 $pg.reservedConnections))) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "elitea-main.validateRuntime" -}}
{{- $runtime := .Values.main.runtime | default dict -}}
{{- $agent := $runtime.agentExecutionDispatch | default dict -}}
{{- $ingest := $runtime.indexIngestDispatch | default dict -}}
{{- $scheduling := $runtime.indexScheduling | default dict -}}
{{- $redis := $runtime.redis | default dict -}}
{{- $listeners := $runtime.listeners | default dict -}}
{{- $material := $runtime.material | default dict -}}
{{- $env := .Values.main.env | default dict -}}

{{- if not $runtime.enabled -}}
{{/*
  Runtime OFF. config.go returns an empty configuration and ignores every other
  ELITEA_RUNTIME_* name, so a half-filled block here is silently dead rather
  than active. That is the failure this chart is meant to end, so say it now.
*/}}
{{- range $field := list "commandStream" -}}
{{- if get $runtime $field -}}
{{- fail (printf "runtime.%s is set but runtime.enabled is false, so the runtime plane stays dark and the setting does nothing. Set runtime.enabled=true, or clear runtime.%s." $field $field) -}}
{{- end -}}
{{- end -}}
{{- if get $redis "url" -}}
{{- fail "runtime.redis.url is set but runtime.enabled is false, so the runtime plane stays dark and the setting does nothing. Set runtime.enabled=true, or clear runtime.redis.url." -}}
{{- end -}}
{{- if get $material "secretName" -}}
{{- fail "runtime.material.secretName is set but runtime.enabled is false. Set runtime.enabled=true, or clear runtime.material.secretName." -}}
{{- end -}}
{{- if or $agent.enabled $ingest.enabled $scheduling.enabled -}}
{{- fail "a runtime dispatch plane is enabled but runtime.enabled is false. internal/runtimecomposition/config.go ignores every dispatch flag while ELITEA_RUNTIME_ENABLED is off, so the routes stay dark. Set runtime.enabled=true." -}}
{{- end -}}
{{- else -}}

{{/*
  cmd/elitea-main/main.go refuses to compose the runtime without a principal
  validator and a forwarded-identity verifier. Both come from production
  authentication, and this chart builds it only from the auth configuration
  file. This is the first prerequisite issue #382 asks the chart to state.
*/}}
{{- if not .Values.main.fileConfig.authConfig.enabled -}}
{{- fail "runtime.enabled=true needs production authentication. Set fileConfig.authConfig.enabled=true and point fileConfig.authConfig.configMapName at an auth configuration ConfigMap. cmd/elitea-main refuses to compose the runtime without a PrincipalValidator and a ForwardedIdentityVerifier, and both are built from that file." -}}
{{- end -}}

{{/* The base plane. config.go asks for all three unconditionally. */}}
{{- if not $runtime.commandStream -}}
{{- fail "runtime.enabled=true needs runtime.commandStream (ELITEA_RUNTIME_COMMAND_STREAM). internal/runtimecomposition/config.go requires it and the process exits without it." -}}
{{- end -}}
{{- if not $runtime.maxOutstanding -}}
{{- fail "runtime.enabled=true needs runtime.maxOutstanding (ELITEA_RUNTIME_MAX_OUTSTANDING), a positive integer no greater than 1024." -}}
{{- end -}}
{{- if not $runtime.streamMaxEntries -}}
{{- fail "runtime.enabled=true needs runtime.streamMaxEntries (ELITEA_RUNTIME_STREAM_MAX_ENTRIES), a positive integer no greater than 1024." -}}
{{- end -}}

{{/* Redis. config.go demands a rediss:// URL that carries an ACL username, no
     password, and an explicit /0 database. A redis:// URL is refused. */}}
{{- if not $redis.url -}}
{{- fail "runtime.enabled=true needs runtime.redis.url (ELITEA_RUNTIME_REDIS_URL)." -}}
{{- end -}}
{{- if not (hasPrefix "rediss://" ($redis.url | toString)) -}}
{{- fail (printf "runtime.redis.url must be a rediss:// URL. internal/runtimecomposition/config.go refuses anything else: \"runtime Redis URL must be a rediss URL with an ACL username\". Got %q." ($redis.url | toString)) -}}
{{- end -}}
{{- if not (hasSuffix "/0" ($redis.url | toString)) -}}
{{- fail (printf "runtime.redis.url must select database zero explicitly, so it must end with \"/0\". Got %q." ($redis.url | toString)) -}}
{{- end -}}
{{- if not $redis.poolSize -}}
{{- fail "runtime.enabled=true needs runtime.redis.poolSize (ELITEA_RUNTIME_REDIS_POOL_SIZE), a positive integer no greater than 64." -}}
{{- end -}}

{{/* Command signing. composition.go cross-checks the key id against the
     keyring file and refuses to start on a mismatch. */}}
{{- if not $runtime.signingKeyId -}}
{{- fail "runtime.enabled=true needs runtime.signingKeyId (ELITEA_RUNTIME_SIGNING_KEY_ID). It must equal the key id inside command-signing-keyring.json, or startup fails on the mismatch." -}}
{{- end -}}

{{/* Three listeners, three distinct addresses. */}}
{{- $addresses := list ($listeners.controlAddress | toString) ($listeners.outputAddress | toString) ($listeners.contentAddress | toString) -}}
{{- range $address := $addresses -}}
{{- if not $address -}}
{{- fail "runtime.enabled=true needs runtime.listeners.controlAddress, runtime.listeners.outputAddress and runtime.listeners.contentAddress, each with a numeric TCP port." -}}
{{- end -}}
{{- end -}}
{{- if ne (len ($addresses | uniq)) 3 -}}
{{- fail (printf "runtime.listeners needs three distinct addresses. internal/runtimecomposition/config.go refuses a repeat: \"runtime listeners require distinct addresses\". Got %v." $addresses) -}}
{{- end -}}

{{/* The material volume. Every remaining runtime name is a FILE PATH, and the
     chart resolves all of them under runtime.material.mountPath, so the
     operator supplies one source rather than thirteen paths. */}}
{{- if not $material.mountPath -}}
{{- fail "runtime.enabled=true needs runtime.material.mountPath. Every runtime key, password and certificate is read from a file under it." -}}
{{- end -}}
{{- if not (hasPrefix "/" ($material.mountPath | toString)) -}}
{{- fail (printf "runtime.material.mountPath must be absolute. internal/security/securefile refuses a relative path. Got %q." ($material.mountPath | toString)) -}}
{{- end -}}
{{- if eq ($material.mountPath | toString | trimSuffix "/") "" -}}
{{- fail "runtime.material.mountPath cannot be the filesystem root. The material needs its own directory, and Config.MaterialDirectory in internal/runtimecomposition refuses the root for the same reason." -}}
{{- end -}}
{{/*
  Two sources, and exactly one of them (issue #404).

  runtime.material.secretName is the supported answer: a plain Kubernetes
  Secret, which is how every other secret in this platform arrives. The chart
  adds an init container that copies the Secret into a memory-backed emptyDir
  with owner-only bits, because internal/security/securefile refuses the Secret
  volume itself — it is a symlink farm, and its files belong to root while this
  pod runs as nonroot.

  runtime.material.volume stays for a deployment that already materialises the
  files by another mechanism, such as a CSI secret driver.
*/}}
{{- if and $material.secretName $material.volume -}}
{{- fail "set runtime.material.secretName OR runtime.material.volume, not both. The first makes the chart copy a Kubernetes Secret into an emptyDir; the second mounts a volume that you supply. Two sources for one mount path cannot both apply." -}}
{{- end -}}
{{- if not (or $material.secretName $material.volume) -}}
{{- fail "runtime.enabled=true needs runtime.material.secretName, a Kubernetes Secret that carries the runtime material. Read the runtime.material comment in values.yaml for the key names it must hold. Use runtime.material.volume instead only when another mechanism already writes those files as real, owner-owned files." -}}
{{- end -}}
{{- if $material.secretName -}}
{{/*
  The init container has to READ the Secret, and the kubelet owns each key as
  root. So the mode must carry the read bit for other users, or the read bit
  for the group together with a pod fsGroup that puts this pod in that group.
  Neither one, and the init container stops on a permission error.
*/}}
{{- $mode := $material.secretDefaultMode | int -}}
{{- $otherRead := div (mod $mode 8) 4 -}}
{{- $groupRead := div (mod (div $mode 8) 8) 4 -}}
{{- $fsGroup := get (.Values.main.podSecurityContext | default dict) "fsGroup" -}}
{{- if and (eq $otherRead 0) (or (eq $groupRead 0) (not $fsGroup)) -}}
{{- fail (printf "runtime.material.secretDefaultMode %v does not let this pod read the Secret. The kubelet owns each key as root, so the mode needs the read bit for other users (0444), or the read bit for the group (0440) together with podSecurityContext.fsGroup. The init container would stop on a permission error." $material.secretDefaultMode) -}}
{{- end -}}
{{- if not $material.sizeLimit -}}
{{- fail "runtime.material.secretName needs runtime.material.sizeLimit, the bound on the memory-backed emptyDir that holds the installed material." -}}
{{- end -}}
{{- end -}}

{{/* Agent-execution dispatch: the four agent turn routes. */}}
{{- if $agent.enabled -}}
{{- if not $agent.commandStream -}}
{{- fail "runtime.agentExecutionDispatch.enabled=true needs runtime.agentExecutionDispatch.commandStream." -}}
{{- end -}}
{{- if not $agent.consumerGroup -}}
{{- fail "runtime.agentExecutionDispatch.enabled=true needs runtime.agentExecutionDispatch.consumerGroup." -}}
{{- end -}}
{{- if not $agent.streamMaxEntries -}}
{{- fail "runtime.agentExecutionDispatch.enabled=true needs runtime.agentExecutionDispatch.streamMaxEntries." -}}
{{- end -}}
{{/*
  currentMainBaseUrl is OPTIONAL. Empty means "call my own listener": elitea-main
  derives http://127.0.0.1:<port> from ELITEA_HTTP_ADDRESS, because the
  next-input-suggestion policy call is a self call and this process serves
  cleartext. Set it only where another server answers that path, and then it must
  be an origin that TERMINATES TLS. An https origin aimed at elitea-main's own
  cleartext port is what internal/runtimecomposition/config.go now refuses at
  boot, after it made every chat turn log a failed policy call.
*/}}
{{- if and $agent.currentMainBaseUrl (not (hasPrefix "https://" ($agent.currentMainBaseUrl | toString))) -}}
{{- fail (printf "runtime.agentExecutionDispatch.currentMainBaseUrl must be an https origin with no path, query or fragment, or empty to call elitea-main's own listener. internal/runtimecomposition/config.go validates it. Got %q." ($agent.currentMainBaseUrl | toString)) -}}
{{- end -}}
{{- if eq ($agent.commandStream | toString) ($runtime.commandStream | toString) -}}
{{- fail "runtime.agentExecutionDispatch.commandStream must differ from runtime.commandStream. config.go: \"runtime agent execution cannot share the configuration-validation stream\"." -}}
{{- end -}}
{{- else -}}
{{- if or $agent.commandStream $agent.consumerGroup $agent.currentMainBaseUrl -}}
{{- fail "runtime.agentExecutionDispatch settings are present but runtime.agentExecutionDispatch.enabled is false. config.go refuses that combination: \"runtime agent execution dispatch settings require explicit enablement\"." -}}
{{- end -}}
{{- end -}}

{{/* Index-ingest dispatch: index start, index cancel, index metadata. */}}
{{- if $ingest.enabled -}}
{{/*
  Issue #382 acceptance criterion 4 asks the chart to state this prerequisite.
  The index-ingest handlers compose through the Configurations dependency, so
  the flag is useless without it.
*/}}
{{- if ne (include "elitea-main.configurationsEnabled" .) "true" -}}
{{- fail "runtime.indexIngestDispatch.enabled=true needs env.ELITEA_CONFIGURATIONS_ENABLED=\"true\". The index-ingest handlers compose through the Configurations dependency, and the embedding binding resolves from the same configuration rows." -}}
{{- end -}}
{{- if not $ingest.commandStream -}}
{{- fail "runtime.indexIngestDispatch.enabled=true needs runtime.indexIngestDispatch.commandStream." -}}
{{- end -}}
{{- if not $ingest.consumerGroup -}}
{{- fail "runtime.indexIngestDispatch.enabled=true needs runtime.indexIngestDispatch.consumerGroup." -}}
{{- end -}}
{{- if not $ingest.streamMaxEntries -}}
{{- fail "runtime.indexIngestDispatch.enabled=true needs runtime.indexIngestDispatch.streamMaxEntries." -}}
{{- end -}}
{{- if eq ($ingest.commandStream | toString) ($runtime.commandStream | toString) -}}
{{- fail "runtime.indexIngestDispatch.commandStream must differ from runtime.commandStream. config.go: \"runtime index ingest requires a dedicated command stream\"." -}}
{{- end -}}
{{/*
  Sharing ONE stream with the agent plane is supported, and the standalone
  compose stack does exactly that so a single worker serves both capabilities.
  config.go allows it only when the consumer group matches too.
*/}}
{{- if and $agent.enabled (eq ($ingest.commandStream | toString) ($agent.commandStream | toString)) -}}
{{- if ne ($ingest.consumerGroup | toString) ($agent.consumerGroup | toString) -}}
{{- fail "runtime.indexIngestDispatch and runtime.agentExecutionDispatch share a command stream but not a consumer group. config.go: \"runtime agent execution sharing the index stream must share its consumer group\". Give them the same consumerGroup, or give each its own commandStream." -}}
{{- end -}}
{{- end -}}
{{- else -}}
{{- if or $ingest.commandStream $ingest.consumerGroup -}}
{{- fail "runtime.indexIngestDispatch settings are present but runtime.indexIngestDispatch.enabled is false. config.go: \"runtime index ingest dispatch settings require explicit enablement\"." -}}
{{- end -}}
{{- end -}}

{{/* Index scheduling: the index schedule write and delete routes. */}}
{{- if $scheduling.enabled -}}
{{- if not $ingest.enabled -}}
{{- fail "runtime.indexScheduling.enabled=true needs runtime.indexIngestDispatch.enabled=true. config.go: \"runtime index scheduling requires index ingest dispatch\"." -}}
{{- end -}}
{{- if not $scheduling.instanceId -}}
{{- fail "runtime.indexScheduling.enabled=true needs runtime.indexScheduling.instanceId (ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID). It must start with a lowercase letter or digit and hold only lowercase letters, digits, dot, underscore and hyphen." -}}
{{- end -}}
{{- else -}}
{{- if $scheduling.instanceId -}}
{{- fail "runtime.indexScheduling.instanceId is set but runtime.indexScheduling.enabled is false. config.go: \"runtime scheduler instance ID requires explicit index scheduling\"." -}}
{{- end -}}
{{- end -}}

{{- end -}}
{{- end }}

{{/*
elitea-main.runtimeEnv — the ELITEA_RUNTIME_* block, rendered as ConfigMap data.

Emits nothing while runtime.enabled is false, because config.go refuses a
dispatch setting whose plane is off. Every file path resolves under
runtime.material.mountPath, and the file NAMES match what
deploy/scripts/gen-runtime-certs.sh writes — so the Secret or CSI object that
carries the material must use those names as its keys.
*/}}
{{- define "elitea-main.runtimeEnv" -}}
{{- $runtime := .Values.main.runtime | default dict -}}
{{- if $runtime.enabled -}}
{{- $agent := $runtime.agentExecutionDispatch | default dict -}}
{{- $ingest := $runtime.indexIngestDispatch | default dict -}}
{{- $scheduling := $runtime.indexScheduling | default dict -}}
{{- $redis := $runtime.redis | default dict -}}
{{- $listeners := $runtime.listeners | default dict -}}
{{- $dir := $runtime.material.mountPath | toString | trimSuffix "/" -}}
ELITEA_RUNTIME_ENABLED: "true"
ELITEA_RUNTIME_COMMAND_STREAM: {{ $runtime.commandStream | quote }}
ELITEA_RUNTIME_MAX_OUTSTANDING: {{ $runtime.maxOutstanding | toString | quote }}
ELITEA_RUNTIME_STREAM_MAX_ENTRIES: {{ $runtime.streamMaxEntries | toString | quote }}
{{- if $agent.enabled }}
ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED: "true"
ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM: {{ $agent.commandStream | quote }}
ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP: {{ $agent.consumerGroup | quote }}
ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES: {{ $agent.streamMaxEntries | toString | quote }}
{{- if $agent.currentMainBaseUrl }}
ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL: {{ $agent.currentMainBaseUrl | quote }}
{{- end }}
{{- end }}
{{- if $ingest.enabled }}
ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED: "true"
ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM: {{ $ingest.commandStream | quote }}
ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP: {{ $ingest.consumerGroup | quote }}
ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES: {{ $ingest.streamMaxEntries | toString | quote }}
{{- end }}
{{- if $scheduling.enabled }}
ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED: "true"
ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID: {{ $scheduling.instanceId | quote }}
{{- end }}
ELITEA_RUNTIME_REDIS_URL: {{ $redis.url | quote }}
ELITEA_RUNTIME_REDIS_POOL_SIZE: {{ $redis.poolSize | toString | quote }}
ELITEA_RUNTIME_REDIS_PASSWORD_FILE: {{ printf "%s/redis-producer-password" $dir | quote }}
ELITEA_RUNTIME_REDIS_CA_FILE: {{ printf "%s/runtime-ca.crt" $dir | quote }}
ELITEA_RUNTIME_SIGNING_KEY_ID: {{ $runtime.signingKeyId | quote }}
ELITEA_RUNTIME_SIGNING_KEY_FILE: {{ printf "%s/command-signing-key.pem" $dir | quote }}
ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE: {{ printf "%s/command-signing-keyring.json" $dir | quote }}
ELITEA_RUNTIME_CONTROL_ADDRESS: {{ $listeners.controlAddress | quote }}
ELITEA_RUNTIME_CONTROL_TLS_CERT_FILE: {{ printf "%s/control-server.crt" $dir | quote }}
ELITEA_RUNTIME_CONTROL_TLS_KEY_FILE: {{ printf "%s/control-server.key" $dir | quote }}
ELITEA_RUNTIME_CONTROL_TLS_CLIENT_CA_FILE: {{ printf "%s/runtime-ca.crt" $dir | quote }}
ELITEA_RUNTIME_OUTPUT_ADDRESS: {{ $listeners.outputAddress | quote }}
ELITEA_RUNTIME_OUTPUT_TLS_CERT_FILE: {{ printf "%s/output-server.crt" $dir | quote }}
ELITEA_RUNTIME_OUTPUT_TLS_KEY_FILE: {{ printf "%s/output-server.key" $dir | quote }}
ELITEA_RUNTIME_OUTPUT_TLS_CLIENT_CA_FILE: {{ printf "%s/runtime-ca.crt" $dir | quote }}
ELITEA_RUNTIME_CONTENT_ADDRESS: {{ $listeners.contentAddress | quote }}
ELITEA_RUNTIME_CONTENT_TLS_CERT_FILE: {{ printf "%s/content-server.crt" $dir | quote }}
ELITEA_RUNTIME_CONTENT_TLS_KEY_FILE: {{ printf "%s/content-server.key" $dir | quote }}
ELITEA_RUNTIME_CONTENT_TLS_CLIENT_CA_FILE: {{ printf "%s/runtime-ca.crt" $dir | quote }}
{{- end -}}
{{- end }}

{{/*
elitea-main.runtimePort — the numeric port of a runtime listener address.
The address is a listen address such as ":9443", so the port follows the last
colon. Kubernetes needs the number on both the container port and the Service.
*/}}
{{- define "elitea-main.runtimePort" -}}
{{- $parts := splitList ":" (. | toString) -}}
{{- index $parts (sub (len $parts) 1) -}}
{{- end }}

{{/*
elitea-main.runtimeMaterialSourcePath — where the init container reads the raw
Kubernetes Secret (issue #404).

DERIVED from runtime.material.mountPath rather than configured, so the two can
never disagree. The suffix also guarantees the two paths are siblings: a
directory and that same directory plus a suffix cannot contain each other, so
the raw Secret mount can never shadow, or be shadowed by, the installed
material.

The elitea-main container never mounts this path. Only the init container
reads the Secret; the service reads the copies.
*/}}
{{- define "elitea-main.runtimeMaterialSourcePath" -}}
{{- printf "%s-source" (.Values.main.runtime.material.mountPath | toString | trimSuffix "/") -}}
{{- end }}
