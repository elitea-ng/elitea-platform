#!/usr/bin/env bash
# Test suite for deploy/scripts/drain-workers.sh.
#
# Every case runs the script against a stateful kubectl stub placed on
# PATH. The stub keeps its state in a per-case directory. It models a
# manual worker Deployment and a KEDA ScaledObject fleet. The pod list
# always follows the fleet owner. The fleet owner is spec.replicas or
# the ScaledObject paused-replicas or minReplicaCount. The suite needs
# no cluster.
#
# Usage: deploy/tests/drain-workers-test.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
TARGET="$REPO_ROOT/deploy/scripts/drain-workers.sh"

PASS_COUNT=0
FAIL_COUNT=0

ok() { PASS_COUNT=$((PASS_COUNT + 1)); echo "  ok: $1"; }
bad() { FAIL_COUNT=$((FAIL_COUNT + 1)); echo "  FAIL: $1"; }

assert_rc() { # assert_rc <want> <got> <desc>
  if [ "$2" -eq "$1" ]; then
    ok "$3 (rc=$2)"
  else
    bad "$3 (want rc=$1, got rc=$2)"
  fi
}

assert_has() { # assert_has <needle> <haystack> <desc>
  case "$2" in
    *"$1"*) ok "$3" ;;
    *) bad "$3 (output lacks: $1)" ;;
  esac
}

assert_lacks() { # assert_lacks <needle> <haystack> <desc>
  case "$2" in
    *"$1"*) bad "$3 (output has: $1)" ;;
    *) ok "$3" ;;
  esac
}

write_stub() { # write_stub <dir>
  cat > "$1/kubectl" <<'STUB'
#!/usr/bin/env bash
# Stateful kubectl stub for the drain-workers.sh test suite.
#
# State files in $STUB_STATE_DIR:
#   deployment_replicas          manual fleet replica count
#   scaledobject                 present = KEDA fleet (holds the name)
#   minreplicas                  ScaledObject spec.minReplicaCount
#   annotations                  ScaledObject annotations, key=value lines
#   keep_pods                    one pod that never terminates (0/1)
#   fail_pod_reads               every "kubectl get pods" fails
#   fail_pod_reads_after_scale   "kubectl scale" arms fail_pod_reads
#   fail_pod_reads_after_annotate "kubectl annotate" arms fail_pod_reads
#   deployment_missing           "get deployment/*" answers NotFound
set -u

STATE="${STUB_STATE_DIR:?STUB_STATE_DIR must name a state dir}"

val() {
  cat "$STATE/$1" 2>/dev/null
}

effective_replicas() {
  local n
  if [ -f "$STATE/scaledobject" ]; then
    n="$(grep '^autoscaling.keda.sh/paused-replicas=' "$STATE/annotations" 2>/dev/null | cut -d= -f2-)"
    if [ -n "$n" ]; then
      echo "$n"
      return
    fi
    n="$(val minreplicas)"
  else
    n="$(val deployment_replicas)"
  fi
  [ -n "$n" ] || n=0
  echo "$n"
}

pod_names() {
  local n i out=""
  if [ -f "$STATE/keep_pods" ]; then
    echo "elitea-worker-stuck"
    return
  fi
  n="$(effective_replicas)"
  [ "$n" -ge 1 ] || return
  for i in $(seq 0 $((n - 1))); do
    out="$out elitea-worker-$i"
  done
  echo "${out# }"
}

pod_rows() {
  local n i
  if [ -f "$STATE/keep_pods" ]; then
    echo "elitea-worker-stuck   0/1   Terminating   0   5m"
    return
  fi
  n="$(effective_replicas)"
  [ "$n" -ge 1 ] || return
  for i in $(seq 0 $((n - 1))); do
    echo "elitea-worker-$i   1/1   Running   0   5m"
  done
}

fail_pod_read() {
  echo "The connection to the server localhost:8080 was refused - did you specify the right host or port?" >&2
  exit 1
}

[ $# -ge 1 ] || { echo "stub: no subcommand" >&2; exit 1; }
CMD="$1"
shift

case "$CMD" in
  get)
    res=""
    out=""
    noheaders=0
    while [ $# -gt 0 ]; do
      case "$1" in
        -o) out="$2"; shift 2 ;;
        --no-headers) noheaders=1; shift ;;
        -n) shift 2 ;;
        -l) shift 2 ;;
        -*) shift ;;
        *) res="$1"; shift ;;
      esac
    done
    case "$res" in
      deployment/*)
        if [ ! -f "$STATE/deployment_replicas" ] && [ -f "$STATE/deployment_missing" ]; then
          echo 'Error from server (NotFound): the server did not find the requested deployment' >&2
          exit 1
        fi
        case "$out" in
          *jsonpath=*) val deployment_replicas ;;
        esac
        exit 0
        ;;
      pods)
        [ -f "$STATE/fail_pod_reads" ] && fail_pod_read
        if [ -n "$out" ]; then
          pod_names
          exit 0
        fi
        if [ "$noheaders" -eq 1 ]; then
          pod_rows
        fi
        exit 0
        ;;
      scaledobject/*)
        [ -f "$STATE/scaledobject" ] || {
          echo 'Error from server (NotFound): the server did not find the requested scaledobject' >&2
          exit 1
        }
        case "$out" in
          *metadata.annotations*)
            json="{"
            first=1
            if [ -f "$STATE/annotations" ]; then
              while IFS='=' read -r k v; do
                [ "$first" -eq 1 ] || json="$json,"
                json="$json\"$k\":\"$v\""
                first=0
              done < "$STATE/annotations"
            fi
            json="$json}"
            echo "$json"
            ;;
          *metadata.name*) cat "$STATE/scaledobject" ;;
          *spec.minReplicaCount*) val minreplicas ;;
        esac
        exit 0
        ;;
      *)
        echo "stub: unsupported get resource '$res'" >&2
        exit 1
        ;;
    esac
    ;;
  annotate)
    while [ $# -gt 0 ]; do
      case "$1" in
        scaledobject/*) shift ;;
        -n) shift 2 ;;
        --overwrite) shift ;;
        *=*)
          key="${1%%=*}"
          v="${1#*=}"
          grep -v "^$key=" "$STATE/annotations" 2>/dev/null > "$STATE/annotations.tmp"
          echo "$key=$v" >> "$STATE/annotations.tmp"
          mv "$STATE/annotations.tmp" "$STATE/annotations"
          shift
          ;;
        *-*)
          key="${1%-}"
          if grep -q "^$key=" "$STATE/annotations" 2>/dev/null; then
            grep -v "^$key=" "$STATE/annotations" > "$STATE/annotations.tmp"
            mv "$STATE/annotations.tmp" "$STATE/annotations"
          fi
          shift
          ;;
        *) shift ;;
      esac
    done
    if [ -f "$STATE/fail_pod_reads_after_annotate" ]; then
      touch "$STATE/fail_pod_reads"
    fi
    exit 0
    ;;
  scale)
    reps=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --replicas=*) reps="${1#--replicas=}"; shift ;;
        --replicas) reps="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ -n "$reps" ] || { echo "stub: scale without --replicas" >&2; exit 1; }
    echo "$reps" > "$STATE/deployment_replicas"
    if [ -f "$STATE/fail_pod_reads_after_scale" ]; then
      touch "$STATE/fail_pod_reads"
    fi
    exit 0
    ;;
  rollout)
    exit 0
    ;;
  *)
    echo "stub: unsupported command '$CMD'" >&2
    exit 1
    ;;
esac
STUB
  chmod +x "$1/kubectl"
}

setup() {
  STATE_DIR="$(mktemp -d)"
  STUB_DIR="$(mktemp -d)"
  write_stub "$STUB_DIR"
  : > "$STATE_DIR/annotations"
  export STUB_STATE_DIR="$STATE_DIR"
  export PATH="$STUB_DIR:$PATH"
}

teardown() {
  rm -rf "$STATE_DIR" "$STUB_DIR"
}

manual_fleet() { # manual_fleet <replicas>
  echo "$1" > "$STATE_DIR/deployment_replicas"
}

keda_fleet() { # keda_fleet <minReplicaCount|- > [annotation=...]...
  echo "elitea-worker" > "$STATE_DIR/scaledobject"
  if [ "$1" != "-" ]; then
    echo "$1" > "$STATE_DIR/minreplicas"
  fi
  shift
  while [ $# -gt 0 ]; do
    echo "$1" >> "$STATE_DIR/annotations"
    shift
  done
}

run_script() { # run_script <args...> -> OUT / RC
  OUT="$(bash "$TARGET" "$@" 2>&1)"
  RC=$?
}

# ---- manual fleet ----------------------------------------------------------

test_manual_drain_and_restore() {
  echo "manual_drain_and_restore"
  setup
  manual_fleet 2
  run_script drain
  assert_rc 0 "$RC" "drain exits 0"
  assert_has "Scale worker Deployment to 0 replicas" "$OUT" "drain scales the fleet to 0"
  assert_has "Workers drained" "$OUT" "drain reports completion"
  [ "$(cat "$STATE_DIR/deployment_replicas")" = "0" ] && ok "state: replicas=0" || bad "state: replicas=0"
  run_script restore
  assert_rc 0 "$RC" "restore exits 0"
  assert_has "Scale worker Deployment to 1 replica(s)" "$OUT" "restore scales the fleet to 1"
  assert_has "Worker capacity restored: 1 replica(s)" "$OUT" "restore reports completion"
  [ "$(cat "$STATE_DIR/deployment_replicas")" = "1" ] && ok "state: replicas=1" || bad "state: replicas=1"
  teardown
}

test_manual_noops() {
  echo "manual_noops"
  setup
  manual_fleet 0
  run_script drain
  assert_rc 0 "$RC" "drain exits 0"
  assert_has "Already drained" "$OUT" "drain is a no-op at 0 replicas with no pods"
  teardown
  setup
  manual_fleet 1
  run_script restore
  assert_rc 0 "$RC" "restore exits 0"
  assert_has "No-op: worker Deployment already at 1 replica(s), rollout available" "$OUT" "restore is a no-op at the target"
  teardown
}

# ---- KEDA fleet -------------------------------------------------------------

test_keda_drain_and_restore() {
  echo "keda_drain_and_restore"
  setup
  keda_fleet 1
  run_script drain
  assert_rc 0 "$RC" "drain exits 0"
  assert_has "Pause KEDA scaling and hold the worker fleet at 0 replicas" "$OUT" "drain pauses the ScaledObject"
  grep -q '^autoscaling.keda.sh/paused=true$' "$STATE_DIR/annotations" && ok "state: paused=true set" || bad "state: paused=true set"
  grep -q '^autoscaling.keda.sh/paused-replicas=0$' "$STATE_DIR/annotations" && ok "state: paused-replicas=0 set" || bad "state: paused-replicas=0 set"
  run_script restore
  assert_rc 0 "$RC" "restore exits 0"
  assert_has "Resume KEDA scaling (remove the pause annotations)" "$OUT" "restore removes the pause annotations"
  [ ! -s "$STATE_DIR/annotations" ] && ok "state: annotations empty" || bad "state: annotations empty"
  assert_has "Worker capacity restored: 1 Ready pod(s)" "$OUT" "restore waits for the minReplicaCount floor"
  teardown
}

test_keda_drain_idempotent() {
  echo "keda_drain_idempotent"
  setup
  keda_fleet 1 "autoscaling.keda.sh/paused=true" "autoscaling.keda.sh/paused-replicas=0"
  run_script drain
  assert_rc 0 "$RC" "drain exits 0"
  assert_has "Already drained: KEDA scaling is paused and no worker pod remains" "$OUT" "drain is a no-op when paused with no pods"
  teardown
}

test_keda_paused_alone() {
  echo "keda_paused_alone"
  setup
  keda_fleet 1 "autoscaling.keda.sh/paused=true"
  run_script status
  assert_rc 0 "$RC" "status exits 0"
  assert_has "keda pause:  PAUSED" "$OUT" "status reports paused for paused=true alone"
  run_script restore
  assert_rc 0 "$RC" "restore exits 0"
  assert_has "Resume KEDA scaling (remove the pause annotations)" "$OUT" "restore removes the pause"
  [ ! -s "$STATE_DIR/annotations" ] && ok "state: annotations empty (absent key removal is a no-op)" || bad "state: annotations empty"
  assert_has "Worker capacity restored: 1 Ready pod(s)" "$OUT" "restore waits for the minReplicaCount floor"
  teardown
}

test_keda_restore_minReplicaCount_default_0() {
  echo "keda_restore_minReplicaCount_default_0"
  setup
  keda_fleet - "autoscaling.keda.sh/paused=true" "autoscaling.keda.sh/paused-replicas=0"
  run_script restore
  assert_rc 0 "$RC" "restore exits 0 with the KEDA default floor"
  assert_has "Wait for at least 0 Ready worker pod(s)" "$OUT" "restore defaults a missing minReplicaCount to 0"
  assert_has "Worker capacity restored: 0 Ready pod(s)" "$OUT" "restore completes at the 0 floor"
  assert_lacks "spec.minReplicaCount is empty" "$OUT" "restore does not abort on an omitted minReplicaCount"
  [ ! -s "$STATE_DIR/annotations" ] && ok "state: annotations empty" || bad "state: annotations empty"
  teardown
}

# ---- status -----------------------------------------------------------------

test_status_manual_and_paused() {
  echo "status_manual_and_paused"
  setup
  manual_fleet 2
  run_script status
  assert_rc 0 "$RC" "status exits 0"
  assert_has "manual, spec.replicas=2" "$OUT" "status reports the manual fleet"
  assert_has "pods:        2" "$OUT" "status counts the worker pods"
  assert_has "ready pods:  2" "$OUT" "status counts the ready pods"
  teardown
  setup
  keda_fleet 1 "autoscaling.keda.sh/paused=true" "autoscaling.keda.sh/paused-replicas=0"
  run_script status
  assert_rc 0 "$RC" "status exits 0"
  assert_has "KEDA ScaledObject 'elitea-worker'" "$OUT" "status reports the KEDA fleet"
  assert_has "keda pause:  PAUSED" "$OUT" "status reports the pause"
  assert_has "pods:        0" "$OUT" "status counts 0 pods while paused at 0"
  teardown
}

test_status_pod_read_error_marker() {
  echo "status_pod_read_error_marker"
  setup
  manual_fleet 2
  touch "$STATE_DIR/fail_pod_reads"
  run_script status
  assert_rc 0 "$RC" "status exits 0 with a failed pod read"
  assert_has "pods:        <read error: kubectl get pods failed>" "$OUT" "status shows the read-error marker"
  assert_lacks "pods:        2" "$OUT" "status does not print a pod count on a failed read"
  teardown
}

# ---- dry-run ----------------------------------------------------------------

test_dry_run() {
  echo "dry_run"
  setup
  manual_fleet 2
  run_script drain --dry-run
  assert_rc 0 "$RC" "drain --dry-run exits 0"
  assert_has "dry-run: kubectl scale" "$OUT" "dry-run prints the scale step"
  [ "$(cat "$STATE_DIR/deployment_replicas")" = "2" ] && ok "state: replicas unchanged" || bad "state: replicas unchanged"
  teardown
  setup
  keda_fleet 1
  run_script drain --dry-run
  assert_rc 0 "$RC" "keda drain --dry-run exits 0"
  assert_has "dry-run: kubectl annotate" "$OUT" "dry-run prints the annotate step"
  [ ! -s "$STATE_DIR/annotations" ] && ok "state: annotations untouched" || bad "state: annotations untouched"
  teardown
}

# ---- safety wait ------------------------------------------------------------

test_safety_wait_aborts_on_stuck_pods() {
  echo "safety_wait_aborts_on_stuck_pods"
  setup
  manual_fleet 2
  touch "$STATE_DIR/keep_pods"
  run_script drain --timeout 6
  assert_rc 1 "$RC" "drain aborts when a pod never terminates"
  assert_has "worker pod(s) still present after 6s" "$OUT" "abort names the stuck pods"
  teardown
}

test_drain_aborts_when_pod_reads_fail() {
  echo "drain_aborts_when_pod_reads_fail"
  setup
  manual_fleet 2
  touch "$STATE_DIR/fail_pod_reads_after_scale"
  run_script drain --timeout 6
  assert_rc 1 "$RC" "drain aborts when kubectl get pods keeps failing"
  assert_has "kubectl get pods kept failing" "$OUT" "abort names the API failure"
  assert_has "kubectl get pods failed; retrying in 5s" "$OUT" "the wait retries the failed read"
  assert_lacks "Safety wait passed" "$OUT" "the safety wait never passes"
  teardown
}

test_drain_aborts_on_pre_read_failure() {
  echo "drain_aborts_on_pre_read_failure"
  setup
  manual_fleet 2
  touch "$STATE_DIR/fail_pod_reads"
  run_script drain
  assert_rc 1 "$RC" "drain aborts before any mutation on a failed pod read"
  assert_has "cannot read worker pods in namespace elitea: kubectl get pods failed" "$OUT" "abort names the read failure"
  [ "$(cat "$STATE_DIR/deployment_replicas")" = "2" ] && ok "state: replicas unchanged" || bad "state: replicas unchanged"
  [ ! -s "$STATE_DIR/annotations" ] && ok "state: no annotation set" || bad "state: no annotation set"
  teardown
}

test_restore_aborts_when_ready_reads_fail() {
  echo "restore_aborts_when_ready_reads_fail"
  setup
  keda_fleet 1 "autoscaling.keda.sh/paused=true" "autoscaling.keda.sh/paused-replicas=0"
  touch "$STATE_DIR/fail_pod_reads_after_annotate"
  run_script restore --timeout 6
  assert_rc 1 "$RC" "restore aborts when the ready read keeps failing"
  assert_has "kubectl get pods kept failing; cannot prove that 1 Ready worker pod(s) are present" "$OUT" "abort is the distinct ready-read failure message"
  assert_lacks "only 0 Ready" "$OUT" "abort is not the ready-count message"
  [ ! -s "$STATE_DIR/annotations" ] && ok "state: annotations removed (restore reached the wait)" || bad "state: annotations removed"
  teardown
}

test_drain_aborts_when_deployment_missing() {
  echo "drain_aborts_when_deployment_missing"
  setup
  touch "$STATE_DIR/deployment_missing"
  run_script drain
  assert_rc 1 "$RC" "drain aborts when the Deployment is missing"
  assert_has "deployment/elitea-worker not found in namespace elitea" "$OUT" "abort is the deployment-missing message"
  teardown
}

# ---- main -------------------------------------------------------------------

test_manual_drain_and_restore
test_manual_noops
test_keda_drain_and_restore
test_keda_drain_idempotent
test_keda_paused_alone
test_keda_restore_minReplicaCount_default_0
test_status_manual_and_paused
test_status_pod_read_error_marker
test_dry_run
test_safety_wait_aborts_on_stuck_pods
test_drain_aborts_when_pod_reads_fail
test_drain_aborts_on_pre_read_failure
test_restore_aborts_when_ready_reads_fail
test_drain_aborts_when_deployment_missing

echo ""
echo "drain-workers-test: $PASS_COUNT passed, $FAIL_COUNT failed"
[ "$FAIL_COUNT" -eq 0 ]
