{{/*
elitea.applyFeatureSwitches — ONE SWITCH PER FEATURE (S10).

DeepWiki and Inventory are each one feature in two halves: the provider
(deepwiki.* / inventory.*) and elitea-main's facade (main.env.ELITEA_<APP>_*
plus main.fileConfig.<app>ClientMaterial). Until S10 an operator wired the
second half by hand — the enable flag, three certificate paths, the client
material Secret and a git allowlist that had to equal the provider's — and a
half that disagreed with the other was a boot failure or a feature that
looked on and did nothing. Now <app>.enabled is the switch and this helper
derives the facade half from it, so the names below are exactly the ones
cmd/elitea-main reads (internal/api/v2/deepwiki/config.go,
internal/providerhost/facade ConfigFromEnv):

  ELITEA_<APP>_ENABLED              "true"
  ELITEA_<APP>_CLIENT_CERT_FILE     <fileConfig.<app>ClientMaterial.mountPath>/tls.crt
  ELITEA_<APP>_CLIENT_KEY_FILE      <mountPath>/tls.key
  ELITEA_<APP>_CA_FILE              <mountPath>/ca.crt
  ELITEA_<APP>_GIT_ALLOWLIST        <app>.env.ELITEA_<APP>_GIT_ALLOWLIST
  fileConfig.<app>ClientMaterial    enabled, secretName <app>.mtls.clientSecretName
  (ELITEA_<APP>_PLATFORM_GRPC_TARGET is derived in main/configmap.yaml from the
   provider's Service, as before; ELITEA_VECTOR_* and the introspection client
   lists follow vector.enabled through elitea-vector.mainClientEnabled.)

An entry the operator states wins when it agrees. Three disagreements are
refused, because each would be a facade that contradicts its own provider:
ELITEA_<APP>_ENABLED "false" beside <app>.enabled=true, the client material
explicitly off (enabled: false) beside it, and a facade git allowlist that
differs from the provider's.

It WRITES INTO .Values (main.env and main.fileConfig), on purpose: every
reader — the ConfigMap, the Deployment's volumes, the facade validators, the
ExternalSecret inventory — then sees one derived state, and none of them has
to learn a second way to ask "is the facade on". Each write happens only
while the target is empty, so the helper is idempotent: every template that
reads these values calls it first, whatever order Helm renders them in.
*/}}
{{- define "elitea.applyFeatureSwitches" -}}
{{- if .Values.main.enabled -}}
{{- if not (kindIs "map" .Values.main.env) -}}{{- $_ := set .Values.main "env" dict -}}{{- end -}}
{{- $env := .Values.main.env -}}
{{- range $app := list "deepwiki" "inventory" -}}
{{- $p := get $.Values $app -}}
{{- if $p.enabled -}}
{{- $APP := upper $app -}}
{{- $flag := printf "ELITEA_%s_ENABLED" $APP -}}
{{- $raw := get $env $flag -}}
{{- $stated := "" -}}
{{- if not (kindIs "invalid" $raw) -}}{{- $stated = lower (toString $raw) -}}{{- end -}}
{{- if has $stated (list "0" "false" "no" "off") -}}
{{- fail (printf "main.env.%s is %q but %s.enabled is true. The provider and elitea-main's facade are one feature with one switch (S10): set %s.enabled=false to turn %s off, and leave main.env.%s empty." $flag (get $env $flag) $app $app $app $flag) -}}
{{- end -}}
{{- if eq $stated "" -}}{{- $_ := set $env $flag "true" -}}{{- end -}}
{{- $material := get $.Values.main.fileConfig (printf "%sClientMaterial" $app) -}}
{{- if and (kindIs "bool" $material.enabled) (not $material.enabled) -}}
{{- fail (printf "main.fileConfig.%sClientMaterial.enabled is false but %s.enabled is true: elitea-main's facade would dial the provider with no client certificate. The material follows %s.enabled (S10); leave enabled unset, and set secretName or volume only to mount material of your own." $app $app $app) -}}
{{- end -}}
{{- $_ := set $material "enabled" true -}}
{{- if and (not $material.secretName) (not $material.volume) -}}
{{- $_ := set $material "secretName" $p.mtls.clientSecretName -}}
{{- end -}}
{{- $mount := trimSuffix "/" ($material.mountPath | toString) -}}
{{- range $pair := list (list "CLIENT_CERT_FILE" "tls.crt") (list "CLIENT_KEY_FILE" "tls.key") (list "CA_FILE" "ca.crt") -}}
{{- $name := printf "ELITEA_%s_%s" $APP (index $pair 0) -}}
{{- if not (get $env $name) -}}{{- $_ := set $env $name (printf "%s/%s" $mount (index $pair 1)) -}}{{- end -}}
{{- end -}}
{{- $allowName := printf "ELITEA_%s_GIT_ALLOWLIST" $APP -}}
{{- $providerAllow := get ($p.env | default dict) $allowName | default "" | toString -}}
{{- $facadeAllow := get $env $allowName | default "" | toString -}}
{{- if and $providerAllow (not $facadeAllow) -}}
{{- $_ := set $env $allowName $providerAllow -}}
{{- else if and $providerAllow $facadeAllow (ne $providerAllow $facadeAllow) -}}
{{- fail (printf "main.env.%s is %q but %s.env.%s is %q. The facade checks the allowlist before opening the vault and the provider before cloning, so two values mean an invocation that starts and then fails. Set it once, in %s.env; the chart copies it to elitea-main." $allowName $facadeAllow $app $allowName $providerAllow $app) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea.featureDependencies — what an enabled feature needs that is switched
off, one line per need, each naming the switch that resolves it. Empty when
every enabled feature has what it needs. Reported together with the missing
values (templates/guards/required/values.yaml), so turning one feature off
tells the operator everything it takes with it in one message.

The decision for vector.enabled=false (S10): REFUSE, never switch the
dependants off silently. A feature that disappears because another switch
moved is the failure this chart exists to prevent, and each dependant has a
second answer (a store deployed elsewhere) the chart cannot guess.
*/}}
{{- define "elitea.featureDependencies" -}}
{{- $v := .Values -}}
{{- $out := list -}}
{{- $runtime := and $v.main.enabled $v.main.runtime.enabled -}}
{{- if not $runtime -}}
{{- range $c := list "worker" "vector" "deepwiki" "inventory" -}}
{{- if (get $v $c).enabled -}}
{{- $out = append $out (printf "%s.enabled needs main.runtime.enabled: it reaches elitea-main only through the runtime plane's listeners. Turn the runtime on, or set %s.enabled=false." $c $c) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if not $v.vector.enabled -}}
{{- if and $v.worker.enabled (include "elitea.indexingEnabled" .) (not (($v.worker.runtime | default dict).vectorTarget)) -}}
{{- $out = append $out "indexing (main.runtime.indexIngestDispatch.enabled) needs a vector store and vector.enabled is false: set main.runtime.indexIngestDispatch.enabled=false, or worker.runtime.vectorTarget (plus main.env.ELITEA_VECTOR_GRPC_TARGET) for an elitea-vector deployed elsewhere." -}}
{{- end -}}
{{- range $app := list "deepwiki" "inventory" -}}
{{- $p := get $v $app -}}
{{- $url := printf "ELITEA_%s_VECTOR_URL" (upper $app) -}}
{{- if and $p.enabled (include (printf "elitea-%s.modelCalls" $app) $) (not (get ($p.env | default dict) $url)) -}}
{{- $out = append $out (printf "%s.enabled runs the native engine, which keeps its vectors in elitea-vector, and vector.enabled is false: set %s.enabled=false, or %s.env.%s for an elitea-vector deployed elsewhere." $app $app $app $url) -}}
{{- end -}}
{{- end -}}
{{- if ($v.qdrant | default dict).enabled -}}
{{- $out = append $out "qdrant.enabled runs a Qdrant that only elitea-vector reads, and vector.enabled is false: set qdrant.enabled=false too." -}}
{{- end -}}
{{- end -}}
{{- join "\n" $out -}}
{{- end }}

{{/*
elitea.requiredValuesMissing — every value a default install needs that only
the operator can supply, one line per value; empty when there is none. The
chart defaults everything it can (fixed Secret names, cert-manager
Certificates, the runtime streams); what is left is an address, an identity
or a posture no chart can guess. templates/guards/required/values.yaml fails
the render ONCE with this whole list, so a first install is one edit of a
values file and not a dozen render-fix-render rounds.

Each line restates a refusal some template makes later on its own (named in
the comment beside it), so a value that slips past this list is still refused,
only one at a time.
*/}}
{{- define "elitea.requiredValuesMissing" -}}
{{- $v := .Values -}}
{{- $out := list -}}
{{- $runtime := and $v.main.enabled $v.main.runtime.enabled -}}
{{- /* Production authentication: elitea-main.validateCapabilities refuses the runtime without it. */ -}}
{{- if $runtime -}}
{{- $auth := $v.main.fileConfig.authConfig -}}
{{- if not $auth.enabled -}}
{{- $out = append $out "main.fileConfig.authConfig: enabled=true with a document (a Form document whose public_origin is your browser origin, e.g. https://elitea.example.com; values-gke.yaml has a complete one) or configMapName, and material.secretName (the three files the document names). The runtime plane composes only with production authentication." -}}
{{- else if $auth.document -}}
{{- if not $auth.document.public_origin -}}
{{- $out = append $out "main.fileConfig.authConfig.document.public_origin: the origin browsers open, e.g. https://elitea.example.com" -}}
{{- end -}}
{{- end -}}
{{- if and $auth.enabled (not $auth.material.secretName) (not $auth.material.volume) -}}
{{- $out = append $out "main.fileConfig.authConfig.material.secretName: the Secret holding the files the authentication document names (e.g. elitea-main-auth-material)" -}}
{{- end -}}
{{- /* elitea-main.validateCapabilities: index ingest composes through the Configurations plane. */ -}}
{{- if and (include "elitea.indexingEnabled" .) (ne (include "elitea-main.configurationsEnabled" .) "true") (not (get ($v.main.env | default dict) "ELITEA_CONFIGURATIONS_ENABLED")) -}}
{{- $out = append $out "platform.aiProjectId: the platform's public project id (e.g. \"1\"). Indexing composes through the Configurations plane, which comes on with authentication plus this id" -}}
{{- end -}}
{{- /* elitea-main.validateCapabilities: the self-referential-credential guard of the Configurations plane. */ -}}
{{- $mainEnv := $v.main.env | default dict -}}
{{- if and (or (eq (include "elitea-main.configurationsEnabled" .) "true") (include "elitea.indexingEnabled" .)) (not (get $mainEnv "DEPLOYMENT_URL")) (not (get $mainEnv "ELITEA_SELF_LLM_ORIGINS")) -}}
{{- $out = append $out "main.env.DEPLOYMENT_URL: this deployment's public base URL, e.g. https://elitea.example.com (or main.env.ELITEA_SELF_LLM_ORIGINS; the Configurations plane refuses a credential whose api_base points back at <it>/llm)" -}}
{{- end -}}
{{- if and (not $v.main.runtime.material.secretName) (not $v.main.runtime.material.volume) -}}
{{- $out = append $out "main.runtime.material.secretName: the runtime material Secret (default elitea-runtime-material; deploy/scripts/gen-runtime-certs.sh mints its files)" -}}
{{- end -}}
{{- end -}}
{{- /* The worker's identity: elitea-main.validateModelServiceClients and the workload-session Job. */ -}}
{{- if and $runtime $v.worker.enabled -}}
{{- if not $v.worker.runtimeSession.expectedIdentity -}}
{{- $out = append $out "worker.runtimeSession.expectedIdentity: worker.enabled needs worker.runtimeSession.expectedIdentity, the SPIFFE ID (the one URI SAN) of the worker's agent-worker-client certificate, e.g. spiffe://elitea.standalone/ns/default/sa/agent-worker (gen-runtime-certs.sh's default)" -}}
{{- end -}}
{{- if not $v.worker.materialSecretName -}}
{{- $out = append $out "worker.materialSecretName: the worker material Secret (default elitea-worker-material)" -}}
{{- end -}}
{{- end -}}
{{- /* The git allowlists: elitea-deepwiki.validateGuards, elitea-inventory's native-engine guard. */ -}}
{{- if $v.deepwiki.enabled -}}
{{- if not (get ($v.deepwiki.env | default dict) "ELITEA_DEEPWIKI_GIT_ALLOWLIST") -}}
{{- $out = append $out "deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST: the git hosts DeepWiki may clone from, comma-separated (e.g. \"github.com,*.github.com\"), or \"*\" to disable the control; the chart copies it to elitea-main" -}}
{{- end -}}
{{- end -}}
{{- if include "elitea-inventory.modelCalls" . -}}
{{- if not (get ($v.inventory.env | default dict) "ELITEA_INVENTORY_GIT_ALLOWLIST") -}}
{{- $out = append $out "inventory.env.ELITEA_INVENTORY_GIT_ALLOWLIST: the git hosts the Inventory engine may clone from, or \"*\"; the chart copies it to elitea-main" -}}
{{- end -}}
{{- end -}}
{{- /* Bring-your-own Qdrant: elitea-vector.validateGuards. */ -}}
{{- if and $v.vector.enabled (not ($v.qdrant | default dict).enabled) (not $v.vector.qdrant.external.url) -}}
{{- $out = append $out "vector.qdrant.external.url: qdrant.enabled is false, so elitea-vector needs the gRPC URL of the Qdrant you run, e.g. https://qdrant.example.internal:6334 (plus vector.qdrant.external.caSecretName for a private CA)" -}}
{{- end -}}
{{- /* Who reaches elitea-main from outside the chart: guards.yaml. */ -}}
{{- $np := $v.networkPolicies -}}
{{- if and $np.enabled $v.main.enabled (not $np.main.ingressFrom) (not $np.main.extraIngressFrom) (not $np.main.noExternalIngress) -}}
{{- $out = append $out "networkPolicies.main.ingressFrom: an explicit decision about who may reach elitea-main:8080 — the ingress controller or Gateway API gateway, as NetworkPolicyPeer entries (or networkPolicies.main.noExternalIngress=true when only kubectl port-forward does)" -}}
{{- end -}}
{{- /* The LLM gateway's postures: llmGateway/deployment.yaml. */ -}}
{{- if $v.llmGateway.enabled -}}
{{- if not (get ($v.llmGateway.env | default dict) "GATEWAY_SELF_LLM_ORIGINS") -}}
{{- $out = append $out "llmGateway.env.GATEWAY_SELF_LLM_ORIGINS: every origin that reaches this deployment's /llm surface, comma-separated, e.g. \"https://elitea.example.com/llm/v1,http://elitea-main:8080/llm/v1\"" -}}
{{- end -}}
{{- if not (has ($v.llmGateway.egressPosture | default "" | toString) (list "allowlist" "public-unrestricted")) -}}
{{- $out = append $out "llmGateway.egressPosture must be one of \"allowlist\" (with your model hosts in llmGateway.env.GATEWAY_EGRESS_ALLOWLIST) or \"public-unrestricted\"" -}}
{{- end -}}
{{- end -}}
{{- join "\n" $out -}}
{{- end }}

{{/*
elitea.callbackClientIssued — "true" when THIS provider's
callbackClient.certManager renders the cert-manager Certificate for its
callbackClient.secretName (S10: no hand-minted callback certificate on a
default install). Pass (dict "ctx" $ "provider" "deepwiki"|"inventory").

One Secret has one Certificate: when Inventory names the Secret DeepWiki
already issues (the two engines present the same identity), Inventory issues
nothing and mounts DeepWiki's.
*/}}
{{- define "elitea.callbackClientIssued" -}}
{{- $p := get .ctx.Values .provider -}}
{{- $cm := ($p.callbackClient | default dict).certManager | default dict -}}
{{- if and $p.enabled $cm.enabled $p.callbackClient.secretName -}}
{{- if eq .provider "inventory" -}}
{{- $dw := .ctx.Values.deepwiki -}}
{{- if not (and (include "elitea.callbackClientIssued" (dict "ctx" .ctx "provider" "deepwiki")) (eq ($dw.callbackClient.secretName | toString) ($p.callbackClient.secretName | toString))) -}}true{{- end -}}
{{- else -}}
true
{{- end -}}
{{- end -}}
{{- end }}

{{/*
elitea.callbackClientCertificate — the Certificate itself, shared by both
providers' certificates.yaml. The identity is fixed: elitea-main's platform
listener admits the sub-application callback as dns:elitea-subapp-callback
(ELITEA_MODEL_SERVICE_CLIENTS, ELITEA_ARTIFACT_SERVICE_CLIENTS,
ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS), the same name
deploy/scripts/gen-runtime-certs.sh mints, with exactly one DNS SAN and
client auth only. PKCS#8, which the Go host and the Rust engines both read.
*/}}
{{- define "elitea.callbackClientCertificate" -}}
{{- $p := get .ctx.Values .provider -}}
{{- $cm := $p.callbackClient.certManager -}}
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ printf "elitea-%s-callback-client" .provider }}
  labels:
    {{- include (printf "elitea-%s.labels" .provider) .ctx | nindent 4 }}
spec:
  # The callback identity on elitea-main's platform listener (#1359 U2/U5),
  # mounted by the {{ .provider }} host and engine at
  # {{ include (printf "elitea-%s.callbackClientPath" .provider) .ctx }}.
  secretName: {{ $p.callbackClient.secretName }}
  duration: {{ $cm.duration }}
  renewBefore: {{ $cm.renewBefore }}
  isCA: false
  usages:
    - client auth
  commonName: elitea-subapp-callback
  dnsNames:
    - elitea-subapp-callback
  privateKey:
    algorithm: ECDSA
    size: 256
    encoding: PKCS8
    rotationPolicy: Always
  issuerRef:
    name: {{ $cm.issuerRef.name }}
    kind: {{ $cm.issuerRef.kind }}
    group: cert-manager.io
{{- end }}
