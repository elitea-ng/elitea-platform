{{/*
Shared innards of the workload-session provisioning script: the container spec
and the volumes it mounts.

Used by BOTH runtime-session-job.yaml (the pre-install/pre-upgrade hook that
provisions elitea_runtime.workload_sessions before the worker's first start)
and runtime-session-renew-cronjob.yaml (the recurring renewal — see that file
for why one is not enough: nothing else re-stamps expires_at, so a cluster
never upgraded goes dark on a timer with the exact signature of never having
been provisioned).

Extracted here rather than duplicated so the two callers cannot drift: the
CronJob has to run EXACTLY the upsert-and-verify logic the pre-install hook
runs, including the SPIFFE-SAN extraction and validation, or a renewal could
silently authorize a different identity than the one actually presented.

deploy/runtime/provision-runtime-session.sh is a VERBATIM MIRROR of the shell
script embedded in the `command:` block below, run by compose's
`runtime-session-init` service so the standalone stack authorizes the same
row before its worker starts. It is a second copy, not a shared file the two
render from, because Helm charts here stay self-contained artifacts
independent of the monorepo layout they ship from — see the comment on
`config:` next to `otelCollector` in values.yaml for the same rule applied to
a different file. Change one script, change the other, in the same commit.
*/}}

{{- define "elitea-worker-python.runtimeSessionContainer" -}}
{{- $rs := .Values.worker.runtimeSession -}}
- name: provision
  image: "{{ $rs.image.repository }}:{{ $rs.image.tag }}"
  imagePullPolicy: {{ $rs.image.pullPolicy }}
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop: ["ALL"]
  env:
    # The URL carries a password, so it comes from the same Secret
    # elitea-main reads rather than from any rendered manifest.
    - name: PGURL
      valueFrom:
        secretKeyRef:
          name: {{ required "postgresql.existingSecret is required to provision the workload session" (default .Values.postgresql.existingSecret $rs.database.secretName) }}
          key: {{ required "postgresql.key is required to provision the workload session" (default .Values.postgresql.key $rs.database.key) }}
    - name: SESSION_ID
      value: {{ .Values.worker.runtime.workloadSessionId | quote }}
    - name: PRODUCER_ID
      value: {{ .Values.worker.runtime.producerId | quote }}
    - name: EXPECTED_IDENTITY
      value: {{ $rs.expectedIdentity | quote }}
    - name: TTL
      value: {{ $rs.ttl | quote }}
    - name: WAIT_ATTEMPTS
      value: {{ $rs.waitAttempts | quote }}
    - name: WAIT_INTERVAL
      value: {{ $rs.waitIntervalSeconds | quote }}
  command:
    - /bin/sh
    - -ec
    - |
      CERT=/run/elitea-runtime/agent-worker-client.crt

      # ── The identity, read from the certificate itself ────────────
      #
      # internal/auth/workloadidentity/identity.go accepts a
      # certificate with EXACTLY ONE URI SAN and no DNS names, and uses
      # that URI verbatim. Reproduce that rule here rather than trusting
      # a value: a row naming an identity the worker does not present
      # authorizes nobody, and fails as an indistinguishable
      # "unauthorized" hours later at the first chat turn.
      SAN="$(openssl x509 -in "$CERT" -noout -ext subjectAltName 2>/dev/null || true)"
      if [ -z "$SAN" ]; then
        echo "ERROR: $CERT has no subjectAltName extension; the runtime" >&2
        echo "       plane derives every identity from one, so this" >&2
        echo "       certificate can never be authorized." >&2
        exit 1
      fi

      IDENTITY="$(printf '%s' "$SAN" \
        | tr ',' '\n' \
        | sed -n 's/.*URI:\(.*\)/\1/p' \
        | tr -d ' \t')"
      COUNT="$(printf '%s\n' "$IDENTITY" | grep -c . || true)"

      if [ "$COUNT" -ne 1 ]; then
        echo "ERROR: expected exactly one URI SAN on $CERT, found $COUNT." >&2
        echo "       workloadidentity.Certificate refuses anything else," >&2
        echo "       so this certificate cannot be authorized as-is." >&2
        echo "       subjectAltName was: $SAN" >&2
        exit 1
      fi

      # Go requires exactly one URI SAN AND ZERO DNS names
      # (internal/auth/workloadidentity/identity.go). Checking only the
      # URI count would let a certificate carrying both pass here and be
      # refused there — provisioning a row that authorizes nobody while
      # this Job prints success.
      DNSCOUNT="$(printf '%s' "$SAN" | tr ',' '\n' | grep -c '^ *DNS:' || true)"
      if [ "$DNSCOUNT" -ne 0 ]; then
        echo "ERROR: $CERT carries $DNSCOUNT DNS SAN(s) alongside its URI SAN." >&2
        echo "       workloadidentity.Certificate accepts one URI SAN and NO" >&2
        echo "       DNS names, so this certificate can never be authorized." >&2
        echo "       subjectAltName was: $SAN" >&2
        exit 1
      fi

      case "$IDENTITY" in
        spiffe://*) : ;;
        *) echo "ERROR: URI SAN '$IDENTITY' is not a spiffe:// identity." >&2; exit 1 ;;
      esac

      if [ -n "$EXPECTED_IDENTITY" ] && [ "$EXPECTED_IDENTITY" != "$IDENTITY" ]; then
        echo "ERROR: the worker certificate presents '$IDENTITY' but" >&2
        echo "       runtimeSession.expectedIdentity asserts" >&2
        echo "       '$EXPECTED_IDENTITY'. Refusing to authorize an" >&2
        echo "       identity the chart did not expect." >&2
        exit 1
      fi

      echo "identity:   $IDENTITY"
      echo "session id: $SESSION_ID"
      echo "producer:   $PRODUCER_ID"

      # ── Wait for the schema ──────────────────────────────────────
      #
      # elitea-main owns and migrates elitea_runtime. On a fresh
      # install its migration may still be running, and on this
      # deployment it is a separate release entirely, so the table is
      # waited for rather than assumed.
      attempt=0
      until [ "$(psql "$PGURL" -Atqc \
                  "SELECT to_regclass('elitea_runtime.workload_sessions') IS NOT NULL" \
                  2>/dev/null)" = "t" ]; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge "$WAIT_ATTEMPTS" ]; then
          echo "ERROR: elitea_runtime.workload_sessions did not appear after" >&2
          echo "       $WAIT_ATTEMPTS attempts. Has elitea-main's migration run?" >&2
          exit 1
        fi
        echo "  waiting for elitea_runtime.workload_sessions ($attempt/$WAIT_ATTEMPTS)…"
        sleep "$WAIT_INTERVAL"
      done

      # ── Upsert ───────────────────────────────────────────────────
      #
      # Idempotent, and re-stamps expires_at every run. That renewal is
      # not incidental: expires_at is NOT NULL, nothing in the product
      # ever writes this table, and the verifier requires
      # issued_at <= now() < expires_at AND revoked_at IS NULL — so
      # without a periodic re-stamp a working cluster goes dark on a
      # timer. revoked_at is cleared for the same reason: an upgrade
      # (or a scheduled renewal) is a re-authorization.
      psql "$PGURL" -v ON_ERROR_STOP=1 \
        -v identity="$IDENTITY" \
        -v session="$SESSION_ID" \
        -v producer="$PRODUCER_ID" \
        -v ttl="$TTL" <<'SQL'
      INSERT INTO elitea_runtime.workload_sessions
          (workload_session_id, workload_identity, producer_id, expires_at)
      VALUES
          (:'session', :'identity', :'producer',
           clock_timestamp() + :'ttl'::interval)
      ON CONFLICT (workload_session_id) DO UPDATE
          SET workload_identity = EXCLUDED.workload_identity,
              producer_id       = EXCLUDED.producer_id,
              expires_at        = EXCLUDED.expires_at,
              revoked_at        = NULL;
      SQL

      # ── Prove it, with the query the server actually runs ─────────
      #
      # Re-checking all three columns together is the point: it is the
      # same conjunction WorkloadSessionsRepository.VerifyActiveSession
      # applies, so a pass here means the worker will be admitted and
      # not merely that a row was written.
      ok="$(psql "$PGURL" -Atq \
        -v identity="$IDENTITY" \
        -v session="$SESSION_ID" \
        -v producer="$PRODUCER_ID" <<'SQL'
      SELECT EXISTS (
        SELECT 1 FROM elitea_runtime.workload_sessions
        WHERE workload_session_id = :'session'
          AND workload_identity   = :'identity'
          AND producer_id         = :'producer'
          AND issued_at <= clock_timestamp()
          AND expires_at > clock_timestamp()
          AND revoked_at IS NULL);
      SQL
      )"

      if [ "$ok" != "t" ]; then
        echo "ERROR: the row was written but does not verify. The worker" >&2
        echo "       would be refused at ClaimCommand." >&2
        exit 1
      fi

      echo "workload session authorized; expires in $TTL"
  resources:
    {{- toYaml $rs.resources | nindent 4 }}
  volumeMounts:
    - name: material
      mountPath: /run/elitea-runtime
      readOnly: true
{{- end }}

{{- define "elitea-worker-python.runtimeSessionVolumes" -}}
# The worker's own material, read-only and for one file: the
# certificate whose SAN is the identity being authorized. Reading it
# here rather than taking the identity as a value is what makes the row
# and the certificate impossible to disagree about.
- name: material
  secret:
    secretName: {{ required "materialSecretName is required" .Values.worker.materialSecretName }}
    items:
      - key: agent-worker-client.crt
        path: agent-worker-client.crt
    defaultMode: 0440
{{- end }}
