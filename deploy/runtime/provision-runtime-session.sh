#!/bin/sh
set -eu

# Authorizes the worker's certificate identity by upserting the row
# elitea_runtime.workload_sessions that every runtime control-plane call is
# checked against. Without this row every chat turn dies in the worker with
# AUTHORIZATION_FAILED / delivery_quarantined and the browser shows nothing.
#
# THIS IS A VERBATIM MIRROR of the script body in
# deploy/helm/elitea/templates/worker/_runtime-session.tpl (the container
# `command:` under `elitea-worker-python.runtimeSessionContainer`), which
# provisions the same row for a Kubernetes install. The two are not one file
# read twice: Helm charts here are kept self-contained artifacts independent
# of the monorepo layout they ship from (see the comment on `config:` next to
# `otelCollector` in deploy/helm/elitea/values.yaml for the same rule applied
# to a different file), so the Helm template keeps its own inline copy. Change
# one, change the other, in the same commit.
#
# Six env vars parameterize it, matching the Helm Job's container env exactly:
#   PGURL             postgres connection string (may carry a password)
#   SESSION_ID        elitea_runtime.workload_sessions.workload_session_id
#   PRODUCER_ID       elitea_runtime.workload_sessions.producer_id
#   EXPECTED_IDENTITY optional assertion; empty accepts whatever the cert says
#   TTL               postgres interval literal, e.g. "2160h"
#   WAIT_ATTEMPTS     how many times to poll for the schema before giving up
#   WAIT_INTERVAL     seconds between polls
#
# CERT is not an env var here (compose has one worker identity per stack, not
# a chart value), but it is the same file the Helm Job reads: the worker's own
# client certificate, whose single URI SAN is the identity being authorized.
CERT="${CERT:-/run/elitea-runtime/agent-worker-client.crt}"

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

if [ -n "${EXPECTED_IDENTITY:-}" ] && [ "$EXPECTED_IDENTITY" != "$IDENTITY" ]; then
  echo "ERROR: the worker certificate presents '$IDENTITY' but" >&2
  echo "       EXPECTED_IDENTITY asserts" >&2
  echo "       '$EXPECTED_IDENTITY'. Refusing to authorize an" >&2
  echo "       identity the operator did not expect." >&2
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
# (or a re-run of this script) is a re-authorization.
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
