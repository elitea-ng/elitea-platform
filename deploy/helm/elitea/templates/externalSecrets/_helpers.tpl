{{/*
  elitea.externalSecrets.remoteRef — where ONE key of ONE Secret comes from, as
  JSON {key, property, version}. First match wins:

    1. externalSecrets.secrets.<Secret>.keys.<key>.remoteKey — that remote
       secret; `property` only when given (empty = the whole value, the usual
       GCP Secret Manager shape of one secret per value).
    2. externalSecrets.secrets.<Secret>.remoteKey — one remote secret per
       Kubernetes Secret, a JSON object whose properties are the key names
       verbatim ("tls.crt", "database-url").
    3. externalSecrets.remoteKey — one remote secret for the whole install, a
       FLAT JSON object whose properties are "<Secret>/<key>"
       ("elitea-main-db/database-url").

  The property names keep their dots: ESO's GCP, AWS and Vault providers try the
  property as a literal JSON key before reading it as a gjson path, so
  "tls.crt" is the key "tls.crt" and not tls -> crt. "/" separates the Secret
  from its key in (3) because no gjson path syntax uses it.

  Nothing matching is a render failure naming the key, not an ExternalSecret
  that ESO rejects after the install already looked healthy.
*/}}
{{- define "elitea.externalSecrets.remoteRef" -}}
{{- $k := get (.mapped.keys | default dict) .key | default dict -}}
{{- if $k.remoteKey -}}
{{- toJson (dict "key" $k.remoteKey "property" ($k.property | default "") "version" ($k.version | default "")) -}}
{{- else if .mapped.remoteKey -}}
{{- toJson (dict "key" .mapped.remoteKey "property" ($k.property | default .key) "version" (.mapped.version | default "")) -}}
{{- else if .es.remoteKey -}}
{{- toJson (dict "key" .es.remoteKey "property" ($k.property | default (printf "%s/%s" .secret .key)) "version" (.es.version | default "")) -}}
{{- else -}}
{{- fail (printf "externalSecrets: no remote reference for key %q of Secret %q. Set externalSecrets.remoteKey (one remote secret for the install, property %q), externalSecrets.secrets.%s.remoteKey (one remote secret for this Secret, property %q), or externalSecrets.secrets.%s.keys.%s.remoteKey." .key .secret (printf "%s/%s" .secret .key) .secret .key .secret .key) -}}
{{- end -}}
{{- end }}

{{/*
  elitea.externalSecrets.validate — the configurations that would render and
  then misbehave.

  * No store: every ExternalSecret would sit in SecretSyncedError.
  * Double ownership: a Secret written by two controllers is rewritten by each
    in turn, and which value a pod reads depends on who wrote last. The chart
    renders ONE Secret itself (the egress proxy's public keys from
    egressProxy.publicKeys.keys) and cert-manager writes every Certificate's
    Secret; neither may also be an ExternalSecret.
  * A mapping for a Secret nothing reads: almost always a typo of a real name,
    which would leave the real one unfilled. `extra: true` says it is
    deliberate (a Secret the install needs that no pod mounts).
  * An open Secret (operator-owned key names) with neither keys listed nor its
    own remote secret.
  * A credential as a plain env value next to externalSecrets: it is in a
    ConfigMap, the thing this block exists to prevent.
*/}}
{{- define "elitea.externalSecrets.validate" -}}
{{- $es := .Values.externalSecrets -}}
{{- $store := $es.secretStoreRef | default dict -}}
{{- if not $store.name -}}
{{- fail "externalSecrets.enabled needs externalSecrets.secretStoreRef.name: the (Cluster)SecretStore the External Secrets Operator reads from, created outside this chart." -}}
{{- end -}}
{{- if not (has ($store.kind | default "ClusterSecretStore") (list "ClusterSecretStore" "SecretStore")) -}}
{{- fail (printf "externalSecrets.secretStoreRef.kind is %q; it must be ClusterSecretStore or SecretStore." $store.kind) -}}
{{- end -}}
{{- $inv := fromJson (include "elitea.externalSecrets.inventory" .) -}}
{{- $issued := fromJsonArray (include "elitea.externalSecrets.certManagerSecrets" .) -}}
{{- $chartOwned := list -}}
{{- if and .Values.egressProxy.enabled (not .Values.egressProxy.publicKeys.existingSecret) -}}
{{- $chartOwned = append $chartOwned (include "elitea-egress-proxy.keysSecretName" .) -}}
{{- end -}}
{{- range $name, $m := ($es.secrets | default dict) -}}
{{- if has $name $chartOwned -}}
{{- fail (printf "externalSecrets.secrets.%s: this chart renders that Secret itself from egressProxy.publicKeys.keys, so an ExternalSecret would be its second owner. Move the keys to your secret manager and set egressProxy.publicKeys.existingSecret instead, or drop this entry." $name) -}}
{{- end -}}
{{- if has $name $issued -}}
{{- fail (printf "externalSecrets.secrets.%s: a cert-manager Certificate in this chart issues that Secret, so an ExternalSecret would be its second owner and would race every renewal. Drop this entry." $name) -}}
{{- end -}}
{{- if not (kindIs "map" $m) -}}
{{- fail (printf "externalSecrets.secrets.%s must be a map (enabled, remoteKey, keys, type, extra)." $name) -}}
{{- end -}}
{{- if and (not (hasKey $inv $name)) (not $m.extra) (ne (toString $m.enabled) "false") -}}
{{- fail (printf "externalSecrets.secrets.%s names no Secret this release reads (it reads: %s). Fix the name, or set extra: true with its keys for a Secret the install needs that no pod in this chart mounts." $name (keys $inv | sortAlpha | join ", ")) -}}
{{- end -}}
{{- if and $m.extra (not $m.keys) -}}
{{- fail (printf "externalSecrets.secrets.%s is extra: true and needs keys: the chart cannot know a Secret it does not read." $name) -}}
{{- end -}}
{{- end -}}
{{- range $name, $entry := $inv -}}
{{- $m := get ($es.secrets | default dict) $name | default dict -}}
{{- if and $entry.open (ne (toString $m.enabled) "false") (not $m.keys) (not $m.remoteKey) -}}
{{- fail (printf "externalSecrets: the key names of Secret %q (%s) are yours, not the chart's, so the chart cannot list them. Set externalSecrets.secrets.%s.keys (each key, {} for the default remote reference), or externalSecrets.secrets.%s.remoteKey to copy every property of one remote secret, or enabled: false to keep providing it yourself." $name (join ", " $entry.consumers) $name $name) -}}
{{- end -}}
{{- end -}}
{{- range $component := list "main" "llmGateway" "scheduler" -}}
{{- $env := (get $.Values $component).env | default dict -}}
{{- range $var := list "DATABASE_URL" "APPLICATION_SECRET_KEY" "OIDC_CLIENT_SECRET" "S3_ACCESS_KEY" "S3_SECRET_KEY" "AZURE_STORAGE_KEY" "SMTP_PASSWORD" "GATEWAY_IDENTITY_SECRET" "GATEWAY_HOP_SECRET" -}}
{{- if ne (get $env $var | toString) "" -}}
{{- fail (printf "%s.env.%s is set while externalSecrets.enabled: env is rendered into a ConfigMap, so the credential is readable by anything that reads ConfigMaps. Reference it from %s.secrets.%s (secretName/key) and let the ExternalSecret fill that Secret." $component $var $component $var) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
