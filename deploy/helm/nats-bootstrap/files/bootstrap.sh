#!/usr/bin/env sh
# bootstrap.sh — create and reconcile EVERY JetStream asset elitea uses (#1076).
#
# The bootstrap is the ONLY owner. The services bind to these assets and verify
# what they depend on; none of them may create, update, delete or purge a
# stream (deploy/helm/nats/values.yaml, the permission table). So a setting
# here is the setting the cluster runs with — before #1076 the gateway
# re-created its streams on every boot and silently overwrote the retention
# below.
#
#   stream GATEWAY_BUDGET          budget counters (Nats-Incr, AllowMsgCounter);
#                                  one running total per subject
#                                  gateway.budget.counter.<scope>.<id>.<period>
#   stream GATEWAY_RATELIMIT       governance rate-limit counters (Nats-Incr);
#                                  MaxAge expires each minute's window
#   stream GATEWAY_BUDGET_DELTAS   write-behind deltas, subject gateway.budget.delta,
#                                  drained by elitea-scheduler's budget-writeback
#   KV     GATEWAY_ALERT_COOLDOWN  80% soft-alert cooldown (kv.Create + bucket TTL)
#   KV     ELITEA_CANVAS_PRESENCE  elitea-main canvas presence rosters
#
# Create when absent, EDIT when present: re-running this script puts a drifted
# asset back to the values below. An edit the server cannot apply (storage
# type, or switching the counter flag off) fails the Job, which is the point:
# it needs an operator, not a silent skip.
#
# There is NO GATEWAY_BUDGET KV bucket any more. Earlier versions created one
# (stream KV_GATEWAY_BUDGET) that nothing read: the counters were always the
# GATEWAY_BUDGET stream. An install that ran an older bootstrap may delete it
# by hand (`nats kv del GATEWAY_BUDGET`); nothing depends on it either way.
#
# The --replicas value MUST match the deployment profile:
#   scale-1 profile -> 1   (default; no RAFT quorum, HA waived)
#   HA profile      -> 3   (or 5; quorum-replicated)
#
# Requires: nats CLI 0.3.0+ (--allow-counter) against NATS Server 2.12.0+.
#
# Connection:
#   NATS_URL                server URL (tls://… in a cluster; nats://… in compose)
#   NATS_TLS_CA_FILE        } client certificate material. All three or none:
#   NATS_TLS_CERT_FILE      } the certificate's URI SAN is the bootstrap identity
#   NATS_TLS_KEY_FILE       } (spiffe://elitea.internal/nats/elitea-nats-bootstrap)
#   NATS_INBOX_PREFIX       reply-subject prefix; the permission table lets the
#                           bootstrap identity subscribe to _INBOX_elitea-nats-bootstrap.> only
#   NATS_ARGS               extra args appended to every nats invocation
#
# Tuning:
#   NATS_REPLICAS               stream/KV replicas (default 1)
#   NATS_ALERT_COOLDOWN         GATEWAY_ALERT_COOLDOWN TTL (default 4h)
#   NATS_BUDGET_DUPE_WINDOW     GATEWAY_BUDGET duplicate window (default 12m;
#                               the gateway refuses less than its 12m recovery
#                               replay window)
#   NATS_RATELIMIT_MAX_AGE      GATEWAY_RATELIMIT MaxAge (default 5m)
#   NATS_DELTAS_DUPE_WINDOW     GATEWAY_BUDGET_DELTAS duplicate window (default 12m)
#   NATS_DELTAS_MAX_AGE         MaxAge  (default 72h)
#   NATS_DELTAS_MAX_BYTES       MaxBytes (default 1 GiB)
#   NATS_DELTAS_MAX_MSGS        MaxMsgs (default 5000000)
#   NATS_PRESENCE_TTL           ELITEA_CANVAS_PRESENCE TTL (default 2m — must
#                               be at least elitea-main's 120s roster TTL)
set -eu

REPLICAS="${NATS_REPLICAS:-1}"
ALERT_COOLDOWN="${NATS_ALERT_COOLDOWN:-4h}"
BUDGET_DUPE_WINDOW="${NATS_BUDGET_DUPE_WINDOW:-12m}"
RATELIMIT_MAX_AGE="${NATS_RATELIMIT_MAX_AGE:-5m}"
DELTAS_DUPE_WINDOW="${NATS_DELTAS_DUPE_WINDOW:-12m}"
DELTAS_MAX_AGE="${NATS_DELTAS_MAX_AGE:-72h}"
DELTAS_MAX_BYTES="${NATS_DELTAS_MAX_BYTES:-1073741824}"   # 1 GiB
DELTAS_MAX_MSGS="${NATS_DELTAS_MAX_MSGS:-5000000}"
PRESENCE_TTL="${NATS_PRESENCE_TTL:-2m}"

log() { echo "[nats-bootstrap] $*"; }

# ── Connection arguments ────────────────────────────────────────────────────
CONN=""
if [ -n "${NATS_URL:-}" ]; then
  CONN="--server ${NATS_URL}"
fi
TLS_SET=0
for v in "${NATS_TLS_CA_FILE:-}" "${NATS_TLS_CERT_FILE:-}" "${NATS_TLS_KEY_FILE:-}"; do
  if [ -n "$v" ]; then TLS_SET=$((TLS_SET + 1)); fi
done
case "$TLS_SET" in
  0)
    case "${NATS_URL:-}" in
      tls://*)
        log "FAILED: NATS_URL is tls:// and no client certificate is set (NATS_TLS_CA_FILE, NATS_TLS_CERT_FILE, NATS_TLS_KEY_FILE)."
        exit 1 ;;
    esac
    log "WARN: connecting without TLS and without a client identity (nats_auth=none tls=false)."
    log "      That is the compose posture only; a cluster's NATS refuses this connection."
    ;;
  3)
    CONN="${CONN} --tlsca ${NATS_TLS_CA_FILE} --tlscert ${NATS_TLS_CERT_FILE} --tlskey ${NATS_TLS_KEY_FILE}"
    log "connecting with a client certificate (nats_auth=mtls tls=true)"
    ;;
  *)
    log "FAILED: NATS TLS is half-configured: set all of NATS_TLS_CA_FILE, NATS_TLS_CERT_FILE and NATS_TLS_KEY_FILE, or none."
    exit 1 ;;
esac
if [ -n "${NATS_INBOX_PREFIX:-}" ]; then
  CONN="${CONN} --inbox-prefix ${NATS_INBOX_PREFIX}"
fi
# shellcheck disable=SC2086  # CONN and NATS_ARGS are intentionally word-split
NATS="nats ${CONN} ${NATS_ARGS:-}"

# ── Helpers ─────────────────────────────────────────────────────────────────
# ensure_stream NAME STORAGE FLAGS...: add with FLAGS when absent, edit to
# FLAGS when present. --storage is creation-only (the server cannot change it).
ensure_stream() {
  name="$1"; storage="$2"; shift 2
  if $NATS stream info "$name" >/dev/null 2>&1; then
    log "stream ${name} exists — reconciling its configuration"
    $NATS stream edit "$name" "$@" --force >/dev/null
  else
    log "creating stream ${name}"
    $NATS stream add "$name" --storage "$storage" "$@" --defaults >/dev/null
  fi
}

# ensure_kv BUCKET FLAGS...: same contract for a KV bucket.
ensure_kv() {
  bucket="$1"; shift
  if $NATS kv info "$bucket" >/dev/null 2>&1; then
    log "KV ${bucket} exists — reconciling its configuration"
    $NATS kv edit "$bucket" "$@" >/dev/null
  else
    log "creating KV ${bucket}"
    $NATS kv add "$bucket" --storage file "$@" >/dev/null
  fi
}

# ── GATEWAY_BUDGET: the budget counter stream ───────────────────────────────
# AllowMsgCounter makes every publish carrying Nats-Incr an atomic add whose
# running total comes back in the PubAck. Only the total matters, so one
# message per subject. The duplicate window lets the gateway's recovery replay
# reuse a Nats-Msg-Id across retries without double counting (design §8.5).
ensure_stream GATEWAY_BUDGET file \
  --subjects "gateway.budget.counter.>" \
  --replicas "${REPLICAS}" \
  --retention limits \
  --discard old \
  --allow-counter \
  --max-msgs-per-subject 1 \
  --max-msgs=-1 \
  --max-bytes=-1 \
  --max-age 0s \
  --max-msg-size=-1 \
  --dupe-window "${BUDGET_DUPE_WINDOW}" \
  --no-allow-rollup \
  --no-deny-delete \
  --no-deny-purge

# ── GATEWAY_RATELIMIT: the rate-limit counter stream ────────────────────────
# A separate stream from GATEWAY_BUDGET because a window's counter is dead the
# moment its minute ends; MaxAge bounds the subject space instead.
ensure_stream GATEWAY_RATELIMIT file \
  --subjects "gateway.ratelimit.counter.>" \
  --replicas "${REPLICAS}" \
  --retention limits \
  --discard old \
  --allow-counter \
  --max-msgs-per-subject 1 \
  --max-msgs=-1 \
  --max-bytes=-1 \
  --max-age "${RATELIMIT_MAX_AGE}" \
  --max-msg-size=-1 \
  --no-allow-rollup \
  --no-deny-delete \
  --no-deny-purge

# ── GATEWAY_BUDGET_DELTAS: the write-behind stream ──────────────────────────
# Publish-side dedup via Nats-Msg-Id=event_id within the window (design §8.6).
# The retention caps bound growth while Postgres write-behind is stalled.
ensure_stream GATEWAY_BUDGET_DELTAS file \
  --subjects "gateway.budget.delta" \
  --replicas "${REPLICAS}" \
  --retention limits \
  --discard old \
  --max-age "${DELTAS_MAX_AGE}" \
  --max-bytes "${DELTAS_MAX_BYTES}" \
  --max-msgs "${DELTAS_MAX_MSGS}" \
  --max-msgs-per-subject=-1 \
  --max-msg-size=-1 \
  --dupe-window "${DELTAS_DUPE_WINDOW}" \
  --no-allow-rollup \
  --no-deny-delete \
  --no-deny-purge

# ── GATEWAY_ALERT_COOLDOWN: soft-alert cooldown ─────────────────────────────
# kv.Create (SETNX-equivalent) plus the bucket TTL enforces one alert per key
# per cooldown (design §8.3).
ensure_kv GATEWAY_ALERT_COOLDOWN \
  --replicas "${REPLICAS}" \
  --history 1 \
  --ttl "${ALERT_COOLDOWN}" \
  --description "elitea-llm-gateway 80% soft-alert cooldown"

# ── ELITEA_CANVAS_PRESENCE: canvas presence rosters ─────────────────────────
# One entry per (roster, editor). Every heartbeat is a new message, and the
# bucket TTL (MaxAge) is the garbage collector for editors nobody refreshes.
ensure_kv ELITEA_CANVAS_PRESENCE \
  --replicas "${REPLICAS}" \
  --history 1 \
  --ttl "${PRESENCE_TTL}" \
  --description "elitea-main canvas presence rosters (one entry per editor; TTL-bounded)"

# ── The informational listings (issue #486) ─────────────────────────────────
# Read by an operator, never the status the Job reports.
log "assets:"
$NATS kv ls || true
$NATS stream ls || true

# ── The verdict (issue #486) ────────────────────────────────────────────────
# Every asset read back by name. The ensure_* calls above either created or
# edited, so a run that did nothing looks the same as one that did all of it;
# only a read-back tells them apart. The count is a floor, so a deleted block
# shows up as a shortfall rather than as silence.
EXPECTED_ASSERTIONS=5
ASSERTED=0

assert_asset() {
  kind="$1"
  name="$2"
  if ! $NATS "${kind}" info "${name}" >/dev/null 2>&1; then
    log "FAILED: ${kind} ${name} does not exist after bootstrap"
    exit 1
  fi
  ASSERTED=$((ASSERTED + 1))
  log "  ok ${kind} ${name}"
}

log "verifying the five assets"
assert_asset stream GATEWAY_BUDGET
assert_asset stream GATEWAY_RATELIMIT
assert_asset stream GATEWAY_BUDGET_DELTAS
assert_asset kv GATEWAY_ALERT_COOLDOWN
assert_asset kv ELITEA_CANVAS_PRESENCE

log "${ASSERTED} of ${EXPECTED_ASSERTIONS} expected assertions reported a result"
if [ "${ASSERTED}" -ne "${EXPECTED_ASSERTIONS}" ]; then
  log "FAILED: ${ASSERTED} assertion(s) reported a result, and ${EXPECTED_ASSERTIONS} were expected."
  log "  An assertion that reports nothing is not a passed assertion. If you"
  log "  added or removed an asset, move EXPECTED_ASSERTIONS with it."
  exit 1
fi

log "done"
