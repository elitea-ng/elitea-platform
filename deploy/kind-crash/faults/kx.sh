#!/usr/bin/env bash
# KX fault commands for the kind crash cluster. One function per scenario.
# Usage: deploy/kind-crash/faults/kx.sh <kx01|kx03|kx03-restore|kx04|kx04-restore|kx05|kx06-on|kx06-off|kx07|kx08|kx09|kx11|kx12|crictl-restart|np-probe>
# Nothing here runs unless named. The harness (crashctl) times and records each call;
# this file is the plain command list. Destructive: kx04, kx08.
set -euo pipefail
CLUSTER=elitea-crash
CTX="kind-${CLUSTER}"
NS="${CRASH_NAMESPACE:-elitea}"
WORKER_NODE="${CLUSTER}-worker"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
k() { kubectl --context "$CTX" -n "$NS" "$@"; }

# component=worker, not name=elitea-worker-python: the platform edge shares that name label.
kx01() { k delete pod -l app.kubernetes.io/component=worker --grace-period=0 --force; }
kx03() { kubectl --context "$CTX" drain "$WORKER_NODE" --ignore-daemonsets --delete-emptydir-data --grace-period=30 --timeout=120s; }
kx03_restore() { kubectl --context "$CTX" uncordon "$WORKER_NODE"; }
kx04() { docker kill "$WORKER_NODE"; }
kx04_restore() { docker start "$WORKER_NODE"; }
kx05() { k rollout restart deploy/elitea-sandbox-supervisor; }
kx06_on() { kubectl --context "$CTX" apply -f "${HERE}/deny-worker-nats.yaml"; }
kx06_off() { kubectl --context "$CTX" delete -f "${HERE}/deny-worker-nats.yaml"; }
kx07() { k delete pod elitea-nats-0; }   # PVC kept
# KX-08: DESTRUCTIVE. Deletes the NATS pod and its JetStream PVC. The PVC name
# (data-... / elitea-nats-js-...) is the upstream chart's: list it first.
kx08() {
  k get pvc -l app.kubernetes.io/name=nats
  read -r -p "PVC name to delete: " pvc
  k delete pvc "$pvc" --wait=false
  k delete pod elitea-nats-0
}
kx09() {
  local pod
  pod="$(k get pod -l app.kubernetes.io/name=elitea-main -o jsonpath='{.items[0].metadata.name}')"
  k delete pod "$pod"
}
kx11() { k delete pod postgres-0; }      # PVC-backed StatefulSet, deploy/kind-crash/manifests/postgres.yaml
# KX-12: reuses the existing live test; it deletes the Supervisor pod while a sandbox pod runs.
kx12() { python3 "${REPO_ROOT}/scripts/runtime/test_kubernetes_supervisor_recovery.py" --context "$CTX" --namespace "$NS" --worker-material "${CRASH_WORKER_MATERIAL:?directory with the worker client material}"; }
# Container-only restart (keeps the pod, so the emptyDir spool survives; D2). kind nodes are
# containers running containerd, so crictl runs inside the node container.
crictl_restart() {
  local pod="${1:?pod name}" cid
  cid="$(docker exec "$WORKER_NODE" crictl ps --name worker -q | head -1)"
  [ -n "$cid" ] || { docker exec "$WORKER_NODE" crictl ps; echo "no container matched; pick one and run: docker exec $WORKER_NODE crictl stop <id>" >&2; return 1; }
  echo "pod ${pod}: stopping container ${cid} (kubelet restarts it)"
  docker exec "$WORKER_NODE" crictl stop "$cid"
}
# Does the CNI enforce NetworkPolicy? Run before KX-06. Expect the second wget to FAIL.
np_probe() {
  k run np-probe-srv --image=docker.io/library/busybox:1.36 --restart=Never --labels=probe=srv -- sh -c 'echo ok > /tmp/i; httpd -f -p 8080 -h /tmp'
  k wait --for=condition=Ready pod/np-probe-srv --timeout=60s
  cat <<YAML | k apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: np-probe-deny}
spec: {podSelector: {matchLabels: {probe: srv}}, policyTypes: [Ingress]}
YAML
  local ip; ip="$(k get pod np-probe-srv -o jsonpath='{.status.podIP}')"
  k run np-probe-cli --rm -i --restart=Never --image=docker.io/library/busybox:1.36 -- wget -T 3 -qO- "http://${ip}:8080" \
    && echo "NOT ENFORCED: KX-06 needs Calico" || echo "ENFORCED: the connection was denied"
  k delete networkpolicy np-probe-deny pod/np-probe-srv --ignore-not-found
}

case "${1:-}" in
  kx01) kx01 ;; kx03) kx03 ;; kx03-restore) kx03_restore ;; kx04) kx04 ;; kx04-restore) kx04_restore ;;
  kx05) kx05 ;; kx06-on) kx06_on ;; kx06-off) kx06_off ;; kx07) kx07 ;; kx08) kx08 ;;
  kx09) kx09 ;; kx11) kx11 ;; kx12) kx12 ;; crictl-restart) crictl_restart "${2:-}" ;; np-probe) np_probe ;;
  *) sed -n 2,5p "${BASH_SOURCE[0]}" >&2; exit 2 ;;
esac
