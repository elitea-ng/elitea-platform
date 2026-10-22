{{/*
  The Secret inventory: every Kubernetes Secret this release's pods reference
  and an operator (not cert-manager, not this chart) must create, with the keys
  they read. templates/externalSecrets/externalsecrets.yaml renders one
  ExternalSecret per entry.

  WHY IT IS DERIVED AND NOT LISTED. A hand-written list of Secret names in
  values.yaml would be a second copy of decisions the workload templates
  already make — which component is on, which reference wins over the
  postgresql.existingSecret fallback, whether material comes from a Secret or a
  CSI volume. The first time the two disagreed, an ExternalSecret would fill a
  Secret no pod reads while the pod waits on one nobody fills, and the install
  would look exactly like a slow secret manager. So every rule below restates a
  rule in the template named next to it, and
  deploy/helm/tests/render-external-secrets.sh compares the result with the
  secretKeyRef and secret volumes of the RENDERED workloads, so a template that
  starts reading a new Secret fails that test until it is added here.

  Returned as JSON (Helm defines return strings):
    { "<Secret name>": { "keys": [...], "open": bool, "consumers": [...] } }
  `open` marks a Secret whose key names belong to the operator rather than to
  the chart (the sandbox supervisor material, the egress proxy's public keys,
  authentication material named by an external ConfigMap): the chart cannot
  list them, so the operator must (externalSecrets.secrets.<name>.keys) or map
  the whole remote secret (externalSecrets.secrets.<name>.remoteKey).

  NOT IN IT, ON PURPOSE:
    * Secrets a cert-manager Certificate in this chart issues (the gateway,
      DeepWiki, Inventory, elitea-vector, Qdrant and egress-audit mTLS pairs,
      the sub-application callback client, every NATS client identity).
      cert-manager owns them; a second writer would race every renewal. A
      reference that points at one of them is skipped here.
    * image pull secrets (image.pullSecrets, <component>.imagePullSecrets): a
      dockerconfigjson is cluster plumbing, normally replaced on GKE by
      Artifact Registry access through the node or workload identity.
    * the egress proxy's public-key Secret when the chart renders it from
      egressProxy.publicKeys.keys (validate refuses a second owner).
*/}}
{{- define "elitea.externalSecrets.inventory" -}}
{{- $inv := dict -}}
{{- $v := .Values -}}
{{- $pg := $v.postgresql | default dict -}}
{{- $pgRef := dict -}}
{{- if $pg.existingSecret -}}{{- $pgRef = dict "secretName" $pg.existingSecret "key" $pg.key -}}{{- end -}}
{{- $issued := fromJsonArray (include "elitea.externalSecrets.certManagerSecrets" .) -}}
{{- $tlsKeys := list "tls.crt" "tls.key" "ca.crt" -}}

{{- /* ── elitea-main: templates/main/deployment.yaml, migrate-job.yaml ── */ -}}
{{- if $v.main.enabled -}}
{{- $secrets := $v.main.secrets | default dict -}}
{{- if and $pgRef (not (hasKey $secrets "DATABASE_URL")) -}}
{{- $secrets = merge (deepCopy $secrets) (dict "DATABASE_URL" $pgRef) -}}
{{- end -}}
{{- $masterKeyRef := include "elitea-main.masterKeyRef" . -}}
{{- if and $masterKeyRef (not (hasKey $secrets "SECRETS_MASTER_KEY")) -}}
{{- $secrets = merge (deepCopy $secrets) (dict "SECRETS_MASTER_KEY" (fromJson $masterKeyRef)) -}}
{{- end -}}
{{- include "elitea.externalSecrets.addEnvMap" (dict "inv" $inv "ctx" . "secrets" $secrets "consumer" "elitea-main") -}}
{{- /* With the pooler on, the migrate Job keeps the DIRECT DSN even when
       main.secrets.DATABASE_URL points elitea-main at the pooler. */ -}}
{{- if and $v.main.migrations.enabled ($v.pgbouncer | default dict).enabled $pgRef -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $pgRef.secretName "keys" (list $pgRef.key) "consumer" "elitea-main migrate Job") -}}
{{- end -}}
{{- $runtime := $v.main.runtime | default dict -}}
{{- if and $runtime.enabled $runtime.material.secretName -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $runtime.material.secretName "keys" (fromJsonArray (include "elitea.externalSecrets.mainRuntimeMaterialKeys" .)) "consumer" "elitea-main runtime material") -}}
{{- end -}}
{{- $auth := $v.main.fileConfig.authConfig -}}
{{- if and $auth.enabled $auth.material.secretName -}}
{{- if $auth.document -}}
{{- $keys := list -}}
{{- range (splitList "\n" (include "elitea-main.authMaterialPaths" .)) -}}
{{- if . }}{{- $keys = append $keys (base .) -}}{{- end -}}
{{- end -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $auth.material.secretName "keys" $keys "consumer" "elitea-main authentication material") -}}
{{- else -}}
{{- /* configMapName mode: the three file names live in a document this
       chart never sees. */ -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $auth.material.secretName "keys" (list) "open" true "consumer" "elitea-main authentication material") -}}
{{- end -}}
{{- end -}}
{{- range $material := list "llmGatewayClientMaterial" "deepwikiClientMaterial" "inventoryClientMaterial" -}}
{{- $m := get $v.main.fileConfig $material -}}
{{- if and $m.enabled (not $m.volume) $m.secretName (not (has $m.secretName $issued)) -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $m.secretName "keys" $tlsKeys "consumer" (printf "elitea-main fileConfig.%s" $material)) -}}
{{- end -}}
{{- end -}}
{{- $vm := $v.main.fileConfig.vectorClientMaterial -}}
{{- if and (not (include "elitea-vector.mainClientEnabled" .)) $vm.enabled (not $vm.volume) $vm.secretName (not (has $vm.secretName $issued)) -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $vm.secretName "keys" $tlsKeys "consumer" "elitea-main fileConfig.vectorClientMaterial") -}}
{{- end -}}
{{- if $v.egressProxy.enabled -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $v.egressProxy.signingKey.existingSecret "keys" (list $v.egressProxy.signingKey.key) "consumer" "elitea-main egress run-token signing key") -}}
{{- end -}}
{{- end -}}

{{- /* ── elitea-llm-gateway: templates/llmGateway/deployment.yaml ── */ -}}
{{- if $v.llmGateway.enabled -}}
{{- $secrets := $v.llmGateway.secrets | default dict -}}
{{- if and $pgRef (not (hasKey $secrets "DATABASE_URL")) -}}
{{- $secrets = merge (deepCopy $secrets) (dict "DATABASE_URL" $pgRef) -}}
{{- end -}}
{{- $masterKeyRef := include "elitea-llm-gateway.masterKeyRef" . -}}
{{- if $masterKeyRef -}}
{{- $secrets = merge (omit $secrets "SECRETS_MASTER_KEY") (dict "SECRETS_MASTER_KEY" (fromJson $masterKeyRef)) -}}
{{- else -}}
{{- $secrets = omit $secrets "SECRETS_MASTER_KEY" -}}
{{- end -}}
{{- include "elitea.externalSecrets.addEnvMap" (dict "inv" $inv "ctx" . "secrets" $secrets "consumer" "elitea-llm-gateway") -}}
{{- end -}}

{{- /* ── elitea-scheduler: templates/scheduler/deployment.yaml ── */ -}}
{{- if $v.scheduler.enabled -}}
{{- $secrets := $v.scheduler.secrets | default dict -}}
{{- if and $pgRef (not (hasKey $secrets "DATABASE_URL")) -}}
{{- $secrets = merge (deepCopy $secrets) (dict "DATABASE_URL" $pgRef) -}}
{{- end -}}
{{- include "elitea.externalSecrets.addEnvMap" (dict "inv" $inv "ctx" . "secrets" $secrets "consumer" "elitea-scheduler") -}}
{{- end -}}

{{- /* ── DeepWiki and Inventory: their deployment.yaml and migrate-job.yaml,
       through elitea.provider.databaseSecret like the templates. ── */ -}}
{{- range $provider := list "deepwiki" "inventory" -}}
{{- $p := get $v $provider -}}
{{- if $p.enabled -}}
{{- $urlKey := printf "ELITEA_%s_DATABASE_URL" (upper $provider) -}}
{{- $secrets := fromJson (include "elitea.provider.databaseSecret" (dict "ctx" $ "provider" $provider "urlKey" $urlKey)) -}}
{{- include "elitea.externalSecrets.addEnvMap" (dict "inv" $inv "ctx" $ "secrets" $secrets "consumer" (printf "elitea-%s" $provider)) -}}
{{- /* The callback hop: the operator's runtime-CA client certificate, and
       ONLY runtime-ca.crt out of the worker's material Secret. */ -}}
{{- if not (has $p.callbackClient.secretName $issued) -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $p.callbackClient.secretName "keys" (list "tls.crt" "tls.key") "consumer" (printf "elitea-%s callback client" $provider)) -}}
{{- end -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $v.worker.materialSecretName "keys" (list "runtime-ca.crt") "consumer" (printf "elitea-%s runtime CA" $provider)) -}}
{{- end -}}
{{- end -}}

{{- /* ── the worker: templates/worker/deployment.yaml, _runtime-session.tpl ── */ -}}
{{- if $v.worker.enabled -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $v.worker.materialSecretName "keys" (splitList " " (include "elitea-worker.materialFiles" . | trim)) "consumer" "elitea-worker material") -}}
{{- $rs := $v.worker.runtimeSession | default dict -}}
{{- if $rs.enabled -}}
{{- $db := $rs.database | default dict -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" (default $pg.existingSecret $db.secretName) "keys" (list (default $pg.key $db.key)) "consumer" "workload-session provisioning Job") -}}
{{- end -}}
{{- end -}}

{{- /* ── elitea-vector and its Qdrant: templates/vector/ ── */ -}}
{{- if $v.vector.enabled -}}
{{- $q := $v.vector.qdrant -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $q.apiKeySecret.name "keys" (list $q.apiKeySecret.key) "consumer" "elitea-vector / Qdrant API key") -}}
{{- if and (not (include "elitea-vector.qdrantTls" .)) $q.external.caSecretName -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $q.external.caSecretName "keys" (list "ca.crt") "consumer" "elitea-vector external Qdrant CA") -}}
{{- end -}}
{{- end -}}

{{- /* ── the egress proxy's public keys, when an existing Secret holds them ── */ -}}
{{- if and $v.egressProxy.enabled $v.egressProxy.publicKeys.existingSecret -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $v.egressProxy.publicKeys.existingSecret "keys" (list) "open" true "consumer" "elitea-egress-proxy run-token public keys") -}}
{{- end -}}

{{- /* ── PgBouncer: templates/pgbouncer/deployment.yaml ── */ -}}
{{- if ($v.pgbouncer | default dict).enabled -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $v.pgbouncer.credentialsSecretName "keys" (list "user" "password") "consumer" "elitea-pgbouncer") -}}
{{- end -}}

{{- /* ── the one-shot Jobs ── */ -}}
{{- if $v.dbInit.enabled -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $v.dbInit.database.existingSecret "keys" (list $v.dbInit.database.key) "consumer" "db-init Job (CREATE DATABASE)") -}}
{{- end -}}
{{- if $v.objectInit.enabled -}}
{{- $c := $v.objectInit.credentials -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $c.secretName "keys" (list $c.accessKeyKey $c.secretKeyKey) "consumer" "object-init Job") -}}
{{- end -}}
{{- if $v.agentStateMigrations.enabled -}}
{{- $d := $v.agentStateMigrations.databaseSecret -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $d.name "keys" (list $d.key) "consumer" "agent-state migrate Job") -}}
{{- end -}}

{{- /* ── the sandbox supervisor: templates/sandbox/supervisor.yaml ── */ -}}
{{- $sk := $v.sandboxKubernetes | default dict -}}
{{- if and ($sk.supervisor | default dict).enabled $sk.supervisor.materialSecret -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $inv "name" $sk.supervisor.materialSecret "keys" (list) "open" true "consumer" "sandbox supervisor material") -}}
{{- end -}}

{{- $inv | toJson -}}
{{- end }}

{{/*
  elitea.externalSecrets.add — one consumer's reference into the inventory.
  Mutates .inv in place (a sprig dict is shared by reference), so callers
  discard the output. Keys are unioned: two consumers of one Secret that read
  different keys need both.
*/}}
{{- define "elitea.externalSecrets.add" -}}
{{- if .name -}}
{{- $e := get .inv .name | default (dict "keys" (list) "open" false "consumers" (list)) -}}
{{- $_ := set $e "keys" (concat $e.keys (.keys | default list) | uniq | sortAlpha) -}}
{{- $_ := set $e "open" (or $e.open (.open | default false)) -}}
{{- $_ := set $e "consumers" (append $e.consumers .consumer | uniq) -}}
{{- $_ := set .inv .name $e -}}
{{- end -}}
{{- end }}

{{/*
  elitea.externalSecrets.addEnvMap — a component's resolved `secrets` map
  (ENV NAME -> {secretName, key, optional}), the shape every Deployment ranges
  over to render its secretKeyRef entries.

  A REQUIRED reference is always added: the pod does not start without it.

  An `optional: true` reference is added only when the deployment uses it,
  because ESO refuses to sync an ExternalSecret one of whose properties is
  missing remotely — it would then withhold the WHOLE Secret, required keys
  included, over a credential this deployment never reads (an Azure key on a
  GCS install). "Uses it" is the rule each optional entry's own comment in
  values.yaml states; an entry with no rule here (ELITEA_LEGACY_PGVECTOR_
  ADMIN_URL, an operator's own optional entry) is added only when
  externalSecrets.secrets names that Secret.
*/}}
{{- define "elitea.externalSecrets.addEnvMap" -}}
{{- $ctx := .ctx -}}
{{- $mainEnv := $ctx.Values.main.env | default dict -}}
{{- $mapped := $ctx.Values.externalSecrets.secrets | default dict -}}
{{- $backend := get $mainEnv "STORAGE_BACKEND" | toString -}}
{{- range $name, $ref := .secrets -}}
{{- if and $ref (kindIs "map" $ref) $ref.secretName $ref.key -}}
{{- $wanted := not $ref.optional -}}
{{- if $ref.optional -}}
{{- if has $name (list "SECRETS_MASTER_KEY" "GATEWAY_HOP_SECRET") -}}{{- $wanted = true -}}
{{- else if eq $name "ELITEA_DEEPWIKI_IDENTITY_SECRET" -}}{{- $wanted = $ctx.Values.deepwiki.enabled -}}
{{- else if eq $name "ELITEA_INVENTORY_IDENTITY_SECRET" -}}{{- $wanted = $ctx.Values.inventory.enabled -}}
{{- else if eq $name "SMTP_PASSWORD" -}}{{- $wanted = ne (get $mainEnv "SMTP_USERNAME" | toString) "" -}}
{{- else if has $name (list "S3_ACCESS_KEY" "S3_SECRET_KEY") -}}{{- $wanted = eq $backend "s3" -}}
{{- else if eq $name "AZURE_STORAGE_KEY" -}}{{- $wanted = eq $backend "azure" -}}
{{- end -}}
{{- if hasKey $mapped $ref.secretName -}}{{- $wanted = true -}}{{- end -}}
{{- end -}}
{{- if $wanted -}}
{{- include "elitea.externalSecrets.add" (dict "inv" $.inv "name" $ref.secretName "keys" (list $ref.key) "consumer" $.consumer) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
  elitea.externalSecrets.mainRuntimeMaterialKeys — the file names elitea-main's
  runtime material Secret must carry, as a JSON list. The same names
  elitea-main.runtimeEnv and elitea.codeNodes.env write into the ConfigMap; the
  init container (/elitea-runtime-material) derives its copy list from that
  ConfigMap, so the two cannot disagree without the render test noticing.
*/}}
{{- define "elitea.externalSecrets.mainRuntimeMaterialKeys" -}}
{{- $r := .Values.main.runtime -}}
{{- $keys := list "runtime-ca.crt" "command-signing-key.pem" "command-signing-keyring.json" "control-server.crt" "control-server.key" "output-server.crt" "output-server.key" "content-server.crt" "content-server.key" -}}
{{- if ($r.rustCompiledSnapshots | default dict).enabled -}}{{- $keys = concat $keys (list "rust-compiled-profiles.json" "agent-checkpoint-connection") -}}{{- end -}}
{{- if ($r.codeOwnerRecovery | default dict).enabled -}}{{- $keys = concat $keys (list "code-owner-client.crt" "code-owner-client.key") -}}{{- end -}}
{{- if ($r.codePlatform | default dict).enabled -}}{{- $keys = append $keys "code-platform-content-keys.json" -}}{{- end -}}
{{- if ($r.codeDebugArtifacts | default dict).enabled -}}{{- $keys = append $keys "agent-checkpoint-connection" -}}{{- end -}}
{{- $keys | uniq | toJson -}}
{{- end }}

{{/*
  elitea.externalSecrets.certManagerSecrets — the Secret names a cert-manager
  Certificate in THIS chart writes, as a JSON list, each under the same
  condition as its certificates.yaml. A reference to one of them is never an
  ExternalSecret: cert-manager is its owner.

  The NATS client identities (templates/natsClient.yaml) are not listed: no
  operator-settable reference can name them, so the inventory never meets one.
*/}}
{{- define "elitea.externalSecrets.certManagerSecrets" -}}
{{- $v := .Values -}}
{{- $names := list -}}
{{- range $c := list "llmGateway" "deepwiki" "inventory" -}}
{{- $p := get $v $c -}}
{{- if and $p.enabled $p.mtls.enabled -}}
{{- $names = concat $names (list $p.mtls.serverSecretName $p.mtls.clientSecretName) -}}
{{- end -}}
{{- end -}}
{{- if $v.vector.enabled -}}
{{- $names = concat $names (list $v.vector.mtls.serverSecretName $v.vector.mtls.clientSecretName) -}}
{{- if include "elitea-vector.mainClientEnabled" . -}}{{- $names = append $names $v.vector.mainClient.secretName -}}{{- end -}}
{{- with include "elitea-vector.qdrantTlsSecret" . -}}{{- if include "elitea-vector.qdrantTls" $ -}}{{- $names = append $names . -}}{{- end -}}{{- end -}}
{{- end -}}
{{- range $c := list "deepwiki" "inventory" -}}
{{- if include "elitea.callbackClientIssued" (dict "ctx" $ "provider" $c) -}}
{{- $names = append $names (get $v $c).callbackClient.secretName -}}
{{- end -}}
{{- end -}}
{{- if include "elitea-egress-proxy.auditEnabled" . -}}
{{- $names = append $names $v.egressProxy.audit.clientSecretName -}}
{{- end -}}
{{- $names | toJson -}}
{{- end }}
