#!/usr/bin/env bash
#
# The worker's KEDA autoscaling renders correctly, and refuses when it cannot
# work.
#
# WHAT THIS IS GUARDING AGAINST. This chart has already shipped an autoscaler
# that could not scale: the gateway's HPA named a metric the gateway never
# published, so enabling it produced ScalingActive=False and pinned the
# deployment at minReplicas with nothing anywhere saying so. Every check below
# is a way that could happen again.
#
# The sharpest is the second: a scaler pointed at a queue nobody serves reports
# ZERO lag, and zero lag scales the fleet DOWN. The failure looks like a
# healthy, quiet deployment while the backlog grows.
set -euo pipefail

CHART="deploy/helm/elitea"

BASE=(-f "$CHART/values-standalone.yaml"
      --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
      --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
      --set worker.enabled=true)
KEDA=(--api-versions keda.sh/v1alpha1)

failures=0
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }
pass() { printf '  ok: %s\n' "$1"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# ------------------------------------------------------- the capability guard
#
# `helm template` reports a fixed default API set, so the guard fires unless
# --api-versions supplies keda.sh/v1alpha1. That makes this checkable here in
# exactly the way it is checkable at install time.
if helm template ac "$CHART" "${BASE[@]}" --set worker.autoscaling.enabled=true >/dev/null 2>&1; then
  fail "autoscaling rendered without keda.sh/v1alpha1 available; on a cluster with no KEDA this creates an object no controller ever reads"
else
  pass "autoscaling is refused when the cluster does not serve keda.sh/v1alpha1"
fi

if helm template ac "$CHART" "${BASE[@]}" "${KEDA[@]}" --set worker.autoscaling.enabled=true >/dev/null 2>&1; then
  pass "autoscaling renders when KEDA is available"
else
  fail "autoscaling was refused even with keda.sh/v1alpha1 available; the guard rejects a valid deployment"
fi

# ---------------------------------------------------------------- the trigger
helm template ac "$CHART" "${BASE[@]}" "${KEDA[@]}" \
  --set worker.autoscaling.enabled=true > "$work/on.yaml"

# The scaler must watch the SAME stream and group the worker consumes. Read
# both out of the manifest and compare, rather than asserting a literal here:
# a literal in this file is a third place the names are written, and the point
# of the check is that there are not several.
worker_stream=$(grep -o '"nats_stream":"[^"]*"' "$work/on.yaml" | head -1 | cut -d'"' -f4)
worker_consumer=$(grep -o '"nats_consumer":"[^"]*"' "$work/on.yaml" | head -1 | cut -d'"' -f4)
scaler_stream=$(awk '/type: nats-jetstream/{f=1} f && /^ *stream:/{gsub(/.*stream: "|"/,""); print; exit}' "$work/on.yaml")
scaler_consumer=$(awk '/type: nats-jetstream/{f=1} f && /^ *consumer:/{gsub(/.*consumer: "|"/,""); print; exit}' "$work/on.yaml")

if [ -n "$worker_stream" ] && [ "$worker_stream" = "$scaler_stream" ]; then
  pass "the scaler watches the stream the worker pulls from"
else
  fail "the scaler watches stream '$scaler_stream' but the worker pulls from '$worker_stream'; a scaler on the wrong stream reports zero lag, which scales the fleet DOWN"
fi
if [ -n "$worker_consumer" ] && [ "$worker_consumer" = "$scaler_consumer" ]; then
  pass "the scaler watches the durable consumer the worker binds"
else
  fail "the scaler watches consumer '$scaler_consumer' but the worker binds '$worker_consumer'"
fi

# The command bus lives in the NATS chart's RUNTIME account; the scaler's
# /jsz?acc= query names it, and any other account reads as zero lag.
scaler_account=$(awk '/type: nats-jetstream/{f=1} f && /^ *account:/{gsub(/.*account: "|"/,""); print; exit}' "$work/on.yaml")
if [ "$scaler_account" = "RUNTIME" ]; then
  pass "the scaler reads the RUNTIME account's /jsz"
else
  fail "the scaler reads account '$scaler_account', not RUNTIME (deploy/helm/nats accounts); it would see zero lag"
fi

# Consumer LAG (num_pending), not a pending-entries count.
if grep -q 'lagThreshold:' "$work/on.yaml" && ! grep -q 'pendingEntriesCount:' "$work/on.yaml"; then
  pass "the trigger measures consumer lag"
else
  fail "the trigger does not measure JetStream consumer lag"
fi
# KEDA's metric is num_pending + num_ack_pending (v2.10..v2.21 getMaxMsgLag),
# so the default threshold is one replica's in-flight share.
want_lag=$(grep -o '"delivery_max_concurrency":[0-9]*' "$work/on.yaml" | head -1 | cut -d: -f2)
got_lag=$(awk '/type: nats-jetstream/{f=1} f && /^ *lagThreshold:/{gsub(/.*lagThreshold: "|"/,""); print; exit}' "$work/on.yaml")
if [ -n "$want_lag" ] && [ "$got_lag" = "$want_lag" ]; then
  pass "the default lagThreshold is one replica's delivery_max_concurrency ($got_lag)"
else
  fail "lagThreshold is '$got_lag', want delivery_max_concurrency '$want_lag' (KEDA counts pulled-but-unacked work too)"
fi
if helm template ac "$CHART" "${BASE[@]}" "${KEDA[@]}" --set worker.autoscaling.enabled=true \
     --set-string worker.autoscaling.lagThreshold=500 >/dev/null 2>&1; then
  fail "a lagThreshold above one replica's in-flight capacity rendered"
else
  pass "a lagThreshold above one replica's in-flight capacity is refused"
fi
# An explicit 0 is not "unset": `default` would swap it for the default
# threshold without a word, so it is refused with its own message.
for zero in "--set worker.autoscaling.lagThreshold=0" "--set-string worker.autoscaling.lagThreshold=0" "--set worker.autoscaling.lagThreshold=-1"; do
  # shellcheck disable=SC2086 # the flag and its value are two words on purpose
  if out=$(helm template ac "$CHART" "${BASE[@]}" "${KEDA[@]}" --set worker.autoscaling.enabled=true \
       $zero 2>&1 >/dev/null); then
    fail "lagThreshold below 1 ($zero) rendered"
  elif grep -q 'lagThreshold .* must be at least 1' <<<"$out"; then
    pass "lagThreshold below 1 ($zero) is refused with a clear message"
  else
    fail "lagThreshold below 1 ($zero) was refused, but not by the lower-bound guard: $out"
  fi
done
# The HA fallback builds <server_name>.<endpoint host>: only the headless
# service has those pod DNS names.
if grep -qE 'natsServerMonitoringEndpoint: "elitea-nats-headless\.[^"]+:8222"' "$work/on.yaml"; then
  pass "the scaler reads the headless NATS service (per-node /jsz on a cluster)"
else
  fail "the scaler's endpoint is not the headless NATS service; on the HA cluster KEDA cannot reach the consumer leader by name"
fi
if grep -qE 'natsServerMonitoringEndpoint: "[^"]+:8222"' "$work/on.yaml"; then
  pass "the scaler reads the NATS monitoring port"
else
  fail "the scaler names no NATS monitoring endpoint"
fi
if grep -q 'kind: TriggerAuthentication' "$work/on.yaml"; then
  fail "a TriggerAuthentication renders; the jsz endpoint takes no credential and none may be handed to the scaler"
else
  pass "no credential is handed to the scaler"
fi

# --------------------------------------------------------- replicas ownership
#
# The Deployment must NOT carry `replicas` while KEDA owns it. A value there is
# rewritten by every `helm upgrade`, so the fleet snaps back and KEDA scales it
# out again — a burst of terminations at each deploy, each waiting out an
# in-flight agent execution.
worker_replicas=$(awk '/^# Source: elitea\/templates\/worker\/deployment.yaml/{f=1} f && /^ *replicas:/{print; exit} f && /^# Source:/ && !/worker\/deployment/{exit}' "$work/on.yaml")
if [ -z "$worker_replicas" ]; then
  pass "the worker Deployment omits replicas while KEDA owns it"
else
  fail "the worker Deployment still sets '$worker_replicas' with autoscaling on; every helm upgrade would fight the autoscaler"
fi

helm template ac "$CHART" "${BASE[@]}" > "$work/off.yaml"
worker_replicas_off=$(awk '/^# Source: elitea\/templates\/worker\/deployment.yaml/{f=1} f && /^ *replicas:/{print; exit} f && /^# Source:/ && !/worker\/deployment/{exit}' "$work/off.yaml")
if [ -n "$worker_replicas_off" ]; then
  pass "the worker Deployment sets replicas when autoscaling is off"
else
  fail "the worker Deployment omits replicas even with autoscaling off; it would default to 1 with no way to raise it"
fi
if grep -q 'kind: ScaledObject' "$work/off.yaml"; then
  fail "a ScaledObject renders with autoscaling disabled"
else
  pass "no ScaledObject renders with autoscaling off"
fi

# ------------------------------------------------------------ the route guard
if helm template ac "$CHART" "${BASE[@]}" "${KEDA[@]}" \
     --set worker.autoscaling.enabled=true \
     --set worker.runtime.natsConsumer=elitea-index-worker-v1 >/dev/null 2>&1; then
  fail "a stream/durable pair outside the contract rendered; the scaler and the worker would watch a durable nobody pulls"
else
  pass "a stream/durable pair outside the contract is refused"
fi

if [ "$failures" -ne 0 ]; then
  printf '\n%d check(s) failed.\n' "$failures" >&2
  exit 1
fi
printf '\nAll worker autoscaling checks passed.\n'
