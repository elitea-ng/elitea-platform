#!/usr/bin/env sh
# bootstrap.sh — create and reconcile EVERY JetStream asset elitea uses (#1076).
#
# The bootstrap is the ONLY owner. The services bind to these assets and verify
# what they depend on; none of them may create or update a stream, and nobody
# may delete or purge one (deploy/helm/nats/values.yaml, the permission table).
# So a setting here is the setting the cluster runs with — before #1076 the
# gateway re-created its streams on every boot and silently overwrote the
# retention below.
#
# The server has ONE ACCOUNT PER PLANE, each with its own JetStream and its
# own bootstrap identity (spiffe://elitea.internal/nats/
# elitea-nats-bootstrap-<plane>), which may administer that account's assets
# and nothing else. The script connects once per account:
#
#   account MAIN
#     KV     ELITEA_CANVAS_PRESENCE  elitea-main canvas presence rosters
#   account GATEWAY
#     stream GATEWAY_BUDGET          budget counters (Nats-Incr, AllowMsgCounter);
#                                    one running total per subject
#                                    gateway.budget.counter.<scope>.<id>.<period>
#     stream GATEWAY_RATELIMIT       governance rate-limit counters (Nats-Incr);
#                                    MaxAge expires each minute's window
#     stream GATEWAY_BUDGET_DELTAS   write-behind deltas, subject gateway.budget.delta,
#                                    drained by elitea-scheduler's budget-writeback
#     KV     GATEWAY_ALERT_COOLDOWN  80% soft-alert cooldown (kv.Create + bucket TTL)
#     consumer GATEWAY_BUDGET_DELTAS/budget-writeback
#                                    the durable PULL consumer elitea-scheduler
#                                    drains; it only binds to it
#   account RUNTIME (the runtime command bus, docs/runtime-command-bus.md)
#     stream ELITEA_RT_V1_VALIDATE   } one WorkQueue stream per route, subjects
#     stream ELITEA_RT_V1_AGENT      } elitea.rt.v1.<route>.d.<sha256(delivery_id)>,
#     stream ELITEA_RT_V1_INDEX      } each with its durable pull consumer
#                                      elitea-{configuration,agent,index}-worker-v1
#   account WORKER (the command bus consumer; it reaches RUNTIME's durables
#                   through service imports only)
#     KV     ELITEA_RT_V1_DEADLETTER poison commands the workers recorded (7d)
#
# Create when absent, EDIT when present: re-running this script puts a drifted
# asset back to the values below. An edit the server cannot apply (storage
# type, or switching the counter flag off) fails the Job, which is the point:
# it needs an operator, not a silent skip.
#
# The --replicas value MUST match the deployment profile:
#   scale-1 profile -> 1   (default; no RAFT quorum, HA waived)
#   HA profile      -> 3   (or 5; quorum-replicated)
#
# Requires: nats CLI 0.3.0+ (--allow-counter) against NATS Server 2.12.0+.
#
# Which accounts:
#   NATS_BOOTSTRAP_ACCOUNTS  space-separated, of main, gateway, runtime, worker
#                            (default: all four, in that order)
#
# Connection (one of three postures):
#   NATS_URL                server URL (tls://… in a cluster; nats://… in compose)
#   NATS_TLS_DIR            the cluster posture: one directory per account,
#                           NATS_TLS_DIR/<account>/{ca.crt,tls.crt,tls.key},
#                           holding that account's bootstrap certificate. The
#                           inbox prefix is _INBOX_elitea-nats-bootstrap-<account>,
#                           the only one the permission table lets it read.
#   NATS_TLS_CA_FILE        } ONE identity's material, for exactly one account
#   NATS_TLS_CERT_FILE      } in NATS_BOOTSTRAP_ACCOUNTS (the tests run the
#   NATS_TLS_KEY_FILE       } script as a service identity to prove it is refused)
#   NATS_INBOX_PREFIX       overrides the inbox prefix in that posture
#   (none of the above)     plaintext, no identity: compose. The compose
#                           server has no accounts, so every plane's assets
#                           land in its one global account.
#   NATS_ARGS               extra args appended to every nats invocation
#   NATS_CONNECT_WAIT       seconds to wait for the server to accept an
#                           identity before its first command (default 120):
#                           a server still starting, or a bootstrap
#                           certificate cert-manager has not issued yet.
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
#   NATS_WRITEBACK_ACK_WAIT     budget-writeback AckWait (default 30s; must
#                               exceed the scheduler's worst-case batch apply)
#   NATS_WRITEBACK_MAX_DELIVER  budget-writeback MaxDeliver (default 10)
#   NATS_PRESENCE_TTL           ELITEA_CANVAS_PRESENCE TTL (default 2m — must
#                               be at least elitea-main's 120s roster TTL)
#   NATS_RT_VALIDATE_MAX_MSGS   } a command stream's capacity (MaxMsgs, 1..1024;
#   NATS_RT_AGENT_MAX_MSGS      } default 1024). A full stream refuses the
#   NATS_RT_INDEX_MAX_MSGS      } publish and the command waits in the outbox
#   NATS_RT_VALIDATE_MAX_AGE    } a command stream's MaxAge: the route's longest
#   NATS_RT_AGENT_MAX_AGE       } execution deadline plus margin (defaults 3h,
#   NATS_RT_INDEX_MAX_AGE       } 26h, 26h); elitea-main refuses less at boot
#   NATS_RT_DEADLETTER_TTL      ELITEA_RT_V1_DEADLETTER TTL (default 168h)
set -eu

ACCOUNTS="${NATS_BOOTSTRAP_ACCOUNTS:-main gateway runtime worker}"
REPLICAS="${NATS_REPLICAS:-1}"
ALERT_COOLDOWN="${NATS_ALERT_COOLDOWN:-4h}"
BUDGET_DUPE_WINDOW="${NATS_BUDGET_DUPE_WINDOW:-12m}"
RATELIMIT_MAX_AGE="${NATS_RATELIMIT_MAX_AGE:-5m}"
DELTAS_DUPE_WINDOW="${NATS_DELTAS_DUPE_WINDOW:-12m}"
DELTAS_MAX_AGE="${NATS_DELTAS_MAX_AGE:-72h}"
DELTAS_MAX_BYTES="${NATS_DELTAS_MAX_BYTES:-1073741824}"   # 1 GiB
DELTAS_MAX_MSGS="${NATS_DELTAS_MAX_MSGS:-5000000}"
WRITEBACK_ACK_WAIT="${NATS_WRITEBACK_ACK_WAIT:-30s}"
WRITEBACK_MAX_DELIVER="${NATS_WRITEBACK_MAX_DELIVER:-10}"
PRESENCE_TTL="${NATS_PRESENCE_TTL:-2m}"
RT_VALIDATE_MAX_MSGS="${NATS_RT_VALIDATE_MAX_MSGS:-1024}"
RT_AGENT_MAX_MSGS="${NATS_RT_AGENT_MAX_MSGS:-1024}"
RT_INDEX_MAX_MSGS="${NATS_RT_INDEX_MAX_MSGS:-1024}"
RT_VALIDATE_MAX_AGE="${NATS_RT_VALIDATE_MAX_AGE:-3h}"
RT_AGENT_MAX_AGE="${NATS_RT_AGENT_MAX_AGE:-26h}"
RT_INDEX_MAX_AGE="${NATS_RT_INDEX_MAX_AGE:-26h}"
RT_DEADLETTER_TTL="${NATS_RT_DEADLETTER_TTL:-168h}"
CONNECT_WAIT="${NATS_CONNECT_WAIT:-120}"

log() { echo "[nats-bootstrap] $*"; }

# ── Connection posture ──────────────────────────────────────────────────────
SERVER=""
if [ -n "${NATS_URL:-}" ]; then
  SERVER="--server ${NATS_URL}"
fi
TLS_SET=0
for v in "${NATS_TLS_CA_FILE:-}" "${NATS_TLS_CERT_FILE:-}" "${NATS_TLS_KEY_FILE:-}"; do
  if [ -n "$v" ]; then TLS_SET=$((TLS_SET + 1)); fi
done
NUM_ACCOUNTS=0
for a in $ACCOUNTS; do
  case "$a" in
    main|gateway|runtime|worker) NUM_ACCOUNTS=$((NUM_ACCOUNTS + 1)) ;;
    *) log "FAILED: unknown account '${a}' in NATS_BOOTSTRAP_ACCOUNTS (main, gateway, runtime, worker)."; exit 1 ;;
  esac
done
if [ "$NUM_ACCOUNTS" -eq 0 ]; then
  log "FAILED: NATS_BOOTSTRAP_ACCOUNTS names no account."
  exit 1
fi
if [ -n "${NATS_TLS_DIR:-}" ]; then
  if [ "$TLS_SET" -ne 0 ]; then
    log "FAILED: set NATS_TLS_DIR (one certificate per account) or NATS_TLS_*_FILE (one identity), not both."
    exit 1
  fi
  POSTURE=dir
  log "connecting with one client certificate per account (nats_auth=mtls tls=true)"
else
  case "$TLS_SET" in
    0)
      case "${NATS_URL:-}" in
        tls://*)
          log "FAILED: NATS_URL is tls:// and no client certificate is set (NATS_TLS_DIR, or NATS_TLS_CA_FILE, NATS_TLS_CERT_FILE and NATS_TLS_KEY_FILE)."
          exit 1 ;;
      esac
      POSTURE=plain
      log "WARN: connecting without TLS and without a client identity (nats_auth=none tls=false)."
      log "      That is the compose posture only; a cluster's NATS refuses this connection,"
      log "      and without accounts every plane's assets share one account."
      ;;
    3)
      if [ "$NUM_ACCOUNTS" -ne 1 ]; then
        log "FAILED: NATS_TLS_*_FILE is ONE identity, and an identity belongs to one account, but NATS_BOOTSTRAP_ACCOUNTS names ${NUM_ACCOUNTS}. Use NATS_TLS_DIR for several."
        exit 1
      fi
      POSTURE=files
      log "connecting with a client certificate (nats_auth=mtls tls=true)"
      ;;
    *)
      log "FAILED: NATS TLS is half-configured: set all of NATS_TLS_CA_FILE, NATS_TLS_CERT_FILE and NATS_TLS_KEY_FILE, or none."
      exit 1 ;;
  esac
fi

# use_account ACCOUNT: point $NATS at ACCOUNT's bootstrap identity.
use_account() {
  acct="$1"
  conn="${SERVER}"
  case "$POSTURE" in
    dir)
      d="${NATS_TLS_DIR%/}/${acct}"
      conn="${conn} --tlsca ${d}/ca.crt --tlscert ${d}/tls.crt --tlskey ${d}/tls.key --inbox-prefix _INBOX_elitea-nats-bootstrap-${acct}"
      ;;
    files)
      conn="${conn} --tlsca ${NATS_TLS_CA_FILE} --tlscert ${NATS_TLS_CERT_FILE} --tlskey ${NATS_TLS_KEY_FILE} --inbox-prefix ${NATS_INBOX_PREFIX:-_INBOX_elitea-nats-bootstrap-${acct}}"
      ;;
    plain)
      if [ -n "${NATS_INBOX_PREFIX:-}" ]; then
        conn="${conn} --inbox-prefix ${NATS_INBOX_PREFIX}"
      fi
      ;;
  esac
  # shellcheck disable=SC2086  # conn and NATS_ARGS are intentionally word-split
  NATS="nats ${conn} ${NATS_ARGS:-}"
}

# wait_for_server: the server accepts this identity. A server that is not up
# yet and a certificate cert-manager has not issued (or re-issued: the
# bootstrap certificates are short-lived) look the same from here, so wait a
# bounded time for either to change, then fail with the server's answer.
wait_for_server() {
  waited=0
  until $NATS rtt >/dev/null 2>&1; do
    if [ "$waited" -ge "$CONNECT_WAIT" ]; then
      log "FAILED: account ${acct}: the server did not accept this identity within ${CONNECT_WAIT}s:"
      $NATS rtt 2>&1 | sed 's/^/[nats-bootstrap]   /' || true
      exit 1
    fi
    if [ "$waited" -eq 0 ]; then
      log "account ${acct}: waiting up to ${CONNECT_WAIT}s for the server to accept the bootstrap identity"
    fi
    sleep 5
    waited=$((waited + 5))
  done
}

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

# ensure_pull_consumer STREAM NAME FILTER FLAGS...: a durable PULL consumer,
# added when absent and edited to FLAGS when present. The services bind to
# it and cannot create, redefine or delete it: a consumer-create grant would
# also let them point a push consumer's deliveries at any subject in their
# account.
ensure_pull_consumer() {
  stream="$1"; name="$2"; filter="$3"; shift 3
  if $NATS consumer info "$stream" "$name" >/dev/null 2>&1; then
    log "consumer ${stream}/${name} exists — reconciling its configuration"
    $NATS consumer edit "$stream" "$name" "$@" --force >/dev/null
  else
    log "creating consumer ${stream}/${name}"
    $NATS consumer add "$stream" "$name" --pull --filter "$filter" --ack explicit \
      --deliver all --replay instant "$@" --defaults >/dev/null
  fi
}

# The verdict (issue #486). The ensure_* calls either created or edited, so a
# run that did nothing looks the same as one that did all of it; only a
# read-back tells them apart. Each account adds what it expects, so a deleted
# block shows up as a shortfall rather than as silence.
EXPECTED_ASSERTIONS=0
ASSERTED=0
expect() { EXPECTED_ASSERTIONS=$((EXPECTED_ASSERTIONS + $1)); }
assert_asset() {
  kind="$1"
  shift
  if ! $NATS "${kind}" info "$@" >/dev/null 2>&1; then
    log "FAILED: ${kind} $* does not exist after bootstrap"
    exit 1
  fi
  ASSERTED=$((ASSERTED + 1))
  log "  ok ${kind} $* (account ${acct})"
}

# ── account MAIN ────────────────────────────────────────────────────────────
bootstrap_main() {
  expect 1
  # ELITEA_CANVAS_PRESENCE: canvas presence rosters. One entry per (roster,
  # editor). Every heartbeat is a new message, and the bucket TTL (MaxAge) is
  # the garbage collector for editors nobody refreshes.
  ensure_kv ELITEA_CANVAS_PRESENCE \
    --replicas "${REPLICAS}" \
    --history 1 \
    --ttl "${PRESENCE_TTL}" \
    --description "elitea-main canvas presence rosters (one entry per editor; TTL-bounded)"

  assert_asset kv ELITEA_CANVAS_PRESENCE
}

# ── account GATEWAY ─────────────────────────────────────────────────────────
bootstrap_gateway() {
  expect 5
  # GATEWAY_BUDGET: the budget counter stream. AllowMsgCounter makes every
  # publish carrying Nats-Incr an atomic add whose running total comes back in
  # the PubAck. allow_direct is what the gateway reads the total with (a direct
  # get); its NATS permissions grant that and not the stream API's MSG.GET,
  # and it refuses to bind a stream without it. Only the total matters, so one
  # message per subject. The duplicate window lets the gateway's recovery
  # replay reuse a Nats-Msg-Id across retries without double counting
  # (design §8.5).
  ensure_stream GATEWAY_BUDGET file \
    --subjects "gateway.budget.counter.>" \
    --replicas "${REPLICAS}" \
    --retention limits \
    --discard old \
    --allow-counter \
    --allow-direct \
    --max-msgs-per-subject 1 \
    --max-msgs=-1 \
    --max-bytes=-1 \
    --max-age 0s \
    --max-msg-size=-1 \
    --dupe-window "${BUDGET_DUPE_WINDOW}" \
    --no-allow-rollup \
    --no-deny-delete \
    --no-deny-purge

  # GATEWAY_RATELIMIT: the rate-limit counter stream. A separate stream from
  # GATEWAY_BUDGET because a window's counter is dead the moment its minute
  # ends; MaxAge bounds the subject space instead.
  ensure_stream GATEWAY_RATELIMIT file \
    --subjects "gateway.ratelimit.counter.>" \
    --replicas "${REPLICAS}" \
    --retention limits \
    --discard old \
    --allow-counter \
    --allow-direct \
    --max-msgs-per-subject 1 \
    --max-msgs=-1 \
    --max-bytes=-1 \
    --max-age "${RATELIMIT_MAX_AGE}" \
    --max-msg-size=-1 \
    --no-allow-rollup \
    --no-deny-delete \
    --no-deny-purge

  # GATEWAY_BUDGET_DELTAS: the write-behind stream. Publish-side dedup via
  # Nats-Msg-Id=event_id within the window (design §8.6). The retention caps
  # bound growth while Postgres write-behind is stalled.
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

  # GATEWAY_ALERT_COOLDOWN: soft-alert cooldown. kv.Create (SETNX-equivalent)
  # plus the bucket TTL enforces one alert per key per cooldown (design §8.3).
  ensure_kv GATEWAY_ALERT_COOLDOWN \
    --replicas "${REPLICAS}" \
    --history 1 \
    --ttl "${ALERT_COOLDOWN}" \
    --description "elitea-llm-gateway 80% soft-alert cooldown"

  # budget-writeback: the durable pull consumer elitea-scheduler drains
  # GATEWAY_BUDGET_DELTAS with (design §8.6). Created here, not by the
  # scheduler, whose identity may only read its info, pull and ack.
  ensure_pull_consumer GATEWAY_BUDGET_DELTAS budget-writeback "gateway.budget.delta" \
    --wait "${WRITEBACK_ACK_WAIT}" \
    --max-deliver "${WRITEBACK_MAX_DELIVER}" \
    --description "elitea-scheduler budget write-back (design §8.6)"

  assert_asset stream GATEWAY_BUDGET
  assert_asset stream GATEWAY_RATELIMIT
  assert_asset stream GATEWAY_BUDGET_DELTAS
  assert_asset kv GATEWAY_ALERT_COOLDOWN
  assert_asset consumer GATEWAY_BUDGET_DELTAS budget-writeback
}

# ── account RUNTIME ─────────────────────────────────────────────────────────
# The runtime command bus (docs/runtime-command-bus.md). One WorkQueue stream
# per route. The settings ARE the contract:
#   WorkQueue          an ack removes the message, so capacity is released on
#                      settlement; the server refuses an overlapping consumer
#   discard new        a full stream refuses the publish (backpressure: the
#                      command stays in PostgreSQL's outbox), never evicts
#   max-msgs-per-subject 1 + discard-per-subject
#                      one live message per delivery subject; a PostgreSQL
#                      re-offer of a live delivery is refused atomically
#   max-bytes 64 MiB, max-msg-size 65536 (payload + headers,
#                      max_transport_message_bytes)
#   max-age            the route's deadline + margin: a safety net only
#   dupe-window 2m     Nats-Msg-Id de-duplication for an ambiguous retry
#   allow-direct       the producer reads a live copy back for its compare
#   deny-delete/purge  only an ack removes a command
rt_stream() {
  name="$1"; route="$2"; max_msgs="$3"; max_age="$4"
  case "$max_msgs" in
    ''|*[!0-9]*) log "FAILED: ${name} max msgs '${max_msgs}' is not a positive integer"; exit 1 ;;
  esac
  if [ "$max_msgs" -lt 1 ] || [ "$max_msgs" -gt 1024 ]; then
    log "FAILED: ${name} max msgs ${max_msgs} is outside 1..1024"
    exit 1
  fi
  ensure_stream "$name" file \
    --subjects "elitea.rt.v1.${route}.d.*" \
    --replicas "${REPLICAS}" \
    --retention work \
    --discard new \
    --discard-per-subject \
    --max-msgs-per-subject 1 \
    --max-msgs "${max_msgs}" \
    --max-bytes 67108864 \
    --max-msg-size 65536 \
    --max-age "${max_age}" \
    --dupe-window 2m \
    --allow-direct \
    --deny-delete \
    --deny-purge \
    --no-allow-rollup
}

# The route's ONE durable pull consumer, shared by every worker replica.
# Workers bind and never create it: a worker that could create its consumer
# could, after losing one, re-create it in a way that skips commands (and a
# consumer-create grant could point a push consumer anywhere in RUNTIME).
#   ack explicit, wait 60s   redelivery after twice the 30s claim lease; the
#                            workers send +WPI every 5s for what they own
#   max-deliver -1           PostgreSQL bounds retries, not the broker
#   max-pending 1024         = the largest stream; never the binding limit
#   max-waiting 512, max-pull-batch 64, max-pull-expire 30s
# MaxWaiting is fixed at creation (the server refuses to edit it), so the
# reconcile edits the rest; ensure_pull_consumer passes the same flags to
# both, which is why the command bus has its own.
ensure_rt_consumer() {
  stream="$1"; durable="$2"; route="$3"
  if $NATS consumer info "$stream" "$durable" >/dev/null 2>&1; then
    log "consumer ${stream}/${durable} exists — reconciling its configuration"
    $NATS consumer edit "$stream" "$durable" \
      --wait 60s \
      --max-deliver=-1 \
      --max-pending 1024 \
      --max-pull-batch 64 \
      --max-pull-expire 30s \
      --force >/dev/null
  else
    log "creating consumer ${stream}/${durable}"
    $NATS consumer add "$stream" "$durable" \
      --pull \
      --filter "elitea.rt.v1.${route}.d.*" \
      --deliver all \
      --ack explicit \
      --replay instant \
      --wait 60s \
      --max-deliver=-1 \
      --max-pending 1024 \
      --max-waiting 512 \
      --max-pull-batch 64 \
      --max-pull-expire 30s \
      --no-headers-only \
      --backoff none \
      --defaults >/dev/null
  fi
}

bootstrap_runtime() {
  expect 6
  rt_stream ELITEA_RT_V1_VALIDATE validate "${RT_VALIDATE_MAX_MSGS}" "${RT_VALIDATE_MAX_AGE}"
  rt_stream ELITEA_RT_V1_AGENT agent "${RT_AGENT_MAX_MSGS}" "${RT_AGENT_MAX_AGE}"
  rt_stream ELITEA_RT_V1_INDEX index "${RT_INDEX_MAX_MSGS}" "${RT_INDEX_MAX_AGE}"
  ensure_rt_consumer ELITEA_RT_V1_VALIDATE elitea-configuration-worker-v1 validate
  ensure_rt_consumer ELITEA_RT_V1_AGENT elitea-agent-worker-v1 agent
  ensure_rt_consumer ELITEA_RT_V1_INDEX elitea-index-worker-v1 index

  assert_asset stream ELITEA_RT_V1_VALIDATE
  assert_asset stream ELITEA_RT_V1_AGENT
  assert_asset stream ELITEA_RT_V1_INDEX
  assert_asset consumer ELITEA_RT_V1_VALIDATE elitea-configuration-worker-v1
  assert_asset consumer ELITEA_RT_V1_AGENT elitea-agent-worker-v1
  assert_asset consumer ELITEA_RT_V1_INDEX elitea-index-worker-v1
}

# ── account WORKER ──────────────────────────────────────────────────────────
bootstrap_worker() {
  expect 1
  # ELITEA_RT_V1_DEADLETTER: poison commands (owner decision Q3). One record
  # per poison delivery, key <route>.<sha256(delivery_id)>, written by the
  # worker that could not verify or decode it. The record names where to
  # look, never what the command said. The alert is the bucket being
  # non-empty.
  #
  # It lives in WORKER, the worker's own account, and not in RUNTIME with the
  # command streams: the server answers a JetStream API request on a reply
  # subject it does not check against the requester's permissions, so a
  # bucket in RUNTIME could be written by elitea-main-runtime naming
  # $KV.ELITEA_RT_V1_DEADLETTER.<key> as the reply of a stream-info request
  # (a forged dead letter), and the worker could not be kept out of the
  # command streams the same way. WORKER holds this bucket and nothing else
  # (its account JetStream allows one stream).
  ensure_kv ELITEA_RT_V1_DEADLETTER \
    --replicas "${REPLICAS}" \
    --history 1 \
    --ttl "${RT_DEADLETTER_TTL}" \
    --max-value-size 4096 \
    --max-bucket-size 67108864 \
    --description "runtime command bus dead letters (poison commands; TTL-bounded)"

  assert_asset kv ELITEA_RT_V1_DEADLETTER
}

# ── Run ─────────────────────────────────────────────────────────────────────
for acct in $ACCOUNTS; do
  use_account "$acct"
  wait_for_server
  log "account ${acct}: reconciling"
  "bootstrap_${acct}"
  # The informational listing (issue #486). Read by an operator, never the
  # status the Job reports.
  log "account ${acct}: assets"
  $NATS stream ls 2>/dev/null || true
done

log "${ASSERTED} of ${EXPECTED_ASSERTIONS} expected assertions reported a result"
if [ "${ASSERTED}" -ne "${EXPECTED_ASSERTIONS}" ]; then
  log "FAILED: ${ASSERTED} assertion(s) reported a result, and ${EXPECTED_ASSERTIONS} were expected."
  log "  An assertion that reports nothing is not a passed assertion. If you"
  log "  added or removed an asset, move its account's expect() with it."
  exit 1
fi

log "done"
