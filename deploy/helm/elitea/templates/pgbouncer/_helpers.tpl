{{- define "elitea-pgbouncer.name" -}}
{{- default "elitea-pgbouncer" .Values.pgbouncer.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-pgbouncer.fullname" -}}
{{- default .Values.pgbouncer.service.name .Values.pgbouncer.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "elitea-pgbouncer.labels" -}}
helm.sh/chart: {{ printf "%s-%s" "elitea-pgbouncer" .Chart.Version | replace "+" "_" }}
{{ include "elitea-pgbouncer.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "elitea-pgbouncer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "elitea-pgbouncer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
elitea-pgbouncer.validate — the half-configured pooler, refused at render time.

The pooler fails in the direction a health check cannot see: pgbouncer starts,
reports Ready on its listener, and then refuses every connection — to the
server because its userlist is wrong, or for every client because no
[databases] entry names the database they ask for. Every refusal below names
the field and the failure it prevents, in the style of the other render-time
guards in this chart.
*/}}
{{- define "elitea-pgbouncer.validate" -}}
{{- $pb := .Values.pgbouncer | default dict -}}
{{- if not $pb.enabled -}}
{{/*
  OFF. A half-filled block is dead rather than active, and this chart exists to
  make dead settings loud.
*/}}
{{- range $field := list "postgresHost" "database" "credentialsSecretName" }}
{{- if get $pb $field }}
{{- fail (printf "pgbouncer.%s is set but pgbouncer.enabled is false, so no pooler is rendered and the setting does nothing. Set pgbouncer.enabled=true, or clear pgbouncer.%s." $field $field) }}
{{- end }}
{{- end }}
{{- else }}

{{- if not $pb.postgresHost }}
{{- fail "pgbouncer.enabled=true needs pgbouncer.postgresHost, the Postgres server the pooler dials. pgbouncer's [databases] section takes host= and port= rather than a DSN, so this chart cannot derive it from postgresql.existingSecret." }}
{{- end }}
{{- if not $pb.database }}
{{- fail "pgbouncer.enabled=true needs pgbouncer.database, the single database the pooler fronts. One [databases] entry is deliberate: a pooler that proxies arbitrary database names proxies every database on the server, which is a hole rather than a pool. It must match the database in the DATABASE_URL that elitea-main's pods will point at this pooler." }}
{{- end }}
{{- if not $pb.credentialsSecretName }}
{{- fail "pgbouncer.enabled=true needs pgbouncer.credentialsSecretName, a Kubernetes Secret with two keys: user and password. The init container builds the pgbouncer userlist from them. It must carry the PASSWORD, not a SCRAM verifier: pgbouncer checks the clients against the userlist and also logs in to Postgres with it, and a verifier cannot do the second half." }}
{{- end }}

{{/*
  The raw values, on purpose: sprig's `default` treats 0 as unset, so
  `default 48 .poolSize` would let the validator see 48 while configmap.yaml
  renders the operator's literal 0 — the exact value the checks below exist
  to refuse. values.yaml always carries these keys.
*/}}
{{- $pool := int $pb.poolSize }}
{{- $reserve := int $pb.reservePoolSize }}
{{- $min := int $pb.minPoolSize }}
{{- if lt $pool 1 }}
{{- fail (printf "pgbouncer.poolSize is %d. It must be at least 1: it is the steady number of server connections every elitea-main replica shares, and 0 is a pooler that queues everything." $pool) }}
{{- end }}
{{- if lt $reserve 0 }}
{{- fail (printf "pgbouncer.reservePoolSize is %d. It cannot be negative; 0 disables the burst reserve, which is a choice, but only a non-negative one is renderable." $reserve) }}
{{- end }}
{{- if gt $min $pool }}
{{- fail (printf "pgbouncer.minPoolSize (%d) exceeds pgbouncer.poolSize (%d). pgbouncer keeps minPoolSize server connections open at all times, so it cannot ask for more idle connections than the pool holds." $min $pool) }}
{{- end }}
{{- $client := int $pb.maxClientConn }}
{{- if lt $client 1 }}
{{- fail (printf "pgbouncer.maxClientConn is %d. It is the ceiling on elitea-main's client connections and must be at least 1." $client) }}
{{- end }}
{{- $prepared := int $pb.maxPreparedStatements }}
{{- if lt $prepared 1 }}
{{- fail (printf "pgbouncer.maxPreparedStatements is %d. elitea-main's pgx driver caches named prepared statements per client connection, and 0 disables pgbouncer's tracking of them: every backend reassignment would then answer 42P05 'prepared statement already exists', measured 2026-09-01. It must be at least 1." $prepared) }}
{{- end }}
{{- end }}
{{- end }}
