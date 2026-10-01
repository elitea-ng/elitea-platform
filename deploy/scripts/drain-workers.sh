#!/usr/bin/env bash
# drain-workers.sh — stop agent worker consumption before a main replica
# recreation, and restore it after. Issue #968, work items 2 and 3.
#
# WHY THIS ORDER IS SAFE
#
# The worker has no consumption-pause. Its only drain is SIGTERM. On
# termination the worker cancels intake and finishes in-flight work under
# its shutdown deadline (shutdown_timeout_millis, default 30000). It
# creates no shutdown ACK. Unfinished stream entries stay PENDING and
# remain reclaimable.
# Stopping the worker fleet around a main replica recreation therefore loses
# no durable work: PostgreSQL stays the source of truth for execution state.
#
# DRAIN ORDER
#
#   1. deploy/scripts/drain-workers.sh drain     stop worker consumption
#   2. recreate or scale the elitea-main replica
#   3. deploy/scripts/drain-workers.sh restore   restore worker capacity
#
# The same kubectl steps, spelled out for both the scale-down and the
# scale-up, live in deploy/README.md under
# "Scaling order with live workers (#968)".
#
# USAGE
#
#   deploy/scripts/drain-workers.sh drain   [options]
#   deploy/scripts/drain-workers.sh restore [options]
#   deploy/scripts/drain-workers.sh status  [options]
#
# OPTIONS
#
#   --namespace NAME    namespace, default "elitea" or $DRAIN_NAMESPACE
#   --deployment NAME   worker Deployment, default "elitea-worker" or
#                       $DRAIN_WORKER_DEPLOYMENT
#   --replicas N        restore target count, default 1 or
#                       $DRAIN_RESTORE_REPLICAS
#   --timeout SECONDS   safety wait budget, default 300 or
#                       $DRAIN_TIMEOUT_SECONDS
#   --dry-run           print each step, change nothing
#
# KEDA NOTE
#
# When the worker ScaledObject exists, KEDA owns the replica count. A bare
# `kubectl scale` loses to the next KEDA poll. This script pauses the
# ScaledObject instead. The pause is `autoscaling.keda.sh/paused=true`,
# alone or with `autoscaling.keda.sh/paused-replicas=0`. The script sets
# both annotations on drain and removes both on restore.

set -u

NAMESPACE="${DRAIN_NAMESPACE:-elitea}"
DEPLOYMENT="${DRAIN_WORKER_DEPLOYMENT:-elitea-worker}"
RESTORE_REPLICAS="${DRAIN_RESTORE_REPLICAS:-1}"
TIMEOUT_SECONDS="${DRAIN_TIMEOUT_SECONDS:-300}"
DRY_RUN=0

# Pod label from the worker Deployment's selector. The platform-edge pods
# share the name/instance labels but not this one, so this selects the
# worker pods exactly.
POD_LABEL="app.kubernetes.io/component=worker"

abort() { echo "ERROR: $1" >&2; exit 1; }
step()  { echo "==> $1"; }

usage() {
  cat <<'EOF'
usage: drain-workers.sh <drain|restore|status> [options]

options:
  --namespace NAME    namespace (default: elitea)
  --deployment NAME   worker Deployment (default: elitea-worker)
  --replicas N        restore target count (default: 1)
  --timeout SECONDS   safety wait budget (default: 300)
  --dry-run           print each step, change nothing
EOF
}

[ $# -ge 1 ] || { usage >&2; exit 2; }
COMMAND="$1"
shift
case "$COMMAND" in
  drain|restore|status) ;;
  -h|--help|help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace)  [ $# -ge 2 ] || { echo "ERROR: $1 needs a value" >&2; exit 2; }; NAMESPACE="$2"; shift 2 ;;
    --deployment) [ $# -ge 2 ] || { echo "ERROR: $1 needs a value" >&2; exit 2; }; DEPLOYMENT="$2"; shift 2 ;;
    --replicas)   [ $# -ge 2 ] || { echo "ERROR: $1 needs a value" >&2; exit 2; }; RESTORE_REPLICAS="$2"; shift 2 ;;
    --timeout)    [ $# -ge 2 ] || { echo "ERROR: $1 needs a value" >&2; exit 2; }; TIMEOUT_SECONDS="$2"; shift 2 ;;
    --dry-run)    DRY_RUN=1; shift ;;
    *) usage >&2; exit 2 ;;
  esac
done

case "$RESTORE_REPLICAS" in
  ''|*[!0-9]*) abort "--replicas must be a non-negative integer, got '$RESTORE_REPLICAS'" ;;
esac
case "$TIMEOUT_SECONDS" in
  ''|*[!0-9]*) abort "--timeout must be a positive integer, got '$TIMEOUT_SECONDS'" ;;
esac
[ "$TIMEOUT_SECONDS" -ge 1 ] || abort "--timeout must be at least 1"

command -v kubectl >/dev/null 2>&1 || abort "kubectl is not on PATH"

# Run a step. Print the step. Execute the step. Fail loudly on error.
# Every mutation goes through here so no step fails silently.
run() {
  local label="$1"
  shift
  step "$label"
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "    dry-run: $*"
    return 0
  fi
  if ! "$@"; then
    abort "$label failed"
  fi
}

deployment_exists() {
  kubectl get "deployment/$DEPLOYMENT" -n "$NAMESPACE" >/dev/null 2>&1
}

deployment_replicas() {
  # Empty when the Deployment spec omits replicas (KEDA-owned fleet).
  kubectl get "deployment/$DEPLOYMENT" -n "$NAMESPACE" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null
}

worker_pod_names() {
  kubectl get pods -n "$NAMESPACE" -l "$POD_LABEL" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

worker_pod_count() {
  local names
  if ! names="$(worker_pod_names)"; then
    echo "ERROR: kubectl get pods failed in namespace $NAMESPACE" >&2
    return 1
  fi
  if [ -z "$names" ]; then
    echo 0
    return 0
  fi
  set -- $names
  echo $#
}

# READY column of a single-container pod reads "1/1" when Ready, "0/1" while
# not. Count pods where the two sides of the fraction agree.
worker_ready_count() {
  local rows
  if ! rows="$(kubectl get pods -n "$NAMESPACE" -l "$POD_LABEL" --no-headers 2>/dev/null)"; then
    echo "ERROR: kubectl get pods failed in namespace $NAMESPACE" >&2
    return 1
  fi
  printf '%s\n' "$rows" \
    | awk '{split($2, p, "/"); if (p[1] == p[2] && p[1] != "") c++} END {print c + 0}'
}

keda_managed() {
  local out
  out="$(kubectl get "scaledobject/$DEPLOYMENT" -n "$NAMESPACE" \
    -o jsonpath='{.metadata.name}' 2>/dev/null)" || return 1
  [ -n "$out" ]
}

keda_paused() {
  local ann
  ann="$(kubectl get "scaledobject/$DEPLOYMENT" -n "$NAMESPACE" \
    -o jsonpath='{.metadata.annotations}' 2>/dev/null)" || return 1
  case "$ann" in
    *"autoscaling.keda.sh/paused-replicas"*|*'"autoscaling.keda.sh/paused":"true"'*)
      return 0 ;;
    *) return 1 ;;
  esac
}

keda_min_replicas() {
  kubectl get "scaledobject/$DEPLOYMENT" -n "$NAMESPACE" \
    -o jsonpath='{.spec.minReplicaCount}' 2>/dev/null
}

wait_pods_gone() {
  if [ "$DRY_RUN" -eq 1 ]; then
    step "Safety wait: no worker pod may remain (skipped, dry-run)"
    return 0
  fi
  step "Safety wait: no worker pod may remain (budget ${TIMEOUT_SECONDS}s)"
  local deadline now count read_ok
  deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
  while :; do
    read_ok=1
    count="$(worker_pod_count)" || read_ok=0
    if [ "$read_ok" -eq 1 ] && [ "$count" -eq 0 ]; then
      step "Safety wait passed: no worker pod remains"
      return 0
    fi
    now="$(date +%s)"
    if [ "$now" -ge "$deadline" ]; then
      if [ "$read_ok" -eq 1 ]; then
        abort "worker pod(s) still present after ${TIMEOUT_SECONDS}s: $(worker_pod_names)"
      fi
      abort "safety wait aborted after ${TIMEOUT_SECONDS}s: kubectl get pods kept failing; cannot prove that no worker pod remains"
    fi
    if [ "$read_ok" -eq 1 ]; then
      echo "    ${count} worker pod(s) still terminating; retrying in 5s"
    else
      echo "    kubectl get pods failed; retrying in 5s"
    fi
    sleep 5
  done
}

wait_pods_ready_at_least() {
  local want="$1"
  if [ "$DRY_RUN" -eq 1 ]; then
    step "Wait for at least ${want} Ready worker pod(s) (skipped, dry-run)"
    return 0
  fi
  step "Wait for at least ${want} Ready worker pod(s) (budget ${TIMEOUT_SECONDS}s)"
  local deadline now count read_ok
  deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
  while :; do
    read_ok=1
    count="$(worker_ready_count)" || read_ok=0
    if [ "$read_ok" -eq 1 ] && [ "$count" -ge "$want" ]; then
      step "Worker capacity restored: ${count} Ready pod(s)"
      return 0
    fi
    now="$(date +%s)"
    if [ "$now" -ge "$deadline" ]; then
      if [ "$read_ok" -eq 1 ]; then
        abort "only ${count} Ready worker pod(s) after ${TIMEOUT_SECONDS}s (wanted ${want})"
      fi
      abort "safety wait aborted after ${TIMEOUT_SECONDS}s: kubectl get pods kept failing; cannot prove that ${want} Ready worker pod(s) are present"
    fi
    if [ "$read_ok" -eq 1 ]; then
      echo "    ${count} Ready worker pod(s); retrying in 5s"
    else
      echo "    kubectl get pods failed; retrying in 5s"
    fi
    sleep 5
  done
}

do_drain() {
  deployment_exists || abort "deployment/$DEPLOYMENT not found in namespace $NAMESPACE"

  local keda=0
  keda_managed && keda=1

  local count replicas
  count="$(worker_pod_count)" || \
    abort "cannot read worker pods in namespace $NAMESPACE: kubectl get pods failed"
  replicas="$(deployment_replicas)"

  if [ "$keda" -eq 1 ]; then
    if [ "$count" -eq 0 ] && keda_paused; then
      step "Already drained: KEDA scaling is paused and no worker pod remains"
      echo "    Recreate the main replica."
      echo "    Run: deploy/scripts/drain-workers.sh restore --namespace $NAMESPACE"
      exit 0
    fi
    run "Pause KEDA scaling and hold the worker fleet at 0 replicas" \
      kubectl annotate "scaledobject/$DEPLOYMENT" -n "$NAMESPACE" --overwrite \
        "autoscaling.keda.sh/paused=true" "autoscaling.keda.sh/paused-replicas=0"
  else
    if [ "$count" -eq 0 ] && [ "$replicas" = "0" ]; then
      step "Already drained: spec.replicas is 0 and no worker pod remains"
      echo "    Recreate the main replica."
      echo "    Run: deploy/scripts/drain-workers.sh restore --namespace $NAMESPACE"
      exit 0
    fi
    run "Scale worker Deployment to 0 replicas" \
      kubectl scale "deployment/$DEPLOYMENT" -n "$NAMESPACE" --replicas=0
  fi

  wait_pods_gone
  step "Workers drained."
  echo "    Recreate the main replica."
  echo "    Run: deploy/scripts/drain-workers.sh restore --namespace $NAMESPACE"
}

do_restore() {
  deployment_exists || abort "deployment/$DEPLOYMENT not found in namespace $NAMESPACE"

  local keda=0
  keda_managed && keda=1

  if [ "$keda" -eq 1 ]; then
    if keda_paused; then
      run "Resume KEDA scaling (remove the pause annotations)" \
        kubectl annotate "scaledobject/$DEPLOYMENT" -n "$NAMESPACE" \
          "autoscaling.keda.sh/paused-" "autoscaling.keda.sh/paused-replicas-"
    else
      step "KEDA scaling is not paused; nothing to resume"
    fi
    local min
    min="$(keda_min_replicas)" || abort "cannot read spec.minReplicaCount of scaledobject/$DEPLOYMENT"
    [ -n "$min" ] || min=0   # KEDA's documented default when the field is omitted
    wait_pods_ready_at_least "$min"
    if [ "$min" -eq 0 ]; then
      echo "    WARNING: KEDA minReplicaCount is 0. Restore leaves the fleet at 0 pods until KEDA scales it."
    fi
  else
    local replicas
    replicas="$(deployment_replicas)"
    if [ "$replicas" = "$RESTORE_REPLICAS" ]; then
      run "Verify the worker rollout is available" \
        kubectl rollout status "deployment/$DEPLOYMENT" -n "$NAMESPACE" --timeout="${TIMEOUT_SECONDS}s"
      step "No-op: worker Deployment already at ${RESTORE_REPLICAS} replica(s), rollout available"
      exit 0
    fi
    run "Scale worker Deployment to ${RESTORE_REPLICAS} replica(s)" \
      kubectl scale "deployment/$DEPLOYMENT" -n "$NAMESPACE" --replicas="$RESTORE_REPLICAS"
    run "Verify the worker rollout is available" \
      kubectl rollout status "deployment/$DEPLOYMENT" -n "$NAMESPACE" --timeout="${TIMEOUT_SECONDS}s"
    step "Worker capacity restored: ${RESTORE_REPLICAS} replica(s)"
  fi
}

do_status() {
  deployment_exists || abort "deployment/$DEPLOYMENT not found in namespace $NAMESPACE"

  step "Worker state in namespace $NAMESPACE"
  local keda=0
  keda_managed && keda=1

  if [ "$keda" -eq 1 ]; then
    echo "    scaling:     KEDA ScaledObject '$DEPLOYMENT'"
    if keda_paused; then
      echo "    keda pause:  PAUSED (a KEDA pause annotation is set)"
    else
      echo "    keda pause:  not paused"
    fi
  else
    local reps
    reps="$(deployment_replicas)"
    [ -n "$reps" ] || reps="<unset>"
    echo "    scaling:     manual, spec.replicas=$reps"
  fi
  local pod_count
  if pod_count="$(worker_pod_count)"; then
    echo "    pods:        ${pod_count}"
  else
    echo "    pods:        <read error: kubectl get pods failed>"
  fi
  local ready_count
  if ready_count="$(worker_ready_count)"; then
    echo "    ready pods:  ${ready_count}"
  else
    echo "    ready pods:  <read error: kubectl get pods failed>"
  fi
  local names
  names="$(worker_pod_names)"
  for p in $names; do
    echo "      - $p"
  done
}

case "$COMMAND" in
  drain)   do_drain ;;
  restore) do_restore ;;
  status)  do_status ;;
esac
